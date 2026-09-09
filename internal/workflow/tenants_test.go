package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Арендатор — команда (решение 0033): личная команда заводится при первом
// входе, проект без явной команды уходит в неё, участников она не принимает.
func TestPersonalTeamAtLogin(t *testing.T) {
	ctx := context.Background()
	store, engine, _, auth, _ := setupPhase4(t, AuthToken)
	adminIdentity(t, ctx, auth)
	srv := fakeGitHubLogin(t)
	auth.UseGitHub(github.NewOAuth(srv.URL, srv.URL, "app-client", "s3cret"), time.Hour).
		OpenProjectCreation(true)

	session, err := auth.LoginGitHub(ctx, "good-code", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := auth.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	me, err := auth.Me(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(me.Teams) != 1 || me.Teams[0].TeamKey != "octo-cat" || me.Teams[0].TeamKind != domain.TeamPersonal ||
		me.Teams[0].Role != domain.TeamRoleLead {
		t.Fatalf("личной команды после входа нет: %+v", me.Teams)
	}
	if !me.CanCreateProjects {
		t.Fatal("вошедший при открытом создании заводит проекты")
	}

	// Повторный вход личную команду не дублирует.
	if _, err := auth.LoginGitHub(ctx, "good-code", ""); err != nil {
		t.Fatal(err)
	}
	if me, err = auth.Me(ctx, id); err != nil || len(me.Teams) != 1 {
		t.Fatalf("второй вход завёл вторую команду: %v %+v", err, me.Teams)
	}

	// Проект без явной команды — в личную; её лид — администратор проекта.
	owner, err := auth.OwnerTeamFor(ctx, id, "")
	if err != nil {
		t.Fatal(err)
	}
	project, err := engine.CreateProject(ctx, id.Principal.ID, postgres.CreateProjectInput{
		Key: "OCTO", Name: "Проект кота", OwnerTeamID: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if project.OwnerTeamKey == nil || *project.OwnerTeamKey != "octo-cat" {
		t.Fatalf("владелец проекта: %+v", project)
	}
	if err := auth.Require(ctx, id, project.ID, domain.MemberAdmin); err != nil {
		t.Fatalf("владелец не администратор проекта: %v", err)
	}

	// Личная команда — один человек.
	mustActor(t, store, "user:friend")
	if _, err := auth.SetTeamMember(ctx, id, "octo-cat", "user:friend", domain.TeamRoleMember); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("личная команда приняла участника: %v", err)
	}

	// Превращение в организацию: проекты остаются, следующий вход заводит
	// новую личную команду.
	org, err := auth.ConvertTeam(ctx, id, "octo-cat", ConvertTeamInput{Key: "octo-org", Name: "Коты"})
	if err != nil {
		t.Fatal(err)
	}
	if org.Kind != domain.TeamOrganization || org.Key != "octo-org" || org.PersonalOf != nil {
		t.Fatalf("после превращения: %+v", org)
	}
	if _, err := auth.ConvertTeam(ctx, id, "octo-org", ConvertTeamInput{}); apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("организация превратилась второй раз: %v", err)
	}
	if _, err := auth.SetTeamMember(ctx, id, "octo-org", "user:friend", domain.TeamRoleMember); err != nil {
		t.Fatalf("организация не приняла участника: %v", err)
	}
	if p, err := store.GetProjectByKey(ctx, store.DB(), "OCTO"); err != nil || *p.OwnerTeamKey != "octo-org" {
		t.Fatalf("проект потерял владельца при превращении: %v %+v", err, p)
	}
	if _, err := auth.LoginGitHub(ctx, "good-code", ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := auth.PersonalTeam(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == org.ID || fresh.Kind != domain.TeamPersonal || fresh.Key != "octo-cat" {
		t.Fatalf("новая личная команда: %+v", fresh)
	}
}

// Организацию заводит любой вошедший человек; бесплатный тариф держит её
// состав, тариф назначает администратор инстанса, выделенный инстанс квоты
// не проверяет.
func TestOrganizationSelfServeAndMemberQuota(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	auth.OpenProjectCreation(true)
	alice := identityOf(t, store, "alice")
	for _, h := range []string{"user:bob", "user:carol", "user:dave"} {
		mustActor(t, store, h)
	}
	bot := Identity{Principal: mustActor(t, store, "agent:bot")}

	if _, err := auth.CreateTeam(ctx, bot, CreateTeamInput{Key: "bots", Name: "Боты"}); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("агент завёл организацию: %v", err)
	}
	org, err := auth.CreateTeam(ctx, alice, CreateTeamInput{Key: "acme", Name: "Acme", CanCreateProjects: false})
	if err != nil {
		t.Fatalf("человек не завёл организацию: %v", err)
	}
	// Право на проекты — по режиму инстанса, а не по полю запроса.
	if org.Kind != domain.TeamOrganization || !org.CanCreateProjects {
		t.Fatalf("новая организация: %+v", org)
	}
	detail, err := auth.GetTeam(ctx, alice, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Plan.Code != "free" || detail.Plan.MembersMax == nil || *detail.Plan.MembersMax != 3 {
		t.Fatalf("тариф по умолчанию: %+v", detail.Plan)
	}

	for _, h := range []string{"user:bob", "user:carol"} {
		if _, err := auth.SetTeamMember(ctx, alice, "acme", h, domain.TeamRoleMember); err != nil {
			t.Fatalf("%s: %v", h, err)
		}
	}
	_, err = auth.SetTeamMember(ctx, alice, "acme", "user:dave", domain.TeamRoleMember)
	if apperr.As(err).Code != apperr.CodeForbidden || !strings.Contains(err.Error(), "не больше 3") {
		t.Fatalf("четвёртый участник на free: %v", err)
	}
	// Смена роли уже состоящего — не рост состава.
	if _, err := auth.SetTeamMember(ctx, alice, "acme", "user:bob", domain.TeamRoleLead); err != nil {
		t.Fatalf("смена роли упёрлась в квоту: %v", err)
	}
	// Незаведённого человека нет; агент заводится на месте, но и он —
	// участник, и квота считает его.
	if _, err := auth.SetTeamMember(ctx, alice, "acme", "user:nobody", domain.TeamRoleMember); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("участник из воздуха: %v", err)
	}

	// Тариф назначает администратор инстанса, не команда.
	if _, err := auth.SetTeamPlan(ctx, alice, "acme", "business"); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("команда назначила себе тариф: %v", err)
	}
	if _, err := auth.SetTeamPlan(ctx, admin, "acme", "no-such-plan"); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("неизвестный тариф: %v", err)
	}
	if _, err := auth.SetTeamPlan(ctx, admin, "acme", "business"); err != nil {
		t.Fatal(err)
	}
	if plan, err := auth.TeamPlan(ctx, alice, "acme"); err != nil || plan.Code != "business" || plan.MembersMax != nil {
		t.Fatalf("тариф после перевода: %+v %v", plan, err)
	}
	if _, err := auth.SetTeamMember(ctx, alice, "acme", "user:dave", domain.TeamRoleMember); err != nil {
		t.Fatalf("business не пустил четвёртого: %v", err)
	}
	if _, err := auth.SetTeamMember(ctx, alice, "acme", "agent:ci", domain.TeamRoleMember); err != nil {
		t.Fatalf("агент не завёлся на месте: %v", err)
	}
	// Возврат на free ничего не удаляет: команда просто не растёт.
	if _, err := auth.SetTeamPlan(ctx, admin, "acme", "free"); err != nil {
		t.Fatal(err)
	}
	if detail, err = auth.GetTeam(ctx, alice, "acme"); err != nil || detail.Plan.Code != "free" || len(detail.Members) != 5 {
		t.Fatalf("после возврата на free: %v %+v", err, detail)
	}
	mustActor(t, store, "user:erin")
	if _, err := auth.SetTeamMember(ctx, alice, "acme", "user:erin", domain.TeamRoleMember); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("переполненная команда выросла: %v", err)
	}

	// Выделенный инстанс: пределы задаёт договор, квоты не проверяются.
	auth.Tenancy(TenancySingle)
	if _, err := auth.SetTeamMember(ctx, alice, "acme", "user:erin", domain.TeamRoleMember); err != nil {
		t.Fatalf("квота на выделенном инстансе: %v", err)
	}
	plans, err := auth.ListPlans(ctx, alice)
	if err != nil || len(plans) != 2 {
		t.Fatalf("тарифы: %v %+v", err, plans)
	}
}

// Вход через провайдера ключуется только субъектом: принципал, заведённый
// под тот же хэндл, вошедшему не достаётся (решение 0033).
func TestIdPLoginNeverLinksByHandle(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	srv := fakeGitHubLogin(t)
	auth.UseGitHub(github.NewOAuth(srv.URL, srv.URL, "app-client", "s3cret"), time.Hour)

	// Кто-то завёл «octo-cat» заранее (режим заголовка, бутстрап, старые
	// данные) и выдал на него токен.
	planted := mustActor(t, store, "user:octo-cat")
	if _, err := auth.IssueToken(ctx, admin, IssueTokenInput{Name: "planted", Principal: "user:octo-cat", ProjectKey: "SDLC"}); err != nil {
		t.Fatal(err)
	}

	session, err := auth.LoginGitHub(ctx, "good-code", "")
	if err != nil {
		t.Fatal(err)
	}
	if session.Principal.ID == planted.ID {
		t.Fatal("вошедший получил чужого принципала по хэндлу")
	}
	if !strings.HasPrefix(session.Principal.Handle, "octo-cat-") {
		t.Fatalf("занятый хэндл не получил суффикс: %q", session.Principal.Handle)
	}
	// Личная команда — по хэндлу с суффиксом, ключ свободен.
	team, err := auth.PersonalTeam(ctx, Identity{Principal: session.Principal})
	if err != nil || team.Key != session.Principal.Handle {
		t.Fatalf("личная команда: %+v %v", team, err)
	}
}
