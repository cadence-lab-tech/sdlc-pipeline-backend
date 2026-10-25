package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// CLAB-214, AC-5: раннер отчитывается о стоимости после выхода агента, и
// аренду к этому моменту могли снять — release самого агента, уход прогона в
// gating под перепроверку, протухание. Отчёт по /runs/{id}/usage принимается
// от того, кто прогон забирал (по ленте stage.claimed), чужой получает 403.
func TestUsageAfterLeaseDropped(t *testing.T) {
	ctx := context.Background()
	srv, store, call := newTaskTokenServer(t)

	personal := func(handle string) string {
		t.Helper()
		code, body := call("POST", "/v1/auth/tokens", rootToken, map[string]any{
			"name": "runner", "principal": "agent:" + handle, "project": "SDLC",
		})
		if code != http.StatusCreated {
			t.Fatalf("личный токен %s: %d %v", handle, code, body)
		}
		return body["token"].(string)
	}
	pool, other := personal("pool"), personal("other")
	claim := func(key string) string {
		t.Helper()
		code, body := call("POST", "/v1/tasks/"+key+"/claim", pool, nil)
		if code != http.StatusOK {
			t.Fatalf("захват %s: %d %v", key, code, body)
		}
		return body["stage_run_id"].(string)
	}
	for _, key := range []string{"SDLC-1", "SDLC-2", "SDLC-3"} {
		if code, body := call("POST", "/v1/tasks/"+key+"/transitions/start", rootToken, nil); code != http.StatusOK {
			t.Fatalf("старт %s: %d %v", key, code, body)
		}
	}

	rows := func(runID string) int {
		t.Helper()
		var n int
		if err := store.DB().QueryRow(ctx, `
			SELECT count(*) FROM stage_run_usage
			WHERE run_id = $1 AND details->>'reported_by' = 'clab-runner'`, runID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	claimedBy := func(runID string) *string {
		t.Helper()
		var holder *string
		if err := store.DB().QueryRow(ctx,
			`SELECT claimed_by::text FROM stage_runs WHERE id = $1`, runID).Scan(&holder); err != nil {
			t.Fatal(err)
		}
		return holder
	}
	report := func(name, key, runID string) {
		t.Helper()
		if holder := claimedBy(runID); holder != nil {
			t.Fatalf("%s: аренда не снята, claimed_by = %s", name, *holder)
		}
		path := "/v1/tasks/" + key + "/runs/" + runID + "/usage"
		body := map[string]any{"model": "claude-sonnet-5", "cost_usd": 0.5,
			"details": map[string]any{"reported_by": "clab-runner"}}
		if code, out := call("POST", path, other, body); code != http.StatusForbidden {
			t.Errorf("%s: отчёт чужого принципала: %d %v, ожидали 403", name, code, out)
		}
		before := rows(runID)
		if code, out := call("POST", path, pool, body); code != http.StatusCreated {
			t.Fatalf("%s: отчёт того, кто забирал: %d %v, ожидали 201", name, code, out)
		}
		if got := rows(runID); got != before+1 {
			t.Fatalf("%s: строк учёта %d, ожидали %d", name, got, before+1)
		}
	}

	// Агент сам отпустил задачу.
	run := claim("SDLC-1")
	if code, body := call("POST", "/v1/tasks/SDLC-1/release", pool, map[string]any{"note": "агент отпустил"}); code != http.StatusOK {
		t.Fatalf("release: %d %v", code, body)
	}
	report("после release", "SDLC-1", run)

	// Переход увёл прогон в gating на перепроверку.
	if again := claim("SDLC-1"); again != run {
		t.Fatalf("повторный захват взял другой прогон: %s, а не %s", again, run)
	}
	for _, kind := range []string{"analysis.brief", "analysis.acceptance_criteria"} {
		if code, body := call("POST", "/v1/tasks/SDLC-1/artifacts", pool, map[string]any{
			"kind": kind, "title": kind, "content": "# " + kind,
		}); code != http.StatusCreated {
			t.Fatalf("сдача %s: %d %v", kind, code, body)
		}
	}
	// Переход заблокирован перепроверкой: ответ — отказ гейтов, а прогон
	// уходит в gating и теряет держателя.
	if _, body := call("POST", "/v1/tasks/SDLC-1/transitions/analysis_done", pool, map[string]any{}); errCode(body) != "gates_not_satisfied" {
		t.Fatalf("переход до перепроверки: %v", body)
	}
	var state string
	if err := store.DB().QueryRow(ctx, `SELECT state FROM stage_runs WHERE id = $1`, run).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "gating" {
		t.Fatalf("прогон не в gating: %s", state)
	}
	report("в gating", "SDLC-1", run)

	// Аренда протухла, воркер вернул прогон в pending.
	run3 := claim("SDLC-3")
	if _, err := store.DB().Exec(ctx,
		`UPDATE stage_runs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, run3); err != nil {
		t.Fatal(err)
	}
	system, err := store.ResolveActor(ctx, store.DB(), "test-worker", domain.PrincipalSystem)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := srv.engine.ExpireLeases(ctx, system.ID, 10); err != nil || n != 1 {
		t.Fatalf("протухание: %d %v", n, err)
	}
	report("после протухания", "SDLC-3", run3)

	// Прогон, которого принципал не забирал вовсе, по-прежнему чужой: никто
	// его не держит, но и в ленте захвата от него нет.
	var untouched string
	if err := store.DB().QueryRow(ctx, `
		SELECT sr.id::text FROM stage_runs sr JOIN tasks t ON t.id = sr.task_id
		WHERE t.key = 'SDLC-2' AND sr.kind = 'stage' AND sr.finished_at IS NULL`).Scan(&untouched); err != nil {
		t.Fatal(err)
	}
	if code, out := call("POST", "/v1/tasks/SDLC-2/runs/"+untouched+"/usage", pool,
		map[string]any{"model": "m"}); code != http.StatusForbidden {
		t.Errorf("отчёт о незабранном прогоне: %d %v, ожидали 403", code, out)
	}
}
