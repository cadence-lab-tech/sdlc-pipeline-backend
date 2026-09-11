package workflow

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/oidc"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
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

// Список принципалов нужен странице администратора: человека добавляют в
// команду до того, как он впервые придёт с токеном. Список общесистемный,
// поэтому и права на него общесистемные.
func TestPrincipalListIsForAdminsOnly(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	alice := identityOf(t, store, "alice")

	all, err := auth.ListPrincipals(ctx, admin, "")
	if err != nil {
		t.Fatalf("список принципалов: %v", err)
	}
	if len(all) < 2 {
		t.Fatalf("в списке %d принципалов, ожидали хотя бы администратора и alice", len(all))
	}

	found, err := auth.ListPrincipals(ctx, admin, "ali")
	if err != nil {
		t.Fatalf("поиск по подстроке: %v", err)
	}
	if len(found) != 1 || found[0].ID != alice.Principal.ID {
		t.Fatalf("поиск нашёл не то: %+v", found)
	}

	if _, err := auth.ListPrincipals(ctx, alice, ""); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("участник не администратор: ожидали отказ, получили %v", err)
	}
}

func TestTeamLevelAddsToDirectMembership(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	pid := projectID(t, store)

	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "platform", Name: "Платформа"}); err != nil {
		t.Fatalf("создание команды: %v", err)
	}
	alice := identityOf(t, store, "alice")
	if _, err := auth.SetTeamMember(ctx, admin, "platform", "user:alice", domain.TeamRoleMember); err != nil {
		t.Fatalf("состав команды: %v", err)
	}

	// Пока команду не пустили в проект, участие в ней доступа не даёт.
	if err := auth.Require(ctx, alice, pid, domain.MemberViewer); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("команда без участия в проекте не должна давать доступ: %v", err)
	}

	if _, err := auth.SetProjectTeam(ctx, admin, "SDLC", "platform", domain.MemberMaintainer, nil); err != nil {
		t.Fatalf("выдача команде уровня в проекте: %v", err)
	}
	if err := auth.Require(ctx, alice, pid, domain.MemberMaintainer); err != nil {
		t.Fatalf("уровень команды не дошёл до её участника: %v", err)
	}

	// Поимённое участие ниже командного не понижает права: уровень — максимум
	// из двух путей, а не последний назначенный.
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:alice", domain.MemberViewer, nil); err != nil {
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
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:alice", domain.MemberAdmin, nil); err != nil {
		t.Fatal(err)
	}
	if role, err = auth.ProjectRole(ctx, alice, pid); err != nil || role != domain.MemberAdmin {
		t.Fatalf("уровень %q (%v) вместо admin", role, err)
	}

	// Проект виден по участию команды, а не только по поимённому.
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:alice", domain.MemberViewer, nil); err != nil {
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
	// Человек добавляется в команду только после того, как заведён (решение 0033).
	lead := identityOf(t, store, "lead")
	bob := identityOf(t, store, "bob")
	if _, err := auth.SetTeamMember(ctx, admin, "platform", "user:lead", domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "platform", "user:bob", domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}

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

	// Проект без явной команды уходит в личную команду (решение 0033), а у
	// неё в закрытом режиме права на проекты нет.
	if _, err := auth.OwnerTeamFor(ctx, lead, ""); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("личная команда завела проект в закрытом режиме: %v", err)
	}
}

func TestLastTeamLeadIsProtected(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
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

	mustActor(t, store, "user:alice")
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
	if _, err := auth.SetProjectTeam(ctx, admin, "SDLC", "platform", domain.MemberMaintainer, nil); err != nil {
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

// ── доступ к GitHub на уровне проекта ───────────────────────────────────────

// fakeGitHub отвечает за провайдера: проверяется правило движка, а не поход в
// сеть — сам поход проверен в internal/github.
type fakeGitHub struct {
	allowed        map[string]bool
	installationID *int64
}

func (f *fakeGitHub) AppConfigured() bool { return true }

func (f *fakeGitHub) CheckRepo(_ context.Context, installationID *int64, _, owner, name string) error {
	f.installationID = installationID
	if f.allowed[owner+"/"+name] {
		return nil
	}
	return errors.New("404 Not Found")
}

func TestRepositoryOutsideInstallationIsRejected(t *testing.T) {
	ctx := context.Background()
	store, _, _, _, _ := setupPhase4(t, AuthHeader)
	gh := &fakeGitHub{allowed: map[string]bool{"acme/api": true}}
	engine := NewEngine(store, testdb.Logger(), Options{StrictGates: true, GitHub: gh})

	// Проект не объявил своих учётных данных — провайдера не спрашиваем вовсе:
	// иначе привязка репозитория зависела бы от доступности GitHub.
	if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/web"}); err != nil {
		t.Fatalf("проект без установки: %v", err)
	}
	if gh.installationID != nil {
		t.Fatal("провайдера спросили у проекта без учётных данных")
	}

	installation := int64(42)
	project, err := engine.SetProjectGitHub(ctx, "SDLC", SetGitHubInput{InstallationID: &installation})
	if err != nil {
		t.Fatal(err)
	}
	if project.GitHubInstallationID == nil || *project.GitHubInstallationID != installation {
		t.Fatalf("установка не сохранилась: %+v", project)
	}

	_, _, err = engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/secrets"})
	if apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("репозиторий вне установки привязался: %v", err)
	}
	if gh.installationID == nil || *gh.installationID != installation {
		t.Fatalf("проверка пошла не с установкой проекта: %v", gh.installationID)
	}
	if _, _, err := engine.AddRepository(ctx, "SDLC", AddRepositoryInput{Repository: "acme/api"}); err != nil {
		t.Fatalf("репозиторий внутри установки: %v", err)
	}

	// Настройки проекта показывают доступ по каждому репозиторию области.
	status, err := engine.CheckGitHubAccess(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	byRepo := map[string]bool{}
	for _, r := range status.Repositories {
		byRepo[r.Repository] = r.OK
	}
	if !byRepo["acme/api"] {
		t.Fatalf("доступный репозиторий показан недоступным: %+v", status.Repositories)
	}
	if byRepo["acme/web"] {
		t.Fatalf("недоступный репозиторий показан доступным: %+v", status.Repositories)
	}
}

func TestWebhookRoutesByInstallation(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, _ := setupPhase4(t, AuthHeader)

	push := func(installationID int64, delivery string) WebhookOutcome {
		t.Helper()
		body := fmt.Sprintf(`{
			"ref": "refs/heads/main", "after": "%s", "deleted": false,
			"repository": {"name": "sdlc-pipeline-backend", "owner": {"login": "cadence-lab-tech"}},
			"installation": {"id": %d}
		}`, strings.Repeat("a", 40), installationID)
		out, err := engine.HandleGitHubEvent(ctx, "push", delivery, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	if _, err := engine.SetProjectGitHub(ctx, "SDLC", SetGitHubInput{InstallationID: ptr(int64(42))}); err != nil {
		t.Fatal(err)
	}

	// Событие чужой установки до проекта не доходит: приложение приносит
	// события всех своих установок разом.
	if out := push(7, "delivery-foreign"); out.Queued != 0 {
		t.Fatalf("прогон поставлен по событию чужой установки: %+v", out)
	}
	if out := push(42, "delivery-own"); out.Queued == 0 {
		t.Fatalf("событие своей установки не поставило прогон: %+v", out)
	}
	// Вебхук, настроенный руками, установки не несёт и не обслуживается
	// (решение 0034): пустая установка перестала быть джокером.
	if out := push(0, "delivery-manual"); out.Queued != 0 || out.Ignored == "" {
		t.Fatalf("доставка без установки обслужена: %+v", out)
	}
	_ = store
}

func ptr[T any](v T) *T { return &v }

// ── квоты по стоимости ──────────────────────────────────────────────────────

func TestExceededBudgetStopsNewClaims(t *testing.T) {
	ctx := context.Background()
	store, engine, _, auth, system := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)

	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "platform", Name: "Платформа"}); err != nil {
		t.Fatal(err)
	}
	team, err := store.GetTeamByKey(ctx, store.DB(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	// Демонстрационный проект отдаётся команде: расход считается по проектам,
	// которыми она владеет.
	if _, err := store.DB().Exec(ctx,
		`UPDATE projects SET owner_team_id = $1 WHERE key = 'SDLC'`, team.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamBudget(ctx, admin, "platform", "month", 10); err != nil {
		t.Fatal(err)
	}

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Работа в кредит"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Execute(ctx, task.Key, "start", system.ID, "")
	if err != nil || res.Blocked {
		t.Fatalf("отдать в работу: %v %+v", err, res.Gates)
	}
	claimed, err := engine.Claim(ctx, task.Key, system.ID, "первый")
	if err != nil {
		t.Fatalf("захват в пределах бюджета: %v", err)
	}

	// Прогон отчитался о расходе выше предела.
	cost := 12.5
	if _, err := engine.ReportUsage(ctx, task.Key, system.ID, ReportUsageInput{
		Model: "claude-opus-5", CostUSD: &cost,
	}); err != nil {
		t.Fatal(err)
	}

	// Начатый прогон не обрывается: агент продолжает держать аренду и
	// доводит этап.
	if _, err := engine.Heartbeat(ctx, claimed.StageRunID, system.ID); err != nil {
		t.Fatalf("начатый прогон оборван перерасходом: %v", err)
	}
	if err := engine.Release(ctx, claimed.StageRunID, system.ID, "перерыв"); err != nil {
		t.Fatal(err)
	}

	// А новый захват закрыт, и в отказе сказано, чей это предел.
	_, err = engine.Claim(ctx, task.Key, system.ID, "второй")
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("захват при исчерпанном бюджете: %v", err)
	}
	if !strings.Contains(err.Error(), "platform") || !strings.Contains(err.Error(), "12.50") {
		t.Fatalf("в отказе не видно, чей предел исчерпан: %v", err)
	}

	// Расход виден вместе с пределом: предел без расхода ничего не говорит.
	budgets, err := auth.TeamBudgets(ctx, admin, "platform")
	if err != nil {
		t.Fatal(err)
	}
	if len(budgets.Budgets) != 1 || budgets.Budgets[0].SpentUSD < 12 {
		t.Fatalf("расход команды: %+v", budgets.Budgets)
	}

	// Поднятый предел снова открывает захват: квота — это предел, а не
	// блокировка команды.
	if _, err := auth.SetTeamBudget(ctx, admin, "platform", "month", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Claim(ctx, task.Key, system.ID, "третий"); err != nil {
		t.Fatalf("захват после повышения предела: %v", err)
	}
}
