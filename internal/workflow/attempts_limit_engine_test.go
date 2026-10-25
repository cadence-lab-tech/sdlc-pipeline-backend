package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Интеграционные проверки предела попыток (CLAB-217) по настоящей схеме:
// счёт попыток и EnterStatus — один SQL, и проверить их согласие можно
// только на базе.

// attemptsCycle — цикл задач с пределом 2 на двух откатах в development и
// откатом без предела. В development задача входит прямым переходом (start),
// откатом с проверки (reject_to_development) и откатом с QA (qa_failed):
// счётчик у них общий.
func attemptsCycle() BundleWorkflow {
	yes := true
	limit := func() []BundleGate {
		return []BundleGate{{Key: "attempts", Name: "Не больше двух попыток", Type: domain.GateAttemptsLimit,
			Blocking: &yes, Config: json.RawMessage(`{"max_attempts": 2}`)}}
	}
	return BundleWorkflow{
		Name: "Attempts", TaskTypes: []string{"task"},
		Statuses: []BundleStatus{
			{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
			{Key: "development", Name: "Development", Category: "active"},
			{Key: "review", Name: "Review", Category: "active"},
			{Key: "qa", Name: "QA", Category: "active"},
			{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
			{Key: "cancelled", Name: "Cancelled", Category: "cancelled", IsTerminal: true},
		},
		Transitions: []BundleTransition{
			{Key: "start", Name: "Start", From: "new", To: "development", Kind: "advance"},
			{Key: "dev_done", Name: "Dev done", From: "development", To: "review", Kind: "advance"},
			{Key: "review_ok", Name: "Review ok", From: "review", To: "qa", Kind: "advance"},
			{Key: "reject_to_development", Name: "Back", From: "review", To: "development", Kind: "reject", Gates: limit()},
			{Key: "qa_failed", Name: "QA failed", From: "qa", To: "development", Kind: "reject", Gates: limit()},
			{Key: "rework", Name: "Rework", From: "review", To: "development", Kind: "reject"},
			{Key: "finish", Name: "Finish", From: "qa", To: "done", Kind: "advance"},
			{Key: "cancel", Name: "Cancel", To: "cancelled", Kind: "cancel"},
		},
	}
}

type attemptsWorld struct {
	store  *postgres.Store
	engine *Engine
	system domain.Principal
}

func newAttemptsWorld(t *testing.T) attemptsWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{attemptsCycle()}}, false)
	if err != nil {
		t.Fatalf("импорт цикла: %v", err)
	}
	if len(report.Validations) != 1 || !report.Validations[0].Valid {
		t.Fatalf("цикл с пределом 2 не прошёл проверку: %+v", report.Validations)
	}
	id, err := uuid.Parse(report.Workflows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authoring.Activate(ctx, id, system.ID); err != nil {
		t.Fatalf("активация цикла с пределом: %v", err)
	}
	return attemptsWorld{store: store, engine: engine, system: system}
}

func (w attemptsWorld) task(t *testing.T, title string) string {
	t.Helper()
	task, err := w.engine.CreateTask(context.Background(), "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: title}})
	if err != nil {
		t.Fatal(err)
	}
	return task.Key
}

// move проводит задачу по переходам, каждый обязан пройти.
func (w attemptsWorld) move(t *testing.T, key string, transitions ...string) TransitionResult {
	t.Helper()
	var res TransitionResult
	for _, tr := range transitions {
		var err error
		res, err = w.engine.Execute(context.Background(), key, tr, w.system.ID, "")
		if err != nil || res.Blocked {
			t.Fatalf("%s %s: %+v %v", key, tr, res.Gates, err)
		}
	}
	return res
}

func (w attemptsWorld) current(t *testing.T, key string) (domain.Task, domain.StageRun) {
	t.Helper()
	ctx := context.Background()
	task, err := w.store.GetTaskByKey(ctx, w.store.DB(), key)
	if err != nil {
		t.Fatal(err)
	}
	run, err := w.store.CurrentStageRun(ctx, w.store.DB(), task.ID, task.StatusID)
	if err != nil {
		t.Fatal(err)
	}
	return task, run
}

func (w attemptsWorld) devRuns(t *testing.T, task domain.Task) int {
	t.Helper()
	var n int
	if err := w.store.DB().QueryRow(context.Background(), `
		SELECT count(*) FROM stage_runs sr JOIN workflow_statuses ws ON ws.id = sr.status_id
		WHERE sr.task_id = $1 AND ws.key = 'development' AND sr.kind = 'stage'`, task.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w attemptsWorld) result(t *testing.T, run domain.StageRun, gateKey string) *domain.GateResult {
	t.Helper()
	var gateID uuid.UUID
	if err := w.store.DB().QueryRow(context.Background(), `
		SELECT g.id FROM gates g JOIN workflow_transitions tr ON tr.id = g.transition_id
		JOIN stage_runs sr ON sr.id = $1
		JOIN tasks tk ON tk.id = sr.task_id AND tk.workflow_id = tr.workflow_id
		WHERE tr.key = $2 AND g.key = 'attempts'`, run.ID, gateKey).Scan(&gateID); err != nil {
		t.Fatal(err)
	}
	r, err := w.store.GateDecision(context.Background(), w.store.DB(), run.ID, gateID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Критерии 6–13: откат в пределе проходит, сверх предела — blocked с цифрами
// в Preview, gate_results и ленте; счёт общий для всех входов в статус и не
// видит прогонов перепроверки; снимает только человек и только waived, и
// один waive — одна попытка.
func TestAttemptsLimitBlocksAndHumanWaives(t *testing.T) {
	ctx := context.Background()
	w := newAttemptsWorld(t)
	key := w.task(t, "Петля проверки")

	// Вход 1 — прямой переход, вход 2 — откат с проверки: в пределе.
	w.move(t, key, "start", "dev_done")
	res := w.move(t, key, "reject_to_development")
	if res.NewRun == nil || res.NewRun.Attempt != 2 {
		t.Fatalf("первый откат: попытка %+v", res.NewRun)
	}
	w.move(t, key, "dev_done", "review_ok")

	// Прогон перепроверки на development — не вход в статус: попытка 50 у
	// него не сдвигает счёт этапа.
	task, qaRun := w.current(t, key)
	if _, err := w.store.DB().Exec(ctx, `
		INSERT INTO stage_runs (task_id, status_id, kind, parent_run_id, gate_id, attempt, state)
		SELECT sr.task_id, sr.status_id, 'review', sr.id, g.id, 50, 'passed'
		FROM stage_runs sr, gates g JOIN workflow_transitions tr ON tr.id = g.transition_id
		WHERE sr.id = $1 AND tr.workflow_id = $2 AND tr.key = 'qa_failed'`,
		res.NewRun.ID, task.WorkflowID); err != nil {
		t.Fatal(err)
	}

	// Третий вход — откат с QA, другим переходом: сверх предела.
	out := gateOutcome(t, ctx, w.engine, task, "qa_failed", "attempts")
	if out.Status != domain.GateResultFailed ||
		!strings.Contains(out.Reason, "попытка 3 из 2 в статус development") {
		t.Fatalf("Preview: %+v", out)
	}
	blocked, err := w.engine.Execute(ctx, key, "qa_failed", w.system.ID, "")
	if err != nil || !blocked.Blocked {
		t.Fatalf("откат сверх предела должен блокироваться: %+v %v", blocked, err)
	}
	task, run := w.current(t, key)
	if task.StatusKey != "qa" || run.ID != qaRun.ID || w.devRuns(t, task) != 2 {
		t.Fatalf("задача обязана остаться на месте: %s, прогонов development %d", task.StatusKey, w.devRuns(t, task))
	}
	if r := w.result(t, run, "qa_failed"); r == nil || r.Status != domain.GateResultFailed || r.DecidedBy != nil {
		t.Fatalf("gate_results: %+v", r)
	}
	events, err := w.store.ListEvents(ctx, w.store.DB(), task.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var failed map[string]any
	for _, ev := range events {
		if ev.Type != domain.EventGateFailed {
			continue
		}
		var p struct {
			Transition string        `json:"transition"`
			Gates      []GateOutcome `json:"gates"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		for _, g := range p.Gates {
			if p.Transition == "qa_failed" && g.GateKey == "attempts" {
				failed = g.Details
			}
		}
	}
	if failed == nil || failed["status"] != "development" || failed["next_attempt"] != float64(3) ||
		failed["max_attempts"] != float64(2) || failed["transition"] != "qa_failed" {
		t.Fatalf("gate.failed без цифр: %+v", failed)
	}

	decide := func(actor uuid.UUID, bound *TaskBinding, status string) error {
		_, err := w.engine.DecideGate(ctx, key, "qa_failed", "attempts", actor, bound, status, json.RawMessage(`{}`))
		return err
	}
	// Токен задачи, агент (уровень в режиме заголовка максимальный) — отказ.
	bound := &TaskBinding{TaskID: task.ID, StageRunID: run.ID, TaskKey: key}
	if err := decide(w.system.ID, bound, domain.GateResultWaived); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("токен задачи снял предел: %v", err)
	}
	agent := mustActor(t, w.store, "agent:loop-bot")
	err = decide(agent.ID, nil, domain.GateResultWaived)
	if apperr.As(err).Code != apperr.CodeForbidden || !strings.Contains(err.Error(), "закрывает человек") {
		t.Fatalf("агент снял предел: %v", err)
	}
	// Человек, но не waived — неверный ввод с подсказкой.
	human := mustActor(t, w.store, "user:maintainer")
	for _, status := range []string{domain.GateResultPassed, domain.GateResultFailed} {
		err := decide(human.ID, nil, status)
		if apperr.As(err).Code != apperr.CodeInvalidInput || !strings.Contains(err.Error(), "waived") {
			t.Fatalf("%s по пределу: %v", status, err)
		}
	}
	if r := w.result(t, run, "qa_failed"); r.Status != domain.GateResultFailed || r.DecidedBy != nil {
		t.Fatalf("отказ записал решение: %+v", r)
	}

	// Человек даёт попытку: waived, и откат проходит третьей попыткой.
	if err := decide(human.ID, nil, domain.GateResultWaived); err != nil {
		t.Fatalf("waive человеком: %v", err)
	}
	res = w.move(t, key, "qa_failed")
	if res.NewRun == nil || res.NewRun.Attempt != 3 {
		t.Fatalf("откат после waive: %+v", res.NewRun)
	}
	if r := w.result(t, run, "qa_failed"); r.Status != domain.GateResultWaived || r.DecidedBy == nil || *r.DecidedBy != human.ID {
		t.Fatalf("переход стёр, кто дал попытку: %+v", r)
	}
	decided := false
	events, _ = w.store.ListEvents(ctx, w.store.DB(), task.ID, 100)
	for _, ev := range events {
		if ev.Type == domain.EventGateDecided && ev.ActorID != nil && *ev.ActorID == human.ID &&
			strings.Contains(string(ev.Payload), `"waived"`) {
			decided = true
		}
	}
	if !decided {
		t.Fatal("в ленте нет gate.decided человека")
	}

	// Следующий круг: прежний waive не действует, попытка 4 — снова failed.
	w.move(t, key, "dev_done")
	task, _ = w.current(t, key)
	if out := gateOutcome(t, ctx, w.engine, task, "reject_to_development", "attempts"); out.Status != domain.GateResultFailed ||
		!strings.Contains(out.Reason, "попытка 4 из 2") {
		t.Fatalf("второй круг: %+v", out)
	}
	if blocked, err := w.engine.Execute(ctx, key, "reject_to_development", w.system.ID, ""); err != nil || !blocked.Blocked {
		t.Fatalf("второй круг должен блокироваться: %+v %v", blocked, err)
	}
}

// Критерий 14: без гейта предела откатывать можно сколько угодно.
func TestAttemptsWithoutLimitAreUnbounded(t *testing.T) {
	w := newAttemptsWorld(t)
	key := w.task(t, "Без предела")
	w.move(t, key, "start")
	var res TransitionResult
	for range 5 {
		w.move(t, key, "dev_done")
		res = w.move(t, key, "rework")
	}
	if res.NewRun == nil || res.NewRun.Attempt != 6 {
		t.Fatalf("пятый откат без предела: %+v", res.NewRun)
	}
}

// Критерий 4: связка переносит гейт без потерь, повторный импорт — unchanged.
func TestAttemptsLimitBundleRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)
	report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{attemptsCycle()}}, false)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := uuid.Parse(report.Workflows[0].ID)
	if _, err := authoring.Activate(ctx, id, system.ID); err != nil {
		t.Fatal(err)
	}
	exported, err := authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	var cycle *BundleWorkflow
	for i := range exported.Workflows {
		if exported.Workflows[i].Name == "Attempts" {
			cycle = &exported.Workflows[i]
		}
	}
	if cycle == nil {
		t.Fatal("в экспорте нет цикла с пределом")
	}
	found := 0
	for _, tr := range cycle.Transitions {
		for _, g := range tr.Gates {
			if g.Type != domain.GateAttemptsLimit {
				continue
			}
			n, err := maxAttempts(g.Config)
			if err != nil || n != 2 || g.Key != "attempts" {
				t.Fatalf("гейт %s в экспорте: %+v %v", tr.Key, g, err)
			}
			found++
		}
	}
	if found != 2 {
		t.Fatalf("в экспорте %d гейтов предела, ждали 2", found)
	}

	again, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{*cycle}}, false)
	if err != nil || len(again.Workflows) != 1 || again.Workflows[0].Outcome != "unchanged" {
		t.Fatalf("повторный импорт: %+v %v", again.Workflows, err)
	}

	// В другой проект — черновик с тем же гейтом.
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO projects (key, name, owner_team_id)
		SELECT 'OTHER', 'Другой', owner_team_id FROM projects WHERE key = 'SDLC'`); err != nil {
		t.Fatal(err)
	}
	other, err := authoring.Import(ctx, "OTHER", system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{*cycle}}, false)
	if err != nil || len(other.Workflows) != 1 || other.Workflows[0].Outcome != "draft_created" ||
		len(other.Validations) != 1 || !other.Validations[0].Valid {
		t.Fatalf("импорт в другой проект: %+v %+v %v", other.Workflows, other.Validations, err)
	}
	draftID, _ := uuid.Parse(other.Workflows[0].ID)
	draft, err := store.GetWorkflow(ctx, store.DB(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	carried := 0
	for _, tr := range draft.Transitions {
		for _, g := range tr.Gates {
			if g.Type == domain.GateAttemptsLimit {
				if n, err := maxAttempts(g.Config); err == nil && n == 2 {
					carried++
				}
			}
		}
	}
	if carried != 2 {
		t.Fatalf("в черновике другого проекта %d гейтов предела", carried)
	}
}

// Критерий 5: up принимает новый тип и по-прежнему отбивает незнакомый; down
// на базе с гейтами нового типа их удаляет, а не падает, и возвращает CHECK
// из десяти типов.
func TestAttemptsLimitMigration(t *testing.T) {
	ctx := context.Background()
	w := newAttemptsWorld(t)
	db := w.store.DB()
	read := func(name string) string {
		body, err := migrations.Schema.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	count := func() int {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM gates WHERE type = 'attempts_limit'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	insert := func(gateType string) error {
		_, err := db.Exec(ctx, `
			INSERT INTO gates (transition_id, key, name, type, config)
			SELECT id, 'probe_' || $1, 'probe', $1, '{"max_attempts": 1}'
			FROM workflow_transitions WHERE key = 'rework' LIMIT 1`, gateType)
		return err
	}
	if count() != 2 {
		t.Fatalf("гейтов предела в активном цикле: %d", count())
	}
	if err := insert("no_such_gate"); err == nil {
		t.Fatal("CHECK пропустил незнакомый тип")
	}

	if _, err := db.Exec(ctx, read("000029_attempts_limit.down.sql")); err != nil {
		t.Fatalf("down на базе с гейтами предела: %v", err)
	}
	if count() != 0 {
		t.Fatal("down оставил гейты предела")
	}
	if err := insert(domain.GateAttemptsLimit); err == nil {
		t.Fatal("после down CHECK принимает attempts_limit")
	}
	if err := insert(domain.GateDependenciesClosed); err != nil {
		t.Fatalf("после down десятый тип не принимается: %v", err)
	}

	if _, err := db.Exec(ctx, read("000029_attempts_limit.up.sql")); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := insert(domain.GateAttemptsLimit); err != nil {
		t.Fatalf("после up attempts_limit не принимается: %v", err)
	}
	if err := insert("no_such_gate"); err == nil {
		t.Fatal("после up CHECK пропустил незнакомый тип")
	}
}

// Критерий 16: предел — свойство цикла; в ролях его нет ни в схеме, ни в
// связке.
func TestAttemptsLimitIsNotARoleSetting(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, _ := setupPhase4(t, AuthHeader)
	var n int
	if err := store.DB().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE column_name = 'max_attempts' AND table_name IN ('roles', 'role_versions')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("у роли появилась колонка max_attempts")
	}
	exported, err := authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := json.Marshal(exported.Roles)
	if len(exported.Roles) == 0 || strings.Contains(string(roles), "max_attempts") {
		t.Fatalf("роли связки несут max_attempts: %s", roles)
	}
}

// Критерий 2: черновик с негодным пределом ложится, но не активируется;
// активный цикл остаётся прежним.
func TestAttemptsLimitInvalidDraftIsNotActivated(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)
	cycle := attemptsCycle()
	cycle.Transitions[3].Gates[0].Config = json.RawMessage(`{"max_attempts": 0}`)
	report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{cycle}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Validations) != 1 || report.Validations[0].Valid {
		t.Fatalf("негодный предел прошёл проверку: %+v", report.Validations)
	}
	id, _ := uuid.Parse(report.Workflows[0].ID)
	if _, err := authoring.Activate(ctx, id, system.ID); err == nil {
		t.Fatal("цикл с max_attempts 0 активирован")
	}
	head, err := store.WorkflowHead(ctx, store.DB(), id)
	if err != nil || head.State != domain.WorkflowDraft {
		t.Fatalf("черновик сменил состояние: %+v %v", head, err)
	}
}
