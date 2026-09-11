// Приглашения ссылкой (решение 0035).
//
// Приглашение — отложенный SetTeamMember или SetMember: права на выдачу те
// же, что на прямое добавление, а участником становится тот, кто вошёл и
// предъявил ссылку. Правила живут в Auth рядом с прямым добавлением, чтобы
// «кому что можно» не расходилось между двумя путями в одну цель.
package workflow

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// InvitePrefix отличает ссылку от токена доступа: предъявленная как Bearer,
// она не найдётся среди agent_tokens и сессией не станет.
const InvitePrefix = "clab_inv_"

const (
	inviteDaysDefault = 7
	inviteDaysMax     = 30
)

// IssuedInvite — ответ на выдачу: единственный момент, когда токен виден.
type IssuedInvite struct {
	Invite domain.Invite `json:"invite"`
	Token  string        `json:"token"`
}

// InviteTarget — куда привело приглашение: клиенту для перехода после приёма.
type InviteTarget struct {
	Kind domain.InviteTargetKind `json:"kind"`
	Key  string                  `json:"key"`
	Name string                  `json:"name"`
}

type AcceptedInvite struct {
	Invite domain.Invite `json:"invite"`
	// Участие уже было и не менялось: смена уровня — прямое действие
	// администратора, а не побочный эффект ссылки.
	AlreadyMember bool         `json:"already_member"`
	Target        InviteTarget `json:"target"`
}

// CreateTeamInvite — приглашение в организацию. Выдаёт лид; лид — максимум в
// команде, поэтому отдельного «не выше своего» здесь не нужно. Квота на
// выдаче не считается: протухшие ссылки держали бы места.
func (a *Auth) CreateTeamInvite(ctx context.Context, id Identity, key string, role domain.TeamRole, days *int) (IssuedInvite, error) {
	if err := requireHuman(id); err != nil {
		return IssuedInvite{}, err
	}
	if !role.Valid() {
		return IssuedInvite{}, apperr.InvalidInput("роль в команде: member или lead, а не %q", role)
	}
	expires, err := inviteExpiry(days)
	if err != nil {
		return IssuedInvite{}, err
	}
	var out IssuedInvite
	err = a.store.InTx(ctx, func(tx pgx.Tx) error {
		team, err := liveTeam(ctx, a.store, tx, key)
		if err != nil {
			return err
		}
		if _, err := a.teamRole(ctx, tx, id, team.ID, true); err != nil {
			return err
		}
		if team.Kind == domain.TeamPersonal {
			return errPersonalTeam()
		}
		out, err = a.issueInvite(ctx, tx, postgres.CreateInviteInput{
			TeamID: &team.ID, TeamRole: &role, CreatedBy: id.Principal.ID, ExpiresAt: expires,
		})
		return err
	})
	return out, err
}

// CreateProjectInvite — приглашение в проект. Выдаёт администратор проекта,
// и уровень не выше его действующего: сегодня это недостижимо, но правило
// общее и удержит будущую выдачу мейнтейнером.
func (a *Auth) CreateProjectInvite(ctx context.Context, id Identity, key string, level domain.MemberRole, roleKeys []string, days *int) (IssuedInvite, error) {
	if err := requireHuman(id); err != nil {
		return IssuedInvite{}, err
	}
	if !level.Valid() {
		return IssuedInvite{}, apperr.InvalidInput(
			"уровень участия: viewer, agent, maintainer или admin, а не %q", level)
	}
	expires, err := inviteExpiry(days)
	if err != nil {
		return IssuedInvite{}, err
	}
	var out IssuedInvite
	err = a.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := liveProject(ctx, a.store, tx, key)
		if err != nil {
			return err
		}
		if err := a.Require(ctx, id, project.ID, domain.MemberAdmin); err != nil {
			return err
		}
		own, err := a.Level(ctx, id, project.ID)
		if err != nil {
			return err
		}
		if !own.Covers(level) {
			return apperr.Forbidden("приглашение не даёт больше, чем есть у выдающего: у вас %s", own)
		}
		if err := a.checkRoleKeys(ctx, tx, project.ID, roleKeys); err != nil {
			return err
		}
		out, err = a.issueInvite(ctx, tx, postgres.CreateInviteInput{
			ProjectID: &project.ID, Level: &level, RoleKeys: roleKeys,
			CreatedBy: id.Principal.ID, ExpiresAt: expires,
		})
		return err
	})
	return out, err
}

func (a *Auth) issueInvite(ctx context.Context, q postgres.Querier, in postgres.CreateInviteInput) (IssuedInvite, error) {
	token, err := newSecret(InvitePrefix)
	if err != nil {
		return IssuedInvite{}, err
	}
	in.TokenHash = HashToken(token)
	inv, err := a.store.CreateInvite(ctx, q, in)
	if err != nil {
		return IssuedInvite{}, err
	}
	return IssuedInvite{Invite: inv, Token: token}, nil
}

// ListTeamInvites — приглашения организации для её лида.
func (a *Auth) ListTeamInvites(ctx context.Context, id Identity, key string) ([]domain.Invite, error) {
	q := a.store.DB()
	team, err := a.store.GetTeamByKey(ctx, q, key)
	if err != nil {
		return nil, err
	}
	if _, err := a.teamRole(ctx, q, id, team.ID, true); err != nil {
		return nil, err
	}
	return a.store.ListTeamInvites(ctx, q, team.ID)
}

// ListProjectInvites — приглашения проекта для его администратора.
func (a *Auth) ListProjectInvites(ctx context.Context, id Identity, key string) ([]domain.Invite, error) {
	q := a.store.DB()
	project, err := a.store.GetProjectByKey(ctx, q, key)
	if err != nil {
		return nil, err
	}
	if err := a.Require(ctx, id, project.ID, domain.MemberAdmin); err != nil {
		return nil, err
	}
	return a.store.ListProjectInvites(ctx, q, project.ID)
}

// RevokeInvite — отзыв выданного. Отзывает тот, кто вправе выдать
// приглашение в эту цель, а не только выдавший: иначе ссылку ушедшего
// администратора было бы некому закрыть.
func (a *Auth) RevokeInvite(ctx context.Context, id Identity, inviteID uuid.UUID) (domain.Invite, error) {
	var out domain.Invite
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := a.store.GetInvite(ctx, tx, inviteID, true)
		if err != nil {
			return err
		}
		if inv.TeamID != nil {
			_, err = a.teamRole(ctx, tx, id, *inv.TeamID, true)
		} else {
			err = a.Require(ctx, id, *inv.ProjectID, domain.MemberAdmin)
		}
		if err != nil {
			return err
		}
		if inv.Status != domain.InvitePending {
			return inviteClosed(inv)
		}
		if err := a.store.RevokeInvite(ctx, tx, inv.ID, id.Principal.ID); err != nil {
			return err
		}
		out, err = a.store.GetInvite(ctx, tx, inv.ID, false)
		return err
	})
	return out, err
}

// LookupInvite показывает, куда зовёт ссылка, до приёма. Прав не требует,
// кроме входа: предъявленный токен и есть доказательство, что ссылку дали.
func (a *Auth) LookupInvite(ctx context.Context, id Identity, token string) (domain.Invite, error) {
	return a.inviteByToken(ctx, a.store.DB(), token, false)
}

// AcceptInvite создаёт участие на вошедшего. Всё одной транзакцией под
// блокировкой приглашения: два одновременных приёма одной ссылки дают одно
// участие. Отказ по квоте откатывает транзакцию, и приглашение остаётся
// выданным — его примут, когда место появится.
func (a *Auth) AcceptInvite(ctx context.Context, id Identity, token string) (AcceptedInvite, error) {
	if err := requireHuman(id); err != nil {
		return AcceptedInvite{}, err
	}
	var out AcceptedInvite
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := a.inviteByToken(ctx, tx, token, true)
		if err != nil {
			return err
		}
		if inv.Status != domain.InvitePending {
			return inviteClosed(inv)
		}
		if inv.TeamID != nil {
			out.AlreadyMember, err = a.acceptIntoTeam(ctx, tx, id, inv)
			out.Target = InviteTarget{Kind: domain.InviteTeam, Key: *inv.TeamKey, Name: inv.TargetName}
		} else {
			out.AlreadyMember, err = a.acceptIntoProject(ctx, tx, id, inv)
			out.Target = InviteTarget{Kind: domain.InviteProject, Key: *inv.ProjectKey, Name: inv.TargetName}
		}
		if err != nil {
			return err
		}
		if err := a.store.MarkInviteAccepted(ctx, tx, inv.ID, id.Principal.ID); err != nil {
			return err
		}
		out.Invite, err = a.store.GetInvite(ctx, tx, inv.ID, false)
		return err
	})
	return out, err
}

func (a *Auth) acceptIntoTeam(ctx context.Context, tx pgx.Tx, id Identity, inv domain.Invite) (bool, error) {
	// Команда под блокировкой до подсчёта: иначе два приёма разных ссылок
	// на последнее место оба прошли бы квоту.
	team, err := a.store.LockTeam(ctx, tx, *inv.TeamID)
	if err != nil {
		return false, err
	}
	if team.ArchivedAt != nil {
		return false, apperr.NotFound("команда %q в архиве", team.Key)
	}
	current, err := a.store.TeamRoleOf(ctx, tx, team.ID, id.Principal.ID)
	if err != nil || current != "" {
		return current != "", err
	}
	if err := a.memberQuota(ctx, tx, team, 1); err != nil {
		return false, err
	}
	// added_by — выдавший: право на участие дал он, а принявший только
	// воспользовался им; сам приём помнит приглашение.
	_, err = a.store.SetTeamMember(ctx, tx, team.ID, id.Principal.ID, *inv.TeamRole, inv.CreatedBy)
	return false, err
}

func (a *Auth) acceptIntoProject(ctx context.Context, tx pgx.Tx, id Identity, inv domain.Invite) (bool, error) {
	project, err := liveProject(ctx, a.store, tx, *inv.ProjectKey)
	if err != nil {
		return false, err
	}
	// Уже участник — это прямое участие. Кто видит проект через команду,
	// получает своё: оно переживёт уход из команды, ради этого и звали.
	current, err := a.store.MemberRole(ctx, tx, project.ID, id.Principal.ID)
	if err != nil || current != "" {
		return current != "", err
	}
	if _, err := a.store.SetMember(ctx, tx, project.ID, id.Principal.ID, *inv.Level, inv.CreatedBy); err != nil {
		return false, err
	}
	return false, a.store.SetMemberRoles(ctx, tx, project.ID, id.Principal.ID, inv.RoleKeys)
}

func (a *Auth) inviteByToken(ctx context.Context, q postgres.Querier, token string, lock bool) (domain.Invite, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, InvitePrefix) {
		return domain.Invite{}, apperr.NotFound("приглашение не найдено")
	}
	return a.store.InviteByHash(ctx, q, HashToken(token), lock)
}

// requireHuman: приглашения выдаёт и принимает человек. Проверяется в любом
// режиме, как апрув человека: это защита от агента, взявшего не свою работу,
// а не от злого умысла.
func requireHuman(id Identity) error {
	if id.Principal.Kind != domain.PrincipalUser {
		return apperr.Forbidden("приглашения выдаёт и принимает человек, а не %s", id.Principal.Kind)
	}
	return nil
}

func inviteExpiry(days *int) (time.Time, error) {
	n := inviteDaysDefault
	if days != nil {
		n = *days
	}
	if n < 1 || n > inviteDaysMax {
		return time.Time{}, apperr.InvalidInput("срок приглашения — от 1 до %d дней, а не %d", inviteDaysMax, n)
	}
	return time.Now().Add(time.Duration(n) * 24 * time.Hour), nil
}

func inviteClosed(inv domain.Invite) error {
	switch inv.Status {
	case domain.InviteAccepted:
		return apperr.Conflict("приглашение уже принято")
	case domain.InviteRevoked:
		return apperr.Conflict("приглашение отозвано")
	case domain.InviteExpired:
		return apperr.Conflict("срок приглашения истёк %s", inv.ExpiresAt.Format("2006-01-02 15:04 MST"))
	}
	return nil
}

func liveTeam(ctx context.Context, store *postgres.Store, q postgres.Querier, key string) (domain.Team, error) {
	team, err := store.GetTeamByKey(ctx, q, key)
	if err != nil {
		return team, err
	}
	if team.ArchivedAt != nil {
		return team, apperr.NotFound("команда %q в архиве", key)
	}
	return team, nil
}

func liveProject(ctx context.Context, store *postgres.Store, q postgres.Querier, key string) (domain.Project, error) {
	project, err := store.GetProjectByKey(ctx, q, key)
	if err != nil {
		return project, err
	}
	if project.ArchivedAt != nil {
		return project, apperr.NotFound("проект %q в архиве", key)
	}
	return project, nil
}
