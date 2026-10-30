package workflow

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Выбор необязательных этапов на задаче и проход движка мимо выключенных
// (CLAB-315, решение 0066, п. 7-16) на живой базе.

// skipCycle — цикл задач new → analysis → design (необязательный,
// выключен) → docs (необязательный, включён) → development → done.
// Обходные — design_done и docs_done; на design_done блокирующий
// human_approval, на docs_done — предел этапа и предел попыток входа в
// development; на analysis_done — свои пределы. Возвраты в design — reject и
// reassign. Ключи — данные цикла, движок их не знает.
func skipCycle() BundleWorkflow {
	yes, off := true, false
	gate := func(key, typ, config string) BundleGate {
		return BundleGate{Key: key, Name: key, Type: typ, Blocking: &yes, Config: json.RawMessage(config)}
	}
	return BundleWorkflow{
		Name: "Пропуск", TaskTypes: []string{"task"},
		Statuses: []BundleStatus{
			{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
			{Key: "analysis", Name: "Analysis", Category: "active", Role: "analyst"},
			{Key: "design", Name: "Design", Category: "active", Role: "architect", IsOptional: true, EnabledByDefault: &off},
			{Key: "docs", Name: "Docs", Category: "active", IsOptional: true},
			{Key: "development", Name: "Development", Category: "active", Role: "developer"},
			{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
			{Key: "cancelled", Name: "Cancelled", Category: "cancelled", IsTerminal: true},
		},
		Transitions: []BundleTransition{
			{Key: "start", Name: "Start", From: "new", To: "analysis", Kind: "advance"},
			{Key: "analysis_done", Name: "Analysis done", From: "analysis", To: "design", Kind: "advance",
				Gates: []BundleGate{
					gate("tries", domain.GateAttemptsLimit, `{"max_attempts": 1}`),
					gate("time", domain.GateStageTimeLimit, `{"max_duration": "1h"}`),
				}},
			{Key: "design_done", Name: "Design done", From: "design", To: "docs", Kind: "advance",
				Gates: []BundleGate{gate("approval", domain.GateHumanApproval, `{"min_approvals": 1}`)}},
			{Key: "docs_done", Name: "Docs done", From: "docs", To: "development", Kind: "advance",
				Gates: []BundleGate{
					gate("time", domain.GateStageTimeLimit, `{"max_duration": "1h"}`),
					gate("tries", domain.GateAttemptsLimit, `{"max_attempts": 1}`),
				}},
			{Key: "finish", Name: "Finish", From: "development", To: "done", Kind: "advance"},
			{Key: "back_to_design", Name: "Back to design", From: "development", To: "design", Kind: "reject"},
			{Key: "reassign_design", Name: "Reassign design", From: "development", To: "design", Kind: "reassign"},
			{Key: "back_to_analysis", Name: "Back to analysis", From: "development", To: "analysis", Kind: "reject"},
			{Key: "cancel", Name: "Cancel", To: "cancelled", Kind: "cancel"},
		},
	}
}

// choreCycle — другой тип задач со своим необязательным этапом в конце пути:
// обход polish ведёт прямо в терминальный.
func choreCycle() BundleWorkflow {
	off := false
	return BundleWorkflow{
		Name: "Мелочи", TaskTypes: []string{"chore"},
		Statuses: []BundleStatus{
			{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
			{Key: "work", Name: "Work", Category: "active", Role: "developer"},
			{Key: "polish", Name: "Polish", Category: "active", IsOptional: true, EnabledByDefault: &off},
			{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
		},
		Transitions: []BundleTransition{
			{Key: "start", Name: "Start", From: "new", To: "work", Kind: "advance"},
			{Key: "work_done", Name: "Work done", From: "work", To: "polish", Kind: "advance"},
			{Key: "polish_done", Name: "Polish done", From: "polish", To: "done", Kind: "advance"},
		},
	}
}

type skipWorld struct {
	attemptsWorld
	authoring    *Authoring
	agent, human domain.Principal
}

func activateCycles(t *testing.T, authoring *Authoring, actor uuid.UUID, cycles ...BundleWorkflow) {
	t.Helper()
	ctx := context.Background()
	report, err := authoring.Import(ctx, "SDLC", actor, Bundle{Version: BundleVersion, Workflows: cycles}, false)
	if err != nil {
		t.Fatalf("импорт циклов: %v", err)
	}
	for i, v := range report.Validations {
		if !v.Valid {
			t.Fatalf("цикл %s не прошёл проверку: %+v", cycles[i].Name, v)
		}
	}
	for _, e := range report.Workflows {
		if _, err := authoring.Activate(ctx, uuid.MustParse(e.ID), actor); err != nil {
			t.Fatalf("активация %s: %v", e.Key, err)
		}
	}
}

func newSkipWorld(t *testing.T) skipWorld {
	t.Helper()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	activateCycles(t, authoring, system.ID, skipCycle(), choreCycle())
	return skipWorld{
		attemptsWorld: attemptsWorld{store: store, engine: engine, system: system},
		authoring:     authoring,
		agent:         mustActor(t, store, "agent:runner"),
		human:         mustActor(t, store, "user:lead"),
	}
}

// create заводит задачу с выбором этапов и отдаёт её.
func (w skipWorld) create(t *testing.T, title, typ string, stages string) domain.Task {
	t.Helper()
	in := CreateTaskInput{CreateTaskInput: postgres.CreateTaskInput{Title: title, TaskType: typ}}
	if stages != "" {
		in.OptionalStages = json.RawMessage(stages)
	}
	task, err := w.engine.CreateTask(context.Background(), "SDLC", w.system.ID, in)
	if err != nil {
		t.Fatalf("заведение %q: %v", title, err)
	}
	return task
}

func (w skipWorld) edit(key string, stages string, extra UpdateTaskInput) (domain.Task, error) {
	raw := json.RawMessage(stages)
	extra.OptionalStages = &raw
	return w.engine.UpdateTask(context.Background(), key, w.system.ID, extra)
}

func choiceOf(t *testing.T, raw json.RawMessage) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("optional_stages не объект bool: %s", raw)
	}
	return out
}

func (w skipWorld) eventsOf(t *testing.T, taskID uuid.UUID, types ...string) []domain.TaskEvent {
	t.Helper()
	all, err := w.store.ListEvents(context.Background(), w.store.DB(), taskID, 500)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, typ := range types {
		want[typ] = true
	}
	// Лента отдаётся от новых к старым — здесь по порядку записи.
	var out []domain.TaskEvent
	for i := len(all) - 1; i >= 0; i-- {
		if want[all[i].Type] {
			out = append(out, all[i])
		}
	}
	return out
}

func payloadOf(t *testing.T, ev domain.TaskEvent) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(ev.Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (w skipWorld) runsOn(t *testing.T, taskID uuid.UUID, status string) []domain.StageRun {
	t.Helper()
	runs, err := w.store.ListStageRuns(context.Background(), w.store.DB(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.StageRun
	for _, r := range runs {
		if r.StatusKey == status && r.Kind == domain.StageKindStage {
			out = append(out, r)
		}
	}
	return out
}

func (w skipWorld) counter(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.store.DB().QueryRow(context.Background(),
		`SELECT task_counter FROM projects WHERE key = 'SDLC'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// AC-1: миграция аддитивна, up → down → up на базе с задачами; CHECK держит
// объект.
func TestTaskOptionalStagesMigration(t *testing.T) {
	ctx := context.Background()
	w := newSkipWorld(t)
	w.create(t, "До миграции", "", `{"design": true}`)
	db := w.store.DB()
	read := func(name string) string {
		body, err := migrations.Schema.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	count := func(where string) int {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE `+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	total := count("true")
	if _, err := db.Exec(ctx, read("000035_task_optional_stages.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if count("true") != total {
		t.Fatal("down потерял задачи")
	}
	if _, err := db.Exec(ctx, read("000035_task_optional_stages.up.sql")); err != nil {
		t.Fatalf("up на базе с задачами: %v", err)
	}
	if n := count(`optional_stages = '{}'::jsonb`); n != total {
		t.Fatalf("после up у всех задач {}: %d из %d", n, total)
	}
	if _, err := db.Exec(ctx, `UPDATE tasks SET optional_stages = '[]'`); err == nil {
		t.Fatal("CHECK пропустил массив")
	}
	if _, err := db.Exec(ctx, read("000035_task_optional_stages.down.sql")); err != nil {
		t.Fatalf("второй down: %v", err)
	}
	if _, err := db.Exec(ctx, read("000035_task_optional_stages.up.sql")); err != nil {
		t.Fatalf("второй up: %v", err)
	}
}

// AC-2, AC-3: заведение с выбором, умолчания, отказы без задачи и без сдвига
// счётчика.
func TestTaskOptionalStagesCreate(t *testing.T) {
	ctx := context.Background()
	w := newSkipWorld(t)

	task := w.create(t, "С дизайном", "", `{"design": true}`)
	if got := choiceOf(t, task.OptionalStages); !reflect.DeepEqual(got, map[string]bool{"design": true, "docs": true}) {
		t.Fatalf("полный набор при заведении: %v", got)
	}
	again, err := w.store.GetTaskByKey(ctx, w.store.DB(), task.Key)
	if err != nil || !reflect.DeepEqual(choiceOf(t, again.OptionalStages), map[string]bool{"design": true, "docs": true}) {
		t.Fatalf("набор в базе: %v %s", err, again.OptionalStages)
	}
	created := w.eventsOf(t, task.ID, domain.EventTaskCreated)
	if len(created) != 1 || !reflect.DeepEqual(payloadOf(t, created[0])["optional_stages"],
		map[string]any{"design": true, "docs": true}) {
		t.Fatalf("набор в task.created: %+v", created)
	}

	if got := choiceOf(t, w.create(t, "Без выбора", "", "").OptionalStages); !reflect.DeepEqual(got,
		map[string]bool{"design": false, "docs": true}) {
		t.Fatalf("умолчания цикла: %v", got)
	}
	if got := choiceOf(t, w.create(t, "Пустой выбор", "", `{}`).OptionalStages); !reflect.DeepEqual(got,
		map[string]bool{"design": false, "docs": true}) {
		t.Fatalf("пустой объект — умолчания: %v", got)
	}
	epic := w.create(t, "Эпик", "epic", "")
	if string(epic.OptionalStages) != "{}" {
		t.Fatalf("цикл без необязательных — {}: %s", epic.OptionalStages)
	}
	if ev := w.eventsOf(t, epic.ID, domain.EventTaskCreated); len(ev) != 1 || payloadOf(t, ev[0])["optional_stages"] != nil {
		t.Fatalf("пустой набор в task.created не пишется: %+v", ev)
	}

	before := w.counter(t)
	for name, c := range map[string]struct{ typ, stages string }{
		"обязательный статус":     {"", `{"development": false}`},
		"нет такого":              {"", `{"foo": true}`},
		"из цикла другого типа":   {"", `{"polish": true}`},
		"значение не bool":        {"", `{"design": "off"}`},
		"значение null":           {"", `{"design": null}`},
		"не объект":               {"", `["design"]`},
		"цикл без необязательных": {"epic", `{"design": false}`},
	} {
		in := CreateTaskInput{CreateTaskInput: postgres.CreateTaskInput{Title: name, TaskType: c.typ},
			OptionalStages: json.RawMessage(c.stages)}
		_, err := w.engine.CreateTask(ctx, "SDLC", w.system.ID, in)
		e := apperr.As(err)
		if e.Code != apperr.CodeInvalidInput {
			t.Errorf("%s: ожидался invalid_input, получили %v", name, err)
			continue
		}
		switch name {
		case "обязательный статус", "нет такого", "из цикла другого типа":
			details, _ := e.Details.(map[string]any)
			if !reflect.DeepEqual(details["allowed"], []string{"design", "docs"}) {
				t.Errorf("%s: details.allowed %v", name, e.Details)
			}
		case "цикл без необязательных":
			if !strings.Contains(e.Message, "у цикла задачи нет необязательных этапов") {
				t.Errorf("%s: %s", name, e.Message)
			}
		}
	}
	if w.counter(t) != before {
		t.Fatalf("отказ сдвинул счётчик ключей: %d → %d", before, w.counter(t))
	}
}

// AC-6, AC-7: правка — слияние, след, «не правка», отказы 409 целиком.
func TestTaskOptionalStagesUpdate(t *testing.T) {
	ctx := context.Background()
	w := newSkipWorld(t)
	task := w.create(t, "Правка", "", "")

	out, err := w.edit(task.Key, `{"design": true}`, UpdateTaskInput{})
	if err != nil || !reflect.DeepEqual(choiceOf(t, out.OptionalStages), map[string]bool{"design": true, "docs": true}) {
		t.Fatalf("слияние по ключам: %v %s", err, out.OptionalStages)
	}
	updated := w.eventsOf(t, task.ID, domain.EventTaskUpdated)
	if len(updated) != 1 {
		t.Fatalf("task.updated: %+v", updated)
	}
	p := payloadOf(t, updated[0])
	if !reflect.DeepEqual(p["fields"], []any{"optional_stages"}) || !reflect.DeepEqual(p["changed"],
		map[string]any{"optional_stages": map[string]any{
			"from": map[string]any{"design": false, "docs": true},
			"to":   map[string]any{"design": true, "docs": true}}}) {
		t.Fatalf("след правки: %v", p)
	}

	// Не правка: пустой объект и тот же набор — 200 без события.
	for _, stages := range []string{`{}`, `{"design": true}`, `{"design": true, "docs": true}`} {
		if _, err := w.edit(task.Key, stages, UpdateTaskInput{}); err != nil {
			t.Fatalf("%s: %v", stages, err)
		}
	}
	if n := len(w.eventsOf(t, task.ID, domain.EventTaskUpdated)); n != 1 {
		t.Fatalf("не правка писала событие: %d", n)
	}
	// Не правка выбора рядом с правкой заголовка: в событии только заголовок.
	title := "Правка заголовка"
	if _, err := w.edit(task.Key, `{"design": true}`, UpdateTaskInput{Title: &title}); err != nil {
		t.Fatal(err)
	}
	last := w.eventsOf(t, task.ID, domain.EventTaskUpdated)
	if f := payloadOf(t, last[len(last)-1])["fields"]; !reflect.DeepEqual(f, []any{"title"}) {
		t.Fatalf("неизменённый выбор в fields: %v", f)
	}

	// Текущий статус выключить нельзя, а запись true — не правка.
	w.move(t, task.Key, "start", "analysis_done") // design включён — задача в design
	cur, _ := w.current(t, task.Key)
	if cur.StatusKey != "design" {
		t.Fatalf("задача в design: %s", cur.StatusKey)
	}
	other := "Отказ"
	_, err = w.edit(task.Key, `{"design": false, "docs": false}`, UpdateTaskInput{Title: &other})
	if e := apperr.As(err); e.Code != apperr.CodeConflict || !strings.Contains(e.Message, "текущий статус задачи") {
		t.Fatalf("выключение текущего статуса: %v", err)
	}
	cur, _ = w.current(t, task.Key)
	if cur.Title != title || !reflect.DeepEqual(choiceOf(t, cur.OptionalStages), map[string]bool{"design": true, "docs": true}) {
		t.Fatalf("отказ 409 изменил задачу: %q %s", cur.Title, cur.OptionalStages)
	}
	n := len(w.eventsOf(t, task.ID, domain.EventTaskUpdated))
	if _, err := w.edit(task.Key, `{"design": true}`, UpdateTaskInput{}); err != nil {
		t.Fatalf("true для текущего: %v", err)
	}
	if len(w.eventsOf(t, task.ID, domain.EventTaskUpdated)) != n {
		t.Fatal("true для текущего писало событие")
	}

	// Неверный ключ и не bool — 400 и при правке.
	if _, err := w.edit(task.Key, `{"polish": true}`, UpdateTaskInput{}); apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("чужой ключ при правке: %v", err)
	}
	// Закрытая задача: непустой выбор — 409, пустой — не правка.
	w.move(t, task.Key, "cancel")
	if _, err := w.edit(task.Key, `{"docs": true}`, UpdateTaskInput{}); apperr.As(err).Code != apperr.CodeConflict ||
		!strings.Contains(apperr.As(err).Message, "закрыта") {
		t.Fatalf("закрытая задача: %v", err)
	}
	if _, err := w.edit(task.Key, `{}`, UpdateTaskInput{}); err != nil {
		t.Fatalf("пустой выбор у закрытой — не правка: %v", err)
	}

	// Права (AC-6): агент без аренды — 403, с арендой своей задачи — можно,
	// сопровождающий — любая задача.
	fresh := w.create(t, "Права", "", "")
	w.move(t, fresh.Key, "start")
	raw := json.RawMessage(`{"design": true}`)
	agentEdit := UpdateTaskInput{OptionalStages: &raw, Level: domain.MemberAgent}
	if _, err := w.engine.UpdateTask(ctx, fresh.Key, w.agent.ID, agentEdit); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("агент без аренды: %v", err)
	}
	if _, err := w.engine.Claim(ctx, fresh.Key, w.agent.ID, ""); err != nil {
		t.Fatal(err)
	}
	if out, err := w.engine.UpdateTask(ctx, fresh.Key, w.agent.ID, agentEdit); err != nil ||
		!choiceOf(t, out.OptionalStages)["design"] {
		t.Fatalf("агент с арендой своей задачи личным токеном: %v", err)
	}
	off := json.RawMessage(`{"design": false}`)
	if _, err := w.engine.UpdateTask(ctx, fresh.Key, w.human.ID, UpdateTaskInput{
		OptionalStages: &off, Level: domain.MemberMaintainer}); err != nil {
		t.Fatalf("сопровождающий: %v", err)
	}
}

// AC-5 на уровне движка: токен задачи — своей задаче 403 раньше разбора и
// целиком, детям — можно; прочую постановку своей задачи правит.
func TestTaskOptionalStagesTaskToken(t *testing.T) {
	ctx := context.Background()
	w := newSkipWorld(t)
	own := w.create(t, "Своя", "", "")
	child, err := w.engine.CreateTask(ctx, "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Ребёнок", ParentKey: own.Key}})
	if err != nil {
		t.Fatal(err)
	}
	bound := &TaskBinding{TaskID: own.ID, TaskKey: own.Key, StageRunID: uuid.New()}

	title := "Новый заголовок"
	for _, stages := range []string{`{"design": true}`, `{"nope": true}`, `{"design": "x"}`, `{}`} {
		raw := json.RawMessage(stages)
		_, err := w.engine.UpdateTask(ctx, own.Key, w.system.ID, UpdateTaskInput{
			Title: &title, OptionalStages: &raw, Bound: bound})
		e := apperr.As(err)
		if e.Code != apperr.CodeForbidden || !strings.Contains(e.Message, "выбор этапов своей задачи — решение человека") ||
			!strings.HasPrefix(e.Message, ExecutorRefusal) {
			t.Fatalf("%s: своей задаче токеном — 403, получили %v", stages, err)
		}
	}
	got, _ := w.store.GetTaskByKey(ctx, w.store.DB(), own.Key)
	if got.Title != own.Title || !reflect.DeepEqual(choiceOf(t, got.OptionalStages), choiceOf(t, own.OptionalStages)) {
		t.Fatalf("отказ изменил задачу: %q %s", got.Title, got.OptionalStages)
	}
	if out, err := w.engine.UpdateTask(ctx, own.Key, w.system.ID, UpdateTaskInput{Title: &title, Bound: bound}); err != nil ||
		out.Title != title {
		t.Fatalf("заголовок своей задачи без выбора: %v", err)
	}
	settings := json.RawMessage(`{"analysis": {"model_level": "high"}}`)
	if _, err := w.engine.UpdateTask(ctx, own.Key, w.system.ID, UpdateTaskInput{StageSettings: &settings, Bound: bound}); err != nil {
		t.Fatalf("stage_settings своей задачи: %v", err)
	}
	raw := json.RawMessage(`{"design": true}`)
	if out, err := w.engine.UpdateTask(ctx, child.Key, w.system.ID, UpdateTaskInput{OptionalStages: &raw, Bound: bound}); err != nil ||
		!choiceOf(t, out.OptionalStages)["design"] {
		t.Fatalf("выбор ребёнка токеном: %v", err)
	}
}

// AC-8: цепочка пропусков — статус, прогоны, попытка, переход входа, лента и
// гейты только у запрошенного перехода.
func TestSkipChainAdvance(t *testing.T) {
	ctx := context.Background()
	w := newSkipWorld(t)
	task := w.create(t, "Мимо дизайна и доков", "", `{"design": false, "docs": false}`)
	w.move(t, task.Key, "start")
	_, analysisRun := w.current(t, task.Key)

	res := w.move(t, task.Key, "analysis_done")
	if res.Task.StatusKey != "development" || !reflect.DeepEqual(res.Skips, []string{"design", "docs"}) {
		t.Fatalf("результат перехода: %s %v", res.Task.StatusKey, res.Skips)
	}
	cur, run := w.current(t, task.Key)
	if cur.StatusKey != "development" {
		t.Fatalf("статус задачи: %s", cur.StatusKey)
	}
	if len(w.runsOn(t, task.ID, "design")) != 0 || len(w.runsOn(t, task.ID, "docs")) != 0 {
		t.Fatal("у пропущенных этапов есть прогоны")
	}
	docsDone := w.transitionKeyID(t, cur, "docs_done")
	if run.Attempt != 1 || run.EnteredViaTransitionID == nil || *run.EnteredViaTransitionID != docsDone ||
		run.RejectedFromRunID != nil {
		t.Fatalf("прогон development: attempt %d via %v rejected %v", run.Attempt, run.EnteredViaTransitionID, run.RejectedFromRunID)
	}

	evs := w.eventsOf(t, task.ID, domain.EventTaskAdvanced, domain.EventStageSkipped, domain.EventStageEntered)
	evs = evs[len(evs)-4:]
	wantTypes := []string{domain.EventTaskAdvanced, domain.EventStageSkipped, domain.EventStageSkipped, domain.EventStageEntered}
	for i, ev := range evs {
		if ev.Type != wantTypes[i] {
			t.Fatalf("порядок ленты: %d %s, ожидали %s", i, ev.Type, wantTypes[i])
		}
	}
	if p := payloadOf(t, evs[0]); p["to"] != "development" || !reflect.DeepEqual(p["skips"], []any{"design", "docs"}) ||
		p["transition"] != "analysis_done" || p["from"] != "analysis" {
		t.Fatalf("task.advanced: %v", p)
	}
	for i, want := range []map[string]any{
		{"status": "design", "bypass_transition": "design_done", "transition": "analysis_done"},
		{"status": "docs", "bypass_transition": "docs_done", "transition": "analysis_done"},
	} {
		if p := payloadOf(t, evs[1+i]); !reflect.DeepEqual(p, want) || evs[1+i].StageRunID != nil {
			t.Fatalf("stage.skipped %d: %v run %v", i, p, evs[1+i].StageRunID)
		}
	}
	if p := payloadOf(t, evs[3]); p["status"] != "development" || p["transition"] != "docs_done" || p["attempt"] != float64(1) {
		t.Fatalf("stage.entered: %v", p)
	}

	// Гейты: у analysis_done записаны, у обходных — нет, хотя на design_done
	// блокирующий human_approval.
	var bypassResults, requested int
	if err := w.store.DB().QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE t.key IN ('design_done', 'docs_done')),
		       count(*) FILTER (WHERE t.key = 'analysis_done')
		FROM gate_results gr JOIN workflow_transitions t ON t.id = gr.transition_id
		WHERE gr.stage_run_id = $1`, analysisRun.ID).Scan(&bypassResults, &requested); err != nil {
		t.Fatal(err)
	}
	if bypassResults != 0 || requested == 0 {
		t.Fatalf("gate_results: обходных %d, запрошенного %d", bypassResults, requested)
	}

	// Один выключенный — один пропуск; оба включены — как всегда, без skips.
	one := w.create(t, "Без дизайна", "", `{"design": false, "docs": true}`)
	w.move(t, one.Key, "start")
	if res := w.move(t, one.Key, "analysis_done"); res.Task.StatusKey != "docs" || !reflect.DeepEqual(res.Skips, []string{"design"}) {
		t.Fatalf("один пропуск: %s %v", res.Task.StatusKey, res.Skips)
	}
	if n := len(w.eventsOf(t, one.ID, domain.EventStageSkipped)); n != 1 {
		t.Fatalf("stage.skipped при одном пропуске: %d", n)
	}
	all := w.create(t, "Все этапы", "", `{"design": true}`)
	w.move(t, all.Key, "start")
	res = w.move(t, all.Key, "analysis_done")
	if res.Task.StatusKey != "design" || len(res.Skips) != 0 || res.Skips == nil {
		t.Fatalf("без пропуска: %s %v", res.Task.StatusKey, res.Skips)
	}
	adv := w.eventsOf(t, all.ID, domain.EventTaskAdvanced)
	if _, has := payloadOf(t, adv[len(adv)-1])["skips"]; has {
		t.Fatal("skips в task.advanced без пропуска")
	}
}

func (w skipWorld) transitionKeyID(t *testing.T, task domain.Task, key string) uuid.UUID {
	t.Helper()
	tr, err := w.store.GetTransitionByKey(context.Background(), w.store.DB(), task.WorkflowID, key)
	if err != nil {
		t.Fatal(err)
	}
	return tr.ID
}

// AC-9: пределы этапа и попыток через пропуск.
func TestSkipChainLimits(t *testing.T) {
	ctx := context.Background()
	w := newSkipWorld(t)
	task := w.create(t, "Пределы", "", `{"design": false, "docs": false}`)
	w.move(t, task.Key, "start")
	// attempts_limit на analysis_done (max 1) считает входы в design — их
	// нет, гейт проходит.
	w.move(t, task.Key, "analysis_done")
	cur, run := w.current(t, task.Key)

	// stage_time_limit обходного docs_done действует на прогон development.
	limit, err := w.engine.StageLimit(ctx, cur)
	if err != nil || limit == nil || limit.Transition != "docs_done" || limit.Status != "development" {
		t.Fatalf("предел этапа после пропуска: %v %+v", err, limit)
	}
	tw := timedWorld{attemptsWorld: w.attemptsWorld}
	tw.backdate(t, run, 2*time.Hour)
	if _, err := w.engine.Claim(ctx, task.Key, w.agent.ID, ""); apperr.As(err).Code != apperr.CodeStageTimeExceeded {
		t.Fatalf("захват после срока: %v", err)
	}
	if _, err := w.engine.DecideGate(ctx, task.Key, "docs_done", "time", w.human.ID, nil,
		domain.GateResultWaived, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("снятие предела на обходном переходе входа: %v", err)
	}
	if _, err := w.engine.Claim(ctx, task.Key, w.agent.ID, ""); err != nil {
		t.Fatalf("захват после снятия: %v", err)
	}
	if err := w.engine.Release(ctx, run.ID, w.agent.ID, ""); err != nil {
		t.Fatal(err)
	}

	// Предел запрошенного analysis_done на прогон за пропуском не действует:
	// docs открыт обходным design_done, у которого предела нет.
	plain := w.create(t, "Без пропуска docs", "", `{"design": false, "docs": true}`)
	w.move(t, plain.Key, "start", "analysis_done")
	pcur, _ := w.current(t, plain.Key)
	if pcur.StatusKey != "docs" {
		t.Fatalf("plain в docs: %s", pcur.StatusKey)
	}
	if l, err := w.engine.StageLimit(ctx, pcur); err != nil || l != nil {
		t.Fatalf("предел analysis_done на прогоне docs за пропуском: %v %+v", err, l)
	}

	// Попытки: второй вход в development после возврата — attempt 2;
	// attempts_limit на docs_done (max 1) при пропуске не вычисляется.
	w.move(t, task.Key, "back_to_analysis")
	if _, again := w.current(t, task.Key); again.Attempt != 2 {
		t.Fatalf("второй вход в analysis: attempt %d", again.Attempt)
	}
	res := w.move(t, task.Key, "analysis_done")
	if res.Task.StatusKey != "development" || res.NewRun.Attempt != 2 {
		t.Fatalf("второй вход в development: %s attempt %d", res.Task.StatusKey, res.NewRun.Attempt)
	}
}

// AC-10: конец цепочки — терминальный; правка выбора ждёт идущий переход.
func TestSkipChainTerminalAndLock(t *testing.T) {
	ctx := context.Background()
	w := newSkipWorld(t)
	chore := w.create(t, "Мелочь", "chore", "")
	if got := choiceOf(t, chore.OptionalStages); !reflect.DeepEqual(got, map[string]bool{"polish": false}) {
		t.Fatalf("выбор мелочи: %v", got)
	}
	res := w.move(t, chore.Key, "start", "work_done")
	if res.Task.StatusKey != "done" || res.NewRun == nil || !reflect.DeepEqual(res.Skips, []string{"polish"}) {
		t.Fatalf("пропуск в терминальный: %+v", res)
	}
	closed, _ := w.current(t, chore.Key)
	if closed.ClosedAt == nil {
		t.Fatal("задача не закрыта входом в терминальный через пропуск")
	}
	// CLAB-331: результат перехода несёт то же время закрытия и обновления.
	if res.Task.ClosedAt == nil || !res.Task.ClosedAt.Equal(*closed.ClosedAt) ||
		!res.Task.UpdatedAt.Equal(closed.UpdatedAt) {
		t.Fatalf("копия задачи в результате: closed_at %v updated_at %v, в базе %v и %v",
			res.Task.ClosedAt, res.Task.UpdatedAt, closed.ClosedAt, closed.UpdatedAt)
	}

	// Переход держит строку задачи: правка выбора ждёт его конца и видит
	// статус, где задача встала.
	task := w.create(t, "Блокировка", "", `{"design": true}`)
	w.move(t, task.Key, "start")
	design := w.statusID(t, task, "design")
	locked, release := make(chan struct{}), make(chan struct{})
	moved := make(chan error, 1)
	go func() {
		moved <- w.store.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := w.store.LockTaskByKey(ctx, tx, task.Key); err != nil {
				close(locked)
				return err
			}
			close(locked)
			<-release
			// «Переход» под блокировкой: задача встаёт в design.
			_, err := tx.Exec(ctx, `UPDATE tasks SET status_id = $2 WHERE id = $1`, task.ID, design)
			return err
		})
	}()
	<-locked
	done := make(chan error, 1)
	go func() {
		_, err := w.edit(task.Key, `{"design": false}`, UpdateTaskInput{})
		done <- err
	}()
	select {
	case err := <-done:
		close(release)
		t.Fatalf("правка не ждала блокировку: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-moved; err != nil {
		t.Fatal(err)
	}
	if err := <-done; apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("правка после перехода видит новый текущий статус: %v", err)
	}
}

func (w skipWorld) statusID(t *testing.T, task domain.Task, key string) uuid.UUID {
	t.Helper()
	statuses, err := w.store.ListStatuses(context.Background(), w.store.DB(), task.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range statuses {
		if st.Key == key {
			return st.ID
		}
	}
	t.Fatalf("нет статуса %s", key)
	return uuid.Nil
}

// AC-11, AC-12: представление перехода и возврат в выключенный этап.
func TestSkipPreviewAndDisabledReturn(t *testing.T) {
	ctx := context.Background()
	w := newSkipWorld(t)
	task := w.create(t, "Представление", "", `{"design": false, "docs": false}`)
	w.move(t, task.Key, "start")

	cur, _ := w.current(t, task.Key)
	previews, err := w.engine.Preview(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]TransitionPreview{}
	for _, p := range previews {
		byKey[p.Key] = p
	}
	ad := byKey["analysis_done"]
	if ad.ToStatus != "design" || ad.LandsIn == nil || *ad.LandsIn != "development" ||
		!reflect.DeepEqual(ad.Skips, []string{"design", "docs"}) || ad.Unavailable != "" {
		t.Fatalf("analysis_done: %+v", ad)
	}
	if c := byKey["cancel"]; c.LandsIn == nil || *c.LandsIn != c.ToStatus || c.Skips == nil || len(c.Skips) != 0 {
		t.Fatalf("переход без пропуска: %+v", c)
	}
	claim, err := w.engine.State(ctx, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(choiceOf(t, claim.Task.OptionalStages), map[string]bool{"design": false, "docs": false}) {
		t.Fatalf("task.optional_stages состояния: %s", claim.Task.OptionalStages)
	}
	for _, spec := range claim.GatesAhead {
		if spec.Transition == "analysis_done" && (spec.LandsIn == nil || *spec.LandsIn != "development" ||
			!reflect.DeepEqual(spec.Skips, []string{"design", "docs"})) {
			t.Fatalf("gates_ahead: %+v", spec)
		}
	}

	w.move(t, task.Key, "analysis_done")
	cur, devRun := w.current(t, task.Key)
	previews, err = w.engine.Preview(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range previews {
		switch p.Key {
		case "back_to_design", "reassign_design":
			if p.Allowed || p.LandsIn != nil || p.Unavailable != UnavailableStageDisabled || p.ToStatus != "design" {
				t.Fatalf("%s недоступен: %+v", p.Key, p)
			}
		case "back_to_analysis":
			if !p.Allowed || p.Unavailable != "" {
				t.Fatalf("возврат в обязательный: %+v", p)
			}
		}
	}
	if p, err := w.engine.CheckGates(ctx, task.Key, "back_to_design"); err != nil || p.Allowed {
		t.Fatalf("CheckGates недоступного: %v %+v", err, p)
	}

	gateRows := func() int {
		var n int
		if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM gate_results WHERE stage_run_id = $1`, devRun.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := len(w.eventsOf(t, task.ID, domain.EventTaskRejected, domain.EventGateFailed, domain.EventReviewRequested))
	for _, tr := range []string{"back_to_design", "reassign_design"} {
		_, err := w.engine.Execute(ctx, task.Key, tr, w.system.ID, "")
		if e := apperr.As(err); e.Code != apperr.CodeTransitionNotAllowed || !strings.Contains(e.Message, "этап design выключен у задачи") {
			t.Fatalf("%s в выключенный: %v", tr, err)
		}
	}
	if gateRows() != 0 || len(w.eventsOf(t, task.ID, domain.EventTaskRejected, domain.EventGateFailed, domain.EventReviewRequested)) != before {
		t.Fatal("отказ возврата оставил следы")
	}
	if c, _ := w.current(t, task.Key); c.StatusKey != "development" {
		t.Fatalf("задача ушла: %s", c.StatusKey)
	}

	if _, err := w.edit(task.Key, `{"design": true}`, UpdateTaskInput{}); err != nil {
		t.Fatal(err)
	}
	res := w.move(t, task.Key, "back_to_design")
	if res.Task.StatusKey != "design" || res.NewRun.RejectedFromRunID == nil || *res.NewRun.RejectedFromRunID != devRun.ID {
		t.Fatalf("возврат после включения: %s %+v", res.Task.StatusKey, res.NewRun)
	}
}

// Цикл без необязательных: ответы отличаются только новыми полями.
func TestSkipPlainCycleUnchanged(t *testing.T) {
	ctx := context.Background()
	_, engine, _, _, system := setupPhase4(t, AuthHeader)
	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Обычная"}})
	if err != nil || string(task.OptionalStages) != "{}" {
		t.Fatalf("задача обычного цикла: %v %s", err, task.OptionalStages)
	}
	previews, err := engine.Preview(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range previews {
		if p.LandsIn == nil || *p.LandsIn != p.ToStatus || len(p.Skips) != 0 || p.Unavailable != "" {
			t.Fatalf("переход обычного цикла: %+v", p)
		}
	}
	res, err := engine.Execute(ctx, task.Key, "start", system.ID, "")
	if err != nil || len(res.Skips) != 0 {
		t.Fatalf("переход обычного цикла: %v %+v", err, res.Skips)
	}
}

// AC-13: переезд бэклога пересчитывает выбор и включает статус переезда.
func TestOptionalStagesBacklogMove(t *testing.T) {
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	off := false
	v1 := BundleWorkflow{
		Name: "Истории", TaskTypes: []string{"story"},
		Statuses: []BundleStatus{
			{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
			{Key: "triage", Name: "Triage", Category: "backlog", Role: "analyst"},
			{Key: "design", Name: "Design", Category: "active", IsOptional: true, EnabledByDefault: &off},
			{Key: "docs", Name: "Docs", Category: "active", IsOptional: true},
			{Key: "work", Name: "Work", Category: "active"},
			{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
		},
		Transitions: []BundleTransition{
			{Key: "start", Name: "Start", From: "new", To: "triage", Kind: "advance"},
			{Key: "triage_done", Name: "Triage done", From: "triage", To: "design", Kind: "advance"},
			{Key: "design_done", Name: "Design done", From: "design", To: "docs", Kind: "advance"},
			{Key: "docs_done", Name: "Docs done", From: "docs", To: "work", Kind: "advance"},
			{Key: "finish", Name: "Finish", From: "work", To: "done", Kind: "advance"},
		},
	}
	activateCycles(t, authoring, system.ID, v1)
	create := func(title, stages string) domain.Task {
		t.Helper()
		task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
			CreateTaskInput: postgres.CreateTaskInput{Title: title, TaskType: "story"},
			OptionalStages:  json.RawMessage(stages)})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	inTriage := create("В разборе", `{"design": true, "docs": false}`)
	if _, err := engine.Execute(ctx, inTriage.Key, "start", system.ID, ""); err != nil {
		t.Fatal(err)
	}
	onNew := create("Новая", `{"design": true, "docs": false}`)
	started := create("Начатая", `{"design": true, "docs": false}`)
	if _, err := engine.Execute(ctx, started.Key, "start", system.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Claim(ctx, started.Key, mustActor(t, store, "agent:runner").ID, ""); err != nil {
		t.Fatal(err)
	}

	v2 := v1
	v2.Statuses = []BundleStatus{
		{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
		{Key: "triage", Name: "Triage", Category: "backlog", Role: "analyst", IsOptional: true, EnabledByDefault: &off},
		{Key: "design", Name: "Design", Category: "active", IsOptional: true, EnabledByDefault: &off},
		{Key: "review", Name: "Review", Category: "active", IsOptional: true, EnabledByDefault: &off},
		{Key: "work", Name: "Work", Category: "active"},
		{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
	}
	v2.Transitions = []BundleTransition{
		{Key: "start", Name: "Start", From: "new", To: "triage", Kind: "advance"},
		{Key: "triage_done", Name: "Triage done", From: "triage", To: "design", Kind: "advance"},
		{Key: "design_done", Name: "Design done", From: "design", To: "review", Kind: "advance"},
		{Key: "review_done", Name: "Review done", From: "review", To: "work", Kind: "advance"},
		{Key: "finish", Name: "Finish", From: "work", To: "done", Kind: "advance"},
	}
	activateCycles(t, authoring, system.ID, v2)

	check := func(task domain.Task, want map[string]bool, moved bool) {
		t.Helper()
		got, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
		if err != nil {
			t.Fatal(err)
		}
		if (got.WorkflowID != task.WorkflowID) != moved {
			t.Fatalf("%s: переезд %v, ожидали %v", task.Key, got.WorkflowID != task.WorkflowID, moved)
		}
		if c := choiceOf(t, got.OptionalStages); !reflect.DeepEqual(c, want) {
			t.Fatalf("%s: выбор %v, ожидали %v", task.Key, c, want)
		}
		evs, err := store.ListEvents(ctx, store.DB(), task.ID, 100)
		if err != nil {
			t.Fatal(err)
		}
		var movedEv []domain.TaskEvent
		for _, ev := range evs {
			if ev.Type == domain.EventTaskWorkflowMoved {
				movedEv = append(movedEv, ev)
			}
		}
		if !moved {
			if len(movedEv) != 0 {
				t.Fatalf("%s: начатая задача переехала", task.Key)
			}
			return
		}
		if len(movedEv) != 1 {
			t.Fatalf("%s: task.workflow_moved %d", task.Key, len(movedEv))
		}
		p := payloadOf(t, movedEv[0])
		stages, _ := p["optional_stages"].(map[string]any)
		wantTo := map[string]any{}
		for k, v := range want {
			wantTo[k] = v
		}
		if !reflect.DeepEqual(stages["from"], map[string]any{"design": true, "docs": false}) ||
			!reflect.DeepEqual(stages["to"], wantTo) {
			t.Fatalf("%s: optional_stages в task.workflow_moved: %v", task.Key, p)
		}
	}
	// Статус переезда triage стал необязательным — включён принудительно.
	check(inTriage, map[string]bool{"triage": true, "design": true, "review": false}, true)
	check(onNew, map[string]bool{"triage": false, "design": true, "review": false}, true)
	check(started, map[string]bool{"design": true, "docs": false}, false)

	// Переехавшая задача идёт по новому выбору: triage включён, design
	// включён, review выключен.
	res, err := engine.Execute(ctx, inTriage.Key, "triage_done", system.ID, "")
	if err != nil || res.Task.StatusKey != "design" {
		t.Fatalf("после переезда: %v %s", err, res.Task.StatusKey)
	}
	res, err = engine.Execute(ctx, inTriage.Key, "design_done", system.ID, "")
	if err != nil || res.Task.StatusKey != "work" || !reflect.DeepEqual(res.Skips, []string{"review"}) {
		t.Fatalf("пропуск нового этапа: %v %s %v", err, res.Task.StatusKey, res.Skips)
	}
}
