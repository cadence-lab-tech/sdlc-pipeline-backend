package workflow

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Право сотрудника на пределы (CLAB-410: AC-7, K-1, K-5) и граница бота
// (AC-15) на цикле, где есть все гейты «только человек»: stage_time_limit на
// переходе входа в development, agent_review с пределом на выходе из него,
// agent_review без предела и апрув на выходе из testing, attempts_limit на
// возврате из testing.

func limitsBundle() Bundle {
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
			Name: "Limits", TaskTypes: []string{"task"},
			Statuses: []BundleStatus{
				{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
				{Key: "development", Name: "Development", Category: "active", Role: "maker", Reviewers: []string{"checker"}},
				{Key: "testing", Name: "Testing", Category: "active", Role: "checker", Reviewers: []string{"maker"}},
				{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
				{Key: "cancelled", Name: "Cancelled", Category: "cancelled", IsTerminal: true},
			},
			Transitions: []BundleTransition{
				{Key: "start", Name: "Start", From: "new", To: "development", Kind: "advance", Gates: []BundleGate{
					{Key: "time", Name: "Предел этапа", Type: domain.GateStageTimeLimit, Blocking: &yes,
						Config: json.RawMessage(`{"max_duration": "1h"}`)},
				}},
				{Key: "dev_done", Name: "Dev done", From: "development", To: "testing", Kind: "advance", Gates: []BundleGate{
					{Key: "peer_review", Name: "Перепроверка с пределом", Type: domain.GateAgentReview, Blocking: &yes,
						Config: json.RawMessage(`{"reviewer_role_key":"checker","max_attempts":2}`)},
					{Key: "selfhosted", Name: "Self-hosted учтён", Type: domain.GateChecklist, Blocking: &yes,
						Config: json.RawMessage(`{"items":[{"id":"selfhosted","text":"Раздел написан","required":true}]}`)},
				}},
				{Key: "testing_passed", Name: "Testing passed", From: "testing", To: "done", Kind: "advance", Gates: []BundleGate{
					{Key: "final_review", Name: "Перепроверка без предела", Type: domain.GateAgentReview, Blocking: &yes,
						Config: json.RawMessage(`{"reviewer_role_key":"maker"}`)},
					{Key: "sign_off", Name: "Апрув", Type: domain.GateHumanApproval, Blocking: &yes,
						Config: json.RawMessage(`{"min_approvals":1}`)},
				}},
				{Key: "testing_failed", Name: "Testing failed", From: "testing", To: "development", Kind: "reject", Gates: []BundleGate{
					{Key: "attempts", Name: "Не больше двух попыток", Type: domain.GateAttemptsLimit, Blocking: &yes,
						Config: json.RawMessage(`{"max_attempts": 2}`)},
				}},
				{Key: "cancel", Name: "Cancel", To: "cancelled", Kind: "cancel"},
			},
		}},
	}
}

type limitsWorld struct {
	t      *testing.T
	ctx    context.Context
	store  *postgres.Store
	engine *Engine
	system domain.Principal
	lead   domain.Principal
	bot    domain.Principal
}

func newLimitsWorld(t *testing.T) limitsWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthToken)
	report, err := authoring.Import(ctx, "SDLC", system.ID, limitsBundle(), false)
	if err != nil {
		t.Fatalf("импорт цикла: %v", err)
	}
	if len(report.Validations) != 1 || !report.Validations[0].Valid {
		t.Fatalf("цикл пределов не прошёл проверку: %+v", report.Validations)
	}
	id, err := uuid.Parse(report.Workflows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authoring.Activate(ctx, id, system.ID); err != nil {
		t.Fatalf("активация цикла: %v", err)
	}
	return limitsWorld{t: t, ctx: ctx, store: store, engine: engine, system: system,
		lead: mustActor(t, store, "user:lead"), bot: mustActor(t, store, "agent:bot")}
}

// task заводит задачу и проводит её до status без права этапа: гейты
// снимает человек (вид принципала читается из базы).
func (w limitsWorld) task(status string) domain.Task {
	w.t.Helper()
	task, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "пределы " + status}})
	if err != nil {
		w.t.Fatal(err)
	}
	moves := map[string][]string{"new": nil, "development": {"start"}, "testing": {"start", "dev_done"}}[status]
	for _, tr := range moves {
		if tr == "dev_done" {
			for _, g := range []string{"peer_review", "selfhosted"} {
				if _, err := w.engine.DecideGate(w.ctx, task.Key, tr, g, w.lead.ID, nil,
					domain.GateResultWaived, json.RawMessage(`{}`)); err != nil {
					w.t.Fatalf("waive %s: %v", g, err)
				}
			}
		}
		if res, err := w.engine.Execute(w.ctx, task.Key, tr, w.lead.ID, ""); err != nil || res.Blocked {
			w.t.Fatalf("%s %s: %v %+v", task.Key, tr, err, res.Gates)
		}
	}
	out, err := w.store.GetTaskByKey(w.ctx, w.store.DB(), task.Key)
	if err != nil || out.StatusKey != status {
		w.t.Fatalf("задача не в %s: %s %v", status, out.StatusKey, err)
	}
	return out
}

func (w limitsWorld) decide(task domain.Task, tr, gate string, who Actor, status string) error {
	_, err := w.engine.DecideGateAs(w.ctx, task.Key, tr, gate, who, status, json.RawMessage(`{}`), nil)
	return err
}

// AC-7, K-1, K-5: таблица сотрудника на пределах — waived да, passed — 400
// «вердикт выносит движок» раньше 403 таблицы, failed — 403
// maintainer_action; agent_review без предела — любой вердикт 403. Тот же
// результат в предсказании (actor_decisions, actor_deny).
func TestEmployeeLimitsTable(t *testing.T) {
	w := newLimitsWorld(t)
	employee := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{}, TokenKind: domain.TokenAPI}
	invalid := func(err error) bool { return err != nil && apperr.As(err).Code == apperr.CodeInvalidInput }
	type want struct {
		tr, gate  string
		decisions []string
	}
	for _, c := range []struct {
		status string
		gates  []want
	}{
		{"development", []want{
			{"dev_done", "peer_review", []string{domain.GateResultWaived}},
			{"dev_done", "selfhosted", []string{domain.GateResultPassed, domain.GateResultFailed}},
		}},
		{"testing", []want{
			{"testing_passed", "final_review", []string{}},
			{"testing_passed", "sign_off", []string{domain.GateResultPassed, domain.GateResultFailed}},
			{"testing_failed", "attempts", []string{domain.GateResultWaived}},
		}},
	} {
		probe := w.task(c.status)
		views, err := w.engine.PreviewFor(w.ctx, probe, &employee)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range c.gates {
			var got *GateOutcome
			for _, v := range views {
				for i := range v.Gates {
					if v.Key == g.tr && v.Gates[i].GateKey == g.gate {
						got = &v.Gates[i]
					}
				}
			}
			if got == nil || got.ActorMayDecide == nil {
				t.Fatalf("%s/%s: нет предсказания", g.tr, g.gate)
			}
			decisions := got.ActorDecisions
			if decisions == nil {
				decisions = []string{}
			}
			if !slices.Equal(decisions, g.decisions) || *got.ActorMayDecide != (len(g.decisions) > 0) {
				t.Fatalf("%s/%s: actor_decisions %v, ждали %v", g.tr, g.gate, got.ActorDecisions, g.decisions)
			}
			if len(g.decisions) == 0 && (got.ActorDeny == nil || got.ActorDeny.Reason != DenyMaintainerAction) {
				t.Fatalf("%s/%s: причина %+v", g.tr, g.gate, got.ActorDeny)
			}
			// Исход каждого вердикта на свежей задаче — тот же.
			for _, verdict := range actorVerdicts {
				task := w.task(c.status)
				err := w.decide(task, g.tr, g.gate, employee, verdict)
				if slices.Contains(g.decisions, verdict) != (err == nil) {
					t.Fatalf("%s/%s %s: исход %v, предсказано %v", g.tr, g.gate, verdict, err, g.decisions)
				}
				limit := g.gate == "peer_review" || g.gate == "attempts"
				switch {
				case err == nil:
				case limit && verdict == domain.GateResultPassed:
					if !invalid(err) {
						t.Fatalf("%s/%s passed: ждали 400, а %v", g.tr, g.gate, err)
					}
				default:
					if !forbidden(err) || reasonOf(err) != DenyMaintainerAction {
						t.Fatalf("%s/%s %s: ждали 403 maintainer_action, а %v", g.tr, g.gate, verdict, err)
					}
				}
			}
		}
	}

	// stage_time_limit снимается на переходе входа (start), а он не из
	// текущего статуса: сотруднику — waived, passed — 400, failed — 403.
	task := w.task("development")
	if err := w.decide(task, "start", "time", employee, domain.GateResultPassed); !invalid(err) {
		t.Fatalf("passed по пределу этапа: %v", err)
	}
	if err := w.decide(task, "start", "time", employee, domain.GateResultFailed); !forbidden(err) || reasonOf(err) != DenyMaintainerAction {
		t.Fatalf("failed по пределу этапа: %v", err)
	}
	if err := w.decide(task, "start", "time", employee, domain.GateResultWaived); err != nil {
		t.Fatalf("waived по пределу этапа: %v", err)
	}
	// Снятый предел попыток пускает возврат; событие несёт via=level.
	task = w.task("testing")
	if err := w.decide(task, "testing_failed", "attempts", employee, domain.GateResultWaived); err != nil {
		t.Fatal(err)
	}
	if res, err := w.engine.ExecuteAs(w.ctx, task.Key, "testing_failed", employee, ""); err != nil || res.Blocked {
		t.Fatalf("testing_failed сотрудником: %v %+v", err, res)
	}
}

// AC-15: бот на пределах — 403 maintainer_action на своём этапе и
// stage_role на чужом, при любом вердикте; human_approval — вид принципала.
func TestBotLimitsUnchanged(t *testing.T) {
	w := newLimitsWorld(t)
	checker := Actor{Principal: w.bot, Level: domain.MemberAgent, Roles: []string{"checker"}, TokenKind: domain.TokenAPI}
	maker := Actor{Principal: w.bot, Level: domain.MemberAgent, Roles: []string{"maker"}, TokenKind: domain.TokenAPI}
	task := w.task("testing")
	for _, verdict := range actorVerdicts {
		if err := w.decide(task, "testing_failed", "attempts", checker, verdict); !forbidden(err) || reasonOf(err) != DenyMaintainerAction {
			t.Fatalf("бот своего этапа %s по пределу: %v", verdict, err)
		}
		if err := w.decide(task, "testing_failed", "attempts", maker, verdict); !forbidden(err) || reasonOf(err) != DenyStageRole {
			t.Fatalf("бот чужого этапа %s по пределу: %v", verdict, err)
		}
	}
	if err := w.decide(task, "testing_passed", "sign_off", checker, domain.GateResultPassed); !forbidden(err) || reasonOf(err) != "" {
		t.Fatalf("апрув ботом: %v", err)
	}
	dev := w.task("development")
	if err := w.decide(dev, "dev_done", "peer_review", maker, domain.GateResultWaived); !forbidden(err) || reasonOf(err) != DenyMaintainerAction {
		t.Fatalf("бот снимает перепроверку с пределом: %v", err)
	}
	if err := w.decide(dev, "start", "time", maker, domain.GateResultWaived); !forbidden(err) || reasonOf(err) != DenyNoStageRole {
		t.Fatalf("бот снимает предел этапа: %v", err)
	}
	if _, err := w.engine.ExecuteAs(w.ctx, dev.Key, "cancel", maker, ""); !forbidden(err) || reasonOf(err) != DenyNoStageRole {
		t.Fatalf("cancel ботом: %v", err)
	}
}
