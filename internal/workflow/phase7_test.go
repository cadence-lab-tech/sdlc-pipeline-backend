package workflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	alice := identityOf(t, store, "alice")
	mustActor(t, store, "user:bob")

	// Закрытый режим: личная команда заводится, но права на проекты у неё
	// нет — его выдаёт администратор.
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

	// Открытый режим выше флага личной команды: проект уходит в неё.
	auth.OpenProjectCreation(true)
	owner, err := auth.OwnerTeamFor(ctx, alice, "")
	if err != nil {
		t.Fatalf("открытый режим: %v", err)
	}
	me, err = auth.Me(ctx, alice)
	if err != nil || !me.CanCreateProjects {
		t.Fatalf("в открытом режиме проект заводит любой: %v %+v", err, me)
	}
	project, err := engine.CreateProject(ctx, alice.Principal.ID, postgres.CreateProjectInput{Key: "ALICE", Name: "Проект Алисы", OwnerTeamID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if project.OwnerTeamKey == nil || *project.OwnerTeamKey != "alice" {
		t.Fatalf("проект не в личной команде: %+v", project)
	}
	// Создатель — администратор проекта: добавляет коллег сам.
	if err := auth.Require(ctx, alice, project.ID, domain.MemberAdmin); err != nil {
		t.Fatalf("создатель не администратор: %v", err)
	}
	if _, err := auth.SetMember(ctx, alice, "ALICE", "user:bob", domain.MemberMaintainer, nil); err != nil {
		t.Fatalf("создатель добавляет коллегу: %v", err)
	}
	// Незаведённого человека добавить нельзя: принципал по хэндлу не
	// заводится (решение 0033).
	if _, err := auth.SetMember(ctx, alice, "ALICE", "user:nobody", domain.MemberViewer, nil); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("участник из воздуха: %v", err)
	}
	// Явно названная команда проверяется по-прежнему: лидом нужно быть.
	if _, err := auth.OwnerTeamFor(ctx, alice, "no-such-team"); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("команда проверяется: %v", err)
	}
}

// Вход CLI через браузер (решение 0032): CLI заводит запрос, человек
// подтверждает код своей сессией, CLI забирает токен ровно один раз.
func TestCLILoginDeviceFlow(t *testing.T) {
	ctx := context.Background()
	_, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)

	start, err := auth.StartCLILogin(ctx, "clab на macbook", 30)
	if err != nil {
		t.Fatalf("старт входа: %v", err)
	}
	if len(start.UserCode) != 9 || start.UserCode[4] != '-' {
		t.Fatalf("код для человека: %q", start.UserCode)
	}

	// До подтверждения — pending, и токена нет.
	poll, err := auth.PollCLILogin(ctx, start.PollSecret)
	if err != nil || poll.Status != "pending" || poll.Token != "" {
		t.Fatalf("опрос до подтверждения: %+v %v", poll, err)
	}
	if _, err := auth.PollCLILogin(ctx, "sdlc_wrong"); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("чужой секрет: %v", err)
	}

	// Без входа подтверждать нельзя; код принимается и строчными без дефиса.
	if _, err := auth.CLILoginByCode(ctx, Identity{Principal: admin.Principal}, start.UserCode); apperr.As(err).Code != apperr.CodeUnauthenticated {
		t.Fatalf("без токена: %v", err)
	}
	loose := strings.ToLower(strings.ReplaceAll(start.UserCode, "-", ""))
	shown, err := auth.CLILoginByCode(ctx, admin, loose)
	if err != nil || shown.Label != "clab на macbook" || shown.TokenDays != 30 {
		t.Fatalf("запрос по коду: %+v %v", shown, err)
	}
	approved, err := auth.ApproveCLILogin(ctx, admin, loose, true)
	if err != nil || approved.Status(time.Now()) != "approved" {
		t.Fatalf("подтверждение: %+v %v", approved, err)
	}
	if _, err := auth.ApproveCLILogin(ctx, admin, start.UserCode, true); apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("повторное подтверждение: %v", err)
	}

	// Токен приходит один раз и принадлежит подтвердившему.
	poll, err = auth.PollCLILogin(ctx, start.PollSecret)
	if err != nil || poll.Status != "approved" || poll.Token == "" {
		t.Fatalf("опрос после подтверждения: %+v %v", poll, err)
	}
	if poll.Record.Name != "clab на macbook" || poll.Record.ExpiresAt == nil || poll.Record.TokenKind != domain.TokenAPI {
		t.Fatalf("запись токена: %+v", poll.Record)
	}
	id, err := auth.Authenticate(ctx, poll.Token)
	if err != nil || id.Principal.ID != admin.Principal.ID {
		t.Fatalf("токен CLI: %+v %v", id, err)
	}
	again, err := auth.PollCLILogin(ctx, start.PollSecret)
	if err != nil || again.Status != "consumed" || again.Token != "" {
		t.Fatalf("повторный опрос: %+v %v", again, err)
	}

	// Отказ закрывает запрос: CLI видит denied.
	start, err = auth.StartCLILogin(ctx, "clab на ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.ApproveCLILogin(ctx, admin, start.UserCode, false); err != nil {
		t.Fatalf("отказ: %v", err)
	}
	if poll, err := auth.PollCLILogin(ctx, start.PollSecret); err != nil || poll.Status != "denied" {
		t.Fatalf("опрос после отказа: %+v %v", poll, err)
	}
}
