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
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Исполнитель этапа (CLAB-208, решение 0049 п. 3–4): белый список токена
// задачи, круг записи, гейты и переходы. Права людей — те же проверки, что
// до задачи: их держат существующие тесты прав без правки ожиданий.

// refused — отказ токену задачи в действии человека: 403 и общий текст.
func refused(t *testing.T, err error, what string) {
	t.Helper()
	if code(err) != apperr.CodeForbidden || !strings.Contains(err.Error(), ExecutorRefusal) {
		t.Fatalf("%s: ожидали отказ «%s», а %v", what, ExecutorRefusal, err)
	}
}

func taskIdentity(principal domain.Principal, admin bool) Identity {
	run, task, key := uuid.New(), uuid.New(), "SDLC-1"
	principal.IsAdmin = admin
	return Identity{Principal: principal, Token: &domain.AccessToken{
		TokenKind: domain.TokenTask, StageRunID: &run, TaskID: &task, TaskKey: &key,
	}}
}

// Критерии 4, 5, 7 (без базы): перечень — отказ по умолчанию, до уровня и
// is_admin; личный токен и режим заголовка перечня не видят.
func TestExecutorMay(t *testing.T) {
	log := testdb.Logger()
	tokens, header := NewAuth(nil, log, AuthToken), NewAuth(nil, log, AuthHeader)
	user := domain.Principal{ID: uuid.New(), Kind: domain.PrincipalUser, Handle: "alice"}
	task, adm := taskIdentity(user, false), taskIdentity(user, true)
	personal := Identity{Principal: user, Token: &domain.AccessToken{TokenKind: domain.TokenAPI}}

	for _, action := range []string{
		"sdlc_put_role", "sdlc_put_document", "sdlc_add_repository", "sdlc_import_bundle",
		"sdlc_export_bundle", "sdlc_import_guide", "sdlc_update_task_scope",
		PromptAction("import_rules"), "sdlc_future_tool",
		RESTAction("PATCH", "/v1/tasks/:taskKey/scope"),
		RESTAction("GET", "/v1/auth/tokens"), RESTAction("DELETE", "/v1/auth/tokens/:tokenID"),
		RESTAction("POST", "/v1/auth/cli/:code"), RESTAction("POST", "/v1/tasks/:taskKey/task-token"),
		RESTAction("GET", "/v1/projects/:projectKey/workflow"), RESTAction("GET", "/v1/projects/:projectKey/index"),
		RESTAction("GET", "/v1/projects/:projectKey/members"),
	} {
		refused(t, tokens.ExecutorMay(task, action), action)
		refused(t, tokens.ExecutorMay(adm, action), action+" (is_admin)")
		if err := tokens.ExecutorMay(personal, action); err != nil {
			t.Fatalf("личный токен: %s: %v", action, err)
		}
		if err := header.ExecutorMay(task, action); err != nil {
			t.Fatalf("режим заголовка: %s: %v", action, err)
		}
	}
	for _, action := range ExecutorActions() {
		if err := tokens.ExecutorMay(task, action); err != nil {
			t.Fatalf("из перечня: %s: %v", action, err)
		}
	}
	err := tokens.ExecutorMay(task, "sdlc_put_role")
	if !strings.HasPrefix(apperr.As(err).Message, ExecutorRefusal+": sdlc_put_role") {
		t.Fatalf("текст отказа: %q", apperr.As(err).Message)
	}
}

// executorFixture — гейты и переходы, которых нет в демо-цикле: чек-лист на
// analysis_done и на глобальном cancel, переход вида reassign и возврат из
// analysis.
func executorFixture(t *testing.T, store *postgres.Store) {
	t.Helper()
	if _, err := store.DB().Exec(context.Background(), `
		WITH wf AS (SELECT workflow_id AS id FROM tasks WHERE key = 'SDLC-1'),
		     st AS (SELECT from_status_id AS ana FROM workflow_transitions
		            WHERE workflow_id = (SELECT id FROM wf) AND key = 'analysis_done'),
		     nw AS (SELECT from_status_id AS new FROM workflow_transitions
		            WHERE workflow_id = (SELECT id FROM wf) AND key = 'start'),
		     g AS (
		       INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
		       SELECT id, 'selfhosted', 'Self-hosted учтён', 'checklist',
		              '{"items":[{"id":"selfhosted","text":"учтён","required":true}]}', true, 9
		       FROM workflow_transitions
		       WHERE workflow_id = (SELECT id FROM wf) AND key IN ('analysis_done', 'cancel'))
		INSERT INTO workflow_transitions (workflow_id, key, name, from_status_id, to_status_id, kind, position)
		SELECT wf.id, 'handoff', 'Передать', st.ana, nw.new, 'reassign', 50 FROM wf, st, nw
		UNION ALL
		SELECT wf.id, 'back', 'Вернуть', st.ana, nw.new, 'reject', 51 FROM wf, st, nw`); err != nil {
		t.Fatal(err)
	}
}

// Критерии 11–14: чек-лист своего этапа — да; остальные решения, апрув
// человека, cancel и reassign — отказ; advance и reject решают гейты.
func TestExecutorGatesAndTransitions(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthToken)
	executorFixture(t, w.store)
	w.claim(t, w.pool, "SDLC-1")
	w.claim(t, w.admin, "SDLC-3")
	_, tid := w.issue(t, w.pool, "SDLC-1")
	_, adm := w.issue(t, w.admin, "SDLC-3")
	bound := w.auth.Bound(tid)

	decide := func(task, transition, gate, status string, b *TaskBinding, actor uuid.UUID) error {
		_, err := w.engine.DecideGate(ctx, task, transition, gate, actor, b, status, json.RawMessage(`{"checked":["selfhosted"]}`))
		return err
	}

	// Пройденный чек-лист своего этапа — принципалом уровня agent.
	if err := decide("SDLC-1", "analysis_done", "selfhosted", domain.GateResultPassed, bound, w.pool.Principal.ID); err != nil {
		t.Fatalf("чек-лист своего этапа: %v", err)
	}
	task, _ := w.store.GetTaskByKey(ctx, w.store.DB(), "SDLC-1")
	run, _ := w.store.CurrentStageRun(ctx, w.store.DB(), task.ID, task.StatusID)
	results, err := w.store.ListGateResults(ctx, w.store.DB(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range results {
		if r.Status == domain.GateResultPassed && r.DecidedBy != nil && *r.DecidedBy == w.pool.Principal.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("решения в gate_results нет: %+v", results)
	}
	before := len(results)

	for _, c := range []struct{ task, transition, gate, status, what string }{
		{"SDLC-1", "analysis_done", "selfhosted", domain.GateResultFailed, "failed"},
		{"SDLC-1", "analysis_done", "selfhosted", domain.GateResultWaived, "waived"},
		{"SDLC-1", "analysis_done", "artifacts", domain.GateResultPassed, "не чек-лист"},
		{"SDLC-1", "architecture_done", "traceability", domain.GateResultPassed, "переход не из текущего статуса"},
		{"SDLC-1", "cancel", "selfhosted", domain.GateResultPassed, "глобальный переход"},
		{"SDLC-1", "analysis_done", "human_approval", domain.GateResultPassed, "апрув человека"},
		{"SDLC-3", "analysis_done", "selfhosted", domain.GateResultPassed, "чужая задача"},
	} {
		refused(t, decide(c.task, c.transition, c.gate, c.status, bound, w.pool.Principal.ID), c.what)
	}
	if results, _ := w.store.ListGateResults(ctx, w.store.DB(), run.ID); len(results) != before {
		t.Fatalf("отказ записал решение: %d → %d", before, len(results))
	}

	// Апрув человека: токен задачи человека — отказ, его личный токен — да.
	refused(t, decide("SDLC-3", "analysis_done", "human_approval", domain.GateResultPassed,
		w.auth.Bound(adm), w.admin.Principal.ID), "апрув токеном задачи человека")
	if err := decide("SDLC-3", "analysis_done", "human_approval", domain.GateResultPassed,
		nil, w.admin.Principal.ID); err != nil {
		t.Fatalf("апрув человека личным токеном: %v", err)
	}

	// Переходы: cancel и reassign — отказ, задача на месте.
	for _, tr := range []string{"cancel", "handoff"} {
		_, err := w.engine.ExecuteBound(ctx, "SDLC-1", tr, w.pool.Principal.ID, bound, "")
		refused(t, err, tr)
	}
	if task, _ := w.store.GetTaskByKey(ctx, w.store.DB(), "SDLC-1"); task.StatusKey != "analysis" {
		t.Fatalf("отказ сдвинул задачу: %s", task.StatusKey)
	}
	// Чужая задача — отказ даже вперёд.
	_, err = w.engine.ExecuteBound(ctx, "SDLC-3", "analysis_done", w.pool.Principal.ID, bound, "")
	refused(t, err, "переход чужой задачи")
	// Вперёд — решают гейты: заблокировано, но не отказ.
	res, err := w.engine.ExecuteBound(ctx, "SDLC-1", "analysis_done", w.pool.Principal.ID, bound, "")
	if err != nil || !res.Blocked {
		t.Fatalf("advance токеном задачи: %+v %v", res, err)
	}
	// Возврат проходит.
	res, err = w.engine.ExecuteBound(ctx, "SDLC-1", "back", w.pool.Principal.ID, bound, "")
	if err != nil || res.Blocked || res.Task.StatusKey != "new" {
		t.Fatalf("reject токеном задачи: %+v %v", res, err)
	}
}

// Критерии 15–18: круг записи — своя задача и её дети; связи по решению
// человека; аренда соседней задачи принципалом круг не расширяет.
func TestExecutorWriteCircle(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthToken)
	create := func(title, parent string) domain.Task {
		t.Helper()
		task, err := w.engine.CreateTask(ctx, "SDLC", w.admin.Principal.ID, CreateTaskInput{
			CreateTaskInput: postgres.CreateTaskInput{Title: title, ParentKey: parent},
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	e := create("E", "")
	c := create("C", e.Key)
	d, d2 := create("D", c.Key), create("D2", c.Key)
	g := create("G", d.Key)
	z := create("Z", "")
	for _, key := range []string{c.Key, z.Key} {
		if _, err := w.engine.Execute(ctx, key, "start", w.admin.Principal.ID, ""); err != nil {
			t.Fatal(err)
		}
		w.claim(t, w.pool, key)
	}
	_, tid := w.issue(t, w.pool, c.Key)

	for _, key := range []string{c.Key, d.Key, d2.Key} {
		if err := w.auth.RequireWriteCircle(ctx, tid, key, "правка"); err != nil {
			t.Fatalf("%s в круге: %v", key, err)
		}
	}
	for _, key := range []string{e.Key, z.Key, g.Key} {
		refused(t, w.auth.RequireWriteCircle(ctx, tid, key, "правка"), key+" вне круга")
	}
	// Личный токен того же принципала (держит C и Z) — круга нет.
	for _, key := range []string{e.Key, z.Key, g.Key} {
		if err := w.auth.RequireWriteCircle(ctx, w.pool, key, "правка"); err != nil {
			t.Fatalf("личный токен %s: %v", key, err)
		}
	}

	for _, l := range []struct{ from, rel, to string }{
		{c.Key, domain.TaskLinkBlocks, d.Key},
		{d.Key, domain.TaskLinkBlocks, c.Key},
		{d.Key, domain.TaskLinkSplitFrom, d2.Key},
		{c.Key, domain.TaskLinkRelatesTo, e.Key},
		{d.Key, domain.TaskLinkSharesSurface, e.Key},
		{c.Key, domain.TaskLinkBlocks, e.Key},
		{e.Key, domain.TaskLinkDuplicates, c.Key},
	} {
		if err := w.auth.RequireLink(ctx, tid, l.from, l.to, l.rel); err != nil {
			t.Fatalf("%s %s %s: %v", l.from, l.rel, l.to, err)
		}
	}
	for _, l := range []struct{ from, rel, to string }{
		{e.Key, domain.TaskLinkBlocks, c.Key},
		{e.Key, domain.TaskLinkBlocks, d.Key},
		{c.Key, domain.TaskLinkSplitFrom, e.Key},
		{z.Key, domain.TaskLinkRelatesTo, e.Key},
		{g.Key, domain.TaskLinkBlocks, z.Key},
	} {
		refused(t, w.auth.RequireLink(ctx, tid, l.from, l.to, l.rel), l.from+" "+l.rel+" "+l.to)
	}
	// Второй конец — любая задача своего проекта (CLAB-305, AC-6): Z не
	// родня C, но связь «своя → Z» разрешена, а «Z blocks свою» — нет.
	for _, l := range []struct{ from, rel, to string }{
		{c.Key, domain.TaskLinkRelatesTo, z.Key},
		{d.Key, domain.TaskLinkSharesSurface, z.Key},
		{z.Key, domain.TaskLinkDuplicates, c.Key},
		{c.Key, domain.TaskLinkBlocks, z.Key},
	} {
		if err := w.auth.RequireLink(ctx, tid, l.from, l.to, l.rel); err != nil {
			t.Fatalf("%s %s %s: %v", l.from, l.rel, l.to, err)
		}
	}
	refused(t, w.auth.RequireLink(ctx, tid, z.Key, c.Key, domain.TaskLinkBlocks), z.Key+" blocks "+c.Key)
	// Задача другого проекта — отказ границы проекта, а не круга.
	foreign, _ := testdb.ForeignTask(t, w.store)
	if err := w.auth.RequireLink(ctx, tid, c.Key, foreign.Key, domain.TaskLinkRelatesTo); code(err) != apperr.CodeForbidden ||
		!strings.Contains(err.Error(), "другому проекту") {
		t.Fatalf("связь с задачей другого проекта: %v", err)
	}
	if err := w.auth.RequireLink(ctx, w.pool, z.Key, c.Key, domain.TaskLinkBlocks); err != nil {
		t.Fatalf("личный токен: %v", err)
	}
}

// Критерий 9: проекты токена задачи — только проект его задачи.
func TestExecutorVisibleProjects(t *testing.T) {
	ctx := context.Background()
	w := newTaskTokenWorld(t, AuthToken)
	if _, err := w.store.DB().Exec(ctx, `
		INSERT INTO projects (key, name, owner_team_id)
		SELECT 'ALT', 'Другой', owner_team_id FROM projects WHERE key = 'SDLC'`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.auth.SetMember(ctx, w.admin, "ALT", "agent:pool", domain.MemberAgent, nil); err != nil {
		t.Fatal(err)
	}
	w.claim(t, w.pool, "SDLC-1")
	_, tid := w.issue(t, w.pool, "SDLC-1")
	all, err := w.auth.VisibleProjects(ctx, w.pool)
	if err != nil || len(all) < 2 {
		t.Fatalf("личный токен видит %d проектов: %v", len(all), err)
	}
	own, err := w.auth.VisibleProjects(ctx, tid)
	if err != nil || len(own) != 1 || own[0].Key != "SDLC" {
		t.Fatalf("токен задачи видит: %+v %v", own, err)
	}
}
