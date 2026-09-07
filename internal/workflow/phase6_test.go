package workflow

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/oidc"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
)

// Многокомандность (решение 0027) по настоящей схеме: права приходят в проект
// и поимённо, и через команду, а проект принадлежит команде.

// identityOf собирает актора без токена: проверяется уровень прав, а не
// транспорт, которым принципал доказал, кто он.
func identityOf(t *testing.T, store *postgres.Store, handle string) Identity {
	t.Helper()
	p, err := store.ResolveActor(context.Background(), store.DB(), handle, domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	return Identity{Principal: p}
}

// adminIdentity — администратор системы: команды заводит только он.
func adminIdentity(t *testing.T, ctx context.Context, auth *Auth) Identity {
	t.Helper()
	if err := auth.Bootstrap(ctx, "admin", "bootstrap-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	id, err := auth.Authenticate(ctx, "bootstrap-token-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTeamLevelAddsToDirectMembership(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	pid := projectID(t, store)

	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "platform", Name: "Платформа"}); err != nil {
		t.Fatalf("создание команды: %v", err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "platform", "user:alice", domain.TeamRoleMember); err != nil {
		t.Fatalf("состав команды: %v", err)
	}
	alice := identityOf(t, store, "alice")

	// Пока команду не пустили в проект, участие в ней доступа не даёт.
	if err := auth.Require(ctx, alice, pid, domain.MemberViewer); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("команда без участия в проекте не должна давать доступ: %v", err)
	}

	if _, err := auth.SetProjectTeam(ctx, admin, "SDLC", "platform", domain.MemberMaintainer); err != nil {
		t.Fatalf("выдача команде уровня в проекте: %v", err)
	}
	if err := auth.Require(ctx, alice, pid, domain.MemberMaintainer); err != nil {
		t.Fatalf("уровень команды не дошёл до её участника: %v", err)
	}

	// Поимённое участие ниже командного не понижает права: уровень — максимум
	// из двух путей, а не последний назначенный.
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:alice", domain.MemberViewer); err != nil {
		t.Fatal(err)
	}
	role, err := auth.ProjectRole(ctx, alice, pid)
	if err != nil {
		t.Fatal(err)
	}
	if role != domain.MemberMaintainer {
		t.Fatalf("действующий уровень %q вместо maintainer", role)
	}

	// Обратное тоже верно: поимённый admin выше командного maintainer.
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:alice", domain.MemberAdmin); err != nil {
		t.Fatal(err)
	}
	if role, err = auth.ProjectRole(ctx, alice, pid); err != nil || role != domain.MemberAdmin {
		t.Fatalf("уровень %q (%v) вместо admin", role, err)
	}

	// Проект виден по участию команды, а не только по поимённому.
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:alice", domain.MemberViewer); err != nil {
		t.Fatal(err)
	}
	if err := auth.RemoveMember(ctx, admin, "SDLC", "user:alice"); err != nil {
		t.Fatal(err)
	}
	projects, err := auth.VisibleProjects(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Key != "SDLC" {
		t.Fatalf("проект команды не виден её участнику: %+v", projects)
	}

	// Команда в архиве прав не даёт: доступ уходит вместе с ней, а строки
	// участия остаются ради истории.
	if err := auth.ArchiveTeam(ctx, admin, "platform"); err != nil {
		t.Fatal(err)
	}
	if err := auth.Require(ctx, alice, pid, domain.MemberViewer); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("архивная команда продолжает давать доступ: %v", err)
	}
}

func TestTeamLeadCreatesProjectAndOwnsIt(t *testing.T) {
	ctx := context.Background()
	store, engine, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)

	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "platform", Name: "Платформа"}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "platform", "user:lead", domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "platform", "user:bob", domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	lead := identityOf(t, store, "lead")
	bob := identityOf(t, store, "bob")

	// Право заводить проекты у команды, а не у лида: пока его нет, не может
	// никто.
	if _, err := auth.OwnerTeamFor(ctx, lead, "platform"); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("команда без права завела проект: %v", err)
	}
	yes := true
	if _, err := auth.UpdateTeam(ctx, admin, "platform", UpdateTeamInput{CanCreateProjects: &yes}); err != nil {
		t.Fatal(err)
	}
	// Право выдаёт администратор системы, а не сама команда.
	if _, err := auth.UpdateTeam(ctx, lead, "platform", UpdateTeamInput{CanCreateProjects: &yes}); err != nil {
		t.Fatalf("повтор того же значения лидом — не изменение: %v", err)
	}
	no := false
	if _, err := auth.UpdateTeam(ctx, lead, "platform", UpdateTeamInput{CanCreateProjects: &no}); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("лид снял право команде сам: %v", err)
	}
	// Участник команды — не лид: проекты заводит лид.
	if _, err := auth.OwnerTeamFor(ctx, bob, "platform"); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("проект завёл не лид: %v", err)
	}

	owner, err := auth.OwnerTeamFor(ctx, lead, "platform")
	if err != nil {
		t.Fatalf("лид не смог завести проект: %v", err)
	}
	project, err := engine.CreateProject(ctx, lead.Principal.ID, postgres.CreateProjectInput{
		Key: "ACME", Name: "Проект команды", OwnerTeamID: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if project.OwnerTeamKey == nil || *project.OwnerTeamKey != "platform" {
		t.Fatalf("владелец проекта не проставлен: %+v", project)
	}

	// Лид команды-владельца — администратор проекта, даже когда его нет в
	// участниках: иначе он потерял бы проект, выведя себя из них.
	if err := auth.RemoveMember(ctx, admin, "ACME", "user:lead"); err != nil {
		t.Fatalf("вывести создателя из участников: %v", err)
	}
	if err := auth.Require(ctx, lead, project.ID, domain.MemberAdmin); err != nil {
		t.Fatalf("лид команды-владельца не администратор проекта: %v", err)
	}
	// Остальные участники команды доступа от владения не получают: его даёт
	// участие команды в проекте, и оно выдаётся явно.
	if err := auth.Require(ctx, bob, project.ID, domain.MemberViewer); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("владение раздало права всей команде: %v", err)
	}

	// Проект без команды заводит только администратор системы.
	if _, err := auth.OwnerTeamFor(ctx, lead, ""); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("проект без владельца завёл не администратор: %v", err)
	}
}

func TestLastTeamLeadIsProtected(t *testing.T) {
	ctx := context.Background()
	_, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)

	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "platform", Name: "Платформа"}); err != nil {
		t.Fatal(err)
	}
	// Создатель команды стал её лидом — иначе команда родилась бы неуправляемой.
	detail, err := auth.GetTeam(ctx, admin, "platform")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Members) != 1 || detail.Members[0].Role != domain.TeamRoleLead {
		t.Fatalf("состав новой команды: %+v", detail.Members)
	}

	if err := auth.RemoveTeamMember(ctx, admin, "platform", "user:admin"); apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("последнего лида вывели из команды: %v", err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "platform", "user:admin", domain.TeamRoleMember); apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("последнего лида понизили: %v", err)
	}

	if _, err := auth.SetTeamMember(ctx, admin, "platform", "user:alice", domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}
	if err := auth.RemoveTeamMember(ctx, admin, "platform", "user:admin"); err != nil {
		t.Fatalf("второго лида вывести можно: %v", err)
	}
}

func TestBootstrapTeamIsIdempotent(t *testing.T) {
	ctx := context.Background()
	_, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)

	for range 2 {
		if err := auth.BootstrapTeam(ctx, "admin", "core", "Ядро"); err != nil {
			t.Fatalf("бутстрап команды: %v", err)
		}
	}
	detail, err := auth.GetTeam(ctx, admin, "core")
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Team.CanCreateProjects {
		t.Fatal("первой команде не выдано право заводить проекты")
	}
	if len(detail.Members) != 1 || detail.Members[0].Handle != "admin" ||
		detail.Members[0].Role != domain.TeamRoleLead {
		t.Fatalf("состав первой команды: %+v", detail.Members)
	}
}

// ── вход через корпоративный провайдер ──────────────────────────────────────

// fakeIdP поднимает провайдера, отвечающего дискавери и ключами, и отдаёт
// функцию подписи ID-токена. Проверять разбор JWT на заглушке бессмысленно:
// смысл кода в том, что подпись сходится с ключом провайдера.
func fakeIdP(t *testing.T) (issuer string, sign func(claims map[string]any) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "use": "sig", "kid": "test",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	})

	sign = func(claims map[string]any) string {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
		body, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		signing := header + "." + base64.RawURLEncoding.EncodeToString(body)
		digest := sha256.Sum256([]byte(signing))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	return srv.URL, sign
}

func TestOIDCLoginIssuesSessionAndSyncsTeams(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	issuer, sign := fakeIdP(t)
	auth.UseOIDC(oidc.New(oidc.Config{Issuer: issuer, ClientID: "sdlc-ui"}), time.Hour)

	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "platform", Name: "Платформа"}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamIdPGroups(ctx, admin, "platform", []string{"sdlc-platform"}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetProjectTeam(ctx, admin, "SDLC", "platform", domain.MemberMaintainer); err != nil {
		t.Fatal(err)
	}

	claims := func(extra map[string]any) map[string]any {
		out := map[string]any{
			"iss": issuer, "aud": "sdlc-ui", "sub": "idp-42",
			"exp":   time.Now().Add(time.Hour).Unix(),
			"email": "Alice.Ivanova@acme.example", "name": "Алиса",
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	session, err := auth.Login(ctx, sign(claims(map[string]any{"groups": []string{"sdlc-platform"}})))
	if err != nil {
		t.Fatalf("вход: %v", err)
	}
	// Хэндл выводится из почты: в задачах и аудите человека зовут именем, а
	// не идентификатором провайдера.
	if session.Principal.Handle != "alice.ivanova" {
		t.Fatalf("хэндл %q", session.Principal.Handle)
	}
	if session.Record.TokenKind != domain.TokenSession {
		t.Fatalf("вид токена %q вместо session", session.Record.TokenKind)
	}

	// Сессия — обычный токен: тот же резолв, те же права, ничего своего.
	id, err := auth.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatalf("сессия не аутентифицирует: %v", err)
	}
	if err := auth.Require(ctx, id, projectID(t, store), domain.MemberMaintainer); err != nil {
		t.Fatalf("уровень команды не дошёл до вошедшего: %v", err)
	}

	// Второй вход тем же `sub` не заводит второго принципала, даже если у
	// провайдера сменилось имя.
	again, err := auth.Login(ctx, sign(claims(map[string]any{
		"groups": []string{"sdlc-platform"}, "name": "Алиса Иванова",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if again.Principal.ID != session.Principal.ID {
		t.Fatal("второй вход завёл нового принципала")
	}

	// Группа ушла — ушло и участие в команде: состав ведёт провайдер.
	if _, err := auth.Login(ctx, sign(claims(nil))); err != nil {
		t.Fatal(err)
	}
	if err := auth.Require(ctx, id, projectID(t, store), domain.MemberViewer); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("доступ остался после ухода из группы провайдера: %v", err)
	}

	// Выход отзывает сессию, и она перестаёт работать сразу.
	if err := auth.Logout(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, session.Token); apperr.As(err).Code != apperr.CodeUnauthenticated {
		t.Fatalf("отозванная сессия жива: %v", err)
	}
}

func TestOIDCRejectsForeignTokens(t *testing.T) {
	ctx := context.Background()
	_, _, _, auth, _ := setupPhase4(t, AuthToken)
	issuer, sign := fakeIdP(t)
	other, signOther := fakeIdP(t)
	auth.UseOIDC(oidc.New(oidc.Config{Issuer: issuer, ClientID: "sdlc-ui"}), time.Hour)

	base := map[string]any{
		"iss": issuer, "aud": "sdlc-ui", "sub": "idp-1",
		"exp": time.Now().Add(time.Hour).Unix(), "email": "bob@acme.example",
	}
	with := func(extra map[string]any) string {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return sign(out)
	}

	cases := map[string]string{
		// Токен соседнего приложения того же провайдера сюда не пускают.
		"чужая аудитория": with(map[string]any{"aud": "grafana"}),
		"истёк":           with(map[string]any{"exp": time.Now().Add(-5 * time.Minute).Unix()}),
		"чужой издатель":  with(map[string]any{"iss": other}),
	}
	for name, token := range cases {
		if _, err := auth.Login(ctx, token); apperr.As(err).Code != apperr.CodeUnauthenticated {
			t.Fatalf("%s: вход прошёл (%v)", name, err)
		}
	}

	// Подпись чужим ключом: заголовок и тело сходятся, ключ — нет.
	foreign := signOther(map[string]any{
		"iss": issuer, "aud": "sdlc-ui", "sub": "idp-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := auth.Login(ctx, foreign); apperr.As(err).Code != apperr.CodeUnauthenticated {
		t.Fatalf("токен с чужой подписью принят: %v", err)
	}
}
