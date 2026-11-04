package workflow

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Перевод задачи в статус (CLAB-408): операция движка MoveTask на живой базе,
// цикл limitsBundle — предел этапа на входе в development, перепроверка с
// пределом на выходе, апрув и перепроверка без предела на выходе из testing,
// предел попыток на возврате.

func (w limitsWorld) employee() Actor {
	return Actor{Principal: w.lead, Level: domain.MemberAgent, Roles: []string{}, TokenKind: domain.TokenSession}
}

func (w limitsWorld) count(sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.store.DB().QueryRow(w.ctx, sql, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w limitsWorld) runs(task domain.Task, status string) []domain.StageRun {
	w.t.Helper()
	all, err := w.store.ListStageRuns(w.ctx, w.store.DB(), task.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []domain.StageRun
	for _, r := range all {
		if r.StatusKey == status && r.Kind == domain.StageKindStage {
			out = append(out, r)
		}
	}
	return out
}

func (w limitsWorld) lastEvent(task domain.Task, typ string) map[string]any {
	w.t.Helper()
	var raw json.RawMessage
	if err := w.store.DB().QueryRow(w.ctx, `
		SELECT payload FROM task_events WHERE task_id = $1 AND type = $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, task.ID, typ).Scan(&raw); err != nil {
		w.t.Fatalf("%s: %v", typ, err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		w.t.Fatal(err)
	}
	return out
}

func (w limitsWorld) move(task domain.Task, to string) MoveResult {
	w.t.Helper()
	res, err := w.engine.MoveTask(w.ctx, task.Key, to, "так нужно", w.employee())
	if err != nil {
		w.t.Fatalf("перевод %s в %s: %v", task.Key, to, err)
	}
	return res
}

// AC-9, AC-10 (тот же статус), AC-12, K-5, K-9: вперёд через несколько
// статусов, назад, в тот же. Прежний прогон abandoned/moved без аренды, новый
// — попытка N+1 без entered_via и rejected_from; гейты не вычисляются, их
// следа нет; событие task.moved с полями контракта и stage.entered.
func TestMoveTaskForwardBackSame(t *testing.T) {
	w := newLimitsWorld(t)
	gateTrail := func(task domain.Task) int {
		return w.count(`SELECT count(*) FROM gate_results gr JOIN stage_runs sr ON sr.id = gr.stage_run_id
			WHERE sr.task_id = $1`, task.ID) +
			w.count(`SELECT count(*) FROM task_events WHERE task_id = $1 AND type LIKE 'gate.%'`, task.ID)
	}

	task := w.task("new")
	// Вперёд мимо development: апрув, перепроверки и пределы не спрашиваются.
	res := w.move(task, "testing")
	if res.Task.StatusKey != "testing" || res.StageRun.StatusKey != "testing" || res.StageRun.Attempt != 1 ||
		res.StageRun.State != domain.StagePending || res.StageRun.EnteredViaTransitionID != nil ||
		res.StageRun.RejectedFromRunID != nil || res.ClosedReviews != 0 {
		t.Fatalf("вперёд: %+v", res)
	}
	prev := res.PreviousRun
	if prev == nil || prev.StatusKey != "new" || prev.State != domain.StageAbandoned ||
		prev.Outcome == nil || *prev.Outcome != domain.RunOutcomeMoved || prev.FinishedAt == nil || prev.LeaseExpires != nil {
		t.Fatalf("прежний прогон: %+v", prev)
	}
	if n := gateTrail(task); n != 0 {
		t.Fatalf("гейты оставили след: %d", n)
	}
	if n := len(w.runs(task, "development")); n != 0 {
		t.Fatalf("у обойдённого статуса прогоны: %d", n)
	}
	ev := w.lastEvent(task, domain.EventTaskMoved)
	want := map[string]any{"from": "new", "to": "testing", "reason": "так нужно", "level": "agent",
		"via": ViaLevel, "token_kind": domain.TokenSession, "stage_role": "", "attempt": float64(1)}
	keys := make([]string, 0, len(ev))
	for k := range ev {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"attempt", "from", "level", "reason", "stage_role", "to", "token_kind", "via"}) {
		t.Fatalf("ключи task.moved: %v", keys)
	}
	for k, v := range want {
		if ev[k] != v {
			t.Fatalf("task.moved %s = %v, ждали %v", k, ev[k], v)
		}
	}
	entered := w.lastEvent(task, domain.EventStageEntered)
	if entered["status"] != "testing" || entered["attempt"] != float64(1) || entered["transition"] != nil {
		t.Fatalf("stage.entered: %v", entered)
	}

	// Назад и в тот же статус: попытки растут, stage_role — роль ушедшего
	// статуса.
	res = w.move(task, "development")
	if res.StageRun.Attempt != 1 || w.lastEvent(task, domain.EventTaskMoved)["stage_role"] != "checker" {
		t.Fatalf("назад: %+v", res.StageRun)
	}
	res = w.move(task, "development")
	if res.StageRun.Attempt != 2 || res.PreviousRun.StatusKey != "development" || res.PreviousRun.Attempt != 1 {
		t.Fatalf("тот же статус: %+v / %+v", res.StageRun, res.PreviousRun)
	}
	if n := gateTrail(task); n != 0 {
		t.Fatalf("гейты оставили след: %d", n)
	}
	// Предела времени у прогона перевода нет (вход не переходом start).
	if limit, err := w.engine.limitFor(w.ctx, w.store.DB(), res.StageRun, time.Now().Add(48*time.Hour)); err != nil || limit != nil {
		t.Fatalf("предел этапа у прогона перевода: %+v %v", limit, err)
	}
	// Лента: оба события перевода на месте, по порядку.
	if n := w.count(`SELECT count(*) FROM task_events WHERE task_id = $1 AND type = 'task.moved'`, task.ID); n != 3 {
		t.Fatalf("task.moved: %d", n)
	}
	// Выход прогона в карточке — перевод с причиной.
	exits, err := w.store.RunExitsByTask(w.ctx, w.store.DB(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if x, ok := exits[res.PreviousRun.ID]; !ok || x.Kind != "moved" || x.ToStatus != "development" ||
		x.Transition != "" || x.Comment == nil || *x.Comment != "так нужно" {
		t.Fatalf("выход прогона: %+v %v", x, ok)
	}
}

// AC-12: предел попыток видит вход переводом — следующий объявленный возврат
// в development упирается в предел; предел времени у прогона перевода не
// действует, и воркер его не просрочивает.
func TestMoveTaskLimits(t *testing.T) {
	w := newLimitsWorld(t)
	task := w.task("testing") // development: попытка 1
	w.move(task, "development")
	w.move(task, "testing")
	cur, _ := w.store.GetTaskByKey(w.ctx, w.store.DB(), task.Key)
	views, err := w.engine.PreviewFor(w.ctx, cur, nil)
	if err != nil {
		t.Fatal(err)
	}
	var attempts *GateOutcome
	for _, v := range views {
		for i := range v.Gates {
			if v.Key == "testing_failed" && v.Gates[i].GateKey == "attempts" {
				attempts = &v.Gates[i]
			}
		}
	}
	if attempts == nil || attempts.Status != domain.GateResultFailed {
		t.Fatalf("предел попыток после двух входов в development: %+v", attempts)
	}

	// Предел времени: прогон перевода старше предела не просрочен воркером.
	w.move(task, "development")
	runs := w.runs(task, "development")
	last := runs[len(runs)-1]
	if _, err := w.store.DB().Exec(w.ctx, `UPDATE stage_runs SET entered_at = now() - interval '5 hours' WHERE id = $1`, last.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.MarkOverdueStages(w.ctx, w.system.ID, 100); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM stage_runs WHERE id = $1 AND overdue_at IS NOT NULL`, last.ID); n != 0 {
		t.Fatal("прогон перевода просрочен пределом этапа")
	}
	if n := w.count(`SELECT count(*) FROM task_events WHERE task_id = $1 AND type = 'stage.overdue'`, task.ID); n != 0 {
		t.Fatal("stage.overdue у прогона перевода")
	}
}

// AC-11, UC-6, AC-19: в терминальный и обратно. closed_at ставится и
// снимается, прогон терминального статуса закрывается переводом.
func TestMoveTaskTerminalAndBack(t *testing.T) {
	w := newLimitsWorld(t)
	task := w.task("development")
	res := w.move(task, "done")
	if res.Task.ClosedAt == nil || res.PreviousRun.StatusKey != "development" {
		t.Fatalf("в done: %+v", res)
	}
	firstClosed := *res.Task.ClosedAt
	// Тот же терминальный: closed_at обновляется (K-5).
	res = w.move(task, "done")
	if res.Task.ClosedAt == nil || res.Task.ClosedAt.Before(firstClosed) || res.StageRun.Attempt != 2 {
		t.Fatalf("done → done: %+v", res)
	}
	res = w.move(task, "cancelled")
	if res.Task.ClosedAt == nil || res.PreviousRun.StatusKey != "done" ||
		res.PreviousRun.Outcome == nil || *res.PreviousRun.Outcome != domain.RunOutcomeMoved {
		t.Fatalf("done → cancelled: %+v", res)
	}
	res = w.move(task, "testing")
	if res.Task.ClosedAt != nil {
		t.Fatalf("closed_at не снят: %+v", res.Task)
	}
	stored, err := w.store.GetTaskByKey(w.ctx, w.store.DB(), task.Key)
	if err != nil || stored.ClosedAt != nil || stored.StatusKey != "testing" {
		t.Fatalf("в базе: %+v %v", stored, err)
	}
	// Во всех статусах открытых прогонов один: новый.
	if n := w.count(`SELECT count(*) FROM stage_runs WHERE task_id = $1 AND kind = 'stage' AND finished_at IS NULL`, task.ID); n != 1 {
		t.Fatalf("открытых прогонов %d", n)
	}
}

// K-9: перевод с открытой перепроверкой — перепроверка закрыта moved, её
// счёт в пределе перепроверок не растёт; родитель в gating закрыт.
func TestMoveTaskClosesReviews(t *testing.T) {
	w := newLimitsWorld(t)
	task := w.task("development")
	if _, err := w.engine.SubmitArtifact(w.ctx, task.Key, w.lead.ID, SubmitArtifactInput{
		Kind: "dev.note", Title: "заметка", Format: "markdown", Content: []byte("# сделано")}); err != nil {
		t.Fatal(err)
	}
	if res, err := w.engine.Execute(w.ctx, task.Key, "dev_done", w.lead.ID, ""); err != nil || !res.Blocked {
		t.Fatalf("dev_done должен ждать перепроверку: %v %+v", err, res)
	}
	parent := w.runs(task, "development")[0]
	open, err := w.store.OpenReviewRuns(w.ctx, w.store.DB(), parent.ID)
	if err != nil || len(open) != 1 {
		t.Fatalf("перепроверка не заведена: %v %v", open, err)
	}

	res := w.move(task, "development")
	if res.ClosedReviews != 1 || res.PreviousRun.ID != parent.ID || res.PreviousRun.State != domain.StageAbandoned {
		t.Fatalf("перевод с перепроверкой: %+v", res)
	}
	review, err := w.store.GetStageRun(w.ctx, w.store.DB(), open[0].ID)
	if err != nil || review.State != domain.StageAbandoned || review.Outcome == nil ||
		*review.Outcome != domain.RunOutcomeMoved || review.FinishedAt == nil {
		t.Fatalf("перепроверка: %+v %v", review, err)
	}
	gate := w.transitionGate(task, "dev_done", "peer_review")
	if n, err := w.store.CountFailedReviews(w.ctx, w.store.DB(), parent.ID, gate); err != nil || n != 0 {
		t.Fatalf("закрытая переводом перепроверка в счёте провалов: %d %v", n, err)
	}
	if open, _ := w.store.OpenReviewRuns(w.ctx, w.store.DB(), parent.ID); len(open) != 0 {
		t.Fatalf("открытые перепроверки остались: %v", open)
	}
}

func (w limitsWorld) transitionGate(task domain.Task, transition, gate string) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.store.DB().QueryRow(w.ctx, `
		SELECT g.id FROM gates g JOIN workflow_transitions wt ON wt.id = g.transition_id
		WHERE wt.workflow_id = $1 AND wt.key = $2 AND g.key = $3`, task.WorkflowID, transition, gate).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// AC-10: ошибки ничего не меняют — пустая причина, статус вне цикла.
func TestMoveTaskInvalidInput(t *testing.T) {
	w := newLimitsWorld(t)
	task := w.task("development")
	before := w.runs(task, "development")
	events := w.count(`SELECT count(*) FROM task_events WHERE task_id = $1`, task.ID)
	for _, c := range []struct{ to, reason string }{
		{"testing", ""}, {"testing", "   "}, {"nope", "причина"}, {"", "причина"},
	} {
		_, err := w.engine.MoveTask(w.ctx, task.Key, c.to, c.reason, w.employee())
		e := apperr.As(err)
		if err == nil || e.Code != apperr.CodeInvalidInput {
			t.Fatalf("%+v: ждали 400, а %v", c, err)
		}
		if c.reason != "" && strings.TrimSpace(c.reason) != "" {
			d, _ := e.Details.(map[string]any)
			statuses, _ := d["statuses"].([]string)
			if !slices.Equal(statuses, []string{"new", "development", "testing", "done", "cancelled"}) {
				t.Fatalf("%+v: details.statuses %v", c, d)
			}
		}
	}
	if _, err := w.engine.MoveTask(w.ctx, "SDLC-99999", "testing", "причина", w.employee()); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("нет задачи: %v", err)
	}
	after := w.runs(task, "development")
	if len(after) != len(before) || after[0].FinishedAt != nil ||
		w.count(`SELECT count(*) FROM task_events WHERE task_id = $1`, task.ID) != events {
		t.Fatalf("отказ изменил задачу: %+v", after)
	}
}

// AC-15, AC-17, K-6: класс актора × перевод — прогноз TaskRights и исход
// MoveTask совпадают. Отказ ничего не меняет.
func TestMoveTaskRights(t *testing.T) {
	w := newLimitsWorld(t)
	viewer := mustActor(t, w.store, "user:watcher")
	for _, c := range []struct {
		name   string
		actor  func(domain.Task) Actor
		allow  bool
		reason string
	}{
		{"сотрудник", func(domain.Task) Actor { return w.employee() }, true, ""},
		{"сопровождающий", func(domain.Task) Actor {
			return Actor{Principal: w.lead, Level: domain.MemberMaintainer, TokenKind: domain.TokenAPI}
		}, true, ""},
		{"бот", func(domain.Task) Actor {
			return Actor{Principal: w.bot, Level: domain.MemberAgent, TokenKind: domain.TokenAPI}
		}, false, DenyHumanAction},
		{"бот-администратор", func(domain.Task) Actor {
			return Actor{Principal: w.bot, Level: domain.MemberAdmin, TokenKind: domain.TokenAPI}
		}, true, ""},
		{"viewer", func(domain.Task) Actor {
			return Actor{Principal: viewer, Level: domain.MemberViewer, TokenKind: domain.TokenAPI}
		}, false, DenyLevel},
		{"токен задачи", func(task domain.Task) Actor {
			run := w.runs(task, task.StatusKey)
			return Actor{Principal: w.lead, Level: domain.MemberAdmin, TokenKind: domain.TokenTask,
				Bound: &TaskBinding{StageRunID: run[len(run)-1].ID, TaskID: task.ID, TaskKey: task.Key}}
		}, false, ""},
	} {
		task := w.task("development")
		who := c.actor(task)
		rights, err := w.engine.TaskRights(w.ctx, task.Key, who)
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.engine.MoveTask(w.ctx, task.Key, "testing", "причина", who)
		if rights.Move != c.allow || (err == nil) != c.allow {
			t.Fatalf("%s: прогноз %v, исход %v", c.name, rights.Move, err)
		}
		if c.allow {
			continue
		}
		deny := rights.ActorDeny[string(ActMove)]
		if !forbidden(err) || reasonOf(err) != c.reason || deny == nil || deny.Reason != c.reason ||
			deny.Message != apperr.As(err).Message || deny.Code != "forbidden" {
			t.Fatalf("%s: отказ %v, прогноз %+v", c.name, err, deny)
		}
		if who.Bound != nil && !strings.HasPrefix(apperr.As(err).Message, ExecutorRefusal) {
			t.Fatalf("%s: текст %q", c.name, apperr.As(err).Message)
		}
		if cur := w.runs(task, "development"); cur[len(cur)-1].FinishedAt != nil {
			t.Fatalf("%s: отказ закрыл прогон", c.name)
		}
	}
	if !strings.Contains(TaskWorkOf(w.employee()).Summary, "перевод в статус") {
		t.Fatalf("сводка сотрудника: %q", TaskWorkOf(w.employee()).Summary)
	}
}

// K-3: сбой после закрытия прогона откатывает всё — прогон открыт, аренда и
// токены на месте, статуса и события нет.
func TestMoveTaskAtomic(t *testing.T) {
	w := newLimitsWorld(t)
	task := w.task("development")
	run := w.runs(task, "development")[0]
	if _, err := w.engine.Claim(w.ctx, task.Key, w.bot.ID, "bot"); err != nil {
		t.Fatal(err)
	}
	// Вход в testing отказывает на уровне базы — после FinishStageRun и
	// SetTaskStatus, в EnterStatus.
	if _, err := w.store.DB().Exec(w.ctx, `
		CREATE FUNCTION move_fault() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'сбой входа'; END $$;
		CREATE TRIGGER move_fault BEFORE INSERT ON stage_runs FOR EACH ROW
		WHEN (NEW.task_id = '`+task.ID.String()+`') EXECUTE FUNCTION move_fault();`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.store.DB().Exec(w.ctx, `DROP TRIGGER IF EXISTS move_fault ON stage_runs; DROP FUNCTION IF EXISTS move_fault()`)
	})
	if _, err := w.engine.MoveTask(w.ctx, task.Key, "testing", "причина", w.employee()); err == nil {
		t.Fatal("перевод прошёл мимо сбоя")
	}
	got, err := w.store.GetStageRun(w.ctx, w.store.DB(), run.ID)
	if err != nil || got.FinishedAt != nil || got.Outcome != nil || got.LeaseExpires == nil || got.ClaimedBy == nil {
		t.Fatalf("прогон после отката: %+v %v", got, err)
	}
	stored, _ := w.store.GetTaskByKey(w.ctx, w.store.DB(), task.Key)
	if stored.StatusKey != "development" ||
		w.count(`SELECT count(*) FROM task_events WHERE task_id = $1 AND type = 'task.moved'`, task.ID) != 0 {
		t.Fatalf("после отката: %s", stored.StatusKey)
	}
}

// K-9: перевод родителя не трогает детей.
func TestMoveTaskParentLeavesChildren(t *testing.T) {
	w := newLimitsWorld(t)
	parent := w.task("development")
	child, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "ребёнок", ParentKey: parent.Key}})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := w.engine.Execute(w.ctx, child.Key, "start", w.lead.ID, ""); err != nil || res.Blocked {
		t.Fatalf("start ребёнка: %v", err)
	}
	before := w.count(`SELECT count(*) FROM task_events WHERE task_id = $1`, child.ID)
	w.move(parent, "done")
	stored, _ := w.store.GetTaskByKey(w.ctx, w.store.DB(), child.Key)
	if stored.StatusKey != "development" || stored.ClosedAt != nil ||
		w.count(`SELECT count(*) FROM task_events WHERE task_id = $1`, child.ID) != before {
		t.Fatalf("ребёнок изменился: %+v", stored)
	}
}

// AC-13, UC-7: после перевода и после возврата захват видит артефакты прежней
// попытки, апрув новой попытки — pending, прежние прогоны и решения на месте.
func TestMoveTaskSecondPass(t *testing.T) {
	w := newLimitsWorld(t)
	task := w.task("testing")
	art, err := w.engine.SubmitArtifact(w.ctx, task.Key, w.lead.ID, SubmitArtifactInput{
		Kind: "review.verdict", Title: "проверка", Format: "markdown", Content: []byte("ок")})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.decide(task, "testing_passed", "sign_off", w.employee(), domain.GateResultPassed); err != nil {
		t.Fatal(err)
	}
	first := w.runs(task, "testing")[0]
	w.move(task, "testing")

	claim, err := w.engine.Claim(w.ctx, task.Key, w.lead.ID, "lead")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Task.Attempt != 2 || !hasPrior(claim, art.ID) {
		t.Fatalf("захват после перевода: попытка %d, вход %+v", claim.Task.Attempt, claim.Context.PriorArtifacts)
	}
	cur, _ := w.store.GetTaskByKey(w.ctx, w.store.DB(), task.Key)
	views, err := w.engine.PreviewFor(w.ctx, cur, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		for _, g := range v.Gates {
			if g.GateKey == "sign_off" && g.Status != domain.GateResultPending {
				t.Fatalf("апрув новой попытки: %s", g.Status)
			}
		}
	}
	if d, err := w.store.GateDecision(w.ctx, w.store.DB(), first.ID, w.transitionGate(task, "testing_passed", "sign_off")); err != nil ||
		d == nil || d.Status != domain.GateResultPassed {
		t.Fatalf("решение прежней попытки: %+v %v", d, err)
	}

	// Возврат в development (предел попыток снят) — захват видит и
	// артефакты попытки testing.
	if err := w.decide(cur, "testing_failed", "attempts", w.employee(), domain.GateResultWaived); err != nil {
		t.Fatal(err)
	}
	if res, err := w.engine.ExecuteAs(w.ctx, task.Key, "testing_failed", w.employee(), "доработать"); err != nil || res.Blocked {
		t.Fatalf("testing_failed: %v %+v", err, res)
	}
	claim, err = w.engine.Claim(w.ctx, task.Key, w.lead.ID, "lead")
	if err != nil || !hasPrior(claim, art.ID) {
		t.Fatalf("захват после возврата: %v %+v", err, claim.Context.PriorArtifacts)
	}
	if n := len(w.runs(task, "testing")); n != 2 {
		t.Fatalf("попыток testing %d", n)
	}
}

func hasPrior(c ClaimResult, artifact uuid.UUID) bool {
	for _, a := range c.Context.PriorArtifacts {
		if a.ID == artifact {
			return true
		}
	}
	return false
}

// K-8, AC-16: метрики статуса — moves отдельно, прогон moved не входит в
// finished_runs, rejections, repeated_runs и часы.
func TestMoveTaskMetrics(t *testing.T) {
	w := newLimitsWorld(t)
	task := w.task("development")
	w.move(task, "development") // попытка 1 закрыта moved
	w.move(task, "testing")     // попытка 2 закрыта moved
	m, err := w.engine.ProjectMetrics(w.ctx, "SDLC", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Statuses {
		if s.StatusKey != "development" {
			continue
		}
		if s.Moves != 2 || s.FinishedRuns != 0 || s.Rejections != 0 || s.RepeatedRuns != 0 || s.MaxHours != 0 {
			t.Fatalf("development: %+v", s)
		}
		return
	}
	t.Fatalf("нет строки development: %+v", m.Statuses)
}

// AC-10: выключенный необязательный этап — 409 stage_disabled с подсказкой,
// задача не меняется; включённый необязательный — перевод проходит.
func TestMoveTaskDisabledStage(t *testing.T) {
	w := newSkipWorld(t)
	task := w.create(t, "перевод в выключенный", "", "")
	who := Actor{Principal: w.human, Level: domain.MemberAgent, Roles: []string{}, TokenKind: domain.TokenAPI}
	_, err := w.engine.MoveTask(t.Context(), task.Key, "design", "причина", who)
	e := apperr.As(err)
	d, _ := e.Details.(map[string]any)
	if e.Code != apperr.CodeStageDisabled || !strings.Contains(e.Message, "clab edit "+task.Key+" --stage design=on") ||
		d["unavailable"] != UnavailableStageDisabled || d["status"] != "design" {
		t.Fatalf("выключенный этап: %v %+v", err, d)
	}
	if runs := w.runsOn(t, task.ID, "new"); len(runs) != 1 || runs[0].FinishedAt != nil {
		t.Fatalf("отказ изменил прогон: %+v", runs)
	}
	res, err := w.engine.MoveTask(t.Context(), task.Key, "docs", "причина", who)
	if err != nil || res.Task.StatusKey != "docs" {
		t.Fatalf("включённый необязательный: %v %+v", err, res.Task)
	}
	// Включённый человеком — перевод проходит.
	if _, err := w.edit(task.Key, `{"design": true}`, UpdateTaskInput{}); err != nil {
		t.Fatal(err)
	}
	if res, err := w.engine.MoveTask(t.Context(), task.Key, "design", "причина", who); err != nil || res.Task.StatusKey != "design" {
		t.Fatalf("после включения: %v", err)
	}
}
