package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Отказ в учётных данных приложения (CLAB-78) — неверная настройка инстанса,
// а не недоступность репозитория: у него свой код и свой текст, и подбор
// установки на нём не уводит репозиторий на учётные данные проекта.

// expectAppRefusal — ошибка домена об отказе приложения: код не
// invalid_input и не internal, репозиторий не назван, сказано, что
// проверить, и ответ провайдера сохранён.
func expectAppRefusal(t *testing.T, err error, repo string) {
	t.Helper()
	e := apperr.As(err)
	if e.Code != apperr.CodeProviderMisconfigured {
		t.Fatalf("код %q вместо provider_misconfigured: %v", e.Code, err)
	}
	if strings.Contains(e.Message, repo) {
		t.Fatalf("в тексте отказа приложения назван репозиторий %s: %q", repo, e.Message)
	}
	for _, want := range []string{github.ErrAppRejected.Error(), "SDLC_GITHUB_APP_KEY_FILE",
		"SDLC_GITHUB_APP_ID", "часы машины", "Bad credentials"} {
		if !strings.Contains(e.Message, want) {
			t.Fatalf("в тексте отказа приложения нет %q: %q", want, e.Message)
		}
	}
	if strings.Contains(e.Message, "недоступен") {
		t.Fatalf("отказ приложения подан как недоступность: %q", e.Message)
	}
}

func repositoryNames(t *testing.T, store *postgres.Store) map[string]bool {
	t.Helper()
	ctx := context.Background()
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	repos, err := store.ListRepositories(ctx, store.DB(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range repos {
		out[r.Owner+"/"+r.Name] = true
	}
	return out
}

func TestAppRejectionIsNotRepositoryRefusal(t *testing.T) {
	ctx := context.Background()
	store, _, _, _, _ := setupPhase4(t, AuthHeader)
	gh := &fakeGitHub{allowed: map[string]bool{"acme/api": true}, appRejected: true}
	engine := NewEngine(store, testdb.Logger(), Options{StrictGates: true, GitHub: gh})
	_, team := ownerTeam(t, store)
	who := mustActor(t, store, "user:lead")
	for _, id := range []int64{503, 504} {
		if _, err := store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
			InstallationID: id, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
			OwnerTeamID: team.ID, ConnectedBy: who.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// AC-4: выбор из списка установки.
	_, err := engine.AttachRepositories(ctx, "SDLC", AttachRepositoriesInput{
		InstallationID: 503, Repositories: []RepoPick{{Owner: "acme", Name: "api"}},
	})
	expectAppRefusal(t, err, "acme/api")
	if repositoryNames(t, store)["acme/api"] {
		t.Fatal("репозиторий привязан при отказе приложения")
	}

	// AC-5: привязка по координатам с явной установкой.
	_, _, err = engine.AddRepository(ctx, "SDLC", AddRepositoryInput{
		Repository: "acme/api", InstallationID: ptr(int64(503)),
	})
	expectAppRefusal(t, err, "acme/api")

	// AC-6: подбор установки останавливается на первом же отказе приложения
	// и не уводит репозиторий на учётные данные проекта.
	gh.calls = 0
	_, _, err = engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/api"})
	expectAppRefusal(t, err, "acme/api")
	if gh.calls != 1 {
		t.Fatalf("провайдера спросили %d раз: перебор установок не остановлен", gh.calls)
	}
	if repositoryNames(t, store)["acme/api"] {
		t.Fatal("репозиторий привязан на учётные данные проекта мимо отказа приложения")
	}

	// AC-7: приложение принято, а репозиторий установке недоступен — как
	// раньше: invalid_input с репозиторием и установкой в тексте.
	gh.appRejected = false
	_, err = engine.AttachRepositories(ctx, "SDLC", AttachRepositoriesInput{
		InstallationID: 503, Repositories: []RepoPick{{Owner: "acme", Name: "secrets"}},
	})
	if e := apperr.As(err); e.Code != apperr.CodeInvalidInput ||
		!strings.Contains(e.Message, "репозиторий acme/secrets недоступен установке 503") {
		t.Fatalf("обычный отказ установки: %v", err)
	}
	// И репозиторий вне всех установок по-прежнему уходит на учётные данные
	// проекта: остановка перебора касается только отказа приложения.
	repo, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/other"})
	if err != nil {
		t.Fatal(err)
	}
	if repo.InstallationID != nil {
		t.Fatalf("репозиторию вне установок приписана установка: %+v", repo)
	}
}

// AC-8: проверка доступа не падает целиком, а у репозитория на установке
// пишет в колонку ошибки отказ приложения; репозиторий на секрете отвечает
// своим результатом.
func TestAccessCheckShowsAppRejection(t *testing.T) {
	ctx := context.Background()
	store, _, _, _, _ := setupPhase4(t, AuthHeader)
	gh := &fakeGitHub{grants: map[string]bool{
		"505 acme/api":       true,
		"repo-x acme/secret": true,
	}}
	engine := NewEngine(store, testdb.Logger(), Options{StrictGates: true, GitHub: gh})
	_, team := ownerTeam(t, store)
	who := mustActor(t, store, "user:lead")
	if _, err := store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
		InstallationID: 505, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: team.ID, ConnectedBy: who.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/api"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{
		Repository: "acme/secret", CredentialRef: "repo-x",
	}); err != nil {
		t.Fatal(err)
	}

	gh.appRejected = true
	status, err := engine.CheckGitHubAccess(ctx, "SDLC")
	if err != nil {
		t.Fatalf("проверка доступа упала целиком: %v", err)
	}
	rows := map[string]RepoAccessResult{}
	for _, r := range status.Repositories {
		rows[r.Repository] = r
	}
	api, ok := rows["acme/api"]
	if !ok || api.OK || api.Source != github.SourceRepositoryInstallation {
		t.Fatalf("строка репозитория на установке: %+v", api)
	}
	expectAppRefusal(t, apperr.New(apperr.CodeProviderMisconfigured, "%s", api.Error), "acme/api")
	if secret := rows["acme/secret"]; !secret.OK || secret.Source != github.SourceRepositorySecret {
		t.Fatalf("строка репозитория на секрете: %+v", secret)
	}
}

// Перевод отказа знает класс по метке, а не по тексту: обёрнутая метка —
// отказ приложения, прочее — нет.
func TestAppRefusalClassifiesByMarker(t *testing.T) {
	wrapped := errors.Join(errors.New("что-то сверху"), github.ErrAppRejected)
	if appRefusal(wrapped) == nil {
		t.Fatal("обёрнутая метка не узнана")
	}
	if appRefusal(errors.New(github.ErrAppRejected.Error())) != nil {
		t.Fatal("отказ узнан по тексту, а не по метке")
	}
	if appRefusal(github.ErrNotFound) != nil {
		t.Fatal("«не найдено» принято за отказ приложения")
	}
}
