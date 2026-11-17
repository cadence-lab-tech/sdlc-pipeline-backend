package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Перенос задачи к другому родителю и вложенность в один уровень (CLAB-196)
// на движке: демонстрационный проект с двумя типами задач на разных циклах
// (task и epic), ключи и статусы — его, а не нашей настройки.

type reparentWorld struct {
	t        *testing.T
	ctx      context.Context
	store    *postgres.Store
	engine   *Engine
	system   domain.Principal
	employee Actor
}

func newReparentWorld(t *testing.T) reparentWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	lead, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	return reparentWorld{t: t, ctx: ctx, store: store, engine: engine, system: system,
		employee: Actor{Principal: lead, Level: domain.MemberAgent}}
}

func (w reparentWorld) create(title, parent, typ string, repos ...string) domain.Task {
	w.t.Helper()
	task, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: title, ParentKey: parent, TaskType: typ, Repositories: repos},
	})
	if err != nil {
		w.t.Fatalf("%s: %v", title, err)
	}
	return task
}

func (w reparentWorld) get(key string) domain.Task {
	w.t.Helper()
	task, err := w.store.GetTaskByKey(w.ctx, w.store.DB(), key)
	if err != nil {
		w.t.Fatal(err)
	}
	return task
}

func (w reparentWorld) reparent(key, parent string) ReparentResult {
	w.t.Helper()
	res, err := w.engine.ReparentTask(w.ctx, key, parent, w.employee)
	if err != nil {
		w.t.Fatalf("перенос %s под %q: %v", key, parent, err)
	}
	return res
}

func (w reparentWorld) count(sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.store.DB().QueryRow(w.ctx, sql, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w reparentWorld) reparented(key string) []map[string]any {
	w.t.Helper()
	rows, err := w.store.DB().Query(w.ctx, `
		SELECT e.payload FROM task_events e JOIN tasks t ON t.id = e.task_id
		WHERE t.key = $1 AND e.type = $2 ORDER BY e.created_at, e.id`, key, domain.EventTaskReparented)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			w.t.Fatal(err)
		}
		m := map[string]any{}
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return out
}

// ranks — ранги группы родителя ("" — верхний уровень) по ключам.
func (w reparentWorld) ranks(parent string) map[string]string {
	w.t.Helper()
	project := w.get("SDLC-1").ProjectID
	var pid *domain.Task
	if parent != "" {
		p := w.get(parent)
		pid = &p
	}
	var group []postgres.RankEntry
	var err error
	if pid == nil {
		group, err = w.store.RankGroup(w.ctx, w.store.DB(), project, nil)
	} else {
		group, err = w.store.RankGroup(w.ctx, w.store.DB(), project, &pid.ID)
	}
	if err != nil {
		w.t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range group {
		out[e.Key] = e.Rank
	}
	return out
}

func (w reparentWorld) last(parent string) string {
	w.t.Helper()
	best, key := "", ""
	for k, r := range w.ranks(parent) {
		if r > best {
			best, key = r, k
		}
	}
	return key
}

// AC-1, AC-8, AC-9: перенос задачи с живой арендой, связью, областью,
// назначением, выбором этапов и уровнями — меняются только родитель и ранг;
// задача последняя в группе нового родителя, ранги прочих не тронуты; событие
// в лентах задачи, прежнего и нового родителя; повтор — без изменений.
func TestReparentKeepsTaskAndMovesRank(t *testing.T) {
	w := newReparentWorld(t)
	// SDLC-1 — ребёнок SDLC-4, blocks SDLC-2, в области backend.
	if _, err := w.engine.Execute(w.ctx, "SDLC-1", "start", w.system.ID, ""); err != nil {
		t.Fatal(err)
	}
	bot, err := w.store.ResolveActor(w.ctx, w.store.DB(), "runner-bot", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.Claim(w.ctx, "SDLC-1", bot.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.Assign(w.ctx, "SDLC-1", "runner-bot", w.employee); err != nil {
		t.Fatal(err)
	}
	level := "high"
	if _, err := w.engine.UpdateTask(w.ctx, "SDLC-1", w.system.ID, UpdateTaskInput{ModelLevel: &level}); err != nil {
		t.Fatal(err)
	}
	testdb.Artifact(t, w.store, w.get("SDLC-1"), "разбор")

	target := w.create("Новый родитель", "", "")
	sibling := w.create("Уже в группе", target.Key, "")
	before := w.get("SDLC-1")
	run0, err := w.store.CurrentStageRun(w.ctx, w.store.DB(), before.ID, before.StatusID)
	if err != nil {
		t.Fatal(err)
	}
	links0, _ := w.store.ListTaskLinks(w.ctx, w.store.DB(), before.ID)
	arts0, _ := w.store.ListArtifacts(w.ctx, w.store.DB(), before.ID, true)
	oldGroup, newGroup := w.ranks("SDLC-4"), w.ranks(target.Key)

	res := w.reparent("SDLC-1", target.Key)
	if !res.Changed || res.From == nil || *res.From != "SDLC-4" || res.To == nil || *res.To != target.Key {
		t.Fatalf("ответ: %+v", res)
	}
	after := w.get("SDLC-1")
	if after.ParentKey == nil || *after.ParentKey != target.Key || res.Task.ParentKey == nil || *res.Task.ParentKey != target.Key {
		t.Fatalf("родитель: %v / %v", after.ParentKey, res.Task.ParentKey)
	}
	if after.Key != before.Key || after.StatusKey != before.StatusKey || after.WorkflowID != before.WorkflowID ||
		after.Type() != before.Type() || len(after.Repositories) != len(before.Repositories) ||
		after.Repositories[0].Name != before.Repositories[0].Name ||
		after.AssigneeID == nil || *after.AssigneeID != bot.ID ||
		string(after.OptionalStages) != string(before.OptionalStages) ||
		string(after.StageSettings) != string(before.StageSettings) ||
		after.ModelLevel == nil || *after.ModelLevel != "high" {
		t.Fatalf("задача изменилась:\nдо    %+v\nпосле %+v", before, after)
	}
	run1, err := w.store.CurrentStageRun(w.ctx, w.store.DB(), after.ID, after.StatusID)
	if err != nil || run1.ID != run0.ID || run1.ClaimedBy == nil || *run1.ClaimedBy != bot.ID ||
		run1.LeaseExpires == nil || !run1.LeaseExpires.Equal(*run0.LeaseExpires) {
		t.Fatalf("прогон и аренда: %+v / %+v %v", run0, run1, err)
	}
	links1, _ := w.store.ListTaskLinks(w.ctx, w.store.DB(), after.ID)
	arts1, _ := w.store.ListArtifacts(w.ctx, w.store.DB(), after.ID, true)
	if len(links1) != len(links0) || len(links0) == 0 || len(arts1) != len(arts0) || len(arts0) == 0 {
		t.Fatalf("связи %d→%d, артефакты %d→%d", len(links0), len(links1), len(arts0), len(arts1))
	}

	// AC-8: последняя в новой группе, прочие ранги обеих групп прежние.
	if got := w.last(target.Key); got != "SDLC-1" {
		t.Fatalf("последняя в группе %s: %s", target.Key, got)
	}
	for k, r := range w.ranks(target.Key) {
		if k != "SDLC-1" && newGroup[k] != r {
			t.Fatalf("ранг %s в новой группе: %s → %s", k, newGroup[k], r)
		}
	}
	for k, r := range w.ranks("SDLC-4") {
		if oldGroup[k] != r {
			t.Fatalf("ранг %s в прежней группе: %s → %s", k, oldGroup[k], r)
		}
	}
	if _, ok := w.ranks("SDLC-4")["SDLC-1"]; ok {
		t.Fatal("задача осталась в прежней группе")
	}
	_ = sibling

	// AC-9: одно событие в каждой из трёх лент, нагрузка {task, from, to}.
	for _, key := range []string{"SDLC-1", "SDLC-4", target.Key} {
		ev := w.reparented(key)
		if len(ev) != 1 || ev[0]["task"] != "SDLC-1" || ev[0]["from"] != "SDLC-4" || ev[0]["to"] != target.Key || len(ev[0]) != 3 {
			t.Fatalf("лента %s: %v", key, ev)
		}
	}

	// Повтор с тем же родителем — без события и без смены ранга.
	rank := w.get("SDLC-1").Rank
	again := w.reparent("SDLC-1", target.Key)
	if again.Changed || *again.To != target.Key || w.get("SDLC-1").Rank != rank || len(w.reparented("SDLC-1")) != 1 {
		t.Fatalf("повтор: %+v", again)
	}
}

// AC-2, AC-8, AC-9: снятие родителя — задача на верхнем уровне последней,
// событие с to: null в лентах задачи и прежнего родителя; повтор снятия — без
// события.
func TestReparentDetach(t *testing.T) {
	w := newReparentWorld(t)
	res := w.reparent("SDLC-2", "")
	if !res.Changed || res.To != nil || res.From == nil || *res.From != "SDLC-4" || res.Task.ParentKey != nil {
		t.Fatalf("снятие: %+v", res)
	}
	if got := w.last(""); got != "SDLC-2" {
		t.Fatalf("последняя на верхнем уровне: %s", got)
	}
	for _, key := range []string{"SDLC-2", "SDLC-4"} {
		ev := w.reparented(key)
		if len(ev) != 1 || ev[0]["from"] != "SDLC-4" || ev[0]["to"] != nil {
			t.Fatalf("лента %s: %v", key, ev)
		}
		if _, ok := ev[0]["to"]; !ok {
			t.Fatalf("лента %s: нет ключа to", key)
		}
	}
	again := w.reparent("SDLC-2", "")
	if again.Changed || len(w.reparented("SDLC-2")) != 1 {
		t.Fatalf("повтор снятия: %+v", again)
	}
}

// AC-3, AC-4, AC-5, AC-19: отказы ничего не меняют — ни родителя, ни рангов,
// ни ленты.
func TestReparentRefusals(t *testing.T) {
	w := newReparentWorld(t)
	top := w.create("Верхний", "", "")
	kid := w.create("Ребёнок верхнего", top.Key, "")
	foreign, _ := testdb.ForeignTask(t, w.store)
	cancelled := w.create("Снятый ребёнок", "", "")
	holder := w.create("Родитель со снятым ребёнком", "", "")
	w.reparent(cancelled.Key, holder.Key)
	if _, err := w.engine.Execute(w.ctx, cancelled.Key, "cancel", w.system.ID, "не нужен"); err != nil {
		t.Fatal(err)
	}

	createErr := func(parent string) error {
		_, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
			CreateTaskInput: postgres.CreateTaskInput{Title: "проба", ParentKey: parent},
		})
		return err
	}
	snapshot := func() string {
		var b strings.Builder
		rows, err := w.store.DB().Query(w.ctx, `SELECT key, coalesce(parent_id::text, ''), rank FROM tasks ORDER BY key`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var k, p, r string
			_ = rows.Scan(&k, &p, &r)
			b.WriteString(k + p + r + ";")
		}
		return b.String() + fmt.Sprint(w.count(`SELECT count(*) FROM task_events WHERE type = $1`, domain.EventTaskReparented))
	}

	for _, c := range []struct {
		name, task, parent string
		want               apperr.Code
		text               []string
	}{
		{"сама себе", top.Key, top.Key, apperr.CodeInvalidInput, []string{"самой себя"}},
		{"собственный ребёнок", top.Key, kid.Key, apperr.CodeInvalidInput, []string{kid.Key, top.Key, "один уровень"}},
		{"родитель вложен", "SDLC-1", kid.Key, apperr.CodeInvalidInput, []string{kid.Key, top.Key, "один уровень"}},
		{"у задачи есть дети", top.Key, "SDLC-4", apperr.CodeInvalidInput, []string{top.Key, "(1)", "один уровень"}},
		{"дети снятые", holder.Key, top.Key, apperr.CodeInvalidInput, []string{holder.Key, "(1)"}},
		{"эпик с детьми под задачу", "SDLC-4", top.Key, apperr.CodeInvalidInput, []string{"SDLC-4", "(3)"}},
		{"нет такой задачи", "SDLC-1", "SDLC-999", apperr.CodeNotFound, nil},
		{"не ключ", "SDLC-1", "нет", apperr.CodeInvalidInput, nil},
		{"другой проект", "SDLC-1", foreign.Key, apperr.CodeInvalidInput, []string{"другого проекта"}},
	} {
		before := snapshot()
		_, err := w.engine.ReparentTask(w.ctx, c.task, c.parent, w.employee)
		if codeOf(err) != c.want {
			t.Errorf("%s: %v, ждали %s", c.name, err, c.want)
			continue
		}
		for _, s := range c.text {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("%s: в тексте %q нет %q", c.name, err, s)
			}
		}
		if snapshot() != before {
			t.Errorf("%s: отказ изменил данные", c.name)
		}
	}
	// AC-5: те же код и текст, что у заведения с таким parent_key.
	for _, parent := range []string{"SDLC-999", foreign.Key, "нет"} {
		_, rerr := w.engine.ReparentTask(w.ctx, "SDLC-1", parent, w.employee)
		cerr := createErr(parent)
		if codeOf(rerr) != codeOf(cerr) || apperr.As(rerr).Message != apperr.As(cerr).Message {
			t.Errorf("%s: перенос %v / заведение %v", parent, rerr, cerr)
		}
	}
	if _, err := w.engine.ReparentTask(w.ctx, "SDLC-999", top.Key, w.employee); codeOf(err) != apperr.CodeNotFound {
		t.Errorf("задачи нет: %v", err)
	}
}

// AC-6, AC-7, AC-16: тип родителя не проверяется (под задачу любого типа, в
// т. ч. того же), область — тоже; заведение у вложенной задачи — 400 с её
// ключом и ключом её родителя, задача не заводится.
func TestReparentTypeScopeAndCreate(t *testing.T) {
	w := newReparentWorld(t)
	plain := w.create("Обычный родитель", "", "", "frontend")
	epic := w.create("Эпик-родитель", "", "epic")

	// Ребёнок того же типа и эпик под обычную задачу; задача с backend под
	// родителя с frontend.
	a := w.create("Под обычную", "", "", "backend")
	w.reparent(a.Key, plain.Key)
	nested := w.create("Эпик без детей", "", "epic")
	w.reparent(nested.Key, plain.Key)
	b := w.create("Под эпик", "", "")
	w.reparent(b.Key, epic.Key)
	if got := w.get(a.Key); len(got.Repositories) != 1 || got.Repositories[0].Name != "sdlc-pipeline-backend" ||
		len(w.get(plain.Key).Repositories) != 1 {
		t.Fatalf("область изменилась: %+v / %+v", got.Repositories, w.get(plain.Key).Repositories)
	}
	// Заведение под задачу верхнего уровня любого типа — как прежде.
	w.create("Ребёнок обычной", plain.Key, "epic")
	w.create("Ребёнок эпика", epic.Key, "")

	// Под вложенную — отказ для всех классов, кто вправе заводить детей.
	tasks := w.count(`SELECT count(*) FROM tasks`)
	bot, err := w.store.ResolveActor(w.ctx, w.store.DB(), "breaker-bot", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.Execute(w.ctx, b.Key, "start", w.system.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.Claim(w.ctx, b.Key, bot.ID, "x"); err != nil {
		t.Fatal(err)
	}
	bTask := w.get(b.Key)
	for name, in := range map[string]CreateTaskInput{
		"сотрудник":      {Level: domain.MemberAgent, Kind: domain.PrincipalUser},
		"сопровождающий": {Level: domain.MemberMaintainer, Kind: domain.PrincipalUser},
		"бот по аренде":  {Level: domain.MemberAgent, Kind: domain.PrincipalAgent},
		"токен задачи":   {Level: domain.MemberMaintainer, Kind: domain.PrincipalUser, Bound: &TaskBinding{TaskID: bTask.ID, TaskKey: bTask.Key}},
		"внутренний":     {},
	} {
		in.CreateTaskInput = postgres.CreateTaskInput{Title: "Внук", ParentKey: b.Key}
		actor := w.employee.Principal.ID
		if in.Kind == domain.PrincipalAgent {
			actor = bot.ID
		}
		_, err := w.engine.CreateTask(w.ctx, "SDLC", actor, in)
		if codeOf(err) != apperr.CodeInvalidInput || !strings.Contains(err.Error(), b.Key+" сама вложена в "+epic.Key) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := w.count(`SELECT count(*) FROM tasks`); n != tasks {
		t.Fatalf("отказ завёл задачу: %d → %d", tasks, n)
	}
}

// AC-10, AC-12: право по классам актора — прогноз actor_rights.reparent и
// исход совпадают; отказы ничего не меняют.
func TestReparentRights(t *testing.T) {
	w := newReparentWorld(t)
	target := w.create("Цель", "", "")
	if _, err := w.engine.Execute(w.ctx, "SDLC-4", "start", w.system.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.Execute(w.ctx, "SDLC-1", "start", w.system.ID, ""); err != nil {
		t.Fatal(err)
	}
	bot, err := w.store.ResolveActor(w.ctx, w.store.DB(), "lease-bot", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.Claim(w.ctx, "SDLC-1", bot.ID, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.Claim(w.ctx, "SDLC-4", bot.ID, "x"); err != nil {
		t.Fatal(err)
	}
	own, epic := w.get("SDLC-1"), w.get("SDLC-4")
	user := w.employee.Principal
	viewer := Actor{Principal: user, Level: domain.MemberViewer}
	maintainer := Actor{Principal: user, Level: domain.MemberMaintainer}
	botActor := Actor{Principal: bot, Level: domain.MemberAgent}
	token := func(task domain.Task) Actor {
		return Actor{Principal: user, Level: domain.MemberAdmin, Bound: &TaskBinding{TaskID: task.ID, TaskKey: task.Key}}
	}

	for _, c := range []struct {
		name   string
		a      Actor
		key    string
		reason string
	}{
		{"токен своей задачи", token(own), "SDLC-1", DenyHumanAction},
		{"токен родителя о ребёнке", token(epic), "SDLC-1", DenyHumanAction},
		{"токен чужой задачи", token(epic), "SDLC-2", DenyHumanAction},
		{"бот с арендой задачи", botActor, "SDLC-1", DenyHumanAction},
		{"бот с арендой родителя", botActor, "SDLC-2", DenyHumanAction},
		{"viewer", viewer, "SDLC-1", DenyLevel},
	} {
		task := w.get(c.key)
		rights, err := w.engine.TaskRights(w.ctx, c.key, c.a)
		if err != nil {
			t.Fatal(err)
		}
		_, rerr := w.engine.ReparentTask(w.ctx, c.key, target.Key, c.a)
		ae := apperr.As(rerr)
		details, _ := ae.Details.(map[string]any)
		if rights.Reparent || rights.ActorDeny["reparent"] == nil || rerr == nil || ae.Code != apperr.CodeForbidden ||
			details["reason"] != c.reason || rights.ActorDeny["reparent"].Reason != c.reason {
			t.Errorf("%s: прогноз %v %+v, исход %v %v", c.name, rights.Reparent, rights.ActorDeny["reparent"], rerr, details)
		}
		if got := w.get(c.key); (got.ParentID == nil) != (task.ParentID == nil) || len(w.reparented(c.key)) != 0 {
			t.Errorf("%s: отказ изменил задачу", c.name)
		}
	}
	for name, a := range map[string]Actor{"сотрудник": w.employee, "сопровождающий": maintainer} {
		rights, err := w.engine.TaskRights(w.ctx, "SDLC-3", a)
		if err != nil || !rights.Reparent || rights.ActorDeny["reparent"] != nil {
			t.Fatalf("%s: прогноз %+v %v", name, rights, err)
		}
	}
	w.reparent("SDLC-3", target.Key)
	if _, err := w.engine.ReparentTask(w.ctx, "SDLC-3", "", maintainer); err != nil {
		t.Fatalf("сопровождающий: %v", err)
	}
	if !strings.Contains(TaskWorkOf(w.employee).Summary, "смена родителя") {
		t.Fatalf("сводка: %q", TaskWorkOf(w.employee).Summary)
	}
}

// AC-15: встречные переносы и перенос против заведения ребёнка в
// параллельных транзакциях — проходит одно, второе 400; после всех попыток в
// задачах нет ни цикла, ни задачи с родителем и детьми одновременно.
func TestReparentConcurrent(t *testing.T) {
	w := newReparentWorld(t)
	for i := range 6 {
		a, b := w.create("A", "", ""), w.create("B", "", "")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); _, errs[0] = w.engine.ReparentTask(w.ctx, a.Key, b.Key, w.employee) }()
		go func() { defer wg.Done(); _, errs[1] = w.engine.ReparentTask(w.ctx, b.Key, a.Key, w.employee) }()
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("встречные %d: %v / %v", i, errs[0], errs[1])
		}
		for _, err := range errs {
			if err != nil && codeOf(err) != apperr.CodeInvalidInput {
				t.Fatalf("встречные %d: %v", i, err)
			}
		}

		c, d := w.create("C", "", ""), w.create("D", "", "")
		wg.Add(2)
		go func() { defer wg.Done(); _, errs[0] = w.engine.ReparentTask(w.ctx, c.Key, d.Key, w.employee) }()
		go func() {
			defer wg.Done()
			_, errs[1] = w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
				CreateTaskInput: postgres.CreateTaskInput{Title: "Ребёнок C", ParentKey: c.Key},
			})
		}()
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("перенос против заведения %d: %v / %v", i, errs[0], errs[1])
		}
		for _, err := range errs {
			if err != nil && codeOf(err) != apperr.CodeInvalidInput {
				t.Fatalf("перенос против заведения %d: %v", i, err)
			}
		}
	}
	// Вне старых цепочек сида: ни у одной задачи нет и родителя, и детей.
	if n := w.count(`SELECT count(*) FROM tasks t WHERE t.parent_id IS NOT NULL
		AND EXISTS (SELECT 1 FROM tasks c WHERE c.parent_id = t.id)`); n != 0 {
		t.Fatalf("задач с родителем и детьми: %d", n)
	}
}

// AC-17: цепочка A→B→C, оставшаяся от прежних версий (прямая запись), —
// читается, гейт зависимостей A видит C; ребёнка у B и C не завести, B под
// другого родителя не перенести; снять родителя с B или C и положить C под A
// можно.
func TestReparentOldChain(t *testing.T) {
	w := newReparentWorld(t)
	a, b, c := w.create("A", "", "epic"), w.create("B", "", ""), w.create("C", "", "")
	other := w.create("Другой", "", "")
	if _, err := w.store.DB().Exec(w.ctx, `UPDATE tasks SET parent_id = $2 WHERE id = $1`, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.DB().Exec(w.ctx, `UPDATE tasks SET parent_id = $2 WHERE id = $1`, c.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.get(c.Key); got.ParentKey == nil || *got.ParentKey != b.Key {
		t.Fatalf("цепочка не читается: %+v", got.ParentKey)
	}
	deps, err := w.store.TaskDependencies(w.ctx, w.store.DB(), a.ID, true, false)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, d := range deps {
		seen[d.Key] = true
	}
	if !seen[b.Key] || !seen[c.Key] {
		t.Fatalf("зависимости A: %+v", deps)
	}
	for _, parent := range []string{b.Key, c.Key} {
		_, err := w.engine.CreateTask(w.ctx, "SDLC", w.system.ID, CreateTaskInput{
			CreateTaskInput: postgres.CreateTaskInput{Title: "Ещё", ParentKey: parent},
		})
		if codeOf(err) != apperr.CodeInvalidInput {
			t.Fatalf("ребёнок у %s: %v", parent, err)
		}
	}
	if _, err := w.engine.ReparentTask(w.ctx, b.Key, other.Key, w.employee); codeOf(err) != apperr.CodeInvalidInput {
		t.Fatalf("B под другого: %v", err)
	}
	w.reparent(c.Key, a.Key)
	w.reparent(c.Key, "")
	w.reparent(b.Key, "")
	if w.get(b.Key).ParentID != nil || w.get(c.Key).ParentID != nil {
		t.Fatal("снятие не прошло")
	}
}
