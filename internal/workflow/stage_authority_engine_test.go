package workflow

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Право этапа в движке (CLAB-379) на демо-цикле: new — без исполнителя,
// analysis — роль analyst, architecture — architect; cancel — глобальный.

type rightsWorld struct {
	t      *testing.T
	ctx    context.Context
	store  *postgres.Store
	engine *Engine
	system domain.Principal
	lead   domain.Principal
}

func newRightsWorld(t *testing.T) rightsWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthToken)
	lead, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	return rightsWorld{t: t, ctx: ctx, store: store, engine: engine, system: system, lead: lead}
}

// task заводит задачу и проводит её до статуса status без права этапа.
func (w rightsWorld) task(status string) domain.Task {
	w.t.Helper()
	task, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{TaskType: "task", Title: "право этапа " + status}})
	if err != nil {
		w.t.Fatal(err)
	}
	if status != "new" {
		w.move(task.Key, "start")
	}
	if status == "architecture" {
		for _, g := range []string{"artifacts", "peer_review", "human_approval", "drift", "repo_scope"} {
			if _, err := w.engine.DecideGate(w.ctx, task.Key, "analysis_done", g, w.lead.ID, nil,
				domain.GateResultWaived, json.RawMessage(`{}`)); err != nil {
				w.t.Fatalf("waive %s: %v", g, err)
			}
		}
		w.move(task.Key, "analysis_done")
	}
	out, err := w.store.GetTaskByKey(w.ctx, w.store.DB(), task.Key)
	if err != nil || out.StatusKey != status {
		w.t.Fatalf("задача не в %s: %s %v", status, out.StatusKey, err)
	}
	return out
}

func (w rightsWorld) move(key, transition string) {
	w.t.Helper()
	res, err := w.engine.Execute(w.ctx, key, transition, w.lead.ID, "")
	if err != nil || res.Blocked {
		w.t.Fatalf("%s %s: %v %+v", key, transition, err, res.Gates)
	}
}

func (w rightsWorld) run(task domain.Task) domain.StageRun {
	w.t.Helper()
	run, err := w.store.CurrentStageRun(w.ctx, w.store.DB(), task.ID, task.StatusID)
	if err != nil {
		w.t.Fatal(err)
	}
	return run
}

func forbidden(err error) bool {
	return err != nil && apperr.As(err).Code == apperr.CodeForbidden
}

func reasonOf(err error) string {
	if m, ok := apperr.As(err).Details.(map[string]any); ok {
		r, _ := m["reason"].(string)
		return r
	}
	return ""
}

// AC-13, AC-A3: предсказание actor_may / actor_may_decide совпадает с
// исходом настоящего запроса для каждой комбинации «уровень × набор ролей ×
// статус (с исполнителем своим и чужим, без исполнителя) × переход каждого
// вида × гейт каждого типа и вердикт», и для токена задачи. Каждая
// комбинация — на свежей задаче: исполненный переход не меняет следующих.
func TestStageAuthorityPredictionMatchesOutcome(t *testing.T) {
	w := newRightsWorld(t)
	actors := []struct {
		name  string
		actor func(domain.Task) Actor
	}{
		{"viewer", func(domain.Task) Actor { return Actor{Principal: w.lead, Level: domain.MemberViewer} }},
		{"agent NULL", func(domain.Task) Actor { return Actor{Principal: w.lead, Level: domain.MemberAgent} }},
		{"agent []", func(domain.Task) Actor { return Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{}} }},
		{"agent [analyst]", func(domain.Task) Actor {
			return Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst"}}
		}},
		{"agent [developer]", func(domain.Task) Actor {
			return Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"}}
		}},
		{"maintainer", func(domain.Task) Actor { return Actor{Principal: w.lead, Level: domain.MemberMaintainer} }},
		{"токен задачи", func(task domain.Task) Actor {
			run := w.run(task)
			return Actor{Principal: w.lead, Level: domain.MemberMaintainer,
				Bound: &TaskBinding{StageRunID: run.ID, TaskID: task.ID, TaskKey: task.Key}}
		}},
	}
	verdicts := []string{domain.GateResultPassed, domain.GateResultFailed, domain.GateResultWaived}
	checked := map[string]int{}

	for _, status := range []string{"new", "analysis", "architecture"} {
		probe := w.task(status)
		previews, err := w.engine.Preview(w.ctx, probe)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range actors {
			for _, p := range previews {
				name := status + " / " + a.name + " / " + p.Key
				task := w.task(status)
				actor := a.actor(task)
				views, err := w.engine.PreviewFor(w.ctx, task, &actor)
				if err != nil {
					t.Fatal(err)
				}
				var view TransitionPreview
				for _, v := range views {
					if v.Key == p.Key {
						view = v
					}
				}
				if view.ActorMay == nil {
					t.Fatalf("%s: нет actor_may: %+v", name, view)
				}

				// Решения по каждому гейту и вердикту — до перехода.
				for _, g := range view.Gates {
					if g.ActorMayDecide == nil {
						t.Fatalf("%s/%s: нет actor_may_decide", name, g.GateKey)
					}
					var firstErr error
					for _, verdict := range verdicts {
						_, err := w.engine.DecideGateAs(w.ctx, task.Key, p.Key, g.GateKey, actor, verdict, json.RawMessage(`{}`), nil)
						if predicted := slices.Contains(g.ActorDecisions, verdict); predicted != (err == nil) {
							t.Fatalf("%s/%s %s: предсказано %v, исход %v", name, g.GateKey, verdict, predicted, err)
						}
						if err != nil && firstErr == nil {
							firstErr = err
						}
						checked["решение"]++
					}
					if !*g.ActorMayDecide {
						if g.ActorDeny == nil || firstErr == nil || g.ActorDeny.Message != apperr.As(firstErr).Message {
							t.Fatalf("%s/%s: причина %+v, исход %v", name, g.GateKey, g.ActorDeny, firstErr)
						}
					}
				}

				_, err = w.engine.ExecuteAs(w.ctx, task.Key, p.Key, actor, "")
				denied := forbidden(err)
				if *view.ActorMay == denied {
					t.Fatalf("%s: actor_may=%v, исход %v", name, *view.ActorMay, err)
				}
				if denied && (view.ActorDeny == nil || view.ActorDeny.Message != apperr.As(err).Message ||
					view.ActorDeny.Reason != reasonOf(err)) {
					t.Fatalf("%s: причина %+v, исход %v", name, view.ActorDeny, err)
				}
				if err != nil && !denied && apperr.As(err).Code != apperr.CodeGatesNotSatisfied {
					t.Fatalf("%s: неожиданная ошибка %v", name, err)
				}
				checked["переход"]++
			}
		}
	}
	t.Logf("сверено: %v", checked)
}

// R3 на живом цикле: что agent может на своём этапе и чего нет; токен
// задачи — по 0049 (AC-1, AC-3, AC-4, AC-6, AC-7, AC-8).
func TestStageAuthorityEngine(t *testing.T) {
	w := newRightsWorld(t)
	analyst := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst"}}
	architect := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"architect"}}
	developer := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"}}
	both := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst", "developer"}}
	maintainer := Actor{Principal: w.lead, Level: domain.MemberMaintainer}

	decide := func(key, tr, gate string, who Actor, status string) error {
		_, err := w.engine.DecideGateAs(w.ctx, key, tr, gate, who, status, json.RawMessage(`{}`), nil)
		return err
	}
	exec := func(key, tr string, who Actor) (TransitionResult, error) {
		return w.engine.ExecuteAs(w.ctx, key, tr, who, "")
	}

	// AC-1, AC-7: аналитик на analysis — апрув passed и failed, но не waived;
	// перепроверку и артефакты не снимает.
	ana := w.task("analysis")
	for _, s := range []string{domain.GateResultFailed, domain.GateResultPassed} {
		if err := decide(ana.Key, "analysis_done", "human_approval", analyst, s); err != nil {
			t.Fatalf("апрув %s аналитиком: %v", s, err)
		}
	}
	for _, c := range []struct{ gate, status string }{
		{"human_approval", domain.GateResultWaived}, {"peer_review", domain.GateResultWaived},
		{"peer_review", domain.GateResultPassed}, {"artifacts", domain.GateResultWaived},
	} {
		if err := decide(ana.Key, "analysis_done", c.gate, analyst, c.status); !forbidden(err) || reasonOf(err) != DenyMaintainerAction {
			t.Fatalf("%s %s аналитиком: %v", c.gate, c.status, err)
		}
	}
	// Решение пишется от имени решающего, лентой gate.decided.
	var decidedBy string
	if err := w.store.DB().QueryRow(w.ctx, `
		SELECT p.handle FROM gate_results gr JOIN gates g ON g.id = gr.gate_id
		JOIN principals p ON p.id = gr.decided_by
		WHERE gr.stage_run_id = $1 AND g.key = 'human_approval'`, w.run(ana).ID).Scan(&decidedBy); err != nil || decidedBy != "lead" {
		t.Fatalf("decided_by: %q %v", decidedBy, err)
	}
	// AC-2, AC-5: на чужом этапе — 403 с ролью статуса и ролями актора.
	err := decide(ana.Key, "analysis_done", "human_approval", developer, domain.GateResultPassed)
	if !forbidden(err) || reasonOf(err) != DenyStageRole || !strings.Contains(err.Error(), "analyst") ||
		!strings.Contains(err.Error(), "[developer]") {
		t.Fatalf("апрув чужого этапа: %v", err)
	}
	if err := decide(ana.Key, "analysis_done", "human_approval", both, domain.GateResultPassed); err != nil {
		t.Fatalf("две роли: %v", err)
	}

	// AC-3: переход своего этапа доходит до гейтов; гейты сняты — проходит.
	if res, err := exec(ana.Key, "analysis_done", analyst); err != nil || !res.Blocked {
		t.Fatalf("analysis_done до гейтов: %v %v", err, res.Blocked)
	}
	if _, err := exec(ana.Key, "analysis_done", developer); !forbidden(err) {
		t.Fatalf("analysis_done разработчиком: %v", err)
	}
	for _, g := range []string{"artifacts", "peer_review", "drift", "repo_scope"} {
		if err := decide(ana.Key, "analysis_done", g, maintainer, domain.GateResultWaived); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := exec(ana.Key, "analysis_done", analyst); err != nil || res.Blocked || res.Task.StatusKey != "architecture" {
		t.Fatalf("analysis_done аналитиком: %v %+v", err, res)
	}
	// Переход не из текущего статуса — transition_not_allowed раньше права.
	if _, err := exec(ana.Key, "analysis_done", developer); apperr.As(err).Code != apperr.CodeTransitionNotAllowed {
		t.Fatalf("переход не из статуса: %v", err)
	}
	// AC-A5: решение по гейту перехода не из текущего статуса — 403 даже с
	// ролью analyst; сопровождающему — как раньше.
	if err := decide(ana.Key, "analysis_done", "human_approval", analyst, domain.GateResultPassed); !forbidden(err) ||
		reasonOf(err) != DenyNoStageRole {
		t.Fatalf("гейт чужого перехода: %v", err)
	}
	if err := decide(ana.Key, "analysis_done", "human_approval", maintainer, domain.GateResultPassed); err != nil {
		t.Fatalf("гейт чужого перехода сопровождающим: %v", err)
	}

	// AC-7: чек-лист своего этапа — passed да, failed нет.
	if err := decide(ana.Key, "architecture_done", "traceability", architect, domain.GateResultFailed); !forbidden(err) {
		t.Fatalf("провал чек-листа: %v", err)
	}
	if err := decide(ana.Key, "architecture_done", "traceability", architect, domain.GateResultPassed); err != nil {
		t.Fatalf("чек-лист: %v", err)
	}
	// AC-4: возврат своего этапа с описанием проблемы; cancel — нет.
	if _, err := w.engine.SubmitArtifact(w.ctx, ana.Key, w.lead.ID, SubmitArtifactInput{
		Kind: "problem.report", Title: "проблема", Content: []byte("# проблема")}); err != nil {
		t.Fatal(err)
	}
	if _, err := exec(ana.Key, "cancel", architect); !forbidden(err) || reasonOf(err) != DenyNoStageRole {
		t.Fatalf("cancel архитектором: %v", err)
	}
	res, err := exec(ana.Key, "reject_to_analysis", architect)
	if err != nil || res.Blocked || res.Task.StatusKey != "analysis" || res.FromRun.ID == res.NewRun.ID {
		t.Fatalf("возврат архитектором: %v %+v", err, res)
	}
	var outcome string
	if err := w.store.DB().QueryRow(w.ctx, `SELECT outcome FROM stage_runs WHERE id = $1`, res.FromRun.ID).Scan(&outcome); err != nil || outcome != "rejected" {
		t.Fatalf("исход прогона: %q %v", outcome, err)
	}
	if _, err := exec(ana.Key, "cancel", maintainer); err != nil {
		t.Fatalf("cancel сопровождающим: %v", err)
	}

	// AC-6: статус без исполнителя — start закрыт для agent с любыми ролями.
	fresh := w.task("new")
	for _, who := range []Actor{analyst, {Principal: w.lead, Level: domain.MemberAgent}} {
		if _, err := exec(fresh.Key, "start", who); !forbidden(err) || reasonOf(err) != DenyNoStageRole {
			t.Fatalf("start agent: %v", err)
		}
	}
	if _, err := exec(fresh.Key, "start", maintainer); err != nil {
		t.Fatalf("start сопровождающим: %v", err)
	}
	// Эпик: planning без исполнителя закрыт, decomposition (orchestrator)
	// открыт agent с NULL; in_progress снова без исполнителя.
	epic, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{TaskType: "epic", Title: "эпик"}})
	if err != nil {
		t.Fatal(err)
	}
	anyRole := Actor{Principal: w.lead, Level: domain.MemberAgent}
	if _, err := exec(epic.Key, "start", anyRole); !forbidden(err) {
		t.Fatalf("start эпика agent: %v", err)
	}
	w.move(epic.Key, "start")
	if err := decide(epic.Key, "breakdown_done", "human_approval", anyRole, domain.GateResultPassed); err != nil {
		t.Fatalf("апрув нарезки agent NULL: %v", err)
	}
	if err := decide(epic.Key, "breakdown_done", "human_approval", analyst, domain.GateResultPassed); !forbidden(err) {
		t.Fatalf("апрув нарезки аналитиком: %v", err)
	}

	// AC-8: токен задачи — рамка 0049 при любом уровне держателя.
	tt := w.task("analysis")
	run := w.run(tt)
	bound := Actor{Principal: w.lead, Level: domain.MemberAdmin, Roles: nil,
		Bound: &TaskBinding{StageRunID: run.ID, TaskID: tt.ID, TaskKey: tt.Key}}
	err = decide(tt.Key, "analysis_done", "human_approval", bound, domain.GateResultPassed)
	if !forbidden(err) || !strings.Contains(err.Error(), ExecutorRefusal) {
		t.Fatalf("апрув токеном задачи: %v", err)
	}
	if _, err := exec(tt.Key, "cancel", bound); !forbidden(err) || !strings.Contains(err.Error(), ExecutorRefusal) {
		t.Fatalf("cancel токеном задачи: %v", err)
	}

	// Бот (принципал вида agent) на своём этапе апрув не ставит — как раньше.
	bot, err := w.store.ResolveActor(w.ctx, w.store.DB(), "bot", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	err = decide(tt.Key, "analysis_done", "human_approval", Actor{Principal: bot, Level: domain.MemberAgent}, domain.GateResultPassed)
	if !forbidden(err) || !strings.Contains(err.Error(), "закрывает человек") {
		t.Fatalf("апрув ботом: %v", err)
	}
}

// AC-11, AC-20: сверка seen_artifact_id и устаревание апрува действуют для
// agent так же; право (403) — раньше сверки (409); апрув agent засчитан.
func TestStageAuthoritySeenAndStale(t *testing.T) {
	w := newRightsWorld(t)
	analyst := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst"}}
	developer := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"}}
	task := w.task("analysis")
	art, err := w.engine.SubmitArtifact(w.ctx, task.Key, w.lead.ID, SubmitArtifactInput{
		Kind: "analysis.brief", Title: "brief", Content: []byte("# brief")})
	if err != nil {
		t.Fatal(err)
	}
	wrong := "00000000-0000-0000-0000-000000000000"
	if _, err := w.engine.DecideGateAs(w.ctx, task.Key, "analysis_done", "human_approval", developer,
		domain.GateResultPassed, json.RawMessage(`{}`), &wrong); !forbidden(err) {
		t.Fatalf("право раньше сверки: %v", err)
	}
	_, err = w.engine.DecideGateAs(w.ctx, task.Key, "analysis_done", "human_approval", analyst,
		domain.GateResultPassed, json.RawMessage(`{}`), &wrong)
	if apperr.As(err).Code != apperr.CodeConflict || reasonOf(err) != SeenConflict {
		t.Fatalf("сверка: %v", err)
	}
	seen := art.ID.String()
	if _, err := w.engine.DecideGateAs(w.ctx, task.Key, "analysis_done", "human_approval", analyst,
		domain.GateResultPassed, json.RawMessage(`{}`), &seen); err != nil {
		t.Fatalf("апрув со сверкой: %v", err)
	}
	gateStatus := func() string {
		res, err := w.engine.CheckGates(w.ctx, task.Key, "analysis_done")
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range res.Gates {
			if g.GateKey == "human_approval" {
				return g.Status
			}
		}
		return ""
	}
	if s := gateStatus(); s != domain.GateResultPassed {
		t.Fatalf("апрув agent не засчитан: %s", s)
	}
	if _, err := w.engine.SubmitArtifact(w.ctx, task.Key, w.lead.ID, SubmitArtifactInput{
		Kind: "analysis.brief", Title: "brief 2", Content: []byte("# brief 2")}); err != nil {
		t.Fatal(err)
	}
	if s := gateStatus(); s != domain.GateResultPending {
		t.Fatalf("апрув после новой сдачи не устарел: %s", s)
	}
}

// AC-12, AC-A4: «Ждут меня» для agent — только этапы его ролей, у элемента
// свой can_decide, верхнеуровневый can_decide — есть ли зона; задачи чужих
// этапов отсекает выборка, и число запросов от них не растёт.
func TestApprovalsAgentZone(t *testing.T) {
	w := newRightsWorld(t)
	pid := w.task("new").ProjectID
	ready := func(status string) domain.Task {
		task := w.task(status)
		tr := map[string]string{"analysis": "analysis_done", "architecture": "architecture_done"}[status]
		gates := []string{"artifacts", "drift", "repo_scope"}
		if status == "analysis" {
			gates = append(gates, "peer_review")
		}
		for _, g := range gates {
			if _, err := w.engine.DecideGate(w.ctx, task.Key, tr, g, w.lead.ID, nil,
				domain.GateResultWaived, json.RawMessage(`{}`)); err != nil {
				t.Fatalf("%s %s: %v", task.Key, g, err)
			}
		}
		return task
	}
	ana, arc := ready("analysis"), ready("architecture")

	list := func(who Actor) Approvals {
		t.Helper()
		out, err := w.engine.Approvals(w.ctx, w.store.DB(), pid, who, 0)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	items := func(a Approvals) []string {
		var out []string
		for _, it := range a.Approvals {
			mark := "-"
			if it.CanDecide {
				mark = "+"
			}
			out = append(out, it.Task.Key+"/"+it.Transition.Key+"/"+it.Gate.GateKey+mark)
		}
		return out
	}
	bot, err := w.store.ResolveActor(w.ctx, w.store.DB(), "bot", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	anaItem := ana.Key + "/analysis_done/human_approval"
	arcCheck, arcApproval := arc.Key+"/architecture_done/traceability", arc.Key+"/architecture_done/human_approval"
	for _, c := range []struct {
		name      string
		who       Actor
		canDecide bool
		want      []string
	}{
		{"maintainer", Actor{Principal: w.lead, Level: domain.MemberMaintainer}, true,
			[]string{anaItem + "+", arcCheck + "+", arcApproval + "+"}},
		{"agent NULL", Actor{Principal: w.lead, Level: domain.MemberAgent}, true,
			[]string{anaItem + "+", arcCheck + "+", arcApproval + "+"}},
		{"agent [analyst]", Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst"}}, true,
			[]string{anaItem + "+"}},
		{"agent [developer]", Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"}}, true, nil},
		{"agent []", Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{}}, false, nil},
		{"viewer", Actor{Principal: w.lead, Level: domain.MemberViewer}, false, nil},
		// Бот на своём этапе видит гейт, но апрув ставит только человек.
		{"бот agent NULL", Actor{Principal: bot, Level: domain.MemberAgent}, true,
			[]string{anaItem + "-", arcCheck + "+", arcApproval + "-"}},
	} {
		got := list(c.who)
		if got.CanDecide != c.canDecide || !slices.Equal(items(got), c.want) {
			t.Fatalf("%s: can_decide=%v %v, ждали %v %v", c.name, got.CanDecide, items(got), c.canDecide, c.want)
		}
	}

	// Задачи чужих этапов число запросов аналитика не увеличивают.
	analyst := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst"}}
	count := func() int {
		q := &countingQ{Querier: w.store.DB()}
		if _, err := w.engine.Approvals(w.ctx, q, pid, analyst, 0); err != nil {
			t.Fatal(err)
		}
		return q.n
	}
	base := count()
	for i := 0; i < 5; i++ {
		ready("architecture")
	}
	if after := count(); after != base {
		t.Fatalf("запросов было %d, с пятью задачами чужого этапа стало %d", base, after)
	}
}

// AC-6 на цикле Epic (CLAB-379, проверка тестировщика): in_progress — статус
// без исполнителя. children_done и replan из него, решения по их гейтам и
// cancel эпика agent закрыты при любом наборе ролей, в том числе NULL и
// с ролью соседнего этапа (orchestrator); предсказание совпадает с отказом;
// сопровождающему — как раньше.
func TestStageAuthorityEpicWithoutExecutor(t *testing.T) {
	w := newRightsWorld(t)
	epic, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{TaskType: "epic", Title: "эпик без исполнителя"}})
	if err != nil {
		t.Fatal(err)
	}
	w.move(epic.Key, "start")
	for _, g := range []string{"artifacts", "human_approval"} {
		if _, err := w.engine.DecideGate(w.ctx, epic.Key, "breakdown_done", g, w.lead.ID, nil,
			domain.GateResultWaived, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("waive %s: %v", g, err)
		}
	}
	w.move(epic.Key, "breakdown_done")
	epic, err = w.store.GetTaskByKey(w.ctx, w.store.DB(), epic.Key)
	if err != nil || epic.StatusKey != "in_progress" {
		t.Fatalf("эпик не в in_progress: %s %v", epic.StatusKey, err)
	}

	agents := []Actor{
		{Principal: w.lead, Level: domain.MemberAgent},
		{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"orchestrator", "analyst"}},
	}
	for _, who := range agents {
		views, err := w.engine.PreviewFor(w.ctx, epic, &who)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range views {
			if v.ActorMay == nil || *v.ActorMay || v.ActorDeny == nil || v.ActorDeny.Reason != DenyNoStageRole {
				t.Fatalf("%v: предсказание %s: %+v", who.Roles, v.Key, v)
			}
			for _, g := range v.Gates {
				if g.ActorMayDecide == nil || *g.ActorMayDecide {
					t.Fatalf("%v: предсказание решения %s/%s: %+v", who.Roles, v.Key, g.GateKey, g)
				}
			}
		}
		for _, tr := range []string{"children_done", "replan", "cancel"} {
			if _, err := w.engine.ExecuteAs(w.ctx, epic.Key, tr, who, ""); !forbidden(err) || reasonOf(err) != DenyNoStageRole {
				t.Fatalf("%v: %s agent: %v", who.Roles, tr, err)
			}
		}
		for _, c := range []struct{ tr, gate string }{{"children_done", "children"}, {"replan", "problem_report"}} {
			for _, s := range []string{domain.GateResultPassed, domain.GateResultWaived} {
				_, err := w.engine.DecideGateAs(w.ctx, epic.Key, c.tr, c.gate, who, s, json.RawMessage(`{}`), nil)
				if !forbidden(err) || reasonOf(err) != DenyNoStageRole {
					t.Fatalf("%v: решение %s %s/%s agent: %v", who.Roles, s, c.tr, c.gate, err)
				}
			}
		}
	}
	after, err := w.store.GetTaskByKey(w.ctx, w.store.DB(), epic.Key)
	if err != nil || after.StatusKey != "in_progress" {
		t.Fatalf("эпик сдвинулся от отказов: %s %v", after.StatusKey, err)
	}
	var decided int
	if err := w.store.DB().QueryRow(w.ctx, `
		SELECT count(*) FROM gate_results gr JOIN gates g ON g.id = gr.gate_id
		JOIN workflow_transitions tr ON tr.id = g.transition_id
		WHERE gr.stage_run_id = $1 AND tr.key IN ('children_done', 'replan') AND gr.decided_by IS NOT NULL`,
		w.run(after).ID).Scan(&decided); err != nil || decided != 0 {
		t.Fatalf("отказы записали решения: %d %v", decided, err)
	}

	maintainer := Actor{Principal: w.lead, Level: domain.MemberMaintainer}
	if res, err := w.engine.ExecuteAs(w.ctx, epic.Key, "children_done", maintainer, ""); err != nil ||
		res.Blocked || res.Task.StatusKey != "done" {
		t.Fatalf("children_done сопровождающим: %v %+v", err, res)
	}
}
