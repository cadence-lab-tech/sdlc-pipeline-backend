package workflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Выход к коллегам (решение 0030) по настоящей схеме: роли специалистов
// выдаются участнику, вход через GitHub, проект заводит любой вошедший.

// Роли специалистов участника ограничивают захват так же, как роли токена,
// а вместе они пересекаются: токен может сузить рамки участия, но не
// расширить их.
func TestMemberRolesBoundClaims(t *testing.T) {
	ctx := context.Background()
	store, engine, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	pid := projectID(t, store)

	if _, err := auth.SetMember(ctx, admin, "SDLC", "agent:bot", domain.MemberAgent, []string{"reviewer"}); err != nil {
		t.Fatalf("участник с ролью: %v", err)
	}
	if _, err := auth.SetMember(ctx, admin, "SDLC", "agent:bot", domain.MemberAgent, []string{"no-such-role"}); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("несуществующая роль обязана отбиваться: %v", err)
	}
	// Токен без ограничений: рамки задаёт участие.
	issued, err := auth.IssueToken(ctx, admin, IssueTokenInput{Name: "bot", Principal: "agent:bot", ProjectKey: "SDLC"})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := auth.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := auth.AllowedRoles(ctx, bot, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 1 || allowed[0] != "reviewer" {
		t.Fatalf("роли участника: %v", allowed)
	}

	if _, err := engine.Execute(ctx, "SDLC-1", "start", admin.Principal.ID, ""); err != nil {
		t.Fatalf("старт задачи: %v", err)
	}
	_, err = engine.ClaimWith(ctx, "SDLC-1", bot.Principal.ID, "test", ClaimOptions{AllowedRoles: allowed})
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("этап аналитика ревьюеру не достаётся: %v", err)
	}

	// Пересечение с токеном: участник может analyst и reviewer, токен —
	// только reviewer. Остаётся reviewer.
	if _, err := auth.SetMember(ctx, admin, "SDLC", "agent:bot", domain.MemberAgent, []string{"analyst", "reviewer"}); err != nil {
		t.Fatal(err)
	}
	narrow, err := auth.IssueToken(ctx, admin, IssueTokenInput{
		Name: "narrow", Principal: "agent:bot", ProjectKey: "SDLC", RoleKeys: []string{"reviewer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	narrowBot, err := auth.Authenticate(ctx, narrow.Token)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err = auth.AllowedRoles(ctx, narrowBot, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 1 || allowed[0] != "reviewer" {
		t.Fatalf("пересечение участия и токена: %v", allowed)
	}
	// А широкий токен идёт по рамкам участия и забирает аналитику.
	allowed, err = auth.AllowedRoles(ctx, bot, pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ClaimWith(ctx, "SDLC-1", bot.Principal.ID, "test", ClaimOptions{AllowedRoles: allowed}); err != nil {
		t.Fatalf("захват аналитики с ролью analyst: %v", err)
	}

	// Путь через команду без ограничений снимает рамки: пути складываются.
	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "bots", Name: "Боты"}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "bots", "agent:bot", domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetProjectTeam(ctx, admin, "SDLC", "bots", domain.MemberAgent, nil); err != nil {
		t.Fatal(err)
	}
	allowed, err = auth.AllowedRoles(ctx, bot, pid)
	if err != nil {
		t.Fatal(err)
	}
	if allowed != nil {
		t.Fatalf("команда без ограничений должна разрешать любые роли, а не %v", allowed)
	}

	// Команда с ролями складывается с поимённым участием.
	if _, err := auth.SetProjectTeam(ctx, admin, "SDLC", "bots", domain.MemberAgent, []string{"qa"}); err != nil {
		t.Fatal(err)
	}
	allowed, err = auth.AllowedRoles(ctx, bot, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 3 {
		t.Fatalf("объединение путей: %v", allowed)
	}

	// Список участников отдаёт роли: интерфейсу их показывать.
	members, err := auth.ListMembers(ctx, admin, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range members {
		if m.Handle == "bot" {
			found = len(m.RoleKeys) == 2
		}
	}
	if !found {
		t.Fatalf("роли участника не в списке: %+v", members)
	}
}

// fakeGitHubLogin отвечает за GitHub: обмен кода и профиль. Проверяется вход,
// а не поход в сеть.
func fakeGitHubLogin(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.FormValue("client_secret") != "s3cret" || r.FormValue("code") != "good-code" {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "gho_user", "token_type": "bearer"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_user" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "login": "Octo-Cat", "name": "Octo"})
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"email": "octo@users.noreply.github.com", "primary": false, "verified": true},
			{"email": "octo@acme.example", "primary": true, "verified": true},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestGitHubLoginIssuesSession(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	srv := fakeGitHubLogin(t)
	auth.UseGitHub(github.NewOAuth(srv.URL, srv.URL, "app-client", "s3cret"), time.Hour)

	settings := auth.GitHubSettings()
	if !settings.Enabled || settings.ClientID != "app-client" || settings.AuthorizationEndpoint != srv.URL+"/login/oauth/authorize" {
		t.Fatalf("настройки входа: %+v", settings)
	}

	session, err := auth.LoginGitHub(ctx, "good-code", "http://ui/login/github")
	if err != nil {
		t.Fatalf("вход: %v", err)
	}
	// Хэндл — логин GitHub, почта — основная подтверждённая из списка.
	if session.Principal.Handle != "octo-cat" || session.Principal.Email == nil || *session.Principal.Email != "octo@acme.example" {
		t.Fatalf("принципал: %+v", session.Principal)
	}
	if session.Record.TokenKind != domain.TokenSession {
		t.Fatalf("вид токена %q", session.Record.TokenKind)
	}
	id, err := auth.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatalf("сессия не аутентифицирует: %v", err)
	}
	// Вошедший без участия проекта не видит: доступ выдаёт администратор.
	if err := auth.Require(ctx, id, projectID(t, store), domain.MemberViewer); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("новый пользователь не должен видеть проект: %v", err)
	}
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:octo-cat", domain.MemberAgent, []string{"developer"}); err != nil {
		t.Fatal(err)
	}
	if err := auth.Require(ctx, id, projectID(t, store), domain.MemberAgent); err != nil {
		t.Fatalf("после добавления: %v", err)
	}

	// Повторный вход — тот же принципал: ключ — идентификатор GitHub.
	again, err := auth.LoginGitHub(ctx, "good-code", "")
	if err != nil {
		t.Fatal(err)
	}
	if again.Principal.ID != session.Principal.ID {
		t.Fatal("второй вход завёл нового принципала")
	}

	// Плохой код — отказ входа, а не внутренняя ошибка.
	if _, err := auth.LoginGitHub(ctx, "bad-code", ""); apperr.As(err).Code != apperr.CodeUnauthenticated {
		t.Fatalf("плохой код: %v", err)
	}
}

func TestOpenProjectCreation(t *testing.T) {
	ctx := context.Background()
	store, engine, _, auth, _ := setupPhase4(t, AuthToken)
	adminIdentity(t, ctx, auth)
	alice := identityOf(t, store, "user:alice")

	// Закрытый режим: постороннему проект не завести.
	if _, err := auth.OwnerTeamFor(ctx, alice, ""); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("закрытый режим: %v", err)
	}
	me, err := auth.Me(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if me.CanCreateProjects {
		t.Fatal("в закрытом режиме кнопки быть не должно")
	}

	auth.OpenProjectCreation(true)
	owner, err := auth.OwnerTeamFor(ctx, alice, "")
	if err != nil || owner != nil {
		t.Fatalf("открытый режим: %v %v", err, owner)
	}
	me, err = auth.Me(ctx, alice)
	if err != nil || !me.CanCreateProjects {
		t.Fatalf("в открытом режиме проект заводит любой: %v %+v", err, me)
	}
	project, err := engine.CreateProject(ctx, alice.Principal.ID, postgres.CreateProjectInput{Key: "ALICE", Name: "Проект Алисы"})
	if err != nil {
		t.Fatal(err)
	}
	// Создатель — администратор проекта: добавляет коллег сам.
	if err := auth.Require(ctx, alice, project.ID, domain.MemberAdmin); err != nil {
		t.Fatalf("создатель не администратор: %v", err)
	}
	if _, err := auth.SetMember(ctx, alice, "ALICE", "user:bob", domain.MemberMaintainer, nil); err != nil {
		t.Fatalf("создатель добавляет коллегу: %v", err)
	}
	// Команда при открытом режиме остаётся необязательной, но и не
	// пропускает чужих: лидом команды нужно быть по-прежнему.
	if _, err := auth.OwnerTeamFor(ctx, alice, "no-such-team"); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("команда проверяется: %v", err)
	}
}
