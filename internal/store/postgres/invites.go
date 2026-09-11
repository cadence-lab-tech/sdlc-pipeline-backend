package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Приглашения ссылкой (решение 0035). Хеш токена наружу не выходит: выборка
// его не читает, и доменная структура о нём не знает. Права и статусы — в
// workflow.Auth, здесь только запросы.

const inviteSelect = `
	SELECT i.id, i.team_id, i.project_id, t.key, pr.key, coalesce(t.name, pr.name),
	       i.team_role, i.level, i.role_keys,
	       i.created_by, cb.handle, i.created_at, i.expires_at,
	       ab.handle, i.accepted_at, rb.handle, i.revoked_at
	FROM invites i
	LEFT JOIN teams t ON t.id = i.team_id
	LEFT JOIN projects pr ON pr.id = i.project_id
	JOIN principals cb ON cb.id = i.created_by
	LEFT JOIN principals ab ON ab.id = i.accepted_by
	LEFT JOIN principals rb ON rb.id = i.revoked_by`

func scanInvite(row interface{ Scan(...any) error }) (domain.Invite, error) {
	var i domain.Invite
	err := row.Scan(&i.ID, &i.TeamID, &i.ProjectID, &i.TeamKey, &i.ProjectKey, &i.TargetName,
		&i.TeamRole, &i.Level, &i.RoleKeys,
		&i.CreatedBy, &i.CreatedByHandle, &i.CreatedAt, &i.ExpiresAt,
		&i.AcceptedByHandle, &i.AcceptedAt, &i.RevokedByHandle, &i.RevokedAt)
	if err != nil {
		return i, err
	}
	i.TargetKind = domain.InviteProject
	if i.TeamID != nil {
		i.TargetKind = domain.InviteTeam
	}
	i.Status = i.StatusAt(time.Now())
	return i, nil
}

type CreateInviteInput struct {
	// Ровно одна цель; поля второй остаются nil.
	TeamID    *uuid.UUID
	TeamRole  *domain.TeamRole
	ProjectID *uuid.UUID
	Level     *domain.MemberRole
	RoleKeys  []string
	TokenHash string
	CreatedBy uuid.UUID
	ExpiresAt time.Time
}

func (s *Store) CreateInvite(ctx context.Context, q Querier, in CreateInviteInput) (domain.Invite, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO invites (team_id, team_role, project_id, level, role_keys, token_hash, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		in.TeamID, in.TeamRole, in.ProjectID, in.Level, in.RoleKeys, in.TokenHash, in.CreatedBy, in.ExpiresAt).Scan(&id)
	if err != nil {
		return domain.Invite{}, translate(err)
	}
	return s.GetInvite(ctx, q, id, false)
}

// GetInvite — приглашение по идентификатору; lock берёт строку под
// FOR UPDATE, чтобы отзыв и приём не прошли на одном состоянии.
func (s *Store) GetInvite(ctx context.Context, q Querier, id uuid.UUID, lock bool) (domain.Invite, error) {
	return s.oneInvite(ctx, q, `i.id = $1`, id, lock)
}

// InviteByHash — приглашение по хешу предъявленного токена.
func (s *Store) InviteByHash(ctx context.Context, q Querier, hash string, lock bool) (domain.Invite, error) {
	return s.oneInvite(ctx, q, `i.token_hash = $1`, hash, lock)
}

func (s *Store) oneInvite(ctx context.Context, q Querier, where string, arg any, lock bool) (domain.Invite, error) {
	sql := inviteSelect + ` WHERE ` + where
	if lock {
		// OF i: блокировка стороны внешнего соединения запрещена, а
		// блокировать нужно только само приглашение.
		sql += ` FOR UPDATE OF i`
	}
	inv, err := scanInvite(q.QueryRow(ctx, sql, arg))
	if err != nil {
		return inv, notFound(err, "приглашение не найдено")
	}
	return inv, nil
}

// ListInvites — приглашения команды или проекта, свежие сверху.
func (s *Store) ListInvites(ctx context.Context, q Querier, teamID, projectID *uuid.UUID) ([]domain.Invite, error) {
	rows, err := q.Query(ctx, inviteSelect+`
		WHERE ($1::uuid IS NULL OR i.team_id = $1)
		  AND ($2::uuid IS NULL OR i.project_id = $2)
		ORDER BY i.created_at DESC`, teamID, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Invite{}
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, inv)
	}
	return out, translate(rows.Err())
}

func (s *Store) RevokeInvite(ctx context.Context, q Querier, id, by uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE invites SET revoked_at = now(), revoked_by = $2 WHERE id = $1`, id, by)
	return translate(err)
}

func (s *Store) MarkInviteAccepted(ctx context.Context, q Querier, id, by uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE invites SET accepted_at = now(), accepted_by = $2 WHERE id = $1`, id, by)
	return translate(err)
}

// LockTeam берёт команду под FOR UPDATE: приёмы в одну команду выстраиваются
// в очередь до подсчёта состава, и два приёма разных ссылок на последнее
// место не проходят квоту оба.
func (s *Store) LockTeam(ctx context.Context, q Querier, id uuid.UUID) (domain.Team, error) {
	t, err := scanTeam(q.QueryRow(ctx, `SELECT `+teamCols+` FROM teams WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return t, notFound(err, "команда не найдена")
	}
	return t, nil
}
