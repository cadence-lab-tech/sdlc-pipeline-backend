package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/oidc"
)

// Профиль single (CLAB-88): на выделенном инстансе компании нет
// самообслуживания общего инстанса — личных команд, организаций по кнопке,
// тарифов. Модель та же; меняется только то, что инстанс заводит и
// показывает. Номера — критерии приёмки задачи.
func TestSingleProfileHasNoSelfServe(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	// Открытое создание включено нарочно: в single оно не должно давать
	// обходного пути через личную команду (К7).
	auth.Tenancy(TenancySingle).OpenProjectCreation(true)

	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := store.DB().QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	personalOf := func(id uuid.UUID) int {
		return count(`SELECT count(*) FROM teams WHERE personal_of = $1`, id)
	}

	// К2: бутстрап администратора личной команды не заводит, первая команда
	// из SDLC_BOOTSTRAP_TEAM — заводится, с правом на проекты.
	admin := adminIdentity(t, ctx, auth)
	if n := personalOf(admin.Principal.ID); n != 0 {
		t.Fatalf("у администратора личных команд: %d", n)
	}
	if err := auth.BootstrapTeam(ctx, "admin", "platform", "Платформа"); err != nil {
		t.Fatal(err)
	}
	platform, err := store.GetTeamByKey(ctx, store.DB(), "platform")
	if err != nil || !platform.CanCreateProjects || platform.Kind != domain.TeamOrganization {
		t.Fatalf("первая команда: %+v %v", platform, err)
	}
	if role, err := store.TeamRoleOf(ctx, store.DB(), platform.ID, admin.Principal.ID); err != nil || role != domain.TeamRoleLead {
		t.Fatalf("администратор в первой команде: %q %v", role, err)
	}

	// К1: вход через GitHub выдаёт сессию, личной команды нет; К7 — и
	// заводить проекты ему нечем, даже при открытом создании.
	srv := fakeGitHubLogin(t)
	auth.UseGitHub(github.NewOAuth(srv.URL, srv.URL, "app-client", "s3cret"), time.Hour)
	session, err := auth.LoginGitHub(ctx, "good-code", "")
	if err != nil {
		t.Fatal(err)
	}
	octo, err := auth.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if n := personalOf(octo.Principal.ID); n != 0 {
		t.Fatalf("вход завёл личную команду: %d", n)
	}
	me, err := auth.Me(ctx, octo)
	if err != nil {
		t.Fatal(err)
	}
	if len(me.Teams) != 0 || me.CanCreateProjects {
		t.Fatalf("после входа: команды %+v, can_create_projects %v", me.Teams, me.CanCreateProjects)
	}
	if me, err := auth.Me(ctx, admin); err != nil || !me.CanCreateProjects {
		t.Fatalf("администратор не заводит проекты: %v %+v", err, me)
	}

	// К4: проект без команды — отказ «нужна команда», и человеку, и
	// администратору. Личная команда при этом не заводится и лениво.
	teamsBefore := count(`SELECT count(*) FROM teams`)
	projectsBefore := count(`SELECT count(*) FROM projects`)
	for who, id := range map[string]Identity{"человек": octo, "администратор": admin} {
		_, err := auth.OwnerTeamFor(ctx, id, "")
		if e := apperr.As(err); e.Code != apperr.CodeInvalidInput ||
			!strings.Contains(e.Message, "команду-владельца") || !strings.Contains(e.Message, "личных команд") {
			t.Fatalf("%s: проект без команды: %v", who, err)
		}
	}
	if _, err := auth.PersonalTeam(ctx, octo); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("личная команда завелась лениво: %v", err)
	}
	if n := count(`SELECT count(*) FROM teams`); n != teamsBefore {
		t.Fatalf("команд было %d, стало %d", teamsBefore, n)
	}
	if n := count(`SELECT count(*) FROM projects`); n != projectsBefore {
		t.Fatalf("проектов было %d, стало %d", projectsBefore, n)
	}

	// К8: команду заводит только администратор.
	_, err = auth.CreateTeam(ctx, octo, CreateTeamInput{Key: "rogue", Name: "Сами"})
	if e := apperr.As(err); e.Code != apperr.CodeForbidden || !strings.Contains(e.Message, "администратор") {
		t.Fatalf("команду завёл не администратор: %v", err)
	}
	acme, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "acme", Name: "Acme"})
	if err != nil || acme.Kind != domain.TeamOrganization {
		t.Fatalf("команда администратора: %+v %v", acme, err)
	}
	if role, err := store.TeamRoleOf(ctx, store.DB(), acme.ID, admin.Principal.ID); err != nil || role != domain.TeamRoleLead {
		t.Fatalf("создатель команды не лид: %q %v", role, err)
	}

	// К5: проект в команде — лиду с правом, администратору в любую; лиду
	// без права и рядовому участнику — отказ. К7: у лида с правом флаг есть.
	if _, err := auth.SetTeamMember(ctx, admin, "acme", "user:"+octo.Principal.Handle, domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}
	if owner, err := auth.OwnerTeamFor(ctx, octo, "acme"); err != nil || owner != acme.ID {
		t.Fatalf("лид с правом: %v %v", owner, err)
	}
	if me, err := auth.Me(ctx, octo); err != nil || !me.CanCreateProjects {
		t.Fatalf("лид с правом без can_create_projects: %v %+v", err, me)
	}
	closed, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "closed", Name: "Без права"})
	if err != nil {
		t.Fatal(err)
	}
	no := false
	if _, err := auth.UpdateTeam(ctx, admin, "closed", UpdateTeamInput{CanCreateProjects: &no}); err != nil {
		t.Fatal(err)
	}
	mustActor(t, store, "user:lead")
	mustActor(t, store, "user:member")
	lead, member := identityOf(t, store, "lead"), identityOf(t, store, "member")
	if _, err := auth.SetTeamMember(ctx, admin, "closed", "user:lead", domain.TeamRoleLead); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamMember(ctx, admin, "acme", "user:member", domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.OwnerTeamFor(ctx, lead, "closed"); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("лид без права завёл проект: %v", err)
	}
	if _, err := auth.OwnerTeamFor(ctx, member, "acme"); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("не лид завёл проект: %v", err)
	}
	if owner, err := auth.OwnerTeamFor(ctx, admin, "closed"); err != nil || owner != closed.ID {
		t.Fatalf("администратор в любую команду: %v %v", owner, err)
	}
	if me, err := auth.Me(ctx, lead); err != nil || me.CanCreateProjects {
		t.Fatalf("лид без права с can_create_projects: %v %+v", err, me)
	}

	// К6: личная команда, оставшаяся с общего инстанса, не подставляется.
	mustActor(t, store, "user:bob")
	bob := identityOf(t, store, "bob")
	auth.Tenancy(TenancyShared)
	legacy, err := auth.PersonalTeam(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	auth.Tenancy(TenancySingle)
	if _, err := auth.OwnerTeamFor(ctx, bob, ""); apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("личная команда с общего инстанса подставилась: %v", err)
	}

	// К9: превращает её в организацию администратор, а не её лид.
	if _, err := auth.ConvertTeam(ctx, bob, legacy.Key, ConvertTeamInput{}); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("лид личной команды превратил её сам: %v", err)
	}
	converted, err := auth.ConvertTeam(ctx, admin, legacy.Key, ConvertTeamInput{Key: "bob-org"})
	if err != nil || converted.Kind != domain.TeamOrganization {
		t.Fatalf("превращение администратором: %+v %v", converted, err)
	}

	// К11: тарифов нет — ни списка, ни перевода, и администратору тоже.
	subsBefore := count(`SELECT count(*) FROM subscriptions`)
	if _, err := auth.ListPlans(ctx, admin); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("список тарифов: %v", err)
	}
	for who, id := range map[string]Identity{"администратор": admin, "лид": octo} {
		if _, err := auth.SetTeamPlan(ctx, id, "acme", "business"); apperr.As(err).Code != apperr.CodeNotFound {
			t.Fatalf("%s: перевод на тариф: %v", who, err)
		}
	}
	if _, err := auth.TeamPlan(ctx, admin, "acme"); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("тариф команды: %v", err)
	}
	if n := count(`SELECT count(*) FROM subscriptions`); n != subsBefore {
		t.Fatalf("подписок было %d, стало %d", subsBefore, n)
	}

	// К12: в команде нет ключа plan, остальное на месте.
	detail, err := auth.GetTeam(ctx, admin, "acme")
	if err != nil || detail.Plan != nil {
		t.Fatalf("команда: %v %+v", err, detail.Plan)
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["plan"]; ok {
		t.Fatalf("ключ plan в ответе: %s", raw)
	}
	for _, k := range []string{"team", "members", "projects", "installations"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("нет ключа %q: %s", k, raw)
		}
	}

	// К16 (обратная сторона): на общем инстансе тариф в ответе есть.
	auth.Tenancy(TenancyShared)
	if detail, err := auth.GetTeam(ctx, admin, "acme"); err != nil || detail.Plan == nil || detail.Plan.Code != "free" {
		t.Fatalf("тариф на общем инстансе: %v %+v", err, detail.Plan)
	}
}

// К3: вход через OIDC на выделенном инстансе даёт человеку команды из групп
// провайдера и ничего сверх них.
func TestSingleProfileOIDCLoginJoinsGroupTeamsOnly(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	auth.Tenancy(TenancySingle)
	admin := adminIdentity(t, ctx, auth)
	issuer, sign := fakeIdP(t)
	auth.UseOIDC(oidc.New(oidc.Config{Issuer: issuer, ClientID: "sdlc-ui"}), time.Hour)

	if _, err := auth.CreateTeam(ctx, admin, CreateTeamInput{Key: "platform", Name: "Платформа"}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamIdPGroups(ctx, admin, "platform", []string{"sdlc-platform"}); err != nil {
		t.Fatal(err)
	}
	session, err := auth.Login(ctx, sign(map[string]any{
		"iss": issuer, "aud": "sdlc-ui", "sub": "idp-7",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"email": "carol@acme.example", "groups": []string{"sdlc-platform"},
	}))
	if err != nil {
		t.Fatalf("вход: %v", err)
	}
	id, err := auth.Authenticate(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	me, err := auth.Me(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(me.Teams) != 1 || me.Teams[0].TeamKey != "platform" {
		t.Fatalf("команды после входа: %+v", me.Teams)
	}
	var n int
	if err := store.DB().QueryRow(ctx, `SELECT count(*) FROM teams WHERE personal_of = $1`,
		id.Principal.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("личных команд: %d %v", n, err)
	}
}
