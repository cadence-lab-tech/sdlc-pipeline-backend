package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Сужение области задачи: репозиторий, бывший справочником, уходит из
// task_repositories, а гейты, которые смотрят на каждый репозиторий области,
// перестают требовать от него PR. Проверяется по настоящей схеме: отложенный
// триггер области и уникальный индекс основного видят каждое изменение.

// scopeTask заводит мультирепозиторную задачу на трёх репозиториях:
// основной — acme/web, хотя по owner/name первым идёт acme/api.
func scopeTask(t *testing.T, ctx context.Context, engine *Engine, system domain.Principal) domain.Task {
	t.Helper()
	for _, repo := range []string{"acme/api", "acme/web", "acme/docs"} {
		if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: repo}); err != nil {
			t.Fatalf("привязка %s: %v", repo, err)
		}
	}
	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{
			Title:        "Задача на трёх репозиториях",
			Properties:   json.RawMessage(`{"multi_repo":true}`),
			Repositories: []string{"acme/web", "acme/docs", "acme/api"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func scopeNames(task domain.Task) (names []string, primary string) {
	for _, r := range task.Repositories {
		names = append(names, r.Owner+"/"+r.Name)
		if r.IsPrimary {
			primary = r.Owner + "/" + r.Name
		}
	}
	return names, primary
}

func wantCode(t *testing.T, err error, code apperr.Code, what string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("%s: ждали %s, получили %v", what, code, err)
	}
}

// Ушёл основной — основным становится первый из оставшихся по owner/name,
// а в ленте то же событие task.scope_updated с убранными репозиториями.
func TestUpdateScopeRemovesRepositoryAndPromotesPrimary(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	task := scopeTask(t, ctx, engine, system)
	if _, primary := scopeNames(task); primary != "acme/web" {
		t.Fatalf("основной до сужения %q", primary)
	}

	out, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		RemoveRepositories: []string{"acme/web"}, Note: "фронт был только справочником",
		Level: domain.MemberMaintainer,
	})
	if err != nil {
		t.Fatalf("сужение: %v", err)
	}
	names, primary := scopeNames(out)
	if len(names) != 2 || primary != "acme/api" {
		t.Fatalf("область после сужения %v, основной %q", names, primary)
	}
	if string(out.Properties) == "" || !out.MultiRepo() || out.Type() != task.Type() {
		t.Fatalf("сужение задело свойства: %s", out.Properties)
	}

	events, err := store.ListEvents(ctx, store.DB(), task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Added   []string `json:"added_repositories"`
		Removed []string `json:"removed_repositories"`
		Primary string   `json:"primary_repository"`
		Note    string   `json:"note"`
	}
	found := false
	for _, e := range events {
		if e.Type != "task.scope_updated" {
			continue
		}
		found = true
		if e.ActorID == nil || *e.ActorID != system.ID {
			t.Fatalf("актор события %v, ждали принципала %s", e.ActorID, system.ID)
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatal("в ленте нет task.scope_updated")
	}
	if len(payload.Removed) != 1 || payload.Removed[0] != "acme/web" ||
		len(payload.Added) != 0 || payload.Primary != "acme/api" ||
		payload.Note != "фронт был только справочником" {
		t.Fatalf("событие сужения: %+v", payload)
	}

	// Убрать неосновной — основной остаётся на месте.
	out, err = engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		RemoveRepositories: []string{"docs"}, Level: domain.MemberAdmin,
	})
	if err != nil {
		t.Fatalf("сужение по имени: %v", err)
	}
	if names, primary := scopeNames(out); len(names) != 1 || primary != "acme/api" {
		t.Fatalf("область %v, основной %q", names, primary)
	}

	// Последний репозиторий не убирается: без области не проходит гейт
	// repo_scope и дрейфу не за чем следить. Откат оставляет область как была.
	_, err = engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		RemoveRepositories: []string{"acme/api"}, Level: domain.MemberAdmin,
	})
	wantCode(t, err, apperr.CodeConflict, "последний репозиторий")
	again, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if names, primary := scopeNames(again); len(names) != 1 || primary != "acme/api" {
		t.Fatalf("отказ изменил область: %v, %q", names, primary)
	}
}

// Замена единственного репозитория однорепозиторной задачи — одним вызовом:
// сужение идёт раньше расширения, и запрет на второй репозиторий не мешает.
func TestUpdateScopeReplacesSingleRepository(t *testing.T) {
	ctx := context.Background()
	_, engine, _, _, system := setupPhase4(t, AuthHeader)
	for _, repo := range []string{"acme/api", "acme/web"} {
		if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: repo}); err != nil {
			t.Fatal(err)
		}
	}
	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Один репозиторий", Repositories: []string{"acme/api"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		AddRepositories: []string{"acme/web"}, RemoveRepositories: []string{"acme/api"},
		Level: domain.MemberMaintainer,
	})
	if err != nil {
		t.Fatalf("замена: %v", err)
	}
	if names, primary := scopeNames(out); len(names) != 1 || primary != "acme/web" || out.MultiRepo() {
		t.Fatalf("после замены %v, основной %q, multi_repo %v", names, primary, out.MultiRepo())
	}

	// Последний репозиторий уходит только вместе с заменой.
	_, err = engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		RemoveRepositories: []string{"acme/web"}, Level: domain.MemberMaintainer,
	})
	wantCode(t, err, apperr.CodeConflict, "последний без замены")

	// Один и тот же репозиторий в обоих списках — ошибка запроса, а не
	// пустая операция: так скорее опечатка, чем намерение.
	_, err = engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		AddRepositories: []string{"acme/web"}, RemoveRepositories: []string{"acme/web"},
		Level: domain.MemberMaintainer,
	})
	wantCode(t, err, apperr.CodeInvalidInput, "в обоих списках")
}

// Отказы сужения: ниже сопровождающего, репозиторий вне области, репозиторий
// с привязанной работой задачи.
func TestUpdateScopeRemovalRefusals(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	task := scopeTask(t, ctx, engine, system)

	// Агент область расширяет, но не сужает: это решение человека.
	_, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		RemoveRepositories: []string{"acme/docs"}, Level: domain.MemberAgent,
	})
	wantCode(t, err, apperr.CodeForbidden, "уровень agent")

	// Репозиторий проекта, но не из области задачи.
	if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/infra"}); err != nil {
		t.Fatal(err)
	}
	_, err = engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		RemoveRepositories: []string{"acme/infra"}, Level: domain.MemberMaintainer,
	})
	wantCode(t, err, apperr.CodeInvalidInput, "не из области")

	// Репозиторий, которого нет в проекте вовсе.
	_, err = engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		RemoveRepositories: []string{"acme/nowhere"}, Level: domain.MemberMaintainer,
	})
	wantCode(t, err, apperr.CodeUnknownRepository, "не из проекта")

	// В репозитории с привязанным PR уже идёт работа задачи.
	if _, err := engine.LinkRepoRef(ctx, task.Key, system.ID, LinkRepoRefInput{
		Repository: "acme/docs", RefType: "pull_request", Ref: "7",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		RemoveRepositories: []string{"acme/api", "acme/docs"}, Level: domain.MemberMaintainer,
	})
	wantCode(t, err, apperr.CodeConflict, "привязанный PR")

	// Отказ откатывает и ту часть, что прошла бы сама по себе.
	again, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := scopeNames(again); len(names) != 3 {
		t.Fatalf("отказ изменил область: %v", names)
	}
	if n := countRows(t, ctx, store, fmt.Sprintf(
		`SELECT count(*) FROM task_events WHERE task_id = '%s' AND type = 'task.scope_updated'`, task.ID)); n != 0 {
		t.Fatalf("отказ оставил в ленте %d событий", n)
	}

	// Режим header — прав нет вовсе, и уровень не передаётся.
	if _, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		RemoveRepositories: []string{"acme/api"},
	}); err != nil {
		t.Fatalf("сужение без уровня: %v", err)
	}
}

// props разбирает свойства задачи из базы в карту.
func props(t *testing.T, task domain.Task) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(task.Properties, &m); err != nil {
		t.Fatalf("свойства %s: %v", task.Properties, err)
	}
	return m
}

// singleRepoTask — однорепозиторная задача на acme/api при заведённом acme/web.
func singleRepoTask(t *testing.T, ctx context.Context, engine *Engine, system domain.Principal, properties string) domain.Task {
	t.Helper()
	for _, repo := range []string{"acme/api", "acme/web"} {
		if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: repo}); err != nil {
			t.Fatal(err)
		}
	}
	in := postgres.CreateTaskInput{Title: "Один репозиторий", Repositories: []string{"acme/api"}}
	if properties != "" {
		in.Properties = json.RawMessage(properties)
	}
	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{CreateTaskInput: in})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// CLAB-353, AC-1, AC-5, AC-9: multi_repo и второй репозиторий — одним
// запросом; свойства сливаются с текущими (type остаётся), событие одно.
func TestUpdateScopeEnablesMultiRepoWithAdd(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	task := singleRepoTask(t, ctx, engine, system, "")
	before := props(t, task)

	// Без multi_repo расширение отбивается, область и свойства целы.
	_, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		AddRepositories: []string{"acme/web"}, Level: domain.MemberMaintainer,
	})
	wantCode(t, err, apperr.CodeScopeExpansionForbidden, "без multi_repo")

	out, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		AddRepositories: []string{"acme/web"}, Note: "нужен фронт",
		Properties: json.RawMessage(`{"multi_repo":true}`), Level: domain.MemberMaintainer,
	})
	if err != nil {
		t.Fatalf("расширение с multi_repo: %v", err)
	}
	if names, _ := scopeNames(out); len(names) != 2 {
		t.Fatalf("область %v", names)
	}
	got := props(t, out)
	if got["multi_repo"] != true || len(got) != len(before)+1 {
		t.Fatalf("свойства до %v, после %v", before, got)
	}
	for k, v := range before {
		if got[k] != v {
			t.Fatalf("ключ %s потерян: до %v, после %v", k, before, got)
		}
	}
	if out.Type() != task.Type() {
		t.Fatalf("type %q стал %q", task.Type(), out.Type())
	}

	events, err := store.ListEvents(ctx, store.DB(), task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type != "task.scope_updated" {
			continue
		}
		n++
		var p struct {
			MultiRepo bool     `json:"multi_repo"`
			Added     []string `json:"added_repositories"`
			Note      string   `json:"note"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if !p.MultiRepo || len(p.Added) != 1 || p.Added[0] != "acme/web" || p.Note != "нужен фронт" {
			t.Fatalf("событие: %s", e.Payload)
		}
	}
	if n != 1 {
		t.Fatalf("событий task.scope_updated %d, ждали 1 (отказ не пишет событий)", n)
	}
}

// CLAB-353, AC-5: слияние. Названные ключи меняются, не названные остаются,
// null снимает ключ; присланный вместе с add_repositories кусок без multi_repo
// не лишает мультирепозиторную задачу права на расширение (проверка в той же
// транзакции идёт по слитым свойствам).
func TestUpdateScopeMergesProperties(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	task := singleRepoTask(t, ctx, engine, system, `{"type":"epic","needs_migration":true,"multi_repo":true}`)

	out, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		Properties: json.RawMessage(`{"needs_migration":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := props(t, out); got["needs_migration"] != false || got["type"] != "epic" || got["multi_repo"] != true || len(got) != 3 {
		t.Fatalf("после слияния %v", got)
	}

	// Расширение с неполным куском свойств: multi_repo не теряется.
	out, err = engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		AddRepositories: []string{"acme/web"}, Properties: json.RawMessage(`{"needs_migration":true}`),
		Level: domain.MemberMaintainer,
	})
	if err != nil {
		t.Fatalf("расширение с неполными свойствами: %v", err)
	}
	if names, _ := scopeNames(out); len(names) != 2 || !out.MultiRepo() {
		t.Fatalf("область %v, multi_repo %v", names, out.MultiRepo())
	}

	// null снимает ключ, остальные на месте.
	out, err = engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		Properties: json.RawMessage(`{"needs_migration":null}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := props(t, out); len(got) != 2 || got["type"] != "epic" || got["multi_repo"] != true {
		t.Fatalf("после null %v", got)
	}

	// Поле null и пустой запрос свойств не трогают.
	before := string(out.Properties)
	out, err = engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		Properties: json.RawMessage(`null`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Properties) != before {
		t.Fatalf("null изменил свойства: %s -> %s", before, out.Properties)
	}
	_ = store
}

// CLAB-353, AC-6: properties — только объект.
func TestUpdateScopeRejectsNonObjectProperties(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	task := singleRepoTask(t, ctx, engine, system, "")

	for _, bad := range []string{`[]`, `"multi_repo"`, `42`, `true`, `["multi_repo"]`} {
		_, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{Properties: json.RawMessage(bad)})
		wantCode(t, err, apperr.CodeInvalidInput, "properties="+bad)
	}
	again, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Properties) != string(task.Properties) {
		t.Fatalf("отказ изменил свойства: %s -> %s", task.Properties, again.Properties)
	}
}

// CLAB-353, AC-4: multi_repo=false при двух репозиториях в области — отказ без
// 5xx и без изменений; вместе с remove, после которого остался один, проходит.
func TestUpdateScopeDisablesMultiRepo(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	task := singleRepoTask(t, ctx, engine, system, `{"multi_repo":true}`)
	if _, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		AddRepositories: []string{"acme/web"}, Level: domain.MemberMaintainer,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		Properties: json.RawMessage(`{"multi_repo":false}`), Level: domain.MemberMaintainer,
	})
	if err == nil || apperr.As(err).Code == apperr.CodeInternal {
		t.Fatalf("ждали отказ клиентского уровня, получили %v", err)
	}
	again, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := scopeNames(again); len(names) != 2 || !again.MultiRepo() {
		t.Fatalf("отказ изменил задачу: %v, multi_repo %v", names, again.MultiRepo())
	}

	out, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		RemoveRepositories: []string{"acme/web"}, Properties: json.RawMessage(`{"multi_repo":false}`),
		Level: domain.MemberMaintainer,
	})
	if err != nil {
		t.Fatalf("выключение вместе с сужением: %v", err)
	}
	if names, _ := scopeNames(out); len(names) != 1 || out.MultiRepo() {
		t.Fatalf("область %v, multi_repo %v", names, out.MultiRepo())
	}
}

// CLAB-391, AC-8, пересмотрено CLAB-411 (K-2): расширение области — человек,
// сопровождающий или сотрудник (вид user, уровень agent). Отказ боту (agent
// без вида — внутренний вызов — тоже бот) — до транзакции, свойства из того
// же вызова не применяются, событий нет; без add_repositories бот свойства
// по-прежнему правит; viewer — отказ по уровню; пустой уровень (внутренние
// вызовы) правила не включает.
func TestUpdateScopeExpansionNeedsMaintainer(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	task := singleRepoTask(t, ctx, engine, system, "")

	for _, c := range []struct {
		level domain.MemberRole
		kind  domain.PrincipalKind
		text  []string
	}{
		{domain.MemberAgent, "", []string{"решение человека", "сопровождающ", "сотрудник"}},
		{domain.MemberAgent, domain.PrincipalAgent, []string{"решение человека", "сопровождающ", "сотрудник"}},
		{domain.MemberViewer, domain.PrincipalUser, []string{"нужен уровень agent"}},
	} {
		_, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
			AddRepositories: []string{"acme/web"}, Properties: json.RawMessage(`{"multi_repo":true}`),
			Level: c.level, Kind: c.kind,
		})
		wantCode(t, err, apperr.CodeForbidden, "расширение уровнем "+string(c.level)+" вида "+string(c.kind))
		msg := apperr.As(err).Message
		for _, want := range c.text {
			if !strings.Contains(msg, want) || strings.HasPrefix(msg, ExecutorRefusal) {
				t.Fatalf("текст отказа %s/%s: %q", c.level, c.kind, msg)
			}
		}
	}
	again, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := scopeNames(again); len(names) != 1 || again.MultiRepo() {
		t.Fatalf("отказ изменил задачу: %v, multi_repo %v", names, again.MultiRepo())
	}
	if n := countRows(t, ctx, store, fmt.Sprintf(
		`SELECT count(*) FROM task_events WHERE task_id = '%s' AND type = 'task.scope_updated'`, task.ID)); n != 0 {
		t.Fatalf("отказ оставил в ленте %d событий", n)
	}

	// Свойства без репозиториев — agent вправе.
	if _, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
		Properties: json.RawMessage(`{"multi_repo":true}`), Level: domain.MemberAgent,
	}); err != nil {
		t.Fatalf("свойства agent: %v", err)
	}

	// maintainer, admin, сотрудник и пустой уровень расширяют.
	for _, repo := range []string{"acme/docs", "acme/infra", "acme/ops"} {
		if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: repo}); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []struct {
		level domain.MemberRole
		kind  domain.PrincipalKind
	}{{domain.MemberMaintainer, ""}, {domain.MemberAdmin, ""}, {domain.MemberAgent, domain.PrincipalUser}, {"", ""}} {
		level := c.level
		repo := []string{"acme/web", "acme/docs", "acme/ops", "acme/infra"}[i]
		out, err := engine.UpdateScope(ctx, task.Key, system.ID, UpdateScopeInput{
			AddRepositories: []string{repo}, Level: level, Kind: c.kind,
		})
		if err != nil {
			t.Fatalf("расширение уровнем %q: %v", level, err)
		}
		if names, _ := scopeNames(out); !slices.Contains(names, repo) {
			t.Fatalf("уровень %q: область %v", level, names)
		}
	}
}
