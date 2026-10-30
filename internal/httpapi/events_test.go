package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// События долгим опросом (CLAB-323, AC-1…AC-9): рукопожатие, пробуждение
// событием, две реплики на одной базе, лента проекта, пределы ожидания,
// права, пул под двадцатью ожиданиями, остановка и план выборки.

// queryCounter считает запросы к базе через трассировщик pgx: так видно,
// сколько стоит ожидание, а не только что оно отвечает.
type queryCounter struct{ n atomic.Int64 }

func (q *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	q.n.Add(1)
	return ctx
}
func (q *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type feedWorld struct {
	t       *testing.T
	store   *postgres.Store
	pool    *pgxpool.Pool
	queries *queryCounter
	cfg     config.Config
	auth    *workflow.Auth
	engine  *workflow.Engine
	root    uuid.UUID
}

// newFeedWorld — база пакета с демо-циклом; maxConns > 0 — свой пул такого
// размера поверх той же базы, со счётчиком запросов.
func newFeedWorld(t *testing.T, cfg config.Config, maxConns int32) *feedWorld {
	t.Helper()
	store, pool := testdb.Store(t)
	w := &feedWorld{t: t, store: store, pool: pool, cfg: cfg, queries: &queryCounter{}}
	pc, err := pgxpool.ParseConfig(testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	if maxConns > 0 {
		pc.MaxConns = maxConns
	}
	pc.ConnConfig.Tracer = w.queries
	w.pool, err = pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.pool.Close)
	w.store = postgres.New(w.pool)
	log := slog.New(slog.DiscardHandler)
	w.engine = workflow.NewEngine(w.store, log, workflow.Options{StrictGates: true})
	w.auth = workflow.NewAuth(w.store, log, workflow.AuthToken)
	if err := w.auth.Bootstrap(context.Background(), "root", rootToken); err != nil {
		t.Fatal(err)
	}
	root, err := w.store.ResolveActor(context.Background(), w.store.DB(), "root", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	w.root = root.ID
	return w
}

// replica — ещё один процесс API на той же базе: свой Server, свой
// будильник, свой адрес.
func (w *feedWorld) replica() (*Server, string) {
	w.t.Helper()
	log := slog.New(slog.DiscardHandler)
	srv := NewServer(w.cfg, log, w.store, w.engine, workflow.NewAuthoring(w.store, log, w.engine), w.auth, nil)
	ts := httptest.NewServer(srv.Handler())
	w.t.Cleanup(ts.Close)
	w.t.Cleanup(srv.StopWaiting)
	return srv, ts.URL
}

func (w *feedWorld) event(taskKey, typ string) {
	w.t.Helper()
	ctx := context.Background()
	task, err := w.store.GetTaskByKey(ctx, w.store.DB(), taskKey)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.store.AppendEvent(ctx, w.store.DB(), task.ID, nil, typ, w.root, map[string]any{"secret": "payload"}); err != nil {
		w.t.Fatal(err)
	}
}

type feedReply struct {
	code    int
	events  []map[string]any
	cursor  string
	errCode string
	took    time.Duration
	raw     string
}

func getFeed(t *testing.T, base, path, token string) feedReply {
	t.Helper()
	start := time.Now()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("GET %s: %v", path, err)
		return feedReply{}
	}
	defer resp.Body.Close()
	var body struct {
		Events []map[string]any `json:"events"`
		Cursor string           `json:"cursor"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	raw := new(strings.Builder)
	dec := json.NewDecoder(resp.Body)
	_ = dec.Decode(&body)
	b, _ := json.Marshal(body)
	raw.Write(b)
	return feedReply{code: resp.StatusCode, events: body.Events, cursor: body.Cursor,
		errCode: body.Error.Code, took: time.Since(start), raw: raw.String()}
}

func handshake(t *testing.T, base, path string) string {
	t.Helper()
	r := getFeed(t, base, path, rootToken)
	if r.code != http.StatusOK || len(r.events) != 0 || r.cursor == "" || r.took > time.Second {
		t.Fatalf("рукопожатие %s: %d %s за %v", path, r.code, r.raw, r.took)
	}
	return r.cursor
}

// AC-2, AC-5: рукопожатие без after — сразу и пусто; событие будит
// ожидание не позже 2 с; поля без payload; новый курсор его не повторяет;
// порядок — по возрастанию.
func TestFeedWakesOnEvent(t *testing.T) {
	w := newFeedWorld(t, config.Config{Env: "test"}, 0)
	_, base := w.replica()
	cursor := handshake(t, base, "/v1/tasks/SDLC-1/events?wait=25")

	done := make(chan feedReply, 1)
	go func() {
		done <- getFeed(t, base, "/v1/tasks/SDLC-1/events?wait=20&after="+url.QueryEscape(cursor), rootToken)
	}()
	time.Sleep(500 * time.Millisecond)
	w.event("SDLC-1", "test.first")
	w.event("SDLC-1", "test.second")
	committed := time.Now()
	r := <-done
	if lag := time.Since(committed); lag > 2*time.Second {
		t.Errorf("ожидание ответило через %v после фиксации, ждали ≤ 2 с", lag)
	}
	if r.code != http.StatusOK || len(r.events) == 0 {
		t.Fatalf("ответ ожидания: %d %s", r.code, r.raw)
	}
	for _, e := range r.events {
		for _, k := range []string{"id", "type", "task_key", "actor", "created_at"} {
			if _, ok := e[k]; !ok {
				t.Errorf("в событии нет %s: %v", k, e)
			}
		}
		if _, ok := e["payload"]; ok {
			t.Errorf("payload в ответе событий: %v", e)
		}
		if e["task_key"] != "SDLC-1" || e["actor"] != "root" {
			t.Errorf("задача и актор события: %v", e)
		}
	}
	// Второе событие могло не попасть в первый ответ, если будильник
	// проснулся между ними, — тогда оно придёт сразу по новому курсору.
	got := r.events
	if len(got) == 1 {
		next := getFeed(t, base, "/v1/tasks/SDLC-1/events?wait=5&after="+url.QueryEscape(r.cursor), rootToken)
		got = append(got, next.events...)
		r = next
	}
	if len(got) != 2 || got[0]["type"] != "test.first" || got[1]["type"] != "test.second" {
		t.Fatalf("события по порядку: %v", got)
	}
	if r.cursor == cursor {
		t.Errorf("курсор не сдвинулся")
	}
	again := getFeed(t, base, "/v1/tasks/SDLC-1/events?after="+url.QueryEscape(r.cursor), rootToken)
	if again.code != http.StatusOK || len(again.events) != 0 || again.cursor != r.cursor {
		t.Errorf("повтор по новому курсору: %d %s", again.code, again.raw)
	}
}

// AC-3: ожидание на одной реплике, событие — через другую.
func TestFeedAcrossReplicas(t *testing.T) {
	w := newFeedWorld(t, config.Config{Env: "test"}, 0)
	_, a := w.replica()
	_, b := w.replica()
	cursor := handshake(t, a, "/v1/tasks/SDLC-1/events")

	done := make(chan feedReply, 1)
	go func() {
		done <- getFeed(t, a, "/v1/tasks/SDLC-1/events?wait=20&after="+url.QueryEscape(cursor), rootToken)
	}()
	time.Sleep(500 * time.Millisecond)
	// Событие пишет настоящая ручка второй реплики: старт задачи.
	req, _ := http.NewRequest("POST", b+"/v1/tasks/SDLC-1/transitions/start", nil)
	req.Header.Set("Authorization", "Bearer "+rootToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("start через вторую реплику: %v %v", resp, err)
	}
	resp.Body.Close()
	committed := time.Now()
	r := <-done
	if lag := time.Since(committed); lag > 2*time.Second {
		t.Errorf("пробуждение через другую реплику — %v, ждали ≤ 2 с", lag)
	}
	if r.code != http.StatusOK || len(r.events) == 0 {
		t.Fatalf("ответ: %d %s", r.code, r.raw)
	}
}

// AC-4: лента проекта будится событием любой своей задачи и не будится
// чужим проектом.
func TestFeedProjectScope(t *testing.T) {
	w := newFeedWorld(t, config.Config{Env: "test"}, 0)
	testdb.ForeignTask(t, w.store)
	_, base := w.replica()
	cursor := handshake(t, base, "/v1/projects/SDLC/events")

	done := make(chan feedReply, 1)
	go func() {
		done <- getFeed(t, base, "/v1/projects/SDLC/events?wait=20&after="+url.QueryEscape(cursor), rootToken)
	}()
	time.Sleep(300 * time.Millisecond)
	w.event("SDLC-3", "test.project")
	r := <-done
	if r.code != http.StatusOK || len(r.events) != 1 || r.events[0]["task_key"] != "SDLC-3" {
		t.Fatalf("событие задачи проекта: %d %s", r.code, r.raw)
	}

	// Чужой проект: ответ по сроку пустым, курсор тот же.
	go func() {
		done <- getFeed(t, base, "/v1/projects/SDLC/events?wait=3&after="+url.QueryEscape(r.cursor), rootToken)
	}()
	time.Sleep(300 * time.Millisecond)
	w.event("OTHER-1", "test.foreign")
	r2 := <-done
	if r2.code != http.StatusOK || len(r2.events) != 0 || r2.cursor != r.cursor {
		t.Fatalf("событие чужого проекта разбудило: %d %s", r2.code, r2.raw)
	}
	if r2.took < 2800*time.Millisecond || r2.took > 4500*time.Millisecond {
		t.Errorf("ответ по сроку wait=3 пришёл за %v", r2.took)
	}
}

// AC-5: потолок ожидания из SDLC_REQUEST_TIMEOUT, неверный ввод, страницы.
func TestFeedWaitLimitFormula(t *testing.T) {
	for rt, want := range map[time.Duration]time.Duration{
		30 * time.Second: 25 * time.Second,
		10 * time.Second: 5 * time.Second,
		0:                25 * time.Second,
		5 * time.Second:  0,
		60 * time.Second: 25 * time.Second,
	} {
		s := &Server{cfg: config.Config{RequestTimeout: rt}}
		if got := s.feedWaitLimit(); got != want {
			t.Errorf("SDLC_REQUEST_TIMEOUT=%v: потолок %v, ждали %v", rt, got, want)
		}
	}
}

func TestFeedLimitsAndPages(t *testing.T) {
	w := newFeedWorld(t, config.Config{Env: "test", RequestTimeout: 10 * time.Second}, 0)
	_, base := w.replica()
	cursor := handshake(t, base, "/v1/tasks/SDLC-1/events")
	esc := url.QueryEscape(cursor)

	// wait=600 при SDLC_REQUEST_TIMEOUT=10s — не дольше 5 с.
	r := getFeed(t, base, "/v1/tasks/SDLC-1/events?wait=600&after="+esc, rootToken)
	if r.code != http.StatusOK || r.took > 5500*time.Millisecond || r.took < 4500*time.Millisecond {
		t.Errorf("wait=600 при 10s: %d за %v", r.code, r.took)
	}
	// Без wait и wait=0 — сразу.
	for _, q := range []string{"", "&wait=0"} {
		if r := getFeed(t, base, "/v1/tasks/SDLC-1/events?after="+esc+q, rootToken); r.code != http.StatusOK || r.took > time.Second {
			t.Errorf("after%s: %d за %v", q, r.code, r.took)
		}
	}
	// Неверный ввод — 400 invalid_input.
	for _, q := range []string{"after=garbage", "after=" + esc + "&wait=-1", "after=" + esc + "&wait=abc",
		"wait=abc", "after=" + url.QueryEscape("bm90LWEtY3Vyc29y")} {
		if r := getFeed(t, base, "/v1/tasks/SDLC-1/events?"+q, rootToken); r.code != http.StatusBadRequest || r.errCode != "invalid_input" {
			t.Errorf("%s: %d %s", q, r.code, r.raw)
		}
	}
	// 150 событий после курсора: 100, затем сразу 50.
	ctx := context.Background()
	task, _ := w.store.GetTaskByKey(ctx, w.store.DB(), "SDLC-1")
	if _, err := w.pool.Exec(ctx, `
		INSERT INTO task_events (task_id, type, actor_id, created_at)
		SELECT $1, 'test.bulk', $2, clock_timestamp() FROM generate_series(1, 150)`, task.ID, w.root); err != nil {
		t.Fatal(err)
	}
	first := getFeed(t, base, "/v1/tasks/SDLC-1/events?wait=5&after="+esc, rootToken)
	second := getFeed(t, base, "/v1/tasks/SDLC-1/events?wait=5&after="+url.QueryEscape(first.cursor), rootToken)
	if len(first.events) != 100 || len(second.events) != 50 || second.took > time.Second {
		t.Errorf("страницы: %d и %d (второй за %v)", len(first.events), len(second.events), second.took)
	}
	seen := map[any]bool{}
	for _, e := range append(first.events, second.events...) {
		if seen[e["id"]] {
			t.Errorf("событие повторилось: %v", e["id"])
		}
		seen[e["id"]] = true
	}
}

// AC-6: участник viewer — 200; не участник — тот же код, что у карточки и
// проекта; токен задачи читает события своего проекта, чужого — нет.
func TestFeedAccess(t *testing.T) {
	w := newFeedWorld(t, config.Config{Env: "test"}, 0)
	testdb.ForeignTask(t, w.store)
	_, base := w.replica()
	ctx := context.Background()
	root, err := w.auth.Authenticate(ctx, rootToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"viewer", "stranger"} {
		if _, err := w.store.ResolveActor(ctx, w.store.DB(), h, domain.PrincipalUser); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.auth.SetMember(ctx, root, "SDLC", "user:viewer", domain.MemberViewer, nil); err != nil {
		t.Fatal(err)
	}
	token := func(principal, project string) string {
		issued, err := w.auth.IssueToken(ctx, root, workflow.IssueTokenInput{Name: "t", Principal: principal, ProjectKey: project})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Token
	}
	viewer := token("user:viewer", "SDLC")
	stranger := token("user:stranger", "")

	for _, p := range []string{"/v1/tasks/SDLC-1/events", "/v1/projects/SDLC/events"} {
		if r := getFeed(t, base, p, viewer); r.code != http.StatusOK {
			t.Errorf("viewer %s: %d %s", p, r.code, r.raw)
		}
	}
	same := func(feed, card string) {
		f, c := getFeed(t, base, feed, stranger), getFeed(t, base, card, stranger)
		if f.code != c.code || f.errCode != c.errCode || f.code == http.StatusOK {
			t.Errorf("не участник: %s %d %s, %s %d %s", feed, f.code, f.errCode, card, c.code, c.errCode)
		}
	}
	same("/v1/tasks/SDLC-1/events", "/v1/tasks/SDLC-1")
	same("/v1/projects/SDLC/events", "/v1/projects/SDLC")

	// Токен задачи: старт, захват, выпуск — как у /clab-run.
	pool := token("agent:pool", "SDLC")
	for _, c := range []struct{ path, tok string }{
		{"/v1/tasks/SDLC-1/transitions/start", rootToken},
		{"/v1/tasks/SDLC-1/claim", pool},
	} {
		req, _ := http.NewRequest("POST", base+c.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+c.tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %v %v", c.path, resp, err)
		}
		resp.Body.Close()
	}
	req, _ := http.NewRequest("POST", base+"/v1/tasks/SDLC-1/task-token", nil)
	req.Header.Set("Authorization", "Bearer "+pool)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("токен задачи: %v %v", resp, err)
	}
	var issued struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&issued)
	resp.Body.Close()

	for _, p := range []string{"/v1/tasks/SDLC-1/events", "/v1/tasks/SDLC-3/events", "/v1/projects/SDLC/events"} {
		if r := getFeed(t, base, p, issued.Token); r.code != http.StatusOK {
			t.Errorf("токен задачи %s: %d %s", p, r.code, r.raw)
		}
	}
	for _, p := range []string{"/v1/tasks/OTHER-1/events", "/v1/projects/OTHER/events"} {
		if r := getFeed(t, base, p, issued.Token); r.code != http.StatusForbidden {
			t.Errorf("токен задачи, чужой проект %s: %d %s", p, r.code, r.raw)
		}
	}
}

// AC-7: пул в 4 соединения, 20 ожиданий — карточка отвечает быстро,
// захваченных соединений не больше пула, ошибок захвата нет.
func TestFeedDoesNotHoldPool(t *testing.T) {
	w := newFeedWorld(t, config.Config{Env: "test"}, 4)
	_, base := w.replica()
	cursor := url.QueryEscape(handshake(t, base, "/v1/projects/SDLC/events"))

	var wg sync.WaitGroup
	var failed atomic.Int64
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := "/v1/projects/SDLC/events?wait=6&after=" + cursor
			if i%2 == 1 {
				path = "/v1/tasks/SDLC-1/events?wait=6&after=" + cursor
			}
			if r := getFeed(t, base, path, rootToken); r.code != http.StatusOK {
				failed.Add(1)
			}
		}()
	}
	time.Sleep(time.Second)
	var maxAcquired int32
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if a := w.pool.Stat().AcquiredConns(); a > maxAcquired {
				maxAcquired = a
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	for range 3 {
		start := time.Now()
		r := getFeed(t, base, "/v1/tasks/SDLC-1", rootToken)
		if r.code != http.StatusOK || time.Since(start) > time.Second {
			t.Errorf("карточка во время ожиданий: %d за %v", r.code, time.Since(start))
		}
		time.Sleep(500 * time.Millisecond)
	}
	wg.Wait()
	close(stop)
	if failed.Load() != 0 {
		t.Errorf("ожиданий с ошибкой: %d", failed.Load())
	}
	if maxAcquired > 4 {
		t.Errorf("захвачено соединений %d при пуле 4", maxAcquired)
	}
	if s := w.pool.Stat(); s.CanceledAcquireCount() > 0 {
		t.Errorf("отменённые захваты пула: %d", s.CanceledAcquireCount())
	}
}

// AC-8: остановка отвечает ожидающим сразу и не ждёт срока wait.
func TestFeedShutdown(t *testing.T) {
	w := newFeedWorld(t, config.Config{Env: "test"}, 0)
	log := slog.New(slog.DiscardHandler)
	api := NewServer(w.cfg, log, w.store, w.engine, workflow.NewAuthoring(w.store, log, w.engine), w.auth, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: api.Handler()}
	srv.RegisterOnShutdown(api.StopWaiting)
	go func() { _ = srv.Serve(ln) }()
	base := "http://" + ln.Addr().String()
	cursor := handshake(t, base, "/v1/tasks/SDLC-1/events")

	done := make(chan feedReply, 1)
	go func() {
		done <- getFeed(t, base, "/v1/tasks/SDLC-1/events?wait=25&after="+url.QueryEscape(cursor), rootToken)
	}()
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("остановка: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("остановка ждала %v", took)
	}
	r := <-done
	if r.code != http.StatusOK || len(r.events) != 0 || r.cursor != cursor || time.Since(start) > 2*time.Second {
		t.Errorf("ответ ожидания на остановке: %d %s через %v", r.code, r.raw, time.Since(start))
	}
}

// AC-9: выборка проекта после курсора на ≥ 100 тыс. событий идёт новым
// индексом, а не проходом по ленте. Лента — как на живой базе: события
// размазаны по сотне задач двух проектов, свежих среди них — минута.
func TestFeedProjectQueryUsesIndex(t *testing.T) {
	w := newFeedWorld(t, config.Config{Env: "test"}, 0)
	testdb.ForeignTask(t, w.store)
	ctx := context.Background()
	if _, err := w.pool.Exec(ctx, `
		INSERT INTO tasks (project_id, key, seq, title, description, workflow_id, status_id)
		SELECT p.id, p.key || '-' || (100 + g), 100 + g, 'нагрузка', '', t.workflow_id, t.status_id
		FROM projects p, tasks t, generate_series(1, 50) g
		WHERE p.key IN ('SDLC', 'OTHER') AND t.key = 'SDLC-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(ctx, `
		INSERT INTO task_events (task_id, type, actor_id, created_at)
		SELECT (SELECT id FROM tasks ORDER BY id OFFSET (g % 100) LIMIT 1),
		       'lease.extended', $1, now() - make_interval(secs => g)
		FROM generate_series(1, 120000) g`, w.root); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(ctx, `ANALYZE task_events; ANALYZE tasks`); err != nil {
		t.Fatal(err)
	}
	project, _ := w.store.GetProjectByKey(ctx, w.store.DB(), "SDLC")
	rows, err := w.pool.Query(ctx, `
		EXPLAIN SELECT e.id, e.type, t.key, p.handle, e.created_at
		FROM task_events e
		JOIN tasks t ON t.id = e.task_id
		LEFT JOIN principals p ON p.id = e.actor_id
		WHERE t.project_id = $1 AND (e.created_at, e.id) > ($2, $3)
		ORDER BY e.created_at, e.id
		LIMIT 100`, project.ID, time.Now().Add(-time.Minute), uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		plan = append(plan, line)
	}
	rows.Close()
	text := strings.Join(plan, "\n")
	if !strings.Contains(text, "task_events_cursor") || strings.Contains(text, "Seq Scan on task_events") {
		t.Fatalf("план выборки проекта:\n%s", text)
	}
	t.Logf("план выборки проекта:\n%s", text)

	// Будильник реплики: окно последних секунд по всей ленте — тем же
	// индексом.
	var hub []string
	rows, err = w.pool.Query(ctx, `
		EXPLAIN SELECT e.id, e.task_id, t.project_id
		FROM task_events e
		JOIN tasks t ON t.id = e.task_id
		WHERE e.created_at > clock_timestamp() - make_interval(secs => 3)
		ORDER BY e.created_at, e.id
		LIMIT 2000`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		hub = append(hub, line)
	}
	rows.Close()
	if text := strings.Join(hub, "\n"); !strings.Contains(text, "task_events_cursor") || strings.Contains(text, "Seq Scan on task_events") {
		t.Fatalf("план будильника:\n%s", text)
	}
}

// TestFeedLoad — замер для отчёта о тестах (CLAB-323, требование человека):
// сколько запросов к базе дают N ожиданий долгим опросом за окно. Включается
// CLAB_FEED_MEASURE=1: идёт минуту на каждое N. Клиент — как у clab wait:
// рукопожатие, затем круги wait=25 подряд без пауз (событий нет).
func TestFeedLoad(t *testing.T) {
	if os.Getenv("CLAB_FEED_MEASURE") == "" {
		t.Skip("CLAB_FEED_MEASURE не задан — замер нагрузки пропущен")
	}
	window := time.Minute
	w := newFeedWorld(t, config.Config{Env: "test", RequestTimeout: 30 * time.Second}, 10)
	_, base := w.replica()
	for _, n := range []int{1, 10, 100} {
		cursor := url.QueryEscape(handshake(t, base, "/v1/tasks/SDLC-1/events"))
		ctx, cancel := context.WithTimeout(context.Background(), window)
		var requests atomic.Int64
		before := w.queries.n.Load()
		var wg sync.WaitGroup
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/tasks/SDLC-1/events?wait=25&after="+cursor, nil)
					req.Header.Set("Authorization", "Bearer "+rootToken)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						return
					}
					resp.Body.Close()
					requests.Add(1)
				}
			}()
		}
		wg.Wait()
		cancel()
		q := w.queries.n.Load() - before
		t.Logf("N=%d за %v: запросов к API %d (%.1f на ожидание), запросов к базе %d (%.1f на ожидание, %.1f в секунду)",
			n, window, requests.Load(), float64(requests.Load())/float64(n), q, float64(q)/float64(n), float64(q)/window.Seconds())
		fmt.Fprintf(os.Stderr, "FEED_LOAD n=%d api=%d db=%d\n", n, requests.Load(), q)
	}
}

// Цена одного запроса долгого опроса в запросах к базе (CLAB-323,
// требование человека): ожидание дёшево, только если дёшев круг.
func TestFeedRequestCost(t *testing.T) {
	w := newFeedWorld(t, config.Config{Env: "test"}, 0)
	_, base := w.replica()
	cursor := url.QueryEscape(handshake(t, base, "/v1/tasks/SDLC-1/events"))
	// Аутентификация, отметка токена, проект пути, уровень, задача ручки и
	// сама выборка: больше восьми — в круг пролез лишний запрос.
	for _, p := range []string{"/v1/tasks/SDLC-1/events?wait=0&after=" + cursor, "/v1/projects/SDLC/events?wait=0&after=" + cursor} {
		before := w.queries.n.Load()
		if r := getFeed(t, base, p, rootToken); r.code != http.StatusOK {
			t.Fatalf("%s: %d", p, r.code)
		}
		n := w.queries.n.Load() - before
		t.Logf("%s: запросов к базе %d", p, n)
		if n > 8 {
			t.Errorf("%s: запросов к базе %d, ждали ≤ 8", p, n)
		}
	}
	// Ответ по сроку в базу не ходит: ожидание wait=2 стоит столько же,
	// сколько ответ сразу, сколько бы тактов будильника ни прошло — такты
	// считаются отдельно и делятся на всех ожидающих реплики.
	before := w.queries.n.Load()
	getFeed(t, base, "/v1/tasks/SDLC-1/events?wait=0&after="+cursor, rootToken)
	instant := w.queries.n.Load() - before
	before = w.queries.n.Load()
	getFeed(t, base, "/v1/tasks/SDLC-1/events?wait=2&after="+cursor, rootToken)
	held := w.queries.n.Load() - before
	if ticks := held - instant; ticks > 3 {
		t.Errorf("ожидание 2 с стоило %d запросов сверх ответа сразу, ждали ≤ 3 (такты будильника)", ticks)
	}
	before = w.queries.n.Load()
	getFeed(t, base, "/v1/tasks/SDLC-1", rootToken)
	t.Logf("карточка задачи: запросов к базе %d", w.queries.n.Load()-before)
}
