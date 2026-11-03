package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// CLAB-390: карточка задачи — один снимок. Раньше getTask читал задачу и её
// прогоны отдельными запросами, и переход между ними давал «рваную»
// карточку: статус новый, открытый прогон старый (или наоборот), а clab wait
// на такой карточке возвращал не тот исход.

// runsMarker — запрос прогонов задачи (ListStageRuns): первый запрос карточки
// после чтения самой задачи. Ловушка срабатывает перед ним.
const runsMarker = "WHERE sr.task_id = $1 ORDER BY sr.entered_at"

type cardView struct {
	Task struct {
		StatusKey string `json:"status_key"`
	} `json:"task"`
	StageRuns []struct {
		StatusKey  string          `json:"status_key"`
		Kind       string          `json:"kind"`
		FinishedAt *time.Time      `json:"finished_at"`
		Exit       json.RawMessage `json:"exit"`
	} `json:"stage_runs"`
}

// consistent проверяет инвариант карточки: статус задачи равен статусу
// единственного открытого прогона этапа, а выход есть только у закрытых.
func (c cardView) consistent() error {
	open := 0
	for _, r := range c.StageRuns {
		isOpen := r.Kind == "stage" && r.FinishedAt == nil
		if isOpen {
			open++
			if r.StatusKey != c.Task.StatusKey {
				return fmt.Errorf("статус задачи %q, открытый прогон на %q", c.Task.StatusKey, r.StatusKey)
			}
		}
		if r.Kind == "stage" && r.FinishedAt == nil && len(r.Exit) > 0 && string(r.Exit) != "null" {
			return fmt.Errorf("у открытого прогона %q есть выход %s", r.StatusKey, r.Exit)
		}
	}
	if open != 1 {
		return fmt.Errorf("открытых прогонов этапа %d, ждали 1 (статус задачи %q)", open, c.Task.StatusKey)
	}
	return nil
}

func (w *waitWorld) card(key string) cardView {
	w.t.Helper()
	raw := w.call("GET", "/v1/tasks/"+key, nil, http.StatusOK)
	b, _ := json.Marshal(raw)
	var c cardView
	if err := json.Unmarshal(b, &c); err != nil {
		w.t.Fatal(err)
	}
	return c
}

// toArchitecture ставит задачу на следующий за analysis статус: гейты
// analysis_done сняты человеком, гейт возврата тоже.
func (w *waitWorld) toArchitecture(key string) {
	w.t.Helper()
	w.start(key)
	for _, g := range []string{"artifacts", "peer_review", "human_approval"} {
		w.decide(key, "analysis_done", g, "waived", nil)
	}
	w.call("POST", "/v1/tasks/"+key+"/transitions/analysis_done", nil, http.StatusOK)
	w.call("POST", "/v1/tasks/"+key+"/claim", map[string]any{}, http.StatusOK)
	w.decide(key, "reject_to_analysis", "problem_report", "waived", nil)
}

// AC-1: переход, лёгший между запросом задачи и запросами прогонов, не
// даёт рваной карточки. Хук в трассировщике пула выполняет возврат сразу
// после чтения задачи в getTask, перед запросом прогонов — на main до снимка этот тест красный.
func TestCardSnapshotAcrossTransition(t *testing.T) {
	w := newWaitWorld(t)
	w.toArchitecture("SDLC-1")
	if before := w.card("SDLC-1"); before.Task.StatusKey == "analysis" {
		t.Fatalf("исходный статус не тот: %+v", before)
	}

	var rejectErr error
	w.db.trap.Store(&queryTrap{marker: runsMarker, fire: func() {
		resp, err := w.postRaw("/v1/tasks/SDLC-1/transitions/reject_to_analysis", map[string]any{"comment": "гонка"})
		if err != nil {
			rejectErr = err
			return
		}
		if resp != http.StatusOK {
			rejectErr = fmt.Errorf("возврат: %d", resp)
		}
	}})
	c := w.card("SDLC-1")
	if w.db.trap.Load() != nil {
		t.Fatal("ловушка не сработала: запрос прогонов карточки не найден")
	}
	if rejectErr != nil {
		t.Fatalf("хук: %v", rejectErr)
	}
	if err := c.consistent(); err != nil {
		t.Errorf("рваная карточка: %v", err)
	}

	// Следующее чтение видит уже возврат и согласовано.
	after := w.card("SDLC-1")
	if after.Task.StatusKey != "analysis" {
		t.Errorf("после возврата статус %q, ждали analysis", after.Task.StatusKey)
	}
	if err := after.consistent(); err != nil {
		t.Errorf("карточка после возврата: %v", err)
	}
}

// postRaw — POST без t.Fatal: зовётся из горутины обработчика.
func (w *waitWorld) postRaw(path string, body any) (int, error) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", w.url+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+waitRoot)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// AC-2: карточка собирается на одном соединении пула — внутри снимка нет
// обращений к пулу, иначе при MaxConns=1 она повисла бы.
func TestCardOnSingleConnection(t *testing.T) {
	w := newWaitWorldPool(t, 1)
	w.toArchitecture("SDLC-1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan cardView, 1)
	go func() { done <- w.card("SDLC-1") }()
	select {
	case c := <-done:
		if err := c.consistent(); err != nil {
			t.Error(err)
		}
	case <-ctx.Done():
		t.Fatal("карточка не собралась на одном соединении пула: второй захват внутри снимка")
	}
}

// tornProxy — сервер «до снимка»: первые torn карточек задачи key отдаёт с
// чужим статусом задачи при открытом прогоне настоящего статуса. Остальное
// — как есть. cards считает чтения карточки.
func tornProxy(t *testing.T, w *waitWorld, key string, torn int64) (*Client, *atomic.Int64) {
	t.Helper()
	var cards atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest(r.Method, w.url+r.URL.RequestURI(), r.Body)
		req.Header = r.Header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			rw.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/"+key && cards.Add(1) <= torn {
			var card map[string]any
			if json.Unmarshal(body, &card) == nil {
				if task, ok := card["task"].(map[string]any); ok {
					task["status_key"] = "stale-status"
					body, _ = json.Marshal(card)
				}
			}
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(resp.StatusCode)
		_, _ = rw.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: srv.URL, token: waitRoot, http: srv.Client()}, &cards
}

// AC-7: рваная карточка (статус задачи не тот, что у открытого прогона) не
// даёт исхода: ожидание перечитывает её и выходит по согласованной.
func TestWaitReadsTornCardAgain(t *testing.T) {
	w := newWaitWorld(t)
	w.start("SDLC-1")
	c, cards := tornProxy(t, w, "SDLC-1", 2)

	out := make(chan waitRun, 1)
	go func() {
		res, code, err := runWait(context.Background(), c, waitOptions{task: "SDLC-1", gate: "human_approval",
			transition: "analysis_done", timeout: time.Minute, timing: fastTiming})
		out <- waitRun{res: res, code: code, err: err}
	}()
	still(t, out, 700*time.Millisecond, "апрува ещё нет")
	if n := cards.Load(); n != 3 {
		t.Errorf("чтений карточки на старте %d, ждали 3 (две рваные и согласованная)", n)
	}
	w.decide("SDLC-1", "analysis_done", "human_approval", "passed", nil)
	r := recv(t, out, 5*time.Second, "апрув")
	if r.err != nil || r.code != waitPassed || r.res.Outcome != "passed" {
		t.Fatalf("исход по согласованной карточке: %+v %d %v", r.res, r.code, r.err)
	}
}

// AC-7: повторы ограничены — карточка, которая всегда рваная, не вешает
// ожидание: на пределе решение по последней, срок работает как обычно.
func TestWaitTornCardRetriesAreBounded(t *testing.T) {
	w := newWaitWorld(t)
	w.start("SDLC-1")
	c, cards := tornProxy(t, w, "SDLC-1", 1<<30)

	start := time.Now()
	_, code, err := runWait(context.Background(), c, waitOptions{task: "SDLC-1", gate: "human_approval",
		transition: "analysis_done", timeout: 3 * time.Second, timing: fastTiming})
	if code != waitTimeout || err != nil {
		t.Fatalf("код %d %v, ждали таймаут %d", code, err, waitTimeout)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("ожидание не уложилось в срок: %v", took)
	}
	// Первый круг: 1 + waitTornRetries; дальше раз в gap/poll идут свои,
	// каждый тоже не больше 1 + waitTornRetries.
	if n := cards.Load(); n < 1+waitTornRetries {
		t.Errorf("чтений карточки %d, ждали не меньше %d", n, 1+waitTornRetries)
	}
}

// Карточка без открытого прогона (задача закрыта) рваной не считается; как
// и карточка сервера, не сообщившего статус прогона, и открытая перепроверка.
func TestWaitCardTorn(t *testing.T) {
	for name, tc := range map[string]struct {
		json string
		torn bool
	}{
		"согласована":              {`{"task":{"status_key":"a"},"stage_runs":[{"status_key":"a","kind":"stage"}]}`, false},
		"прогон другого статуса":   {`{"task":{"status_key":"b"},"stage_runs":[{"status_key":"a","kind":"stage"}]}`, true},
		"открытый — последний":     {`{"task":{"status_key":"b"},"stage_runs":[{"status_key":"a","kind":"stage","finished_at":"2026-11-03T10:00:00Z"},{"status_key":"b","kind":"stage"}]}`, false},
		"закрыт, нет открытого":    {`{"task":{"status_key":"done"},"stage_runs":[{"status_key":"a","kind":"stage","finished_at":"2026-11-03T10:00:00Z"}]}`, false},
		"статус прогона не пришёл": {`{"task":{"status_key":"b"},"stage_runs":[{"kind":"stage"}]}`, false},
		"открыта перепроверка":     {`{"task":{"status_key":"b"},"stage_runs":[{"status_key":"a","kind":"review"}]}`, false},
	} {
		var c waitCard
		if err := json.Unmarshal([]byte(tc.json), &c); err != nil {
			t.Fatal(err)
		}
		if got := c.torn(); got != tc.torn {
			t.Errorf("%s: torn=%v, ждали %v", name, got, tc.torn)
		}
	}
}
