package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Гейт external_check читает то, что принёс воркер, и в сеть не ходит:
// он исполняется внутри транзакции перехода, и сетевой вызов означал бы
// открытую транзакцию на время таймаута чужого API. Отсюда и проверка —
// по строке в базе.
func TestExternalCheckReadsSyncedState(t *testing.T) {
	store, pool := testdb.Store(t)
	ctx := context.Background()

	engine := NewEngine(store, testdb.Logger(), Options{})
	task, err := store.GetTaskByKey(ctx, store.DB(), "SDLC-1")
	if err != nil {
		t.Fatalf("демо-задача: %v", err)
	}

	gate := domain.Gate{
		Key: "ci", Name: "CI", Type: domain.GateExternalCheck, Blocking: true,
		Config: json.RawMessage(`{"provider":"github_checks","required_conclusion":"success"}`),
	}
	eval := func() GateOutcome {
		t.Helper()
		out, err := engine.evalExternalCheck(ctx, store.DB(), EvalInput{Task: task, Gate: gate})
		if err != nil {
			t.Fatalf("прогон гейта: %v", err)
		}
		return out
	}

	// Привязанного PR нет — проверять нечего, и это провал, а не ожидание.
	if out := eval(); out.Status != domain.GateResultFailed {
		t.Fatalf("без PR ожидали failed: %+v", out)
	}

	var repoID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT repository_id FROM task_repositories WHERE task_id = $1 LIMIT 1`,
		task.ID).Scan(&repoID); err != nil {
		t.Fatalf("область задачи: %v", err)
	}
	var refID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO task_repo_refs (task_id, repository_id, ref_type, ref, state)
		VALUES ($1, $2, 'pull_request', '128', 'open') RETURNING id`,
		task.ID, repoID).Scan(&refID); err != nil {
		t.Fatalf("привязка PR: %v", err)
	}

	// PR привязан, но состояние проверок ещё не приезжало: непришедший ответ
	// GitHub — не отрицательное заключение.
	if out := eval(); out.Status != domain.GateResultPending {
		t.Fatalf("без синхронизации ожидали pending: %+v", out)
	}

	sync := func(checks string) {
		t.Helper()
		if err := store.UpdateRepoRefChecks(ctx, store.DB(), refID, "open", "sha1",
			"https://example/pr/128", json.RawMessage(checks)); err != nil {
			t.Fatalf("синхронизация: %v", err)
		}
	}

	sync(`{"conclusion":"","runs":[{"name":"build","status":"in_progress","conclusion":""}]}`)
	if out := eval(); out.Status != domain.GateResultPending {
		t.Fatalf("идущая проверка держит переход как pending: %+v", out)
	}

	sync(`{"conclusion":"failure","runs":[{"name":"build","status":"completed","conclusion":"failure"}]}`)
	if out := eval(); out.Status != domain.GateResultFailed {
		t.Fatalf("упавшая проверка обязана валить гейт: %+v", out)
	}

	sync(`{"conclusion":"success","runs":[{"name":"build","status":"completed","conclusion":"success"}]}`)
	if out := eval(); out.Status != domain.GateResultPassed {
		t.Fatalf("зелёный CI обязан пропускать: %+v", out)
	}

	// Именованные проверки: гейт требует конкретную, а не общий вердикт.
	gate.Config = json.RawMessage(`{"provider":"github_checks","contexts":["e2e"]}`)
	if out := eval(); out.Status != domain.GateResultPending {
		t.Fatalf("не запускавшаяся требуемая проверка — это pending: %+v", out)
	}

	sync(`{"conclusion":"success","runs":[
		{"name":"build","status":"completed","conclusion":"success"},
		{"name":"e2e","status":"completed","conclusion":"failure"}]}`)
	if out := eval(); out.Status != domain.GateResultFailed {
		t.Fatalf("падение именованной проверки обязано валить гейт: %+v", out)
	}
}
