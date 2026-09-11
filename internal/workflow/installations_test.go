package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Установки на арендаторе (решение 0034): установка принадлежит команде,
// репозиторий помнит свою, вебхуки идут сначала по установке, отзыв на
// стороне GitHub останавливает индексацию, не трогая собранное.

func ownerTeam(t *testing.T, store *postgres.Store) (domain.Project, domain.Team) {
	t.Helper()
	ctx := context.Background()
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if project.OwnerTeamID == nil {
		t.Fatal("у демонстрационного проекта нет команды-владельца")
	}
	team, err := store.GetTeamByKey(ctx, store.DB(), deref(project.OwnerTeamKey))
	if err != nil {
		t.Fatal(err)
	}
	return project, team
}

func targetNames(t *testing.T, store *postgres.Store) map[string]bool {
	t.Helper()
	targets, err := store.IndexTargets(context.Background(), store.DB())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, tg := range targets {
		out[tg.Repository()] = true
	}
	return out
}

func TestInstallationLifecycleAndWebhookRouting(t *testing.T) {
	ctx := context.Background()
	store, _, _, _, _ := setupPhase4(t, AuthHeader)
	gh := &fakeGitHub{allowed: map[string]bool{"acme/api": true, "acme/web": true}}
	engine := NewEngine(store, testdb.Logger(), Options{StrictGates: true, GitHub: gh})
	_, team := ownerTeam(t, store)
	who := mustActor(t, store, "user:lead")

	inst, err := store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
		InstallationID: 501, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: team.ID, ConnectedBy: who.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inst.Active() || inst.OwnerTeamKey != team.Key {
		t.Fatalf("установка записана не так: %+v", inst)
	}

	// Та же установка другой команде не достаётся: идентификаторы идут
	// подряд, и приписать себе чужую было бы способом читать чужие репозитории.
	other, err := store.CreateTeam(ctx, store.DB(), postgres.CreateTeamInput{Key: "other", Name: "Другие", CreatedBy: who.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
		InstallationID: 501, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: other.ID, ConnectedBy: who.ID,
	})
	if apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("чужая установка переписалась: %v", err)
	}

	// Привязка пачкой: репозитории помнят установку, вне установки — отказ.
	res, err := engine.AttachRepositories(ctx, "SDLC", AttachRepositoriesInput{
		InstallationID: 501,
		Repositories: []RepoPick{
			{Owner: "acme", Name: "api", DefaultBranch: "main", Private: true, CloneURL: "https://github.com/acme/api.git"},
			{Owner: "acme", Name: "web", DefaultBranch: "main"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 || len(res.Repositories) != 2 {
		t.Fatalf("привязка пачкой: %+v", res)
	}
	for _, r := range res.Repositories {
		if r.InstallationID == nil || *r.InstallationID != 501 {
			t.Fatalf("репозиторий %s не помнит установку: %+v", r.Name, r)
		}
	}
	_, err = engine.AttachRepositories(ctx, "SDLC", AttachRepositoriesInput{
		InstallationID: 501, Repositories: []RepoPick{{Owner: "acme", Name: "secrets"}},
	})
	if apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("репозиторий вне установки привязался: %v", err)
	}
	_, err = engine.AttachRepositories(ctx, "SDLC", AttachRepositoriesInput{
		InstallationID: 777, Repositories: []RepoPick{{Owner: "acme", Name: "api"}},
	})
	if apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("неизвестная установка принята: %v", err)
	}

	push := func(installationID int64, repo, delivery string) WebhookOutcome {
		t.Helper()
		owner, name, _ := strings.Cut(repo, "/")
		body := fmt.Sprintf(`{
			"ref": "refs/heads/main", "after": "%s", "deleted": false,
			"repository": {"name": %q, "owner": {"login": %q}},
			"installation": {"id": %d}
		}`, strings.Repeat("b", 40), name, owner, installationID)
		out, err := engine.HandleGitHubEvent(ctx, "push", delivery, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Сначала установка, потом координаты: событие своей установки ставит
	// прогон, чужой — нет, без установки — не обслуживается.
	if out := push(501, "acme/api", "d-own"); out.Queued != 1 {
		t.Fatalf("событие своей установки: %+v", out)
	}
	if out := push(7, "acme/api", "d-foreign"); out.Queued != 0 || out.Ignored == "" {
		t.Fatalf("событие чужой установки дошло: %+v", out)
	}
	if out := push(0, "acme/api", "d-manual"); out.Queued != 0 || out.Ignored == "" {
		t.Fatalf("доставка без установки обслужена: %+v", out)
	}

	event := func(name, body, delivery string) WebhookOutcome {
		t.Helper()
		out, err := engine.HandleGitHubEvent(ctx, name, delivery, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Репозиторий убрали из установки: индексация стоит, строка остаётся.
	out := event("installation_repositories", `{"action":"removed","installation":{"id":501},
		"repositories_removed":[{"full_name":"acme/web"}]}`, "d-removed")
	if out.Touched != 1 {
		t.Fatalf("убранный репозиторий не помечен: %+v", out)
	}
	if names := targetNames(t, store); names["acme/web"] || !names["acme/api"] {
		t.Fatalf("цели индексации после снятия репозитория: %v", names)
	}
	repos, _ := store.ListRepositories(ctx, store.DB(), res.Repositories[0].ProjectID)
	for _, r := range repos {
		if r.Name == "web" && r.UnavailableAt == nil {
			t.Fatal("unavailable_at не проставлен")
		}
	}
	event("installation_repositories", `{"action":"added","installation":{"id":501},
		"repositories_added":[{"full_name":"acme/web"}]}`, "d-added")
	if names := targetNames(t, store); !names["acme/web"] {
		t.Fatalf("возвращённый репозиторий не индексируется: %v", names)
	}

	// Приостановка останавливает все репозитории установки, возобновление
	// возвращает; снятие — навсегда, пока не подключат заново.
	event("installation", `{"action":"suspend","installation":{"id":501}}`, "d-suspend")
	if names := targetNames(t, store); names["acme/api"] || names["acme/web"] {
		t.Fatalf("репозитории приостановленной установки индексируются: %v", names)
	}
	if _, found, err := store.ClaimIndexRun(ctx); err != nil || found {
		t.Fatalf("прогон приостановленной установки взят в работу: %v %v", found, err)
	}
	event("installation", `{"action":"unsuspend","installation":{"id":501}}`, "d-unsuspend")
	if names := targetNames(t, store); !names["acme/api"] {
		t.Fatalf("репозитории после возобновления: %v", names)
	}
	if out := event("installation", `{"action":"deleted","installation":{"id":999}}`, "d-unknown"); out.Touched != 0 || out.Ignored == "" {
		t.Fatalf("неизвестная установка обработана: %+v", out)
	}
	event("installation", `{"action":"deleted","installation":{"id":501}}`, "d-deleted")
	status, err := engine.ProjectGitHub(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Installations) != 1 || status.Installations[0].RemovedAt == nil {
		t.Fatalf("снятая установка не отмечена: %+v", status.Installations)
	}
	_, err = engine.AttachRepositories(ctx, "SDLC", AttachRepositoriesInput{
		InstallationID: 501, Repositories: []RepoPick{{Owner: "acme", Name: "api"}},
	})
	if apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("снятая установка принимает репозитории: %v", err)
	}
	// Повторное подключение той же командой оживляет установку.
	if _, err := store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
		InstallationID: 501, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: team.ID, ConnectedBy: who.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if names := targetNames(t, store); !names["acme/api"] {
		t.Fatalf("после повторного подключения: %v", names)
	}
}

func TestAddRepositoryPicksTeamInstallation(t *testing.T) {
	ctx := context.Background()
	store, _, _, _, _ := setupPhase4(t, AuthHeader)
	gh := &fakeGitHub{allowed: map[string]bool{"acme/api": true}}
	engine := NewEngine(store, testdb.Logger(), Options{StrictGates: true, GitHub: gh})
	_, team := ownerTeam(t, store)
	who := mustActor(t, store, "user:lead")
	if _, err := store.UpsertInstallation(ctx, store.DB(), postgres.InstallationInput{
		InstallationID: 502, AccountLogin: "acme", AccountID: 9, AccountType: "Organization",
		OwnerTeamID: team.ID, ConnectedBy: who.ID,
	}); err != nil {
		t.Fatal(err)
	}

	// По координатам: установка команды подбирается сама, и `clab repo add`
	// получает ту же маршрутизацию вебхуков, что выбор из списка.
	repo, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/api"})
	if err != nil {
		t.Fatal(err)
	}
	if repo.InstallationID == nil || *repo.InstallationID != 502 {
		t.Fatalf("установка не подобрана: %+v", repo)
	}
	// Недоступный установке репозиторий остаётся на учётных данных проекта —
	// у проекта их нет, и привязка проходит без проверки, как раньше.
	repo, _, err = engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/other"})
	if err != nil {
		t.Fatal(err)
	}
	if repo.InstallationID != nil {
		t.Fatalf("репозиторию вне установки приписана установка: %+v", repo)
	}
	// Явная установка проверяется: чужая или неизвестная не проходит.
	_, _, err = engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/api", InstallationID: ptr(int64(999))})
	if apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("неизвестная установка принята: %v", err)
	}
}

// fakeGitHubSite — GitHub на время теста: обмен кода, установки вошедшего и
// репозитории установки, которые он видит сам.
func fakeGitHubSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != "good-code" {
			json.NewEncoder(w).Encode(map[string]any{"error": "bad_verification_code"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "user-token"})
	})
	mux.HandleFunc("/user/installations", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"installations": []map[string]any{
			{"id": 601, "account": map[string]any{"login": "acme", "id": 9, "type": "Organization"}},
			{"id": 602, "account": map[string]any{"login": "alice", "id": 10, "type": "User"}},
		}})
	})
	mux.HandleFunc("/user/installations/601/repositories", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"repositories": []map[string]any{
			{"name": "api", "default_branch": "main", "private": true,
				"clone_url": "https://github.com/acme/api.git", "owner": map[string]any{"login": "acme"}},
		}})
	})
	mux.HandleFunc("/user/installations/602/repositories", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"repositories": []map[string]any{
			{"name": "dotfiles", "default_branch": "main", "owner": map[string]any{"login": "alice"}},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestConnectGitHubProvesInstallationByUserToken(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	site := fakeGitHubSite(t)
	auth.UseGitHub(github.NewOAuth(site.URL, site.URL, "client", "secret"), time.Hour).GitHubApp("clab")
	if got := auth.GitHubSettings().InstallURL; got != site.URL+"/apps/clab/installations/new" {
		t.Fatalf("адрес установки: %s", got)
	}
	admin := adminIdentity(t, ctx, auth)
	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "acme", Name: "Acme"}); err != nil {
		t.Fatal(err)
	}

	// Установка, которой вошедший не видит, не подключается, каким бы ни был
	// её номер; своя — записывается, и репозитории приходят на выбор только
	// из установок команды.
	_, err := auth.ConnectGitHub(ctx, admin, ConnectGitHubInput{TeamKey: "acme", Code: "good-code", InstallationID: ptr(int64(777))})
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("чужая установка подключилась: %v", err)
	}
	conn, err := auth.ConnectGitHub(ctx, admin, ConnectGitHubInput{TeamKey: "acme", Code: "good-code", InstallationID: ptr(int64(601))})
	if err != nil {
		t.Fatal(err)
	}
	if conn.Connected == nil || conn.Connected.AccountLogin != "acme" || len(conn.Installations) != 1 {
		t.Fatalf("установка не записана: %+v", conn)
	}
	if len(conn.Repositories) != 1 || conn.Repositories[0].Owner != "acme" || conn.Repositories[0].InstallationID != 601 {
		t.Fatalf("репозитории на выбор: %+v", conn.Repositories)
	}
	if _, err := auth.ConnectGitHub(ctx, admin, ConnectGitHubInput{TeamKey: "acme", Code: "bad-code"}); apperr.As(err).Code != apperr.CodeUnauthenticated {
		t.Fatalf("плохой код принят: %v", err)
	}

	// Участник команды перечитывает список, но подключить установку может
	// только лид.
	bob := identityOf(t, store, "bob")
	if _, err := auth.SetTeamMember(ctx, admin, "acme", "user:bob", domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.ConnectGitHub(ctx, bob, ConnectGitHubInput{TeamKey: "acme", Code: "good-code", InstallationID: ptr(int64(602))}); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("участник подключил установку: %v", err)
	}
	conn, err = auth.ConnectGitHub(ctx, bob, ConnectGitHubInput{TeamKey: "acme", Code: "good-code"})
	if err != nil {
		t.Fatal(err)
	}
	if len(conn.Repositories) != 1 {
		t.Fatalf("участник видит не то: %+v", conn.Repositories)
	}
	list, err := auth.ListInstallations(ctx, bob, "acme")
	if err != nil || len(list) != 1 {
		t.Fatalf("список установок: %v %v", list, err)
	}
	outsider := identityOf(t, store, "carol")
	if _, err := auth.ListInstallations(ctx, outsider, "acme"); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("посторонний видит установки: %v", err)
	}
}
