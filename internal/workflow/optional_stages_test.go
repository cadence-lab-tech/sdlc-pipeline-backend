package workflow

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Необязательный этап в версии цикла на живой базе (CLAB-314, решение 0066):
// миграция, правка черновика, копия, активация и связка.

// optionalBundle — цикл с двумя необязательными этапами: design выключен по
// умолчанию, docs включён. Ролей нет — связка не зависит от сида.
func optionalBundle(name string) BundleWorkflow {
	off := false
	return BundleWorkflow{Name: name, TaskTypes: []string{"chore"},
		Statuses: []BundleStatus{
			{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
			{Key: "design", Name: "Design", Category: "waiting", IsOptional: true, EnabledByDefault: &off},
			{Key: "dev", Name: "Dev", Category: "waiting"},
			{Key: "docs", Name: "Docs", Category: "waiting", IsOptional: true},
			{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
		},
		Transitions: []BundleTransition{
			{Key: "start", Name: "Start", From: "new", To: "design", Kind: "advance"},
			{Key: "design_done", Name: "Design done", From: "design", To: "dev", Kind: "advance"},
			{Key: "dev_done", Name: "Dev done", From: "dev", To: "docs", Kind: "advance"},
			{Key: "docs_done", Name: "Docs done", From: "docs", To: "done", Kind: "advance"},
			{Key: "redesign", Name: "Redesign", From: "dev", To: "design", Kind: "reject"},
		},
	}
}

func optionalFlags(wf domain.Workflow) map[string][2]bool {
	out := map[string][2]bool{}
	for _, st := range wf.Statuses {
		out[st.Key] = [2]bool{st.IsOptional, st.EnabledByDefault}
	}
	return out
}

func importOne(t *testing.T, a *Authoring, project string, actor uuid.UUID, w BundleWorkflow) (ImportEntry, Validation, error) {
	t.Helper()
	rep, err := a.Import(context.Background(), project, actor, Bundle{Version: BundleVersion, Workflows: []BundleWorkflow{w}}, false)
	if err != nil {
		return ImportEntry{}, Validation{}, err
	}
	return rep.Workflows[0], rep.Validations[0], nil
}

// AC-1: миграция аддитивна, down → up на заполненной базе проходит, CHECK
// держит пару.
func TestOptionalStagesMigrationOnFilledBase(t *testing.T) {
	ctx := context.Background()
	store, _, _, _, _ := setupPhase4(t, AuthHeader)
	db := store.DB()

	read := func(name string) string {
		body, err := migrations.Schema.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	columns := func() int {
		var n int
		if err := db.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'workflow_statuses'
			  AND column_name IN ('is_optional', 'enabled_by_default')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	rows := func() int {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM workflow_statuses`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := rows()
	if before == 0 {
		t.Fatal("в сиде нет статусов — проверять нечего")
	}

	if _, err := db.Exec(ctx, read("000034_optional_stages.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if columns() != 0 || rows() != before {
		t.Fatalf("down: колонок %d, строк %d → %d", columns(), before, rows())
	}
	if _, err := db.Exec(ctx, read("000034_optional_stages.up.sql")); err != nil {
		t.Fatalf("up на заполненной базе: %v", err)
	}
	if columns() != 2 || rows() != before {
		t.Fatalf("up: колонок %d, строк %d → %d", columns(), before, rows())
	}
	var marked int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM workflow_statuses WHERE is_optional OR NOT enabled_by_default`).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if marked != 0 {
		t.Fatalf("после up все статусы обязательны, отмеченных %d", marked)
	}

	if _, err := db.Exec(ctx, `
		UPDATE workflow_statuses SET is_optional = false, enabled_by_default = false
		WHERE id = (SELECT id FROM workflow_statuses LIMIT 1)`); err == nil {
		t.Fatal("CHECK пропустил обязательный статус с умолчанием «выключен»")
	}
	if _, err := db.Exec(ctx, `
		UPDATE workflow_statuses SET is_optional = true, enabled_by_default = false
		WHERE id = (SELECT id FROM workflow_statuses WHERE NOT is_initial AND NOT is_terminal LIMIT 1)`); err != nil {
		t.Fatalf("необязательный выключенный статус — допустимая пара: %v", err)
	}

	// Повторный down → up: отметка теряется, схема возвращается.
	if _, err := db.Exec(ctx, read("000034_optional_stages.down.sql")); err != nil {
		t.Fatalf("второй down: %v", err)
	}
	if _, err := db.Exec(ctx, read("000034_optional_stages.up.sql")); err != nil {
		t.Fatalf("второй up: %v", err)
	}
	if columns() != 2 {
		t.Fatal("второй up не вернул колонки")
	}
}

// AC-3, AC-4: правило пары на правке черновика, 409 у активной версии.
func TestOptionalStatusEditing(t *testing.T) {
	ctx := context.Background()
	_, _, authoring, _, system := setupPhase4(t, AuthHeader)

	draft, err := authoring.CreateDraft(ctx, "SDLC", system.ID, DraftInput{Name: "Опции"})
	if err != nil {
		t.Fatal(err)
	}
	on, off := true, false
	key := draft.Statuses[1].Key

	st, err := authoring.UpdateStatus(ctx, draft.ID, key, postgres.StatusInput{IsOptional: &on, EnabledByDefault: &off})
	if err != nil || !st.IsOptional || st.EnabledByDefault {
		t.Fatalf("отметка и умолчание: %v %+v", err, st)
	}
	// Снятие отметки возвращает умолчание во «включён».
	st, err = authoring.UpdateStatus(ctx, draft.ID, key, postgres.StatusInput{IsOptional: &off})
	if err != nil || st.IsOptional || !st.EnabledByDefault {
		t.Fatalf("снятие отметки: %v %+v", err, st)
	}
	for name, in := range map[string]postgres.StatusInput{
		"умолчание у обязательного": {EnabledByDefault: &off},
		"пара false/false":          {IsOptional: &off, EnabledByDefault: &off},
	} {
		if _, err := authoring.UpdateStatus(ctx, draft.ID, key, in); apperr.As(err).Code != apperr.CodeInvalidInput {
			t.Errorf("%s: ожидался invalid_input, получили %v", name, err)
		}
	}
	if _, err := authoring.CreateStatus(ctx, draft.ID, postgres.StatusInput{
		Key: "x", Name: "X", Category: "active", EnabledByDefault: &off,
	}); apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("новый статус без is_optional с умолчанием «выключен»: %v", err)
	}
	created, err := authoring.CreateStatus(ctx, draft.ID, postgres.StatusInput{
		Key: "y", Name: "Y", Category: "active", IsOptional: &on,
	})
	if err != nil || !created.IsOptional || !created.EnabledByDefault {
		t.Fatalf("новый необязательный по умолчанию включён: %v %+v", err, created)
	}

	active, err := authoring.store.GetActiveWorkflowForType(ctx, authoring.store.DB(), draft.ProjectID, domain.DefaultTaskType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authoring.UpdateStatus(ctx, active.ID, key, postgres.StatusInput{IsOptional: &on}); apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("активная версия: ожидался conflict, получили %v", err)
	}
	if _, err := authoring.CreateStatus(ctx, active.ID, postgres.StatusInput{
		Key: "z", Name: "Z", Category: "active", IsOptional: &on,
	}); apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("новый статус в активной версии: ожидался conflict, получили %v", err)
	}
}

// AC-5, AC-10: активация с отметками и предупреждениями, копия черновика
// переносит отметки, правка копии активную не трогает; черновик с проблемой
// не активируется.
func TestOptionalStagesActivateAndCopy(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)

	entry, v, err := importOne(t, authoring, "SDLC", system.ID, optionalBundle("Опции"))
	if err != nil {
		t.Fatal(err)
	}
	if !v.Valid || len(byCode(v.Warnings, "optional_reject_target")) != 1 {
		t.Fatalf("валиден с предупреждением optional_reject_target: %+v", v)
	}
	id := uuid.MustParse(entry.ID)
	res, err := authoring.Activate(ctx, id, system.ID)
	if err != nil {
		t.Fatalf("активация: %v", err)
	}
	if len(byCode(res.Warnings, "optional_reject_target")) != 1 {
		t.Fatalf("предупреждения в ответе активации: %+v", res.Warnings)
	}
	activeWF, err := store.GetWorkflow(ctx, store.DB(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]bool{
		"new": {false, true}, "design": {true, false}, "dev": {false, true},
		"docs": {true, true}, "done": {false, true},
	}
	if got := optionalFlags(activeWF); len(got) != len(want) {
		t.Fatalf("статусы активной: %v", got)
	} else {
		for k, w := range want {
			if got[k] != w {
				t.Fatalf("активная версия, %s: %v, ожидали %v", k, got[k], w)
			}
		}
	}

	copyWF, err := authoring.CreateDraft(ctx, "SDLC", system.ID, DraftInput{From: id.String(), TaskTypes: []string{"chore"}})
	if err != nil {
		t.Fatal(err)
	}
	for k, w := range want {
		if optionalFlags(copyWF)[k] != w {
			t.Fatalf("копия, %s: %v, ожидали %v", k, optionalFlags(copyWF)[k], w)
		}
	}
	on := true
	if _, err := authoring.UpdateStatus(ctx, copyWF.ID, "design", postgres.StatusInput{EnabledByDefault: &on}); err != nil {
		t.Fatal(err)
	}
	again, err := store.GetWorkflow(ctx, store.DB(), id)
	if err != nil || optionalFlags(again)["design"] != [2]bool{true, false} {
		t.Fatalf("правка копии не трогает активную: %v %v", err, optionalFlags(again))
	}

	// Проблема держит активацию: design без обхода.
	if err := authoring.DeleteTransition(ctx, copyWF.ID, "design_done"); err != nil {
		t.Fatal(err)
	}
	_, err = authoring.Activate(ctx, copyWF.ID, system.ID)
	e := apperr.As(err)
	if e.Code != apperr.CodeInvalidInput {
		t.Fatalf("черновик с проблемой: ожидался invalid_input, получили %v", err)
	}
	details, _ := e.Details.(map[string]any)
	problems, _ := details["problems"].([]Problem)
	if len(byCode(problems, "optional_no_bypass")) != 1 {
		t.Fatalf("details.problems несёт optional_no_bypass: %+v", e.Details)
	}
	head, err := store.WorkflowHead(ctx, store.DB(), copyWF.ID)
	if err != nil || head.State != domain.WorkflowDraft {
		t.Fatalf("черновик остался черновиком: %v %s", err, head.State)
	}
	if v, err := authoring.Validate(ctx, copyWF.ID); err != nil || len(byCode(v.Problems, "optional_no_bypass")) != 1 {
		t.Fatalf("проверка черновика: %v %+v", err, v)
	}
}

// AC-13: импорт и экспорт с отметками.
func TestOptionalStagesBundle(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)
	testdb.ForeignTask(t, store) // пустой по циклам проект OTHER

	// п. 1: экспорт → импорт в другой проект — те же отметки.
	entry, _, err := importOne(t, authoring, "SDLC", system.ID, optionalBundle("Опции"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := store.GetWorkflow(ctx, store.DB(), uuid.MustParse(entry.ID))
	if err != nil {
		t.Fatal(err)
	}
	exported := *exportWorkflow(src)
	other, _, err := importOne(t, authoring, "OTHER", system.ID, exported)
	if err != nil || other.Outcome != "draft_created" {
		t.Fatalf("импорт в другой проект: %v %+v", err, other)
	}
	dst, err := store.GetWorkflow(ctx, store.DB(), uuid.MustParse(other.ID))
	if err != nil {
		t.Fatal(err)
	}
	for k, w := range optionalFlags(src) {
		if optionalFlags(dst)[k] != w {
			t.Fatalf("круг экспорт → импорт, %s: %v, ожидали %v", k, optionalFlags(dst)[k], w)
		}
	}

	// п. 2: повтор — unchanged, новых версий нет.
	count := func() int {
		var n int
		if err := store.DB().QueryRow(ctx, `SELECT count(*) FROM workflows`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	n := count()
	if again, _, err := importOne(t, authoring, "OTHER", system.ID, exported); err != nil || again.Outcome != "unchanged" {
		t.Fatalf("повтор связки: %v %+v", err, again)
	}
	if count() != n {
		t.Fatal("повтор завёл версию")
	}

	// п. 3: различие только в is_optional и только в enabled_by_default.
	onlyOptional := optionalBundle("Опции")
	onlyOptional.Statuses[3].IsOptional = false
	if e, _, err := importOne(t, authoring, "SDLC", system.ID, onlyOptional); err != nil || e.Outcome != "draft_created" {
		t.Fatalf("различие в is_optional: %v %+v", err, e)
	}
	onlyDefault := optionalBundle("Опции")
	onlyDefault.Statuses[1].EnabledByDefault = nil
	if e, _, err := importOne(t, authoring, "SDLC", system.ID, onlyDefault); err != nil || e.Outcome != "draft_created" {
		t.Fatalf("различие в enabled_by_default: %v %+v", err, e)
	}

	// п. 4-5: связка без полей — все обязательны; is_optional без умолчания —
	// включён.
	plain := optionalBundle("Без отметок")
	for i := range plain.Statuses {
		plain.Statuses[i].IsOptional, plain.Statuses[i].EnabledByDefault = false, nil
	}
	plain.Statuses[3].IsOptional = true
	e, v, err := importOne(t, authoring, "SDLC", system.ID, plain)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetWorkflow(ctx, store.DB(), uuid.MustParse(e.ID))
	for k, flags := range optionalFlags(got) {
		want := [2]bool{k == "docs", true}
		if flags != want {
			t.Fatalf("связка без полей, %s: %v, ожидали %v", k, flags, want)
		}
	}
	// п. 7: проверка импортированного черновика — те же коды.
	if !v.Valid || len(byCode(v.Warnings, "optional_reject_target")) != 0 {
		t.Fatalf("design обязательный — reject в него не предупреждение: %+v", v)
	}
	broken := optionalBundle("Сломанный")
	broken.Transitions = append(broken.Transitions[:1:1], broken.Transitions[2:]...) // у design нет обхода
	if _, v, err := importOne(t, authoring, "SDLC", system.ID, broken); err != nil || v.Valid ||
		len(byCode(v.Problems, "optional_no_bypass")) != 1 || byCode(v.Problems, "optional_no_bypass")[0].Status != "design" {
		t.Fatalf("импорт отдаёт проблемы валидатора: %v %+v", err, v)
	}

	// п. 6: обязательный с умолчанием «выключен» — 400, импорт откатан.
	n = count()
	bad := optionalBundle("Несогласованный")
	off := false
	bad.Statuses[2].EnabledByDefault = &off
	if _, _, err := importOne(t, authoring, "SDLC", system.ID, bad); apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("несогласованная пара: ожидался invalid_input, получили %v", err)
	}
	if count() != n {
		t.Fatal("отказ импорта оставил черновик")
	}
}
