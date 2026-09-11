package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

func intp(n int) *int { return &n }

func codeOf(err error) apperr.Code {
	if err == nil {
		return ""
	}
	return apperr.As(err).Code
}

// Статус вычисляется из времён: конечные состояния важнее срока.
func TestInviteStatusAt(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	cases := []struct {
		name string
		inv  domain.Invite
		want domain.InviteStatus
	}{
		{"выдан", domain.Invite{ExpiresAt: future}, domain.InvitePending},
		{"протух", domain.Invite{ExpiresAt: past}, domain.InviteExpired},
		{"ровно в срок — протух", domain.Invite{ExpiresAt: now}, domain.InviteExpired},
		{"принят и протух", domain.Invite{ExpiresAt: past, AcceptedAt: &past}, domain.InviteAccepted},
		{"отозван", domain.Invite{ExpiresAt: future, RevokedAt: &past}, domain.InviteRevoked},
	}
	for _, c := range cases {
		if got := c.inv.StatusAt(now); got != c.want {
			t.Errorf("%s: %s, ждали %s", c.name, got, c.want)
		}
	}
}

func TestInviteExpiry(t *testing.T) {
	def, err := inviteExpiry(nil)
	if err != nil || time.Until(def) < 7*24*time.Hour-time.Minute || time.Until(def) > 7*24*time.Hour {
		t.Fatalf("срок по умолчанию: %v %v", def, err)
	}
	for _, days := range []int{0, -1, 31} {
		if _, err := inviteExpiry(intp(days)); codeOf(err) != apperr.CodeInvalidInput {
			t.Errorf("срок %d дней принят: %v", days, err)
		}
	}
	if _, err := inviteExpiry(intp(30)); err != nil {
		t.Errorf("30 дней: %v", err)
	}
}

// К1: ограничения схемы проверяются негативными вставками мимо движка.
func TestInviteSchemaConstraints(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	alice := identityOf(t, store, "alice")
	team, err := auth.CreateTeam(ctx, alice, CreateTeamInput{Key: "acme", Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	insert := func(teamID, projectID, teamRole, level any, roleKeys []string, hash string) error {
		_, err := store.DB().Exec(ctx, `
			INSERT INTO invites (team_id, project_id, team_role, level, role_keys, token_hash, created_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now() + interval '1 day')`,
			teamID, projectID, teamRole, level, roleKeys, hash, alice.Principal.ID)
		return err
	}
	want := func(name string, err error, code string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("%s: ждали %s, получили %v", name, code, err)
		}
	}
	if err := insert(team.ID, nil, "member", nil, nil, "h-team"); err != nil {
		t.Fatalf("корректное командное: %v", err)
	}
	if err := insert(nil, project.ID, nil, "agent", []string{"developer"}, "h-project"); err != nil {
		t.Fatalf("корректное проектное: %v", err)
	}
	want("обе цели", insert(team.ID, project.ID, "member", nil, nil, "h1"), "23514")
	want("ни одной цели", insert(nil, nil, "member", nil, nil, "h2"), "23514")
	want("уровень у командного", insert(team.ID, nil, "member", "agent", nil, "h3"), "23514")
	want("роли у командного", insert(team.ID, nil, "member", nil, []string{"developer"}, "h4"), "23514")
	want("роль в команде у проектного", insert(nil, project.ID, "member", "agent", nil, "h5"), "23514")
	want("проектное без уровня", insert(nil, project.ID, nil, nil, nil, "h6"), "23514")
	want("неизвестная роль в команде", insert(team.ID, nil, "owner", nil, nil, "h7"), "23514")
	want("неизвестный уровень", insert(nil, project.ID, nil, "owner", nil, "h8"), "23514")
	want("повтор хеша", insert(team.ID, nil, "member", nil, nil, "h-team"), "23505")

	_, err = store.DB().Exec(ctx, `UPDATE invites SET revoked_at = now() WHERE token_hash = 'h-team'`)
	want("отзыв без отозвавшего", err, "23514")
	_, err = store.DB().Exec(ctx, `
		UPDATE invites SET accepted_at = now(), accepted_by = created_by,
		                   revoked_at = now(), revoked_by = created_by
		WHERE token_hash = 'h-team'`)
	want("принят и отозван", err, "23514")
}

// К2-К4, К6-К12 на организации: выдача, просмотр, приём, квота, отзыв, протухание.
func TestTeamInvites(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	alice := identityOf(t, store, "alice")
	bob := identityOf(t, store, "bob")
	carol := identityOf(t, store, "carol")
	dave := identityOf(t, store, "dave")
	mallory := identityOf(t, store, "mallory")
	bot := Identity{Principal: mustActor(t, store, "agent:bot")}

	team, err := auth.CreateTeam(ctx, alice, CreateTeamInput{Key: "acme", Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetTeamMember(ctx, alice, "acme", "user:bob", domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}

	// К4: выдаёт лид-человек; личная команда и срок вне 1..30 отбиваются.
	if _, err := auth.CreateTeamInvite(ctx, bob, "acme", domain.TeamRoleMember, nil); codeOf(err) != apperr.CodeForbidden {
		t.Fatalf("не лид выдал приглашение: %v", err)
	}
	if _, err := auth.CreateTeamInvite(ctx, mallory, "acme", domain.TeamRoleMember, nil); codeOf(err) != apperr.CodeForbidden {
		t.Fatalf("посторонний выдал приглашение: %v", err)
	}
	if _, err := auth.CreateTeamInvite(ctx, bot, "acme", domain.TeamRoleMember, nil); codeOf(err) != apperr.CodeForbidden {
		t.Fatalf("агент выдал приглашение: %v", err)
	}
	for _, days := range []int{0, 31} {
		if _, err := auth.CreateTeamInvite(ctx, alice, "acme", domain.TeamRoleMember, intp(days)); codeOf(err) != apperr.CodeInvalidInput {
			t.Fatalf("срок %d дней: %v", days, err)
		}
	}
	if _, err := auth.CreateTeamInvite(ctx, alice, "acme", "owner", nil); codeOf(err) != apperr.CodeInvalidInput {
		t.Fatalf("роль owner: %v", err)
	}
	personal, err := auth.PersonalTeam(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.CreateTeamInvite(ctx, alice, personal.Key, domain.TeamRoleMember, nil); codeOf(err) != apperr.CodeForbidden ||
		!strings.Contains(err.Error(), "личный аккаунт") {
		t.Fatalf("приглашение в личную команду: %v", err)
	}
	if _, err := auth.CreateTeamInvite(ctx, alice, "no-such-team", domain.TeamRoleMember, nil); codeOf(err) != apperr.CodeNotFound {
		t.Fatalf("приглашение в несуществующую команду: %v", err)
	}

	// К2: токен со своим префиксом, срок по умолчанию — неделя.
	issued, err := auth.CreateTeamInvite(ctx, alice, "acme", domain.TeamRoleMember, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(issued.Token, InvitePrefix) {
		t.Fatalf("префикс токена: %q", issued.Token)
	}
	if d := time.Until(issued.Invite.ExpiresAt) - 7*24*time.Hour; d > time.Minute || d < -time.Minute {
		t.Fatalf("срок по умолчанию: %v", issued.Invite.ExpiresAt)
	}
	inv := issued.Invite
	if inv.Status != domain.InvitePending || inv.TargetKind != domain.InviteTeam || inv.TeamKey == nil ||
		*inv.TeamKey != "acme" || inv.TeamRole == nil || *inv.TeamRole != domain.TeamRoleMember ||
		inv.Level != nil || inv.CreatedByHandle != "alice" {
		t.Fatalf("выданное приглашение: %+v", inv)
	}
	// Ссылка — не токен доступа.
	if _, err := auth.Authenticate(ctx, issued.Token); err == nil {
		t.Fatal("приглашение принято как Bearer")
	}

	// К3: в базе только хеш; ни одна колонка не хранит токен; списки не отдают ни того, ни другого.
	var hash string
	var leaked int
	if err := store.DB().QueryRow(ctx, `
		SELECT token_hash,
		       (SELECT count(*) FROM invites x WHERE strpos(row_to_json(x)::text, $2) > 0)
		FROM invites WHERE id = $1`, inv.ID, issued.Token).Scan(&hash, &leaked); err != nil {
		t.Fatal(err)
	}
	if hash != HashToken(issued.Token) || leaked != 0 {
		t.Fatalf("хеш %q, строк с токеном %d", hash, leaked)
	}
	list, err := auth.ListTeamInvites(ctx, alice, "acme")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(list)
	if len(list) != 1 || strings.Contains(string(raw), "token") || strings.Contains(string(raw), hash) {
		t.Fatalf("список отдаёт токен или хеш: %s", raw)
	}
	if _, err := auth.ListTeamInvites(ctx, bob, "acme"); codeOf(err) != apperr.CodeForbidden {
		t.Fatalf("список приглашений у не лида: %v", err)
	}

	// К6: просмотр по токену — любым вошедшим; неизвестный токен не найден.
	seen, err := auth.LookupInvite(ctx, carol, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	if seen.ID != inv.ID || seen.TargetName != "Acme" || seen.CreatedByHandle != "alice" || seen.Status != domain.InvitePending {
		t.Fatalf("просмотр: %+v", seen)
	}
	for _, bad := range []string{InvitePrefix + "nope", "sdlc_something", ""} {
		if _, err := auth.LookupInvite(ctx, carol, bad); codeOf(err) != apperr.CodeNotFound {
			t.Fatalf("просмотр по %q: %v", bad, err)
		}
	}

	// К7: принимает человек; участие на принявшего, added_by — выдавший.
	if _, err := auth.AcceptInvite(ctx, bot, issued.Token); codeOf(err) != apperr.CodeForbidden {
		t.Fatalf("агент принял приглашение: %v", err)
	}
	acc, err := auth.AcceptInvite(ctx, carol, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	if acc.AlreadyMember || acc.Target != (InviteTarget{Kind: domain.InviteTeam, Key: "acme", Name: "Acme"}) ||
		acc.Invite.Status != domain.InviteAccepted || acc.Invite.AcceptedByHandle == nil ||
		*acc.Invite.AcceptedByHandle != "carol" || acc.Invite.AcceptedAt == nil {
		t.Fatalf("приём: %+v", acc)
	}
	if role, err := store.TeamRoleOf(ctx, store.DB(), team.ID, carol.Principal.ID); err != nil || role != domain.TeamRoleMember {
		t.Fatalf("роль принявшего: %q %v", role, err)
	}
	if by := teamAddedBy(t, store, team.ID, carol.Principal.ID); by != alice.Principal.ID {
		t.Fatalf("added_by принявшего: %s", by)
	}
	if _, err := auth.AcceptInvite(ctx, dave, issued.Token); codeOf(err) != apperr.CodeConflict {
		t.Fatalf("повторный приём: %v", err)
	}

	// К8: в организации на free трое — четвёртый упирается в квоту, приглашение остаётся выданным.
	forDave, err := auth.CreateTeamInvite(ctx, alice, "acme", domain.TeamRoleMember, intp(1))
	if err != nil {
		t.Fatal(err)
	}
	_, err = auth.AcceptInvite(ctx, dave, forDave.Token)
	if codeOf(err) != apperr.CodeForbidden || !strings.Contains(err.Error(), "не больше 3") ||
		!strings.Contains(err.Error(), "сейчас 3") {
		t.Fatalf("четвёртый на free: %v", err)
	}
	if again, err := auth.LookupInvite(ctx, dave, forDave.Token); err != nil || again.Status != domain.InvitePending {
		t.Fatalf("отказ по квоте закрыл приглашение: %+v %v", again, err)
	}
	if _, err := auth.SetTeamPlan(ctx, admin, "acme", "business"); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.AcceptInvite(ctx, dave, forDave.Token); err != nil {
		t.Fatalf("business не пустил по тому же приглашению: %v", err)
	}
	if _, err := auth.SetTeamPlan(ctx, admin, "acme", "free"); err != nil {
		t.Fatal(err)
	}
	erin := identityOf(t, store, "erin")
	forErin, err := auth.CreateTeamInvite(ctx, alice, "acme", domain.TeamRoleMember, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth.Tenancy(TenancySingle)
	if _, err := auth.AcceptInvite(ctx, erin, forErin.Token); err != nil {
		t.Fatalf("квота на выделенном инстансе: %v", err)
	}
	auth.Tenancy(TenancyShared)

	// К9: уже участник — участие не меняется, приглашение принято.
	promote, err := auth.CreateTeamInvite(ctx, alice, "acme", domain.TeamRoleLead, nil)
	if err != nil {
		t.Fatal(err)
	}
	acc, err = auth.AcceptInvite(ctx, bob, promote.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !acc.AlreadyMember || acc.Invite.Status != domain.InviteAccepted {
		t.Fatalf("приём уже участником: %+v", acc)
	}
	if role, _ := store.TeamRoleOf(ctx, store.DB(), team.ID, bob.Principal.ID); role != domain.TeamRoleMember {
		t.Fatalf("приглашение повысило участника до %q", role)
	}

	// К11: отзывает тот, кто вправе выдать; конечное состояние не отзывается.
	pending, err := auth.CreateTeamInvite(ctx, alice, "acme", domain.TeamRoleMember, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.RevokeInvite(ctx, mallory, pending.Invite.ID); codeOf(err) != apperr.CodeForbidden {
		t.Fatalf("отзыв посторонним: %v", err)
	}
	if _, err := auth.RevokeInvite(ctx, bob, pending.Invite.ID); codeOf(err) != apperr.CodeForbidden {
		t.Fatalf("отзыв не лидом: %v", err)
	}
	rev, err := auth.RevokeInvite(ctx, alice, pending.Invite.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Status != domain.InviteRevoked || rev.RevokedByHandle == nil || *rev.RevokedByHandle != "alice" || rev.RevokedAt == nil {
		t.Fatalf("отозванное: %+v", rev)
	}
	if _, err := auth.RevokeInvite(ctx, alice, pending.Invite.ID); codeOf(err) != apperr.CodeConflict {
		t.Fatalf("повторный отзыв: %v", err)
	}
	if _, err := auth.AcceptInvite(ctx, mallory, pending.Token); codeOf(err) != apperr.CodeConflict ||
		!strings.Contains(err.Error(), "отозвано") {
		t.Fatalf("приём отозванного: %v", err)
	}
	if _, err := auth.RevokeInvite(ctx, alice, issued.Invite.ID); codeOf(err) != apperr.CodeConflict {
		t.Fatalf("отзыв принятого: %v", err)
	}
	if _, err := auth.RevokeInvite(ctx, alice, uuid.New()); codeOf(err) != apperr.CodeNotFound {
		t.Fatalf("отзыв несуществующего: %v", err)
	}

	// К7, К12: протухшее не принимается и не отзывается; статус — на момент чтения.
	stale, err := auth.CreateTeamInvite(ctx, alice, "acme", domain.TeamRoleMember, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx, `
		UPDATE invites SET created_at = now() - interval '10 days', expires_at = now() - interval '1 day'
		WHERE id = $1`, stale.Invite.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.AcceptInvite(ctx, mallory, stale.Token); codeOf(err) != apperr.CodeConflict ||
		!strings.Contains(err.Error(), "срок") {
		t.Fatalf("приём протухшего: %v", err)
	}
	if _, err := auth.RevokeInvite(ctx, alice, stale.Invite.ID); codeOf(err) != apperr.CodeConflict {
		t.Fatalf("отзыв протухшего: %v", err)
	}
	list, err = auth.ListTeamInvites(ctx, alice, "acme")
	if err != nil {
		t.Fatal(err)
	}
	wantStatus := map[uuid.UUID]domain.InviteStatus{
		issued.Invite.ID:  domain.InviteAccepted,
		forDave.Invite.ID: domain.InviteAccepted,
		forErin.Invite.ID: domain.InviteAccepted,
		promote.Invite.ID: domain.InviteAccepted,
		pending.Invite.ID: domain.InviteRevoked,
		stale.Invite.ID:   domain.InviteExpired,
	}
	if len(list) != len(wantStatus) {
		t.Fatalf("в списке %d приглашений, ждали %d", len(list), len(wantStatus))
	}
	for _, got := range list {
		if got.Status != wantStatus[got.ID] {
			t.Errorf("приглашение %s: %s, ждали %s", got.ID, got.Status, wantStatus[got.ID])
		}
	}
}

// К2-К7, К9, К11, К12 на проекте: уровень, роли специалистов, прямое участие.
func TestProjectInvites(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	alice := identityOf(t, store, "alice")
	mallory := identityOf(t, store, "mallory")
	frank := identityOf(t, store, "frank")
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:alice", domain.MemberAdmin, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:mallory", domain.MemberMaintainer, nil); err != nil {
		t.Fatal(err)
	}
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}

	// К5: выдаёт администратор проекта; роль и уровень проверяются.
	if _, err := auth.CreateProjectInvite(ctx, mallory, "SDLC", domain.MemberAgent, nil, nil); codeOf(err) != apperr.CodeForbidden {
		t.Fatalf("мейнтейнер выдал приглашение: %v", err)
	}
	if _, err := auth.CreateProjectInvite(ctx, alice, "SDLC", "owner", nil, nil); codeOf(err) != apperr.CodeInvalidInput {
		t.Fatalf("уровень owner: %v", err)
	}
	if _, err := auth.CreateProjectInvite(ctx, alice, "SDLC", domain.MemberAgent, []string{"no-such-role"}, nil); codeOf(err) != apperr.CodeNotFound {
		t.Fatalf("несуществующая роль: %v", err)
	}
	issued, err := auth.CreateProjectInvite(ctx, alice, "SDLC", domain.MemberAgent, []string{"developer"}, intp(3))
	if err != nil {
		t.Fatal(err)
	}
	inv := issued.Invite
	if inv.TargetKind != domain.InviteProject || inv.ProjectKey == nil || *inv.ProjectKey != "SDLC" ||
		inv.Level == nil || *inv.Level != domain.MemberAgent || inv.TeamRole != nil ||
		len(inv.RoleKeys) != 1 || inv.RoleKeys[0] != "developer" {
		t.Fatalf("проектное приглашение: %+v", inv)
	}
	if d := time.Until(inv.ExpiresAt) - 3*24*time.Hour; d > time.Minute || d < -time.Minute {
		t.Fatalf("срок 3 дня: %v", inv.ExpiresAt)
	}

	// К7: прямое участие с уровнем и ролями приглашения, added_by — выдавший.
	acc, err := auth.AcceptInvite(ctx, frank, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	if acc.AlreadyMember || acc.Target != (InviteTarget{Kind: domain.InviteProject, Key: "SDLC", Name: project.Name}) {
		t.Fatalf("приём в проект: %+v", acc)
	}
	member := projectMember(t, ctx, auth, alice, "frank")
	if member.Role != domain.MemberAgent || len(member.RoleKeys) != 1 || member.RoleKeys[0] != "developer" {
		t.Fatalf("участие принявшего: %+v", member)
	}
	if by := projectAddedBy(t, store, project.ID, frank.Principal.ID); by != alice.Principal.ID {
		t.Fatalf("added_by принявшего: %s", by)
	}

	// Без ролей в приглашении — любые роли (NULL), как у прямого участия.
	anyRoles, err := auth.CreateProjectInvite(ctx, alice, "SDLC", domain.MemberViewer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	grace := identityOf(t, store, "grace")
	if _, err := auth.AcceptInvite(ctx, grace, anyRoles.Token); err != nil {
		t.Fatal(err)
	}
	if m := projectMember(t, ctx, auth, alice, "grace"); m.Role != domain.MemberViewer || m.RoleKeys != nil {
		t.Fatalf("участие без ролей в приглашении: %+v", m)
	}

	// К9: уже участник — уровень, роли и added_by не меняются.
	promote, err := auth.CreateProjectInvite(ctx, alice, "SDLC", domain.MemberAdmin, []string{"developer"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	acc, err = auth.AcceptInvite(ctx, mallory, promote.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !acc.AlreadyMember || acc.Invite.Status != domain.InviteAccepted {
		t.Fatalf("приём уже участником: %+v", acc)
	}
	if m := projectMember(t, ctx, auth, alice, "mallory"); m.Role != domain.MemberMaintainer || m.RoleKeys != nil {
		t.Fatalf("приглашение изменило участие: %+v", m)
	}
	if by := projectAddedBy(t, store, project.ID, mallory.Principal.ID); by != admin.Principal.ID {
		t.Fatalf("приглашение переписало added_by: %s", by)
	}

	// К11, К12: отзыв и список — у администратора проекта.
	pending, err := auth.CreateProjectInvite(ctx, alice, "SDLC", domain.MemberViewer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.RevokeInvite(ctx, mallory, pending.Invite.ID); codeOf(err) != apperr.CodeForbidden {
		t.Fatalf("отзыв мейнтейнером: %v", err)
	}
	if _, err := auth.RevokeInvite(ctx, alice, pending.Invite.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.ListProjectInvites(ctx, mallory, "SDLC"); codeOf(err) != apperr.CodeForbidden {
		t.Fatalf("список у мейнтейнера: %v", err)
	}
	list, err := auth.ListProjectInvites(ctx, alice, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[domain.InviteStatus]int{}
	for _, got := range list {
		statuses[got.Status]++
	}
	if len(list) != 4 || statuses[domain.InviteAccepted] != 3 || statuses[domain.InviteRevoked] != 1 {
		t.Fatalf("статусы проекта: %v", statuses)
	}
}

// К10: два одновременных приёма одной ссылки — одно участие, один успех,
// один конфликт. Прогон повторяется: гонка ловится не с первого раза.
func TestInviteAcceptedOnce(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	alice := identityOf(t, store, "alice")
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:alice", domain.MemberAdmin, nil); err != nil {
		t.Fatal(err)
	}
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 5; round++ {
		issued, err := auth.CreateProjectInvite(ctx, alice, "SDLC", domain.MemberViewer, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		racers := []Identity{
			identityOf(t, store, fmt.Sprintf("racer-%d-a", round)),
			identityOf(t, store, fmt.Sprintf("racer-%d-b", round)),
		}
		errs := race(t, racers, func(id Identity) error {
			_, err := auth.AcceptInvite(ctx, id, issued.Token)
			return err
		})
		ok, conflicts := 0, 0
		for _, err := range errs {
			switch codeOf(err) {
			case "":
				ok++
			case apperr.CodeConflict:
				conflicts++
			default:
				t.Fatalf("раунд %d: неожиданная ошибка %v", round, err)
			}
		}
		joined := 0
		for _, id := range racers {
			if role, _ := store.MemberRole(ctx, store.DB(), project.ID, id.Principal.ID); role != "" {
				joined++
			}
		}
		if ok != 1 || conflicts != 1 || joined != 1 {
			t.Fatalf("раунд %d: успехов %d, конфликтов %d, участий %d", round, ok, conflicts, joined)
		}
	}
}

// Два приёма разных ссылок на последнее место в организации: квоту
// проходит один, второй упирается в предел.
func TestInviteQuotaHoldsUnderRace(t *testing.T) {
	ctx := context.Background()
	store, _, _, auth, _ := setupPhase4(t, AuthToken)
	alice := identityOf(t, store, "alice")
	team, err := auth.CreateTeam(ctx, alice, CreateTeamInput{Key: "acme", Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	identityOf(t, store, "bob")
	if _, err := auth.SetTeamMember(ctx, alice, "acme", "user:bob", domain.TeamRoleMember); err != nil {
		t.Fatal(err)
	}

	racers := []Identity{identityOf(t, store, "carol"), identityOf(t, store, "dave")}
	tokens := map[uuid.UUID]string{}
	for _, id := range racers {
		issued, err := auth.CreateTeamInvite(ctx, alice, "acme", domain.TeamRoleMember, nil)
		if err != nil {
			t.Fatal(err)
		}
		tokens[id.Principal.ID] = issued.Token
	}
	errs := race(t, racers, func(id Identity) error {
		_, err := auth.AcceptInvite(ctx, id, tokens[id.Principal.ID])
		return err
	})
	ok, refused := 0, 0
	for _, err := range errs {
		switch codeOf(err) {
		case "":
			ok++
		case apperr.CodeForbidden:
			refused++
		default:
			t.Fatalf("неожиданная ошибка: %v", err)
		}
	}
	n, err := store.CountTeamMembers(ctx, store.DB(), team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok != 1 || refused != 1 || n != 3 {
		t.Fatalf("успехов %d, отказов %d, в команде %d", ok, refused, n)
	}
}

// race запускает fn для каждого участника одновременно и отдаёт ошибки.
func race(t *testing.T, ids []Identity, fn func(Identity) error) []error {
	t.Helper()
	errs := make([]error, len(ids))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn(id)
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

func projectMember(t *testing.T, ctx context.Context, auth *Auth, viewer Identity, handle string) domain.ProjectMember {
	t.Helper()
	members, err := auth.ListMembers(ctx, viewer, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.Handle == handle {
			return m
		}
	}
	t.Fatalf("%s не участник проекта", handle)
	return domain.ProjectMember{}
}

func teamAddedBy(t *testing.T, store *postgres.Store, teamID, principalID uuid.UUID) uuid.UUID {
	t.Helper()
	var by uuid.UUID
	if err := store.DB().QueryRow(context.Background(), `
		SELECT added_by FROM team_members WHERE team_id = $1 AND principal_id = $2`,
		teamID, principalID).Scan(&by); err != nil {
		t.Fatal(err)
	}
	return by
}

func projectAddedBy(t *testing.T, store *postgres.Store, projectID, principalID uuid.UUID) uuid.UUID {
	t.Helper()
	var by uuid.UUID
	if err := store.DB().QueryRow(context.Background(), `
		SELECT added_by FROM project_members WHERE project_id = $1 AND principal_id = $2`,
		projectID, principalID).Scan(&by); err != nil {
		t.Fatal(err)
	}
	return by
}
