package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// CLAB-288, критерий 13 (R7, UC-4): токен задачи погас вместе с арендой —
// оркестратор забирает этап заново (тот же прогон, та же попытка), пишет
// стоимость сгоревшего захода личным токеном по --run, следующий заход
// отчитывается своим токеном задачи. По прогону две строки учёта.
func TestBurnedVisitUsageReportedByOrchestrator(t *testing.T) {
	ctx := context.Background()
	srv, store, call := newTaskTokenServer(t)

	code, body := call("POST", "/v1/auth/tokens", rootToken, map[string]any{
		"name": "orchestrator", "principal": "agent:orchestrator", "project": "SDLC",
	})
	if code != http.StatusCreated {
		t.Fatalf("личный токен: %d %v", code, body)
	}
	personal := body["token"].(string)
	if code, body := call("POST", "/v1/tasks/SDLC-1/transitions/start", rootToken, nil); code != http.StatusOK {
		t.Fatalf("старт: %d %v", code, body)
	}
	claim := func() (string, float64) {
		t.Helper()
		code, body := call("POST", "/v1/tasks/SDLC-1/claim", personal, nil)
		if code != http.StatusOK {
			t.Fatalf("захват: %d %v", code, body)
		}
		task, _ := body["task"].(map[string]any)
		attempt, _ := task["attempt"].(float64)
		return body["stage_run_id"].(string), attempt
	}
	taskToken := func() string {
		t.Helper()
		code, body := call("POST", "/v1/tasks/SDLC-1/task-token", personal, nil)
		if code != http.StatusCreated {
			t.Fatalf("токен задачи: %d %v", code, body)
		}
		return body["token"].(string)
	}

	run, attempt := claim()
	if attempt != 1 {
		t.Fatalf("первый захват: попытка %v", attempt)
	}
	first := taskToken()

	// Аренда сгорела: воркер вернул прогон в pending, токен первого захода погас.
	if _, err := store.DB().Exec(ctx,
		`UPDATE stage_runs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	system, err := store.ResolveActor(ctx, store.DB(), "test-worker", domain.PrincipalSystem)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := srv.engine.ExpireLeases(ctx, system.ID, 10); err != nil || n != 1 {
		t.Fatalf("протухание: %d %v", n, err)
	}
	if code, body := call("POST", "/v1/tasks/SDLC-1/usage", first, map[string]any{"model": "m", "cost_usd": 1}); code != http.StatusUnauthorized {
		t.Fatalf("погасший токен задачи отчитался: %d %v", code, body)
	}

	// Повторный захват — тот же прогон и та же попытка.
	again, attemptAgain := claim()
	if again != run || attemptAgain != attempt {
		t.Fatalf("повторный захват: прогон %s попытка %v, было %s %v", again, attemptAgain, run, attempt)
	}
	if code, body := call("POST", "/v1/tasks/SDLC-1/runs/"+run+"/usage", personal, map[string]any{
		"model": "claude-sonnet-5-5", "input_tokens": 1000, "output_tokens": 500, "cost_usd": 15,
		"details": map[string]any{"zahod": 1, "note": "токен погас"},
	}); code != http.StatusCreated {
		t.Fatalf("стоимость сгоревшего захода личным токеном: %d %v", code, body)
	}
	second := taskToken()
	if code, body := call("POST", "/v1/tasks/SDLC-1/usage", second, map[string]any{
		"model": "claude-sonnet-5-5", "cost_usd": 2, "details": map[string]any{"zahod": 2},
	}); code != http.StatusCreated {
		t.Fatalf("стоимость второго захода токеном задачи: %d %v", code, body)
	}

	var rows, burned int
	if err := store.DB().QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE details->>'note' = 'токен погас')
		FROM stage_run_usage WHERE run_id = $1`, run).Scan(&rows, &burned); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || burned != 1 {
		t.Fatalf("строк учёта по прогону %d (сгоревших %d), ждали 2 и 1", rows, burned)
	}
}
