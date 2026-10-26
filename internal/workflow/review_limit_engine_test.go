package workflow

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Интеграционные проверки предела перепроверок agent_review (CLAB-218) по
// настоящей схеме: счёт проваленных перепроверок, вычислитель гейта и
// заведение прогона ревью сходятся только на базе.

// reviewCycle — цикл, где переход dev_done держит перепроверка ролью checker
// с config гейта peer_review; maker — исполнитель development. У ролей нет
// схемы вердикта: так перепроверку можно закрыть и без поля, и нечитаемым
// артефактом.
func reviewCycle(gateConfig string) Bundle {
	yes := true
	return Bundle{
		Version: BundleVersion,
		Roles: []BundleRole{
			{Key: "maker", Name: "Исполнитель", Instructions: "Делай.",
				OutputContract: json.RawMessage(`[{"kind":"dev.note","required":true}]`)},
			{Key: "checker", Name: "Проверяющий", Instructions: "Проверяй.",
				OutputContract: json.RawMessage(`[{"kind":"review.verdict","required":true}]`)},
		},
		Workflows: []BundleWorkflow{{
			Name: "Reviews", TaskTypes: []string{"task"},
			Statuses: []BundleStatus{
				{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
				{Key: "development", Name: "Development", Category: "active", Role: "maker", Reviewers: []string{"checker"}},
				{Key: "testing", Name: "Testing", Category: "active", Role: "maker"},
				{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
				{Key: "cancelled", Name: "Cancelled", Category: "cancelled", IsTerminal: true},
			},
			Transitions: []BundleTransition{
				{Key: "start", Name: "Start", From: "new", To: "development", Kind: "advance"},
				{Key: "dev_done", Name: "Dev done", From: "development", To: "testing", Kind: "advance",
					Gates: []BundleGate{{Key: "peer_review", Name: "Перепроверка", Type: domain.GateAgentReview,
						Blocking: &yes, Config: json.RawMessage(gateConfig)}}},
				{Key: "back", Name: "Back", From: "testing", To: "development", Kind: "reject"},
				{Key: "finish", Name: "Finish", From: "testing", To: "done", Kind: "advance"},
				{Key: "cancel", Name: "Cancel", To: "cancelled", Kind: "cancel"},
			},
		}},
	}
}

type reviewWorld struct {
	store     *postgres.Store
	engine    *Engine
	authoring *Authoring
	system    domain.Principal
	key       string
}

func newReviewWorld(t *testing.T, gateConfig string) reviewWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	report, err := authoring.Import(ctx, "SDLC", system.ID, reviewCycle(gateConfig), false)
	if err != nil {
		t.Fatalf("импорт цикла: %v", err)
	}
	if len(report.Validations) != 1 || !report.Validations[0].Valid {
		t.Fatalf("цикл перепроверки не прошёл проверку: %+v", report.Validations)
	}
	id, err := uuid.Parse(report.Workflows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authoring.Activate(ctx, id, system.ID); err != nil {
		t.Fatalf("активация цикла: %v", err)
	}
	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Петля перепроверки"}})
	if err != nil {
		t.Fatal(err)
	}
	w := reviewWorld{store: store, engine: engine, authoring: authoring, system: system, key: task.Key}
	w.mustMove(t, "start")
	return w
}

func (w reviewWorld) current(t *testing.T) (domain.Task, domain.StageRun) {
	t.Helper()
	ctx := context.Background()
	task, err := w.store.GetTaskByKey(ctx, w.store.DB(), w.key)
	if err != nil {
		t.Fatal(err)
	}
	run, err := w.store.CurrentStageRun(ctx, w.store.DB(), task.ID, task.StatusID)
	if err != nil {
		t.Fatal(err)
	}
	return task, run
}

func (w reviewWorld) mustMove(t *testing.T, transition string) TransitionResult {
	t.Helper()
	res, err := w.engine.Execute(context.Background(), w.key, transition, w.system.ID, "")
	if err != nil || res.Blocked {
		t.Fatalf("%s: %+v %v", transition, res.Gates, err)
	}
	return res
}

func (w reviewWorld) mustBlock(t *testing.T) TransitionResult {
	t.Helper()
	res, err := w.engine.Execute(context.Background(), w.key, "dev_done", w.system.ID, "")
	if err != nil || !res.Blocked {
		t.Fatalf("dev_done обязан блокироваться: %+v %v", res, err)
	}
	return res
}

// submit — исполнитель сдаёт новую версию результата в текущий этап.
func (w reviewWorld) submit(t *testing.T) {
	t.Helper()
	_, run := w.current(t)
	if _, err := w.engine.SubmitArtifact(context.Background(), w.key, w.system.ID, SubmitArtifactInput{
		Kind: "dev.note", Title: "Результат", Content: []byte("# результат"), StageRunID: &run.ID,
	}); err != nil {
		t.Fatalf("сдача результата: %v", err)
	}
}

// openReview — незакрытая перепроверка текущего этапа; её быть обязано.
func (w reviewWorld) openReview(t *testing.T) domain.StageRun {
	t.Helper()
	_, run := w.current(t)
	open, err := w.store.OpenReviewRuns(context.Background(), w.store.DB(), run.ID)
	if err != nil || len(open) != 1 {
		t.Fatalf("открытых перепроверок %d, ждали одну: %v", len(open), err)
	}
	return open[0]
}

// verdict — ревьюер закрывает открытую перепроверку артефактом content.
func (w reviewWorld) verdict(t *testing.T, content string) {
	t.Helper()
	review := w.openReview(t)
	if _, err := w.engine.SubmitArtifact(context.Background(), w.key, w.system.ID, SubmitArtifactInput{
		Kind: "review.verdict", Title: "Вердикт", Format: "json", Content: []byte(content), StageRunID: &review.ID,
	}); err != nil {
		t.Fatalf("сдача вердикта: %v", err)
	}
}

// round — круг петли: сдача, запрос перехода, перепроверка с вердиктом.
func (w reviewWorld) round(t *testing.T, content string) {
	t.Helper()
	w.submit(t)
	w.mustBlock(t)
	w.verdict(t, content)
}

const (
	failVerdict = `{"verdict":"fail","findings":["нет тестов"]}`
	passVerdict = `{"verdict":"pass","findings":[]}`
)

// reviews — прогонов перепроверки у родительского прогона run.
func (w reviewWorld) reviews(t *testing.T, run domain.StageRun) int {
	t.Helper()
	var n int
	if err := w.store.DB().QueryRow(context.Background(), `
		SELECT count(*) FROM stage_runs WHERE parent_run_id = $1 AND kind = 'review'`, run.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w reviewWorld) events(t *testing.T, eventType string) []domain.TaskEvent {
	t.Helper()
	task, _ := w.current(t)
	all, err := w.store.ListEvents(context.Background(), w.store.DB(), task.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	out := []domain.TaskEvent{}
	for _, ev := range all {
		if ev.Type == eventType {
			out = append(out, ev)
		}
	}
	return out
}

func (w reviewWorld) gate(t *testing.T) GateOutcome {
	t.Helper()
	task, _ := w.current(t)
	return gateOutcome(t, context.Background(), w.engine, task, "dev_done", "peer_review")
}

func (w reviewWorld) result(t *testing.T, run domain.StageRun) *domain.GateResult {
	t.Helper()
	var gateID uuid.UUID
	if err := w.store.DB().QueryRow(context.Background(), `
		SELECT g.id FROM gates g JOIN workflow_transitions tr ON tr.id = g.transition_id
		JOIN tasks tk ON tk.workflow_id = tr.workflow_id AND tk.key = $1
		WHERE tr.key = 'dev_done' AND g.key = 'peer_review'`, w.key).Scan(&gateID); err != nil {
		t.Fatal(err)
	}
	r, err := w.store.GateDecision(context.Background(), w.store.DB(), run.ID, gateID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// mustLimit — гейт держит переход пределом: failed с цифрами в причине и
// details, где failed_reviews и max_attempts — числа из Preview (int).
func mustLimit(t *testing.T, out GateOutcome, failed, limit int) {
	t.Helper()
	want := "перепроверок " + strconv.Itoa(failed) + " из " + strconv.Itoa(limit)
	if out.Status != domain.GateResultFailed || !strings.Contains(out.Reason, want) ||
		!strings.Contains(out.Reason, "waived") {
		t.Fatalf("гейт без предела в причине %q: %+v", want, out)
	}
	if out.Details["failed_reviews"] != failed || out.Details["max_attempts"] != limit ||
		out.Details["verdict"] == nil || out.Details["verdict"] == "pass" {
		t.Fatalf("details без цифр и вердикта: %+v", out.Details)
	}
}

// Критерии 3, 4, 5, 7, 8, 9: две проваленные перепроверки при пределе 2 —
// переход blocked без новой перепроверки, с цифрами в Preview и ленте;
// снимает только человек и только waived; новая сдача снимает waive; после
// waive переход проходит без перепроверки, и в gate_results остаётся, кто снял.
func TestReviewLimitBlocksAndHumanWaives(t *testing.T) {
	ctx := context.Background()
	w := newReviewWorld(t, `{"reviewer_role_key":"checker","max_attempts":2}`)
	_, parent := w.current(t)

	w.round(t, failVerdict)
	if out := w.gate(t); out.Status != domain.GateResultFailed || !strings.Contains(out.Reason, "вердикт ревьюера: fail") {
		t.Fatalf("первый fail в пределе — обычный вердикт: %+v", out)
	}
	w.round(t, failVerdict)
	mustLimit(t, w.gate(t), 2, 2)

	// Доработка и запрос перехода: blocked, третьей перепроверки нет.
	requested := len(w.events(t, domain.EventReviewRequested))
	w.submit(t)
	mustLimit(t, w.gate(t), 2, 2)
	res := w.mustBlock(t)
	if w.reviews(t, parent) != 2 || len(w.events(t, domain.EventReviewRequested)) != requested || requested != 2 {
		t.Fatalf("сверх предела заведена перепроверка: прогонов %d, review.requested %d→%d",
			w.reviews(t, parent), requested, len(w.events(t, domain.EventReviewRequested)))
	}
	if _, run := w.current(t); run.ID != parent.ID || run.State == domain.StageGating {
		t.Fatalf("этап ушёл в gating или сменился: %+v", run)
	}
	for _, g := range res.Gates {
		if g.GateKey == "peer_review" {
			mustLimit(t, g, 2, 2)
		}
	}
	var failedEvent map[string]any
	for _, ev := range w.events(t, domain.EventGateFailed) {
		var p struct {
			Transition string        `json:"transition"`
			Gates      []GateOutcome `json:"gates"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		for _, g := range p.Gates {
			if p.Transition == "dev_done" && g.GateKey == "peer_review" && g.Details["failed_reviews"] != nil {
				failedEvent = g.Details
			}
		}
	}
	if failedEvent == nil || failedEvent["failed_reviews"] != float64(2) || failedEvent["max_attempts"] != float64(2) {
		t.Fatalf("gate.failed без цифр предела: %+v", failedEvent)
	}

	// Повтор без новой сдачи — тот же blocked.
	w.mustBlock(t)
	if w.reviews(t, parent) != 2 {
		t.Fatal("повторный запрос завёл перепроверку")
	}

	decide := func(actor uuid.UUID, bound *TaskBinding, status string) error {
		_, err := w.engine.DecideGate(ctx, w.key, "dev_done", "peer_review", actor, bound, status, json.RawMessage(`{}`))
		return err
	}
	task, _ := w.current(t)
	bound := &TaskBinding{TaskID: task.ID, StageRunID: parent.ID, TaskKey: w.key}
	if err := decide(w.system.ID, bound, domain.GateResultWaived); apperr.As(err).Code != apperr.CodeForbidden ||
		!strings.Contains(err.Error(), ExecutorRefusal) {
		t.Fatalf("токен задачи снял предел перепроверок: %v", err)
	}
	agent := mustActor(t, w.store, "agent:loop-bot")
	if err := decide(agent.ID, nil, domain.GateResultWaived); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("агент личным токеном снял предел: %v", err)
	}
	human := mustActor(t, w.store, "user:maintainer")
	for _, status := range []string{domain.GateResultPassed, domain.GateResultFailed} {
		if err := decide(human.ID, nil, status); apperr.As(err).Code != apperr.CodeInvalidInput ||
			!strings.Contains(err.Error(), "waived") {
			t.Fatalf("%s человеком по пределу: %v", status, err)
		}
	}

	// Waive, затем новая сдача: waive снят (решение 0042), гейт снова держит.
	if err := decide(human.ID, nil, domain.GateResultWaived); err != nil {
		t.Fatalf("waive человеком: %v", err)
	}
	if out := w.gate(t); out.Status != domain.GateResultWaived {
		t.Fatalf("после waive гейт %+v", out)
	}
	w.submit(t)
	mustLimit(t, w.gate(t), 2, 2)
	w.mustBlock(t)

	// Снова waive — переход проходит без новой перепроверки.
	if err := decide(human.ID, nil, domain.GateResultWaived); err != nil {
		t.Fatalf("повторный waive: %v", err)
	}
	moved := w.mustMove(t, "dev_done")
	if w.reviews(t, parent) != 2 {
		t.Fatal("waive завёл перепроверку")
	}
	if r := w.result(t, parent); r == nil || r.Status != domain.GateResultWaived ||
		r.DecidedBy == nil || *r.DecidedBy != human.ID {
		t.Fatalf("переход стёр, кто снял предел: %+v", r)
	}
	decided := false
	for _, ev := range w.events(t, domain.EventGateDecided) {
		if ev.ActorID != nil && *ev.ActorID == human.ID && strings.Contains(string(ev.Payload), `"waived"`) {
			decided = true
		}
	}
	if !decided || moved.Task.StatusKey != "testing" {
		t.Fatalf("в ленте нет решения человека или задача не в testing: %v %s", decided, moved.Task.StatusKey)
	}

	// Критерий 6: новый вход в статус — новый родительский прогон, счёт с нуля.
	w.mustMove(t, "back")
	_, again := w.current(t)
	if again.ID == parent.ID {
		t.Fatal("возврат не открыл новый прогон")
	}
	w.submit(t)
	w.mustBlock(t)
	if w.reviews(t, again) != 1 {
		t.Fatalf("на новом прогоне перепроверок %d, ждали одну", w.reviews(t, again))
	}
}

// Критерий 5: в счёт идут перепроверки, закрытые без вердикта и нечитаемым
// артефактом; пройденная — нет. При пределе 3 после двух провалов третья
// перепроверка заводится.
func TestReviewLimitCountsOnlyRejected(t *testing.T) {
	w := newReviewWorld(t, `{"reviewer_role_key":"checker","max_attempts":3}`)
	_, parent := w.current(t)

	w.round(t, `{"findings":[]}`) // без поля verdict
	w.round(t, passVerdict)
	w.round(t, "не JSON")
	if out := w.gate(t); out.Status != domain.GateResultFailed || strings.Contains(out.Reason, "перепроверок") {
		t.Fatalf("два провала из трёх — ещё не предел: %+v", out)
	}
	// Третья проваленная перепроверка заводится — пройденная не в счёте.
	w.round(t, failVerdict)
	if w.reviews(t, parent) != 4 {
		t.Fatalf("перепроверок %d, ждали четыре", w.reviews(t, parent))
	}
	mustLimit(t, w.gate(t), 3, 3)
	w.submit(t)
	w.mustBlock(t)
	if w.reviews(t, parent) != 4 {
		t.Fatal("сверх предела 3 заведена перепроверка")
	}
}

// Критерий 10: без max_attempts петля не ограничена, и гейт решает агент с
// личным токеном, как раньше.
func TestReviewWithoutLimitIsUnbounded(t *testing.T) {
	ctx := context.Background()
	w := newReviewWorld(t, `{"reviewer_role_key":"checker"}`)
	_, parent := w.current(t)
	for range 3 {
		w.round(t, failVerdict)
	}
	w.submit(t)
	w.mustBlock(t)
	if w.reviews(t, parent) != 4 {
		t.Fatalf("без предела перепроверок %d, ждали четыре", w.reviews(t, parent))
	}
	agent := mustActor(t, w.store, "agent:loop-bot")
	if _, err := w.engine.DecideGate(ctx, w.key, "dev_done", "peer_review", agent.ID, nil,
		domain.GateResultPassed, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("без предела агент решает гейт, как раньше: %v", err)
	}
}

// Критерий 11: негодный предел в активном цикле (в обход валидатора) не
// роняет Preview и переход и не действует.
func TestReviewLimitInvalidInActiveCycleIsIgnored(t *testing.T) {
	ctx := context.Background()
	w := newReviewWorld(t, `{"reviewer_role_key":"checker","max_attempts":1}`)
	task, parent := w.current(t)
	if _, err := w.store.DB().Exec(ctx, `
		UPDATE gates g SET config = '{"reviewer_role_key":"checker","max_attempts":2.5}'
		FROM workflow_transitions tr
		WHERE tr.id = g.transition_id AND tr.workflow_id = $1 AND g.key = 'peer_review'`, task.WorkflowID); err != nil {
		t.Fatal(err)
	}
	w.round(t, failVerdict)
	w.round(t, failVerdict)
	if out := w.gate(t); out.Status != domain.GateResultFailed || strings.Contains(out.Reason, "перепроверок") {
		t.Fatalf("негодный предел подействовал: %+v", out)
	}
	w.submit(t)
	w.mustBlock(t)
	if w.reviews(t, parent) != 3 {
		t.Fatalf("с негодным пределом перепроверок %d, ждали три", w.reviews(t, parent))
	}
}

// Критерий 1: связка переносит max_attempts перепроверки без потерь, и
// повторный импорт — unchanged.
func TestReviewLimitBundleRoundTrip(t *testing.T) {
	ctx := context.Background()
	w := newReviewWorld(t, `{"reviewer_role_key":"checker","max_attempts":2}`)
	exported, err := w.authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	var cycle *BundleWorkflow
	for i := range exported.Workflows {
		if exported.Workflows[i].Name == "Reviews" {
			cycle = &exported.Workflows[i]
		}
	}
	if cycle == nil {
		t.Fatal("в экспорте нет цикла перепроверки")
	}
	found := false
	for _, tr := range cycle.Transitions {
		for _, g := range tr.Gates {
			if g.Type == domain.GateAgentReview {
				n, err := maxAttempts(g.Config)
				found = err == nil && n == 2 && strings.Contains(string(g.Config), "checker")
			}
		}
	}
	if !found {
		t.Fatalf("экспорт потерял max_attempts: %+v", cycle.Transitions)
	}
	again, err := w.authoring.Import(ctx, "SDLC", w.system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{*cycle}}, false)
	if err != nil || len(again.Workflows) != 1 || again.Workflows[0].Outcome != "unchanged" {
		t.Fatalf("повторный импорт: %+v %v", again.Workflows, err)
	}
}

// Критерий 2: черновик с негодным пределом ложится, но не активируется.
func TestReviewLimitInvalidDraftIsNotActivated(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)
	report, err := authoring.Import(ctx, "SDLC", system.ID,
		reviewCycle(`{"reviewer_role_key":"checker","max_attempts":0}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Validations) != 1 || report.Validations[0].Valid {
		t.Fatalf("негодный предел прошёл проверку: %+v", report.Validations)
	}
	has := false
	for _, p := range report.Validations[0].Problems {
		has = has || p.Code == "review_limit_invalid"
	}
	if !has {
		t.Fatalf("нет проблемы review_limit_invalid: %+v", report.Validations[0].Problems)
	}
	id, _ := uuid.Parse(report.Workflows[0].ID)
	if _, err := authoring.Activate(ctx, id, system.ID); err == nil {
		t.Fatal("цикл с max_attempts 0 у перепроверки активирован")
	}
	head, err := store.WorkflowHead(ctx, store.DB(), id)
	if err != nil || head.State != domain.WorkflowDraft {
		t.Fatalf("черновик сменил состояние: %+v %v", head, err)
	}
}
