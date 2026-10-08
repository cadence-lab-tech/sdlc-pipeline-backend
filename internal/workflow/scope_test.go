package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
