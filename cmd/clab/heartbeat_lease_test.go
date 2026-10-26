package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/httpapi"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// CLAB-288, критерии 1, 2 и 6 на настоящем сервере с короткой арендой: clab
// heartbeat держит её дольше трёх TTL, пока воркер подметает протухшие, и
// токен задачи на ту же аренду всё это время жив.

const leaseRootToken = "bootstrap-token-clab-288-0123456789"

type leaseWorld struct {
	t      *testing.T
	store  *postgres.Store
	engine *workflow.Engine
	url    string
	pool   string // личный токен agent:pool
}

func newLeaseWorld(t *testing.T, ttl time.Duration) *leaseWorld {
	t.Helper()
	store, _ := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{
		StrictGates: true, LeaseTTL: ttl, HeartbeatInterval: ttl / 4})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := httpapi.NewServer(config.Config{Env: "test"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), auth, nil)
	if err := auth.Bootstrap(context.Background(), "root", leaseRootToken); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	w := &leaseWorld{t: t, store: store, engine: engine, url: ts.URL}
	_, body := w.call("POST", "/v1/auth/tokens", leaseRootToken, map[string]any{
		"name": "orchestrator", "principal": "agent:pool", "project": "SDLC"}, http.StatusCreated)
	w.pool = body["token"].(string)
	return w
}

func (w *leaseWorld) call(method, path, token string, body any, want int) (int, map[string]any) {
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
	return resp.StatusCode, out
}

func (w *leaseWorld) client(token string) *Client {
	return &Client{base: w.url + "/v1", origin: w.url, token: token, http: &http.Client{Timeout: 10 * time.Second}}
}

// claim стартует задачу и забирает её личным токеном пула.
func (w *leaseWorld) claim(key string) uuid.UUID {
	w.t.Helper()
	w.call("POST", "/v1/tasks/"+key+"/transitions/start", leaseRootToken, nil, http.StatusOK)
	_, body := w.call("POST", "/v1/tasks/"+key+"/claim", w.pool, nil, http.StatusOK)
	return uuid.MustParse(body["stage_run_id"].(string))
}

// sweep — воркер аренд: подметает протухшие, пока не отменён ctx.
func (w *leaseWorld) sweep(ctx context.Context) (wait func() int) {
	system, err := w.store.ResolveActor(context.Background(), w.store.DB(), "test-worker", domain.PrincipalSystem)
	if err != nil {
		w.t.Fatal(err)
	}
	var wg sync.WaitGroup
	expired := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			n, _ := w.engine.ExpireLeases(context.Background(), system.ID, 100)
			expired += n
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return func() int { wg.Wait(); return expired }
}

func (w *leaseWorld) events(run uuid.UUID, typ string) int {
	w.t.Helper()
	var n int
	if err := w.store.DB().QueryRow(context.Background(),
		`SELECT count(*) FROM task_events WHERE stage_run_id = $1 AND type = $2`, run, typ).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// Критерий 1: --every при TTL 1 с держит аренду больше трёх TTL под воркером;
// токен задачи на ту же аренду всё это время читает задачу.
func TestHeartbeatHoldsLeaseOnServer(t *testing.T) {
	w := newLeaseWorld(t, time.Second)
	run := w.claim("SDLC-1")
	_, issued := w.call("POST", "/v1/tasks/SDLC-1/task-token", w.pool, nil, http.StatusCreated)
	taskToken := issued["token"].(string)

	ctx, stop := context.WithCancel(context.Background())
	expired := w.sweep(ctx)
	var log bytes.Buffer
	h := &heartbeater{c: w.client(w.pool), task: "SDLC-1", every: 250 * time.Millisecond, out: &log, warn: &log}
	done := make(chan error, 1)
	go func() { done <- h.loop(context.Background(), 3500*time.Millisecond) }()
	deadline := time.Now().Add(3200 * time.Millisecond)
	for time.Now().Before(deadline) {
		w.call("GET", "/v1/tasks/SDLC-1", taskToken, nil, http.StatusOK)
		time.Sleep(300 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatalf("цикл: %v\n%s", err, log.String())
	}
	stop()
	if n := expired(); n != 0 {
		t.Fatalf("воркер подмёл %d аренд под продлением", n)
	}

	var claimedBy *uuid.UUID
	var held bool
	if err := w.store.DB().QueryRow(context.Background(),
		`SELECT claimed_by, lease_expires_at > now() FROM stage_runs WHERE id = $1`, run).Scan(&claimedBy, &held); err != nil {
		t.Fatal(err)
	}
	pool, err := w.store.ResolveActor(context.Background(), w.store.DB(), "pool", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	if !held || claimedBy == nil || *claimedBy != pool.ID {
		t.Fatalf("аренда после трёх TTL: жива %v, держатель %v", held, claimedBy)
	}
	if ext, exp := w.events(run, domain.EventLeaseExtended), w.events(run, domain.EventLeaseExpired); ext < 10 || exp != 0 {
		t.Fatalf("lease.extended %d, lease.expired %d", ext, exp)
	}
	if !strings.Contains(log.String(), "--for 3.5s истекло") {
		t.Fatalf("журнал:\n%s", log.String())
	}
}

// Критерий 2: release или переход, снявший аренду, завершает цикл с причиной
// на ближайшем тике; чужого прогона закреплённый цикл не продлевает.
func TestHeartbeatStopsAfterReleaseAndTransition(t *testing.T) {
	w := newLeaseWorld(t, 5*time.Second)
	runs := map[string]uuid.UUID{}
	for _, c := range []struct {
		key  string
		drop func(key string)
	}{
		{"SDLC-1", func(key string) {
			w.call("POST", "/v1/tasks/"+key+"/release", w.pool, map[string]any{"note": "из другого окна"}, http.StatusOK)
		}},
		{"SDLC-2", func(key string) {
			w.call("POST", "/v1/tasks/"+key+"/transitions/cancel", leaseRootToken, map[string]any{"reason": "снята"}, http.StatusOK)
		}},
	} {
		run := w.claim(c.key)
		runs[c.key] = run
		var log bytes.Buffer
		h := &heartbeater{c: w.client(w.pool), task: c.key, every: 150 * time.Millisecond, out: &log, warn: &log}
		done := make(chan error, 1)
		go func() { done <- h.loop(context.Background(), 0) }()
		time.Sleep(500 * time.Millisecond)
		c.drop(c.key)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: цикл вышел ошибкой: %v", c.key, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: цикл не остановился после снятия аренды:\n%s", c.key, log.String())
		}
		if !strings.Contains(log.String(), "больше нет, продление остановлено") {
			t.Fatalf("%s: нет причины:\n%s", c.key, log.String())
		}
		if h.runID != run.String() {
			t.Fatalf("%s: цикл закреплён за %s, а не за %s", c.key, h.runID, run)
		}
	}

	// Тот же принципал держит другой прогон (SDLC-3): цикл, закреплённый за
	// снятым прогоном SDLC-1, его не продлевает — сервер отвечает отказом.
	other := w.claim("SDLC-3")
	before := w.events(other, domain.EventLeaseExtended)
	var log bytes.Buffer
	h := &heartbeater{c: w.client(w.pool), task: "SDLC-1", every: 100 * time.Millisecond, runID: runs["SDLC-1"].String(), out: &log, warn: &log}
	if reason := h.keep(context.Background()); reason == "" {
		t.Fatal("закреплённый цикл не увидел, что его прогона нет")
	}
	if after := w.events(other, domain.EventLeaseExtended); after != before {
		t.Fatalf("чужой прогон продлён: %d → %d", before, after)
	}
}

// Критерий 6: обёрнутая команда дольше трёх TTL — код её, аренда жива, в
// ленте нет lease.expired.
func TestHeartbeatWrapHoldsLeaseOnServer(t *testing.T) {
	needSh(t)
	w := newLeaseWorld(t, time.Second)
	run := w.claim("SDLC-1")
	ctx, stop := context.WithCancel(context.Background())
	expired := w.sweep(ctx)
	var log, stdout bytes.Buffer
	h := &heartbeater{c: w.client(w.pool), task: "SDLC-1", every: 250 * time.Millisecond, out: &log, warn: &log}
	code, err := h.wrap(context.Background(), []string{"sh", "-c", "echo start; sleep 3.5; exit 3"},
		strings.NewReader(""), &stdout, io.Discard, nil)
	stop()
	if err != nil || code != 3 || stdout.String() != "start\n" {
		t.Fatalf("код %d, вывод %q, %v\n%s", code, stdout.String(), err, log.String())
	}
	if n := expired(); n != 0 || w.events(run, domain.EventLeaseExpired) != 0 {
		t.Fatalf("аренда протухла под обёрткой: подметено %d", n)
	}
	var held bool
	if err := w.store.DB().QueryRow(context.Background(),
		`SELECT lease_expires_at > now() FROM stage_runs WHERE id = $1`, run).Scan(&held); err != nil || !held {
		t.Fatalf("аренда после команды: %v %v", held, err)
	}
}
