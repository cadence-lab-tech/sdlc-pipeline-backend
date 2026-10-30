package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/httpapi"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// clab wait (CLAB-323, AC-10…AC-18) на настоящем сервере и на подставных:
// исходы и коды, актор решения, min_approvals и устаревание апрува (0042),
// свой цикл под чужими ключами, старый сервер без ручки, сбои сети.

const waitRoot = "bootstrap-token-clab-323-0123456789"

// fastTiming — сроки для тестов: паузы короткие, долгий опрос — как в бою.
var fastTiming = waitTiming{longPoll: 25 * time.Second, gap: 50 * time.Millisecond,
	poll: 300 * time.Millisecond, retryFirst: 50 * time.Millisecond, retryMax: 200 * time.Millisecond, recheck: 4}

type dbCounter struct{ n atomic.Int64 }

func (d *dbCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	d.n.Add(1)
	return ctx
}
func (d *dbCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type waitWorld struct {
	t        *testing.T
	store    *postgres.Store
	url      string
	db       *dbCounter
	requests atomic.Int64
}

func newWaitWorld(t *testing.T) *waitWorld {
	t.Helper()
	testdb.Store(t)
	w := &waitWorld{t: t, db: &dbCounter{}}
	pc, err := pgxpool.ParseConfig(testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	pc.ConnConfig.Tracer = w.db
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	w.store = postgres.New(pool)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(w.store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(w.store, log, workflow.AuthToken)
	srv := httpapi.NewServer(config.Config{Env: "test", RequestTimeout: 30 * time.Second}, log, w.store, engine,
		workflow.NewAuthoring(w.store, log, engine), auth, nil)
	if err := auth.Bootstrap(context.Background(), "root", waitRoot); err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.requests.Add(1)
		h.ServeHTTP(rw, r)
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(srv.StopWaiting)
	w.url = ts.URL
	return w
}

func (w *waitWorld) call(method, path string, body any, want int) map[string]any {
	w.t.Helper()
	return w.callAs(waitRoot, method, path, body, want)
}

func (w *waitWorld) callAs(token, method, path string, body any, want int) map[string]any {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, w.url+path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if want != 0 && resp.StatusCode != want {
		w.t.Fatalf("%s %s: %d %v, ждали %d", method, path, resp.StatusCode, out, want)
	}
	return out
}

func (w *waitWorld) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.store.DB().Exec(context.Background(), sql, args...); err != nil {
		w.t.Fatal(err)
	}
}

func (w *waitWorld) client() *Client {
	return &Client{base: w.url + "/v1", origin: w.url, token: waitRoot, http: &http.Client{Timeout: 60 * time.Second}}
}

// start ставит задачу в работу и забирает её: на analysis ждёт гейт
// human_approval перехода analysis_done.
func (w *waitWorld) start(key string) {
	w.t.Helper()
	w.call("POST", "/v1/tasks/"+key+"/transitions/start", nil, http.StatusOK)
	w.call("POST", "/v1/tasks/"+key+"/claim", map[string]any{}, http.StatusOK)
}

func (w *waitWorld) decide(key, transition, gate, status string, details any) {
	w.t.Helper()
	body := map[string]any{"status": status}
	if details != nil {
		body["details"] = details
	}
	w.call("POST", "/v1/tasks/"+key+"/transitions/"+transition+"/gates/"+gate+"/decision", body, http.StatusOK)
}

type waitRun struct {
	res  waitResult
	code int
	err  error
	took time.Duration
}

// wait запускает ожидание фоном и отдаёт канал с его исходом.
func (w *waitWorld) wait(o waitOptions) <-chan waitRun {
	if o.timing == (waitTiming{}) {
		o.timing = fastTiming
	}
	out := make(chan waitRun, 1)
	go func() {
		start := time.Now()
		res, code, err := runWait(context.Background(), w.client(), o)
		out <- waitRun{res, code, err, time.Since(start)}
	}()
	return out
}

func still(t *testing.T, ch <-chan waitRun, d time.Duration, what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s: ожидание вышло раньше времени: %+v", what, r)
	case <-time.After(d):
	}
}

func recv(t *testing.T, ch <-chan waitRun, within time.Duration, what string) waitRun {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatalf("%s: ожидание не вышло за %v", what, within)
		return waitRun{}
	}
}

// AC-10, AC-11, AC-13 (уже решено, нечего ждать), AC-18 (без обёртки
// heartbeat ожидание аренду не трогает).
func TestWaitApprovalPassedAndReturned(t *testing.T) {
	w := newWaitWorld(t)
	w.start("SDLC-1")

	before := w.leaseEvents("SDLC-1")
	ch := w.wait(waitOptions{task: "SDLC-1", gate: "human_approval", transition: "analysis_done", timeout: 2 * time.Minute})
	still(t, ch, 1500*time.Millisecond, "апрува ещё нет")
	w.decide("SDLC-1", "analysis_done", "human_approval", "passed", nil)
	decided := time.Now()
	r := recv(t, ch, 5*time.Second, "апрув")
	if lag := time.Since(decided); lag > 3*time.Second {
		t.Errorf("вышло через %v после решения, ждали ≤ 3 с", lag)
	}
	if r.err != nil || r.code != waitPassed || r.res.Outcome != "passed" || r.res.Transition != "analysis_done" ||
		r.res.Gate == nil || r.res.Gate.Type != "human_approval" || r.res.Gate.Status != "passed" ||
		r.res.TransitionDone || r.res.Actor == nil || *r.res.Actor != "root" || r.res.DecidedAt == nil || r.res.Mode != "events" {
		t.Fatalf("исход апрува: %+v код %d %v", r.res, r.code, r.err)
	}
	line := r.res.line()
	for _, part := range []string{"SDLC-1", "passed", "analysis_done", "root"} {
		if !strings.Contains(line, part) {
			t.Errorf("строка исхода без %q: %s", part, line)
		}
	}
	if w.leaseEvents("SDLC-1") != before {
		t.Errorf("clab wait без heartbeat продлевал аренду")
	}

	// Решение уже есть на старте — исход сразу.
	ch = w.wait(waitOptions{task: "SDLC-1", gate: "human_approval", timeout: time.Minute})
	if r := recv(t, ch, 2*time.Second, "уже решено"); r.code != waitPassed {
		t.Fatalf("уже решено: %+v %d %v", r.res, r.code, r.err)
	}

	// AC-11: отклонение с причиной — код 3, причина и актор.
	w.start("SDLC-2")
	ch = w.wait(waitOptions{task: "SDLC-2", gate: "human_approval", transition: "analysis_done", timeout: time.Minute})
	still(t, ch, 500*time.Millisecond, "отклонения ещё нет")
	w.decide("SDLC-2", "analysis_done", "human_approval", "failed", map[string]any{"reason": "нет AC по ошибкам"})
	r = recv(t, ch, 5*time.Second, "отклонение")
	if r.code != waitReturned || r.res.Outcome != "returned" || r.res.Reason != "нет AC по ошибкам" ||
		r.res.Actor == nil || *r.res.Actor != "root" {
		t.Fatalf("отклонение: %+v %d %v", r.res, r.code, r.err)
	}

	// AC-13: ждать нечего — код 2 с перечнем переходов.
	for _, o := range []waitOptions{
		{task: "SDLC-2", gate: "sign_off"},
		{task: "SDLC-2", gate: "human_approval", transition: "nope"},
	} {
		o.timing = fastTiming
		_, code, err := runWait(context.Background(), w.client(), o)
		if code != waitUsage || err == nil || !strings.Contains(err.Error(), "analysis_done") {
			t.Errorf("ждать нечего %+v: код %d %v", o, code, err)
		}
	}
}

func (w *waitWorld) leaseEvents(key string) int {
	var n int
	if err := w.store.DB().QueryRow(context.Background(), `
		SELECT count(*) FROM task_events e JOIN tasks t ON t.id = e.task_id
		WHERE t.key = $1 AND e.type = 'lease.extended'`, key).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// AC-12: возврат переходом reject — код 3 с комментарием; отмена — 4;
// advance, которого не ждали, — 4.
func TestWaitTransitions(t *testing.T) {
	w := newWaitWorld(t)
	// Второй advance из analysis: без него «не тот advance» не проверить.
	w.exec(`
		INSERT INTO workflow_transitions (workflow_id, key, name, from_status_id, to_status_id, kind, position)
		SELECT tr.workflow_id, 'fast_track', 'Сразу в разработку', tr.from_status_id, dev.id, 'advance', 7
		FROM workflow_transitions tr
		JOIN workflow_statuses dev ON dev.workflow_id = tr.workflow_id AND dev.key = 'development'
		WHERE tr.key = 'analysis_done'`)

	// Задача на architecture: гейты analysis_done сняты человеком.
	w.start("SDLC-1")
	for _, g := range []string{"artifacts", "peer_review", "human_approval"} {
		w.decide("SDLC-1", "analysis_done", g, "waived", nil)
	}
	w.call("POST", "/v1/tasks/SDLC-1/transitions/analysis_done", nil, http.StatusOK)
	w.call("POST", "/v1/tasks/SDLC-1/claim", map[string]any{}, http.StatusOK)
	ch := w.wait(waitOptions{task: "SDLC-1", gate: "human_approval", timeout: time.Minute})
	still(t, ch, 500*time.Millisecond, "архитектура")
	// Гейт возврата снят: решение по гейту — тоже событие, но исход оно
	// не меняет (гейт не тот, что ждём).
	w.decide("SDLC-1", "reject_to_analysis", "problem_report", "waived", nil)
	still(t, ch, 500*time.Millisecond, "снят гейт возврата")
	w.call("POST", "/v1/tasks/SDLC-1/transitions/reject_to_analysis", map[string]any{"comment": "нет схемы данных"}, http.StatusOK)
	r := recv(t, ch, 5*time.Second, "reject")
	if r.code != waitReturned || r.res.Reason != "нет схемы данных" || r.res.Status.To != "analysis" ||
		r.res.Transition != "reject_to_analysis" || r.res.Actor == nil {
		t.Fatalf("reject: %+v %d %v", r.res, r.code, r.err)
	}

	// Отмена — status_changed.
	w.start("SDLC-2")
	ch = w.wait(waitOptions{task: "SDLC-2", gate: "human_approval", timeout: time.Minute})
	still(t, ch, 300*time.Millisecond, "отмена")
	w.call("POST", "/v1/tasks/SDLC-2/transitions/cancel", nil, http.StatusOK)
	if r := recv(t, ch, 5*time.Second, "cancel"); r.code != waitStatusChanged || r.res.Outcome != "status_changed" {
		t.Fatalf("cancel: %+v %d %v", r.res, r.code, r.err)
	}

	// advance, которого не ждали.
	w.start("SDLC-3")
	ch = w.wait(waitOptions{task: "SDLC-3", gate: "human_approval", transition: "analysis_done", timeout: time.Minute})
	still(t, ch, 300*time.Millisecond, "fast_track")
	w.call("POST", "/v1/tasks/SDLC-3/transitions/fast_track", nil, http.StatusOK)
	if r := recv(t, ch, 5*time.Second, "fast_track"); r.code != waitStatusChanged || r.res.Transition != "fast_track" {
		t.Fatalf("чужой advance: %+v %d %v", r.res, r.code, r.err)
	}

	// Без --gate: любой advance — passed с transition_done.
	created := w.call("POST", "/v1/projects/SDLC/tasks", map[string]any{"title": "ещё одна"}, http.StatusCreated)
	key := created["key"].(string)
	w.start(key)
	ch = w.wait(waitOptions{task: key, timeout: time.Minute})
	still(t, ch, 300*time.Millisecond, "без гейта")
	w.call("POST", "/v1/tasks/"+key+"/transitions/fast_track", nil, http.StatusOK)
	if r := recv(t, ch, 5*time.Second, "без гейта"); r.code != waitPassed || !r.res.TransitionDone {
		t.Fatalf("без гейта: %+v %d %v", r.res, r.code, r.err)
	}
}

// AC-13: срок — код 5 через 5–7 с.
func TestWaitTimeout(t *testing.T) {
	w := newWaitWorld(t)
	w.start("SDLC-1")
	ch := w.wait(waitOptions{task: "SDLC-1", gate: "human_approval", timeout: 5 * time.Second})
	r := recv(t, ch, 10*time.Second, "срок")
	if r.code != waitTimeout || r.res.Outcome != "timeout" || r.took < 5*time.Second || r.took > 7*time.Second {
		t.Fatalf("срок: %+v %d %v за %v", r.res, r.code, r.err, r.took)
	}
}

// AC-14: min_approvals 2 — после первого апрува ждём, после второго — 0;
// новая сдача после апрува делает его устаревшим (0042).
func TestWaitMinApprovalsAndStale(t *testing.T) {
	w := newWaitWorld(t)
	w.exec(`UPDATE gates SET config = '{"min_approvals":2}' WHERE key = 'human_approval'
		AND transition_id IN (SELECT id FROM workflow_transitions WHERE key = 'analysis_done')`)
	ctx := context.Background()
	if _, err := w.store.ResolveActor(ctx, w.store.DB(), "alice", domain.PrincipalUser); err != nil {
		t.Fatal(err)
	}
	w.call("PUT", "/v1/projects/SDLC/members", map[string]any{"principal": "user:alice", "role": "maintainer"}, http.StatusOK)
	alice := w.call("POST", "/v1/auth/tokens", map[string]any{"name": "alice", "principal": "user:alice", "project": "SDLC"}, http.StatusCreated)["token"].(string)

	w.start("SDLC-1")
	ch := w.wait(waitOptions{task: "SDLC-1", gate: "human_approval", timeout: time.Minute})
	w.decide("SDLC-1", "analysis_done", "human_approval", "passed", nil)
	still(t, ch, 2500*time.Millisecond, "первый апрув из двух")
	w.callAs(alice, "POST", "/v1/tasks/SDLC-1/transitions/analysis_done/gates/human_approval/decision",
		map[string]any{"status": "passed"}, http.StatusOK)
	if r := recv(t, ch, 5*time.Second, "второй апрув"); r.code != waitPassed || *r.res.Actor != "alice" {
		t.Fatalf("второй апрув: %+v %d %v", r.res, r.code, r.err)
	}

	// Устаревший апрув: апрув, затем новая сдача — гейт снова pending.
	w.exec(`UPDATE gates SET config = '{"min_approvals":1}' WHERE key = 'human_approval'`)
	w.start("SDLC-2")
	w.decide("SDLC-2", "analysis_done", "human_approval", "passed", nil)
	time.Sleep(10 * time.Millisecond)
	w.call("POST", "/v1/tasks/SDLC-2/artifacts", map[string]any{"kind": "analysis.brief", "title": "бриф", "content": "# бриф"}, http.StatusCreated)
	ch = w.wait(waitOptions{task: "SDLC-2", gate: "human_approval", timeout: time.Minute})
	still(t, ch, 2500*time.Millisecond, "устаревший апрув")
	w.decide("SDLC-2", "analysis_done", "human_approval", "passed", nil)
	if r := recv(t, ch, 5*time.Second, "свежий апрув"); r.code != waitPassed {
		t.Fatalf("свежий апрув: %+v %d %v", r.res, r.code, r.err)
	}
}

// AC-15: свой цикл под чужими ключами — переход design_ready и гейт
// sign_off типа human_approval; совпадение и по типу, и по ключу.
func TestWaitOwnCycleKeys(t *testing.T) {
	w := newWaitWorld(t)
	w.exec(`UPDATE workflow_transitions SET key = 'design_ready' WHERE key = 'analysis_done'`)
	w.exec(`UPDATE gates SET key = 'sign_off' WHERE key = 'human_approval'
		AND transition_id IN (SELECT id FROM workflow_transitions WHERE key = 'design_ready')`)
	for i, gate := range []string{"human_approval", "sign_off"} {
		key := fmt.Sprintf("SDLC-%d", i+1)
		w.start(key)
		ch := w.wait(waitOptions{task: key, gate: gate, timeout: time.Minute})
		still(t, ch, 300*time.Millisecond, gate)
		w.decide(key, "design_ready", "sign_off", "passed", nil)
		r := recv(t, ch, 5*time.Second, gate)
		if r.code != waitPassed || r.res.Transition != "design_ready" || r.res.Gate.Key != "sign_off" {
			t.Fatalf("--gate %s: %+v %d %v", gate, r.res, r.code, r.err)
		}
	}
}

// AC-15: в коде команды нет ключей статусов, переходов и ролей.
// transition_done — имя поля ответа из контракта, не ключ перехода.
func TestWaitHasNoCycleKeys(t *testing.T) {
	src, err := os.ReadFile("wait.go")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(src), "transition_done", "")
	text = strings.ReplaceAll(text, "TransitionDone", "")
	if m := regexp.MustCompile(`analysis|development|testing|merge|release|_done|_ready`).FindAllString(text, -1); len(m) > 0 {
		t.Fatalf("ключи цикла в wait.go: %v", m)
	}
}

// AC-16: сервер без маршрута событий — опрос карточки, mode poll;
// неизвестная задача — код 1, а не опрос.
func TestWaitOldServerPolls(t *testing.T) {
	w := newWaitWorld(t)
	w.start("SDLC-1")
	inner := w.url
	var cards atomic.Int64
	old := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			http.NotFound(rw, r) // так отвечает gin без маршрута
			return
		}
		if r.URL.Path == "/v1/tasks/SDLC-1" {
			cards.Add(1)
		}
		req, _ := http.NewRequest(r.Method, inner+r.URL.RequestURI(), r.Body)
		req.Header = r.Header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			rw.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		rw.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(rw, resp.Body)
	}))
	defer old.Close()
	c := &Client{base: old.URL + "/v1", origin: old.URL, token: waitRoot, http: old.Client()}

	out := make(chan waitRun, 1)
	go func() {
		res, code, err := runWait(context.Background(), c, waitOptions{task: "SDLC-1", gate: "human_approval", timeout: time.Minute, timing: fastTiming})
		out <- waitRun{res: res, code: code, err: err}
	}()
	still(t, out, time.Second, "старый сервер")
	if n := cards.Load(); n < 2 || n > 6 {
		t.Errorf("карточек за секунду при опросе раз в 300 мс: %d", n)
	}
	w.decide("SDLC-1", "analysis_done", "human_approval", "passed", nil)
	if r := recv(t, out, 3*time.Second, "опрос"); r.code != waitPassed || r.res.Mode != "poll" {
		t.Fatalf("опрос: %+v %d %v", r.res, r.code, r.err)
	}
	if defaultWaitTiming.poll != 15*time.Second {
		t.Errorf("опрос старого сервера — раз в %v, ждали 15 с", defaultWaitTiming.poll)
	}

	_, code, err := runWait(context.Background(), c, waitOptions{task: "SDLC-999", gate: "human_approval", timing: fastTiming})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "не найдена") {
		t.Fatalf("неизвестная задача: код %d %v", code, err)
	}
}

// AC-17: 503, обрыв соединения, затем событие — исход, а не ошибка; 401 —
// ошибка с кодом unauthenticated.
func TestWaitSurvivesFailures(t *testing.T) {
	var polls atomic.Int64
	var mu sync.Mutex
	gate := "pending"
	card := func() string {
		mu.Lock()
		defer mu.Unlock()
		return `{"task":{"key":"X-1","status_key":"s1"},
			"stage_runs":[{"id":"r1","kind":"stage","finished_at":null,"gate_results":[
				{"gate_key":"ok","transition_key":"t1","decided_by_handle":"bob","decided_at":"2026-10-30T10:00:00Z"}]}],
			"transitions":[{"key":"t1","kind":"advance","allowed":false,"gates":[{"gate_key":"ok","type":"human_approval","status":"` + gate + `"}]}]}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/tasks/X-1/events" && r.URL.Query().Get("after") == "":
			_, _ = io.WriteString(rw, `{"events":[],"cursor":"c0"}`)
		case r.URL.Path == "/v1/tasks/X-1/events":
			switch polls.Add(1) {
			case 1:
				rw.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(rw, `<html>503</html>`)
			case 2:
				conn, _, _ := rw.(http.Hijacker).Hijack()
				_ = conn.Close()
			default:
				mu.Lock()
				gate = "passed"
				mu.Unlock()
				_, _ = io.WriteString(rw, `{"events":[{"id":"e1","type":"gate.decided","task_key":"X-1","actor":"bob","created_at":"2026-10-30T10:00:00Z"}],"cursor":"c1"}`)
			}
		case r.URL.Path == "/v1/tasks/X-1":
			_, _ = io.WriteString(rw, card())
		default:
			http.NotFound(rw, r)
		}
	}))
	defer srv.Close()
	c := &Client{base: srv.URL + "/v1", origin: srv.URL, token: "t", http: srv.Client()}
	res, code, err := runWait(context.Background(), c, waitOptions{task: "X-1", gate: "human_approval", timeout: 30 * time.Second, timing: fastTiming})
	if err != nil || code != waitPassed || res.Actor == nil || *res.Actor != "bob" || polls.Load() != 3 {
		t.Fatalf("сбои: %+v %d %v, опросов %d", res, code, err, polls.Load())
	}

	unauth := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(rw, `{"error":{"code":"unauthenticated","message":"токен отозван"}}`)
	}))
	defer unauth.Close()
	c = &Client{base: unauth.URL + "/v1", origin: unauth.URL, token: "t", http: unauth.Client()}
	_, code, err = runWait(context.Background(), c, waitOptions{task: "X-1", gate: "human_approval", timing: fastTiming})
	var apiErr *APIError
	if code != 1 || err == nil || !errorsAs(err, &apiErr) || apiErr.Code != "unauthenticated" {
		t.Fatalf("401: код %d %v", code, err)
	}
}

func errorsAs(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestWaitLoad — замер для отчёта о тестах (требование человека к
// CLAB-323): запросы к API и к базе от N ждущих clab wait, когда решения
// нет. Включается CLAB_FEED_MEASURE=1. Старт ожидания (рукопожатие и
// карточка) считается отдельно от установившегося ожидания: окно в три
// минуты после прогрева, на минуту.
func TestWaitLoad(t *testing.T) {
	if os.Getenv("CLAB_FEED_MEASURE") == "" {
		t.Skip("CLAB_FEED_MEASURE не задан — замер пропущен")
	}
	w := newWaitWorld(t)
	keys := []string{}
	for i := 1; i <= 3; i++ {
		key := fmt.Sprintf("SDLC-%d", i)
		w.start(key)
		keys = append(keys, key)
	}
	warm, window := 30*time.Second, 3*time.Minute
	idleAPI, idleDB := w.requests.Load(), w.db.n.Load()
	time.Sleep(warm)
	t.Logf("N=0 за %v: API %d, база %d", warm, w.requests.Load()-idleAPI, w.db.n.Load()-idleDB)
	// CLAB_FEED_MEASURE_N=1,10,50 — свой набор N.
	ns := []int{1, 10, 50}
	if raw := os.Getenv("CLAB_FEED_MEASURE_N"); raw != "" {
		ns = nil
		for _, f := range strings.Split(raw, ",") {
			var n int
			if _, err := fmt.Sscan(f, &n); err == nil && n > 0 {
				ns = append(ns, n)
			}
		}
	}
	for _, n := range ns {
		api0, db0 := w.requests.Load(), w.db.n.Load()
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, code, err := runWait(context.Background(), w.client(), waitOptions{
					task: keys[i%len(keys)], gate: "human_approval", timeout: warm + window + 5*time.Second, timing: defaultWaitTiming})
				if code != waitTimeout || err != nil {
					t.Errorf("ожидание %d: код %d %v", i, code, err)
				}
			}()
		}
		time.Sleep(warm)
		api1, db1 := w.requests.Load(), w.db.n.Load()
		time.Sleep(window)
		api2, db2 := w.requests.Load(), w.db.n.Load()
		wg.Wait()
		mins := window.Minutes()
		apiMin, dbMin := float64(api2-api1)/mins, float64(db2-db1)/mins
		t.Logf("N=%d: старт и прогрев %v — API %d, база %d; установившееся, в минуту — API %.1f (%.2f на ожидание), база %.1f (%.1f на ожидание, %.2f в секунду всего)",
			n, warm, api1-api0, db1-db0, apiMin, apiMin/float64(n), dbMin, dbMin/float64(n), dbMin/60)
		fmt.Fprintf(os.Stderr, "WAIT_LOAD n=%d start_api=%d start_db=%d api_per_min=%.1f db_per_min=%.1f\n", n, api1-api0, db1-db0, apiMin, dbMin)
	}
}
