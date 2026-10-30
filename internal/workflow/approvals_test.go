package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// «Ждут меня», признак списка и условие сверки (CLAB-326) на двух непохожих
// циклах одного проекта: nova — форма нашего цикла (апрув на выходе из
// аналитики, чек-лист и артефакты на выходе из разработки), forge — чужой
// (чек-лист и апрув на двух людей на одном переходе, апрув приёмки на
// статусе ожидания, возврат с артефактом своего вида).

func novaCycle() BundleWorkflow {
	yes := true
	return BundleWorkflow{
		Name: "Nova", TaskTypes: []string{"nova"},
		Statuses: []BundleStatus{
			{Key: "new", Name: "Новая", Category: "backlog", IsInitial: true},
			{Key: "analysis", Name: "Аналитика", Category: "active"},
			{Key: "development", Name: "Разработка", Category: "active"},
			{Key: "done", Name: "Готово", Category: "done", IsTerminal: true},
			{Key: "cancelled", Name: "Отменена", Category: "cancelled", IsTerminal: true},
		},
		Transitions: []BundleTransition{
			{Key: "start", Name: "В работу", From: "new", To: "analysis", Kind: "advance"},
			{Key: "analysis_done", Name: "Аналитика готова", From: "analysis", To: "development", Kind: "advance", Gates: []BundleGate{
				{Key: "artifacts", Name: "Требования сданы", Type: domain.GateArtifactsRequired, Blocking: &yes, Config: json.RawMessage(`{"kinds":["analysis.brief"]}`)},
				{Key: "human_approval", Name: "Апрув аналитики", Type: domain.GateHumanApproval, Blocking: &yes, Config: json.RawMessage(`{"min_approvals":1}`)},
			}},
			{Key: "dev_done", Name: "Разработка готова", From: "development", To: "done", Kind: "advance", Gates: []BundleGate{
				{Key: "artifacts", Name: "Разбор сдан", Type: domain.GateArtifactsRequired, Blocking: &yes, Config: json.RawMessage(`{"kinds":["dev.summary"]}`)},
				{Key: "selfhosted", Name: "Self-hosted учтён", Type: domain.GateChecklist, Blocking: &yes, Config: json.RawMessage(`{"items":[{"id":"selfhosted","text":"Раздел написан","required":true}]}`)},
			}},
			{Key: "reject_to_analysis", Name: "Вернуть аналитику", From: "development", To: "analysis", Kind: "reject", Gates: []BundleGate{
				{Key: "problem_report", Name: "Описание проблемы", Type: domain.GateArtifactsRequired, Blocking: &yes, Config: json.RawMessage(`{"kinds":["problem.report"]}`)},
			}},
			{Key: "cancel", Name: "Снять", To: "cancelled", Kind: "cancel"},
		},
	}
}

func forgeCycle() BundleWorkflow {
	yes := true
	return BundleWorkflow{
		Name: "Forge Story", TaskTypes: []string{"story"},
		Statuses: []BundleStatus{
			{Key: "backlog", Name: "Backlog", Category: "backlog", IsInitial: true},
			{Key: "build", Name: "Build", Category: "active"},
			{Key: "review", Name: "Review board", Category: "waiting"},
			{Key: "shipped", Name: "Shipped", Category: "done", IsTerminal: true},
			{Key: "dropped", Name: "Dropped", Category: "cancelled", IsTerminal: true},
		},
		Transitions: []BundleTransition{
			{Key: "pick", Name: "Pick up", From: "backlog", To: "build", Kind: "advance"},
			{Key: "ship", Name: "Ship to board", From: "build", To: "review", Kind: "advance", Gates: []BundleGate{
				{Key: "rollback", Name: "Rollback plan", Type: domain.GateChecklist, Blocking: &yes, Config: json.RawMessage(`{"items":[{"id":"plan","text":"Plan written","required":true}]}`)},
				{Key: "two_people", Name: "Two sign-offs", Type: domain.GateHumanApproval, Blocking: &yes, Config: json.RawMessage(`{"min_approvals":2}`)},
			}},
			{Key: "accept", Name: "Accept", From: "review", To: "shipped", Kind: "advance", Gates: []BundleGate{
				{Key: "sign_off", Name: "Go-live sign-off", Type: domain.GateHumanApproval, Blocking: &yes, Config: json.RawMessage(`{"min_approvals":1}`)},
			}},
			{Key: "send_back", Name: "Send back", From: "review", To: "build", Kind: "reject", Gates: []BundleGate{
				{Key: "rework", Name: "Rework note", Type: domain.GateArtifactsRequired, Blocking: &yes, Config: json.RawMessage(`{"kinds":["forge.rework"]}`)},
			}},
			{Key: "drop", Name: "Drop", To: "dropped", Kind: "cancel"},
		},
	}
}

type approvalsWorld struct {
	t      *testing.T
	ctx    context.Context
	store  *postgres.Store
	engine *Engine
	system domain.Principal
	human  domain.Principal
	other  domain.Principal
	pid    uuid.UUID
}

func newApprovalsWorld(t *testing.T) approvalsWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{
		Version: BundleVersion, Workflows: []BundleWorkflow{novaCycle(), forgeCycle()}}, false)
	if err != nil {
		t.Fatalf("импорт циклов: %v", err)
	}
	for i, v := range report.Validations {
		if !v.Valid {
			t.Fatalf("цикл %d не прошёл проверку: %+v", i, v)
		}
	}
	for _, w := range report.Workflows {
		id, err := uuid.Parse(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := authoring.Activate(ctx, id, system.ID); err != nil {
			t.Fatalf("активация %s: %v", w.Key, err)
		}
	}
	human, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.ResolveActor(ctx, store.DB(), "second", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	return approvalsWorld{t: t, ctx: ctx, store: store, engine: engine, system: system, human: human, other: other, pid: project.ID}
}

func (w approvalsWorld) task(typ, title string, moves ...string) string {
	w.t.Helper()
	task, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{TaskType: typ, Title: title}})
	if err != nil {
		w.t.Fatal(err)
	}
	w.move(task.Key, moves...)
	return task.Key
}

func (w approvalsWorld) move(key string, moves ...string) {
	w.t.Helper()
	for _, m := range moves {
		res, err := w.engine.Execute(w.ctx, key, m, w.system.ID, "")
		if err != nil || res.Blocked {
			w.t.Fatalf("%s %s: %v %+v", key, m, err, res.Gates)
		}
	}
}

func (w approvalsWorld) submit(key, kind string) domain.Artifact {
	w.t.Helper()
	a, err := w.engine.SubmitArtifact(w.ctx, key, w.system.ID, SubmitArtifactInput{
		Kind: kind, Title: kind, Format: "markdown", Content: []byte("# " + kind)})
	if err != nil {
		w.t.Fatal(err)
	}
	return a
}

func (w approvalsWorld) decide(key, transition, gate string, who domain.Principal, status, details string) {
	w.t.Helper()
	if _, err := w.engine.DecideGate(w.ctx, key, transition, gate, who.ID, nil, status, json.RawMessage(details)); err != nil {
		w.t.Fatalf("%s %s/%s %s: %v", key, transition, gate, status, err)
	}
}

// lease ставит или снимает живую аренду текущего прогона: статусы этих
// циклов без ролей, и захватом аренду не взять.
func (w approvalsWorld) lease(key string, held bool) {
	w.t.Helper()
	task, err := w.store.GetTaskByKey(w.ctx, w.store.DB(), key)
	if err != nil {
		w.t.Fatal(err)
	}
	run, err := w.store.CurrentStageRun(w.ctx, w.store.DB(), task.ID, task.StatusID)
	if err != nil {
		w.t.Fatal(err)
	}
	if held {
		_, err = w.store.DB().Exec(w.ctx, `UPDATE stage_runs SET claimed_by = $2, claimed_at = now(), lease_expires_at = now() + interval '10 minutes' WHERE id = $1`, run.ID, w.system.ID)
	} else {
		_, err = w.store.DB().Exec(w.ctx, `UPDATE stage_runs SET claimed_by = NULL, claimed_at = NULL, lease_expires_at = NULL WHERE id = $1`, run.ID)
	}
	if err != nil {
		w.t.Fatal(err)
	}
}

// list — элементы выборки строками «КЛЮЧ переход/гейт».
func (w approvalsWorld) list() []string {
	w.t.Helper()
	got, err := w.engine.Approvals(w.ctx, w.store.DB(), w.pid, true, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	out := []string{}
	for _, a := range got.Approvals {
		out = append(out, a.Task.Key+" "+a.Transition.Key+"/"+a.Gate.GateKey)
	}
	return out
}

func TestAwaitsHumanRule(t *testing.T) {
	g := func(typ, status string) GateOutcome {
		return GateOutcome{GateKey: typ, Type: typ, Status: status, Blocking: true}
	}
	keys := func(gs []GateOutcome) string {
		out := []string{}
		for _, x := range gs {
			out = append(out, x.GateKey)
		}
		return strings.Join(out, ",")
	}
	cases := []struct {
		name  string
		gates []GateOutcome
		want  string
	}{
		{"апрув ждёт, остальное прошло", []GateOutcome{g("artifacts_required", "passed"), g("human_approval", "pending")}, "human_approval"},
		{"чек-лист и апрув ждут вместе", []GateOutcome{g("checklist", "pending"), g("human_approval", "pending")}, "checklist,human_approval"},
		{"непройденный автоматический держит", []GateOutcome{g("artifacts_required", "failed"), g("human_approval", "pending")}, ""},
		{"отклонённый апрув ждёт исполнителя", []GateOutcome{g("human_approval", "failed")}, ""},
		{"отклонённый апрув не держит чек-лист", []GateOutcome{g("human_approval", "failed"), g("checklist", "pending")}, "checklist"},
		{"всё прошло", []GateOutcome{g("human_approval", "passed"), g("checklist", "waived")}, ""},
		{"неблокирующий не считается", []GateOutcome{{GateKey: "x", Type: "human_approval", Status: "pending"}}, ""},
	}
	for _, c := range cases {
		if got := keys(AwaitsHuman(c.gates)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// AC-13: каждое правило выборки на двух циклах.
func TestApprovalsTwoCycles(t *testing.T) {
	w := newApprovalsWorld(t)

	// nova: аналитика сдана — ждёт апрува; не сдана — нет.
	ready := w.task("nova", "Сдана", "start")
	w.submit(ready, "analysis.brief")
	notReady := w.task("nova", "Не сдана", "start")
	// nova: отклонённый апрув ждёт исполнителя, а не человека.
	rejected := w.task("nova", "Отклонена", "start")
	w.submit(rejected, "analysis.brief")
	w.decide(rejected, "analysis_done", "human_approval", w.human, domain.GateResultFailed, `{"reason":"нет критериев"}`)
	// nova: разработка сдана — чек-лист без аренды ждёт человека.
	dev := w.task("nova", "Разработка", "start")
	w.submit(dev, "analysis.brief")
	w.decide(dev, "analysis_done", "human_approval", w.human, domain.GateResultPassed, `{}`)
	w.move(dev, "analysis_done")
	w.submit(dev, "dev.summary")
	// nova: снятая задача и задача в бэклоге не ждут.
	cancelled := w.task("nova", "Снята", "start")
	w.submit(cancelled, "analysis.brief")
	w.move(cancelled, "cancel")
	w.task("nova", "Бэклог")

	// forge: чек-лист и апрув на двух людей на одном переходе — два элемента.
	build := w.task("story", "Build", "pick")
	// forge: приёмка на статусе ожидания.
	board := w.task("story", "Board", "pick")
	w.decide(board, "ship", "rollback", w.human, domain.GateResultPassed, `{"checked":["plan"]}`)
	w.decide(board, "ship", "two_people", w.human, domain.GateResultPassed, `{}`)
	w.decide(board, "ship", "two_people", w.other, domain.GateResultPassed, `{}`)
	w.move(board, "ship")
	_ = notReady

	want := []string{
		ready + " analysis_done/human_approval",
		dev + " dev_done/selfhosted",
		build + " ship/rollback",
		build + " ship/two_people",
		board + " accept/sign_off",
	}
	if got := w.list(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("выборка:\n%v\nwant\n%v", got, want)
	}

	// Элемент несёт задачу, переход с подписью статуса, гейт и прогон с
	// последним артефактом этапа.
	got, err := w.engine.Approvals(w.ctx, w.store.DB(), w.pid, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := got.Approvals[0]
	if first.Task.Title != "Сдана" || first.Task.Type != "nova" || first.Task.StatusName != "Аналитика" ||
		first.Transition.Name != "Аналитика готова" || first.Transition.LandsIn != "development" ||
		first.Transition.LandsInName != "Разработка" || first.Gate.GateName != "Апрув аналитики" ||
		first.Gate.Type != domain.GateHumanApproval || first.StageRun.LatestArtifactID == nil || first.StageRun.Attempt != 1 {
		t.Fatalf("элемент: %+v", first)
	}
	if b := got.Approvals[4]; b.Task.StatusName != "Review board" || b.Transition.LandsInName != "Shipped" || b.StageRun.LatestArtifactID != nil {
		t.Fatalf("элемент forge: %+v", b)
	}

	// Чек-лист при живой аренде прогона — у исполнителя, не у человека; апрув
	// при аренде остаётся.
	w.lease(dev, true)
	w.lease(build, true)
	want = []string{ready + " analysis_done/human_approval", build + " ship/two_people", board + " accept/sign_off"}
	if got := w.list(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("с арендой:\n%v\nwant\n%v", got, want)
	}
	w.lease(dev, false)
	w.lease(build, false)

	// Апрув на двух людей: один поставлен — гейт ждёт второго.
	w.decide(build, "ship", "two_people", w.human, domain.GateResultPassed, `{}`)
	w.decide(build, "ship", "rollback", w.human, domain.GateResultPassed, `{"checked":["plan"]}`)
	if got := w.list(); !contains(got, build+" ship/two_people") || contains(got, build+" ship/rollback") {
		t.Fatalf("после одного апрува и чек-листа: %v", got)
	}

	// Предел и признак обрезки.
	cut, err := w.engine.Approvals(w.ctx, w.store.DB(), w.pid, true, 2)
	if err != nil || len(cut.Approvals) != 2 || !cut.Truncated {
		t.Fatalf("предел 2: %+v %v", cut, err)
	}
	full, err := w.engine.Approvals(w.ctx, w.store.DB(), w.pid, true, 500)
	if err != nil || full.Truncated {
		t.Fatalf("предел сверху: %+v %v", full, err)
	}

	// Без права решать — пусто и признак.
	none, err := w.engine.Approvals(w.ctx, w.store.DB(), w.pid, false, 0)
	if err != nil || none.CanDecide || none.Approvals == nil || len(none.Approvals) != 0 {
		t.Fatalf("без права: %+v %v", none, err)
	}
}

// Задача с переходом вперёд, но без прогона на текущем статусе, не роняет
// выборку: её пропускают, соседние ждущие остаются (как awaitsApproval в
// children.go, CLAB-327).
func TestApprovalsSkipsTaskWithoutRun(t *testing.T) {
	w := newApprovalsWorld(t)
	ready := w.task("nova", "Сдана", "start")
	w.submit(ready, "analysis.brief")
	orphan := w.task("nova", "Без прогона", "start")
	w.submit(orphan, "analysis.brief")
	task, err := w.store.GetTaskByKey(w.ctx, w.store.DB(), orphan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.DB().Exec(w.ctx, `DELETE FROM stage_runs WHERE task_id = $1 AND status_id = $2`, task.ID, task.StatusID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.CurrentStageRun(w.ctx, w.store.DB(), task.ID, task.StatusID); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("прогон не снят: %v", err)
	}

	want := []string{ready + " analysis_done/human_approval"}
	if got := w.list(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("выборка:\n%v\nwant\n%v", got, want)
	}
	// Пропущенная задача не попадает и в признак обрезки.
	got, err := w.engine.Approvals(w.ctx, w.store.DB(), w.pid, true, 1)
	if err != nil || got.Truncated || len(got.Approvals) != 1 {
		t.Fatalf("предел 1 без обрезки: %+v %v", got, err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// countingQ считает обращения к базе на пути выборки.
type countingQ struct {
	postgres.Querier
	n int
}

func (c *countingQ) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n++
	return c.Querier.Query(ctx, sql, args...)
}

func (c *countingQ) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n++
	return c.Querier.QueryRow(ctx, sql, args...)
}

// AC-15: выборка на 100 открытых задачах — число запросов не растёт с
// задачами без человеческих гейтов на переходе вперёд; время — в журнал.
func TestApprovalsQueriesBounded(t *testing.T) {
	w := newApprovalsWorld(t)
	count := func() int {
		q := &countingQ{Querier: w.store.DB()}
		if _, err := w.engine.Approvals(w.ctx, q, w.pid, true, 0); err != nil {
			t.Fatal(err)
		}
		return q.n
	}
	// Пять задач, ждущих апрува.
	for i := 0; i < 5; i++ {
		k := w.task("nova", fmt.Sprintf("Ждёт %d", i), "start")
		w.submit(k, "analysis.brief")
	}
	base := count()
	// Ещё 95 открытых задач без человеческих гейтов на выходе: бэклог и
	// задачи, чей переход вперёд гейтов не несёт.
	for i := 0; i < 95; i++ {
		w.task("story", fmt.Sprintf("Бэклог %d", i))
	}
	if after := count(); after != base {
		t.Fatalf("запросов было %d, с 95 задачами без апрува стало %d", base, after)
	}

	const rounds = 20
	var took []time.Duration
	for i := 0; i < rounds; i++ {
		started := time.Now()
		if got := w.list(); len(got) != 5 {
			t.Fatalf("элементов %d", len(got))
		}
		took = append(took, time.Since(started))
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	p95 := took[rounds*95/100-1]
	t.Logf("Approvals на 100 открытых задачах (5 ждут): %d запросов, p95 %v", base, p95)
	if p95 > 300*time.Millisecond {
		t.Fatalf("p95 выборки %v", p95)
	}
}

// AC-20: признак списка совпадает с детьми карточки и с предпросмотром
// задачи; гейты считаются только у задач в работе с апрувом на выходе.
func TestTasksAwaitingApproval(t *testing.T) {
	w := newApprovalsWorld(t)
	waiting := w.task("nova", "Ждёт", "start")
	w.submit(waiting, "analysis.brief")
	notYet := w.task("nova", "Не сдана", "start")
	board := w.task("story", "Board", "pick")
	w.decide(board, "ship", "rollback", w.human, domain.GateResultPassed, `{"checked":["plan"]}`)
	w.decide(board, "ship", "two_people", w.human, domain.GateResultPassed, `{}`)
	w.decide(board, "ship", "two_people", w.other, domain.GateResultPassed, `{}`)
	w.move(board, "ship")
	// Чек-лист рядом с апрувом не отмечен: признак — нет (правило карточки).
	build := w.task("story", "Build", "pick")
	backlog := w.task("story", "Backlog")

	tasks, err := w.store.ListTasks(w.ctx, w.store.DB(), w.pid, postgres.ListTasksFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.engine.TasksAwaitingApproval(w.ctx, w.store.DB(), tasks)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{waiting: true, notYet: false, board: true, build: false, backlog: false}
	seen := 0
	for _, task := range tasks {
		exp, ok := want[task.Key]
		if !ok {
			continue
		}
		seen++
		if got[task.ID] != exp {
			t.Errorf("%s: %v, want %v", task.Key, got[task.ID], exp)
		}
		previews, err := w.engine.Preview(w.ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		if TransitionsAwaitApproval(previews) != got[task.ID] {
			t.Errorf("%s: признак списка расходится с предпросмотром", task.Key)
		}
	}
	if seen != len(want) {
		t.Fatalf("в списке %d из %d задач", seen, len(want))
	}

	// Число запросов не растёт с задачами без апрува на выходе: 30 строк.
	count := func(tasks []domain.Task) int {
		q := &countingQ{Querier: w.store.DB()}
		if _, err := w.engine.TasksAwaitingApproval(w.ctx, q, tasks); err != nil {
			t.Fatal(err)
		}
		return q.n
	}
	base := count(tasks)
	for i := 0; i < 30; i++ {
		w.task("story", fmt.Sprintf("Ещё бэклог %d", i))
	}
	more, err := w.store.ListTasks(w.ctx, w.store.DB(), w.pid, postgres.ListTasksFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if after := count(more); after != base {
		t.Fatalf("запросов к гейтам было %d, с 30 задачами в бэклоге стало %d", base, after)
	}
}

// AC-4: условие сверки в решении по гейту.
func TestDecideGateSeen(t *testing.T) {
	w := newApprovalsWorld(t)
	decided := func(key string) int {
		t.Helper()
		var n int
		if err := w.store.DB().QueryRow(w.ctx, `
			SELECT count(*) FROM task_events e JOIN tasks t ON t.id = e.task_id
			WHERE t.key = $1 AND e.type = $2`, key, domain.EventGateDecided).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	seen := func(s string) *string { return &s }
	decide := func(key string, cond *string) error {
		_, err := w.engine.DecideGateSeen(w.ctx, key, "analysis_done", "human_approval", w.human.ID, nil,
			domain.GateResultPassed, json.RawMessage(`{}`), cond)
		return err
	}
	isChanged := func(err error) bool {
		var e *apperr.Error
		if !errors.As(err, &e) || e.Code != apperr.CodeConflict {
			return false
		}
		d, _ := e.Details.(map[string]any)
		return d["reason"] == SeenConflict
	}

	// На этапе нет артефактов: "" совпадает, id — нет.
	empty := w.task("nova", "Пусто", "start")
	if err := decide(empty, seen(uuid.NewString())); !isChanged(err) || decided(empty) != 0 {
		t.Fatalf("нет артефактов, условие с id: %v", err)
	}
	if err := decide(empty, seen("")); err != nil || decided(empty) != 1 {
		t.Fatalf("нет артефактов, условие пусто: %v", err)
	}

	// Условие совпало.
	key := w.task("nova", "Сдана", "start")
	first := w.submit(key, "analysis.brief")
	if err := decide(key, seen(first.ID.String())); err != nil || decided(key) != 1 {
		t.Fatalf("условие совпало: %v", err)
	}
	// Сдали новее — 409, решение не записано.
	second := w.submit(key, "analysis.brief")
	if err := decide(key, seen(first.ID.String())); !isChanged(err) || decided(key) != 1 {
		t.Fatalf("условие не совпало: %v, решений %d", err, decided(key))
	}
	if err := decide(key, seen(strings.ToUpper(second.ID.String()))); err != nil || decided(key) != 2 {
		t.Fatalf("условие по новой сдаче: %v", err)
	}
	// Условия нет — как раньше.
	w.submit(key, "analysis.brief")
	if err := decide(key, nil); err != nil || decided(key) != 3 {
		t.Fatalf("без условия: %v", err)
	}
}

// «Не пройден» по ручному чек-листу закрывает гейт failed: предпросмотр
// (его же показывают проверка гейтов CLI и карточка) и выборка «Ждут меня»
// видят отказ, переход вперёд закрыт. Повторная отметка пунктов возвращает
// обычное правило: все обязательные — passed, не все — pending.
func TestChecklistFailedDecision(t *testing.T) {
	w := newApprovalsWorld(t)
	dev := w.task("nova", "Разработка", "start")
	w.submit(dev, "analysis.brief")
	w.decide(dev, "analysis_done", "human_approval", w.human, domain.GateResultPassed, `{}`)
	w.move(dev, "analysis_done")
	w.submit(dev, "dev.summary")

	gate := func() GateOutcome {
		t.Helper()
		task, err := w.store.GetTaskByKey(w.ctx, w.store.DB(), dev)
		if err != nil {
			t.Fatal(err)
		}
		previews, err := w.engine.Preview(w.ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range previews {
			if p.Key != "dev_done" {
				continue
			}
			for _, g := range p.Gates {
				if g.GateKey == "selfhosted" {
					return g
				}
			}
		}
		t.Fatal("нет гейта selfhosted в предпросмотре dev_done")
		return GateOutcome{}
	}
	row := dev + " dev_done/selfhosted"

	if g := gate(); g.Status != domain.GateResultPending || !contains(w.list(), row) {
		t.Fatalf("до решения: %+v, выборка %v", g, w.list())
	}

	w.decide(dev, "dev_done", "selfhosted", w.human, domain.GateResultFailed, `{"reason":"нет раздела про апгрейд"}`)
	g := gate()
	if g.Status != domain.GateResultFailed || !strings.Contains(g.Reason, "нет раздела про апгрейд") || g.Details["decision"] == nil {
		t.Fatalf("после «Не пройден»: %+v", g)
	}
	if contains(w.list(), row) {
		t.Fatalf("отклонённый чек-лист остался в выборке: %v", w.list())
	}
	if res, err := w.engine.Execute(w.ctx, dev, "dev_done", w.system.ID, ""); err != nil || !res.Blocked {
		t.Fatalf("переход при отклонённом чек-листе: %v %+v", err, res)
	}
	// Заблокированный переход решение человека не перезаписал.
	if g := gate(); g.Status != domain.GateResultFailed {
		t.Fatalf("после попытки перехода: %+v", g)
	}

	// Отметка без обязательного пункта — обычное pending, строка вернулась.
	w.decide(dev, "dev_done", "selfhosted", w.human, domain.GateResultPassed, `{"checked":[]}`)
	if g := gate(); g.Status != domain.GateResultPending || !contains(w.list(), row) {
		t.Fatalf("неполная отметка: %+v, выборка %v", g, w.list())
	}

	// Без причины — общая формулировка.
	w.decide(dev, "dev_done", "selfhosted", w.human, domain.GateResultFailed, `{}`)
	if g := gate(); g.Status != domain.GateResultFailed || g.Reason != "чек-лист отмечен непройденным" {
		t.Fatalf("без причины: %+v", g)
	}

	// Все обязательные отмечены — passed, переход проходит.
	w.decide(dev, "dev_done", "selfhosted", w.human, domain.GateResultPassed, `{"checked":["selfhosted"]}`)
	if g := gate(); g.Status != domain.GateResultPassed || contains(w.list(), row) {
		t.Fatalf("повторная отметка: %+v, выборка %v", g, w.list())
	}
	w.move(dev, "dev_done")
}
