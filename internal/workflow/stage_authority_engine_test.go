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

// Право этапа в движке (CLAB-379, CLAB-410) на демо-цикле: new — без
// исполнителя, analysis — роль analyst, architecture — architect; cancel —
// глобальный. lead — человек: на уровне agent он сотрудник (CLAB-410); bot —
// агент-принципал того же уровня, право 0072 без изменений.

type rightsWorld struct {
	t      *testing.T
	ctx    context.Context
	store  *postgres.Store
	engine *Engine
	system domain.Principal
	lead   domain.Principal
	bot    domain.Principal
}

func newRightsWorld(t *testing.T) rightsWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthToken)
	lead, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	bot, err := store.ResolveActor(ctx, store.DB(), "bot", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	return rightsWorld{t: t, ctx: ctx, store: store, engine: engine, system: system, lead: lead, bot: bot}
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

// AC-13, AC-A3 (CLAB-379), AC-17 (CLAB-410): предсказание actor_may /
// actor_may_decide совпадает с исходом настоящего запроса для каждой
// комбинации «класс актора (сотрудник, бот, сопровождающий, viewer) × набор
// ролей × статус (с исполнителем своим и чужим, без исполнителя) × переход
// каждого вида × гейт каждого типа и вердикт», и для токена задачи. Каждая
// комбинация — на свежей задаче: исполненный переход не меняет следующих.
func TestStageAuthorityPredictionMatchesOutcome(t *testing.T) {
	w := newRightsWorld(t)
	actors := []struct {
		name  string
		actor func(domain.Task) Actor
	}{
		{"viewer", func(domain.Task) Actor { return Actor{Principal: w.lead, Level: domain.MemberViewer} }},
		{"сотрудник NULL", func(domain.Task) Actor { return Actor{Principal: w.lead, Level: domain.MemberAgent} }},
		{"сотрудник []", func(domain.Task) Actor { return Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{}} }},
		{"сотрудник [analyst]", func(domain.Task) Actor {
			return Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst"}}
		}},
		{"сотрудник [developer]", func(domain.Task) Actor {
			return Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"}}
		}},
		{"бот NULL", func(domain.Task) Actor { return Actor{Principal: w.bot, Level: domain.MemberAgent} }},
		{"бот []", func(domain.Task) Actor { return Actor{Principal: w.bot, Level: domain.MemberAgent, Roles: []string{}} }},
		{"бот [analyst]", func(domain.Task) Actor {
			return Actor{Principal: w.bot, Level: domain.MemberAgent, Roles: []string{"analyst"}}
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

// R3 на живом цикле для бота (решение 0072 без изменений, AC-15 CLAB-410):
// что агент-принципал может на своём этапе и чего нет; токен задачи — по
// 0049 (AC-1, AC-3, AC-4, AC-6, AC-7, AC-8 CLAB-379).
func TestStageAuthorityEngine(t *testing.T) {
	w := newRightsWorld(t)
	analyst := Actor{Principal: w.bot, Level: domain.MemberAgent, Roles: []string{"analyst"}}
	architect := Actor{Principal: w.bot, Level: domain.MemberAgent, Roles: []string{"architect"}}
	developer := Actor{Principal: w.bot, Level: domain.MemberAgent, Roles: []string{"developer"}}
	maintainer := Actor{Principal: w.lead, Level: domain.MemberMaintainer}

	decide := func(key, tr, gate string, who Actor, status string) error {
		_, err := w.engine.DecideGateAs(w.ctx, key, tr, gate, who, status, json.RawMessage(`{}`), nil)
		return err
	}
	exec := func(key, tr string, who Actor) (TransitionResult, error) {
		return w.engine.ExecuteAs(w.ctx, key, tr, who, "")
	}

	// Бот на своём этапе: апрув закрывает человек (вид принципала); waived
	// апрува, перепроверку и артефакты — сопровождающему.
	ana := w.task("analysis")
	for _, s := range []string{domain.GateResultFailed, domain.GateResultPassed} {
		if err := decide(ana.Key, "analysis_done", "human_approval", analyst, s); !forbidden(err) ||
			!strings.Contains(err.Error(), "закрывает человек") {
			t.Fatalf("апрув %s ботом: %v", s, err)
		}
	}
	for _, c := range []struct{ gate, status string }{
		{"human_approval", domain.GateResultWaived}, {"peer_review", domain.GateResultWaived},
		{"peer_review", domain.GateResultPassed}, {"artifacts", domain.GateResultWaived},
	} {
		if err := decide(ana.Key, "analysis_done", c.gate, analyst, c.status); !forbidden(err) || reasonOf(err) != DenyMaintainerAction {
			t.Fatalf("%s %s ботом: %v", c.gate, c.status, err)
		}
	}
	// AC-2, AC-5: на чужом этапе — 403 с ролью статуса и ролями актора.
	err := decide(ana.Key, "analysis_done", "human_approval", developer, domain.GateResultPassed)
	if !forbidden(err) || reasonOf(err) != DenyStageRole || !strings.Contains(err.Error(), "analyst") ||
		!strings.Contains(err.Error(), "[developer]") {
		t.Fatalf("апрув чужого этапа: %v", err)
	}

	// AC-3: переход своего этапа доходит до гейтов; гейты сняты — проходит.
	if res, err := exec(ana.Key, "analysis_done", analyst); err != nil || !res.Blocked {
		t.Fatalf("analysis_done до гейтов: %v %v", err, res.Blocked)
	}
	if _, err := exec(ana.Key, "analysis_done", developer); !forbidden(err) {
		t.Fatalf("analysis_done разработчиком: %v", err)
	}
	for _, g := range []string{"artifacts", "peer_review", "human_approval", "drift", "repo_scope"} {
		if err := decide(ana.Key, "analysis_done", g, maintainer, domain.GateResultWaived); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := exec(ana.Key, "analysis_done", analyst); err != nil || res.Blocked || res.Task.StatusKey != "architecture" {
		t.Fatalf("analysis_done ботом: %v %+v", err, res)
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

	// AC-6: статус без исполнителя — start закрыт для бота с любыми ролями.
	fresh := w.task("new")
	for _, who := range []Actor{analyst, {Principal: w.bot, Level: domain.MemberAgent}} {
		if _, err := exec(fresh.Key, "start", who); !forbidden(err) || reasonOf(err) != DenyNoStageRole {
			t.Fatalf("start ботом: %v", err)
		}
	}
	if _, err := exec(fresh.Key, "start", maintainer); err != nil {
		t.Fatalf("start сопровождающим: %v", err)
	}
	// Эпик: planning без исполнителя закрыт, decomposition (orchestrator)
	// открыт боту с NULL до вида принципала; in_progress снова без исполнителя.
	epic, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{TaskType: "epic", Title: "эпик"}})
	if err != nil {
		t.Fatal(err)
	}
	anyRole := Actor{Principal: w.bot, Level: domain.MemberAgent}
	if _, err := exec(epic.Key, "start", anyRole); !forbidden(err) {
		t.Fatalf("start эпика ботом: %v", err)
	}
	w.move(epic.Key, "start")
	if err := decide(epic.Key, "breakdown_done", "human_approval", anyRole, domain.GateResultPassed); !forbidden(err) ||
		!strings.Contains(err.Error(), "закрывает человек") {
		t.Fatalf("апрув нарезки ботом NULL: %v", err)
	}
	if err := decide(epic.Key, "breakdown_done", "human_approval", analyst, domain.GateResultPassed); !forbidden(err) ||
		reasonOf(err) != DenyStageRole {
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
	// Тот же токен у сотрудника уровня agent: рамка та же (0049).
	employeeBound := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"},
		Bound: &TaskBinding{StageRunID: run.ID, TaskID: tt.ID, TaskKey: tt.Key}}
	for _, tr := range []string{"cancel"} {
		if _, err := exec(tt.Key, tr, employeeBound); !forbidden(err) || !strings.Contains(err.Error(), ExecutorRefusal) {
			t.Fatalf("%s токеном задачи сотрудника: %v", tr, err)
		}
	}
	if err := decide(tt.Key, "analysis_done", "human_approval", employeeBound, domain.GateResultPassed); !forbidden(err) ||
		!strings.Contains(err.Error(), ExecutorRefusal) {
		t.Fatalf("апрув токеном задачи сотрудника: %v", err)
	}
	// Бот с ролью этапа апрув не ставит и на чужой задаче.
	err = decide(tt.Key, "analysis_done", "human_approval", analyst, domain.GateResultPassed)
	if !forbidden(err) || !strings.Contains(err.Error(), "закрывает человек") {
		t.Fatalf("апрув ботом: %v", err)
	}
}

// R-6, R-7 CLAB-410 на демо-цикле (AC-5, AC-7, UC-3, UC-4): сотрудник без
// роли статуса выполняет переходы любого вида — start из статуса без
// исполнителя, advance и reject чужого этапа, cancel — и решает гейты
// чужого этапа по своей таблице; вне таблицы — 403 maintainer_action.
func TestEmployeeStageAuthority(t *testing.T) {
	w := newRightsWorld(t)
	dev := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"}, TokenKind: domain.TokenAPI}
	none := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{}, TokenKind: domain.TokenSession}
	decide := func(key, tr, gate string, who Actor, status string) error {
		_, err := w.engine.DecideGateAs(w.ctx, key, tr, gate, who, status, json.RawMessage(`{}`), nil)
		return err
	}
	exec := func(key, tr string, who Actor) (TransitionResult, error) {
		return w.engine.ExecuteAs(w.ctx, key, tr, who, "")
	}

	// start из new — статуса без исполнителя.
	task := w.task("new")
	if res, err := exec(task.Key, "start", none); err != nil || res.Blocked || res.Task.StatusKey != "analysis" {
		t.Fatalf("start сотрудником без ролей: %v %+v", err, res)
	}
	// Апрув и его провал на чужом этапе; waived апрува, перепроверка без
	// предела и артефакты — вне таблицы.
	for _, s := range []string{domain.GateResultFailed, domain.GateResultPassed} {
		if err := decide(task.Key, "analysis_done", "human_approval", dev, s); err != nil {
			t.Fatalf("апрув %s сотрудником: %v", s, err)
		}
	}
	for _, c := range []struct{ gate, status string }{
		{"human_approval", domain.GateResultWaived}, {"peer_review", domain.GateResultWaived},
		{"peer_review", domain.GateResultPassed}, {"peer_review", domain.GateResultFailed},
		{"artifacts", domain.GateResultWaived}, {"drift", domain.GateResultPassed},
	} {
		if err := decide(task.Key, "analysis_done", c.gate, dev, c.status); !forbidden(err) || reasonOf(err) != DenyMaintainerAction {
			t.Fatalf("%s %s сотрудником: %v", c.gate, c.status, err)
		}
	}
	ev := w.lastEvent(task, domain.EventGateDecided)
	if ev["via"] != ViaLevel || ev["level"] != "agent" || ev["token_kind"] != "api" || ev["stage_role"] != "analyst" {
		t.Fatalf("gate.decided сотрудника: %v", ev)
	}
	// Переход чужого этапа доходит до гейтов (409), а не до права.
	if res, err := exec(task.Key, "analysis_done", dev); err != nil || !res.Blocked {
		t.Fatalf("analysis_done сотрудником до гейтов: %v %+v", err, res)
	}
	for _, g := range []string{"artifacts", "peer_review", "drift", "repo_scope"} {
		if _, err := w.engine.DecideGate(w.ctx, task.Key, "analysis_done", g, w.lead.ID, nil,
			domain.GateResultWaived, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := exec(task.Key, "analysis_done", dev); err != nil || res.Blocked || res.Task.StatusKey != "architecture" {
		t.Fatalf("analysis_done сотрудником: %v %+v", err, res)
	}
	// Чек-лист чужого этапа: passed и failed — да, waived — нет.
	for _, s := range []string{domain.GateResultFailed, domain.GateResultPassed} {
		if err := decide(task.Key, "architecture_done", "traceability", dev, s); err != nil {
			t.Fatalf("чек-лист %s сотрудником: %v", s, err)
		}
	}
	if err := decide(task.Key, "architecture_done", "traceability", dev, domain.GateResultWaived); !forbidden(err) ||
		reasonOf(err) != DenyMaintainerAction {
		t.Fatalf("waived чек-листа сотрудником: %v", err)
	}
	// Возврат чужого этапа (гейт problem_report держит), затем cancel.
	if res, err := exec(task.Key, "reject_to_analysis", dev); err != nil || !res.Blocked {
		t.Fatalf("возврат без описания: %v %+v", err, res)
	}
	if _, err := w.engine.SubmitArtifact(w.ctx, task.Key, w.lead.ID, SubmitArtifactInput{
		Kind: "problem.report", Title: "проблема", Content: []byte("# проблема")}); err != nil {
		t.Fatal(err)
	}
	if res, err := exec(task.Key, "reject_to_analysis", dev); err != nil || res.Blocked || res.Task.StatusKey != "analysis" {
		t.Fatalf("возврат сотрудником: %v %+v", err, res)
	}
	if res, err := exec(task.Key, "cancel", none); err != nil || res.Task.StatusKey != "cancelled" {
		t.Fatalf("cancel сотрудником: %v %+v", err, res)
	}

	// Эпик: start из planning, апрув нарезки — без роли orchestrator.
	epic, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{TaskType: "epic", Title: "эпик сотрудника"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec(epic.Key, "start", dev); err != nil {
		t.Fatalf("start эпика сотрудником: %v", err)
	}
	if err := decide(epic.Key, "breakdown_done", "human_approval", dev, domain.GateResultPassed); err != nil {
		t.Fatalf("апрув нарезки сотрудником: %v", err)
	}
}

// AC-11, AC-20 (CLAB-379), AC-8 (CLAB-410): сверка seen_artifact_id и
// устаревание апрува действуют для сотрудника так же — и на своём этапе, и
// на чужом; право (403) — раньше сверки (409); апрув сотрудника засчитан.
func TestStageAuthoritySeenAndStale(t *testing.T) {
	w := newRightsWorld(t)
	analyst := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst"}}
	// Бот чужой роли — отказ по праву раньше сверки.
	developer := Actor{Principal: w.bot, Level: domain.MemberAgent, Roles: []string{"developer"}}
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

	// Сотрудник вне роли этапа: та же сверка, апрув засчитан и устаревает.
	outsider := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"}}
	_, err = w.engine.DecideGateAs(w.ctx, task.Key, "analysis_done", "human_approval", outsider,
		domain.GateResultPassed, json.RawMessage(`{}`), &seen)
	if apperr.As(err).Code != apperr.CodeConflict || reasonOf(err) != SeenConflict {
		t.Fatalf("сверка сотрудника вне роли: %v", err)
	}
	art2, err := w.store.LatestArtifactOfKind(w.ctx, w.store.DB(), task.ID, "analysis.brief")
	if err != nil {
		t.Fatal(err)
	}
	seen2 := art2.String()
	if _, err := w.engine.DecideGateAs(w.ctx, task.Key, "analysis_done", "human_approval", outsider,
		domain.GateResultPassed, json.RawMessage(`{}`), &seen2); err != nil {
		t.Fatalf("апрув сотрудника вне роли со сверкой: %v", err)
	}
	if s := gateStatus(); s != domain.GateResultPassed {
		t.Fatalf("апрув сотрудника вне роли не засчитан: %s", s)
	}
	if _, err := w.engine.SubmitArtifact(w.ctx, task.Key, w.lead.ID, SubmitArtifactInput{
		Kind: "analysis.brief", Title: "brief 3", Content: []byte("# brief 3")}); err != nil {
		t.Fatal(err)
	}
	if s := gateStatus(); s != domain.GateResultPending {
		t.Fatalf("апрув сотрудника после новой сдачи не устарел: %s", s)
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
		{"сотрудник NULL", Actor{Principal: w.lead, Level: domain.MemberAgent}, true,
			[]string{anaItem + "+", arcCheck + "+", arcApproval + "+"}},
		{"сотрудник [analyst]", Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"analyst"}}, true,
			[]string{anaItem + "+"}},
		{"сотрудник [developer]", Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"}}, true, nil},
		// K-3 CLAB-410: сотрудник без ролей решает гейты любого этапа, список
		// — зона ролей, пуст.
		{"сотрудник []", Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{}}, true, nil},
		{"бот []", Actor{Principal: bot, Level: domain.MemberAgent, Roles: []string{}}, false, nil},
		{"бот [architect]", Actor{Principal: bot, Level: domain.MemberAgent, Roles: []string{"architect"}}, true,
			[]string{arcCheck + "+", arcApproval + "-"}},
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
// cancel эпика боту закрыты при любом наборе ролей, в том числе NULL и
// с ролью соседнего этапа (orchestrator); предсказание совпадает с отказом.
// Сотрудник (CLAB-410, AC-5) закрывает эпик без роли, предсказание — «вправе».
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
		{Principal: w.bot, Level: domain.MemberAgent},
		{Principal: w.bot, Level: domain.MemberAgent, Roles: []string{"orchestrator", "analyst"}},
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

	employee := Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{"developer"}, TokenKind: domain.TokenAPI}
	views, err := w.engine.PreviewFor(w.ctx, after, &employee)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.ActorMay == nil || !*v.ActorMay || v.ActorDeny != nil {
			t.Fatalf("сотрудник: предсказание %s: %+v", v.Key, v)
		}
	}
	if res, err := w.engine.ExecuteAs(w.ctx, epic.Key, "children_done", employee, ""); err != nil ||
		res.Blocked || res.Task.StatusKey != "done" {
		t.Fatalf("children_done сотрудником: %v %+v", err, res)
	}
	adv := w.lastEvent(after, domain.EventTaskAdvanced)
	if adv["transition"] != "children_done" || adv["via"] != ViaLevel || adv["level"] != "agent" || adv["token_kind"] != "api" {
		t.Fatalf("task.advanced эпика сотрудником: %v", adv)
	}
}
