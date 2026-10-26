package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// CLAB-219, критерий 24: отказ захвата по политике учёта проекта. Долг на
// принципале раннера, а не на задаче: в опросе — один захват, без release,
// без счёта сбоев, одна запись Error, повтор — по смене долгов или через
// десять минут.
func TestUsagePolicyRefusalStopsPoll(t *testing.T) {
	var (
		mu     sync.Mutex
		debts  = []string{"run-a"}
		claims []string
		other  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v1/projects/CLAB/queue":
			queue := []any{}
			for _, key := range []string{"CLAB-1", "CLAB-2", "CLAB-3"} {
				queue = append(queue, map[string]any{"task": map[string]any{"key": key},
					"stage_run": map[string]any{"id": "run-" + key}, "role_key": "developer", "claimable": true})
			}
			json.NewEncoder(w).Encode(map[string]any{"queue": queue})
		case strings.HasSuffix(req.URL.Path, "/claim"):
			claims = append(claims, req.URL.Path)
			list := []any{}
			for _, id := range debts {
				list = append(list, map[string]any{"task": "CLAB-9", "stage_run_id": id, "finished_at": time.Now()})
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": codeUsageReportRequired, "message": "захват отбит политикой учёта",
				"details": map[string]any{"debts": list, "total": len(list), "grace_minutes": 60},
			}})
		default:
			other = append(other, req.Method+" "+req.URL.Path)
			w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(srv.Close)

	var logs syncBuffer
	cfg := Config{APIURL: srv.URL + "/v1", Token: runnerToken, Project: "CLAB", Slots: 2,
		Interval: time.Hour, Timeout: time.Second, Command: "/bin/true"}
	r := newRunner(cfg, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r.environ = func() []string { return parentEnv }
	clock := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return clock }

	slots := make(chan struct{}, cfg.Slots)
	var wg sync.WaitGroup
	poll := func() int {
		t.Helper()
		took := r.dispatch(context.Background(), slots, &wg)
		wg.Wait()
		return took
	}
	errors := func() int { return strings.Count(logs.String(), "level=ERROR") }
	claimed := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(claims)
	}

	// Первый опрос: один захват из трёх свободных, одна запись Error.
	if took := poll(); took != 0 {
		t.Fatalf("раннер взял %d задач при долге", took)
	}
	if claimed() != 1 || errors() != 1 {
		t.Fatalf("захватов %d, записей Error %d; ожидали 1 и 1\n%s", claimed(), errors(), logs.String())
	}
	if !strings.Contains(logs.String(), "run-a") || !strings.Contains(logs.String(), "clab usage") {
		t.Fatalf("в записи нет долга или способа погасить:\n%s", logs.String())
	}
	if len(slots) != 0 {
		t.Fatalf("отказ занял слот: %d", len(slots))
	}

	// Следующий опрос с тем же долгом: снова одна проба, запись не повторяется.
	clock = clock.Add(time.Minute)
	poll()
	if claimed() != 2 || errors() != 1 {
		t.Fatalf("второй опрос: захватов %d, записей Error %d", claimed(), errors())
	}

	// Список долгов изменился — новая запись.
	mu.Lock()
	debts = []string{"run-a", "run-b"}
	mu.Unlock()
	poll()
	if errors() != 2 {
		t.Fatalf("новый долг не записан: %d записей", errors())
	}
	// Тот же список через десять минут — повтор.
	clock = clock.Add(9 * time.Minute)
	poll()
	if errors() != 2 {
		t.Fatalf("повтор раньше десяти минут: %d записей", errors())
	}
	clock = clock.Add(time.Minute)
	poll()
	if errors() != 3 {
		t.Fatalf("через десять минут записи нет: %d", errors())
	}

	// Ни release, ни отчёта, ни выпуска токена; счётчик сбоев не тронут.
	mu.Lock()
	defer mu.Unlock()
	if len(other) != 0 {
		t.Fatalf("лишние вызовы при отказе: %v", other)
	}
	if r.fails.blocked("run-CLAB-1", clock) {
		t.Fatal("отказ политики посчитан сбоем прогона")
	}
}
