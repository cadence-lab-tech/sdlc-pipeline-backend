package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Вход через корпоративный провайдер (решение 0027): принципал ключуется
// `sub`, а состав команд синхронизируется по группам провайдера.

// IdPPrincipalInput — то, что приехало в ID-токене.
type IdPPrincipalInput struct {
	Subject     string
	Handle      string
	DisplayName string
	Email       string
}

// UpsertIdPPrincipal находит принципала по `sub` провайдера или заводит его.
//
// Ключ — именно `sub`: почту и имя в IdP меняют, и связывать по ним значило бы
// заводить второго принципала после смены фамилии. Принципал, заведённый до
// подключения провайдера (администратор бутстрапом, участник, добавленный
// руками), связывается с `sub` по хэндлу — иначе у человека оказалось бы две
// учётки, и права остались бы на прежней.
func (s *Store) UpsertIdPPrincipal(ctx context.Context, q Querier, in IdPPrincipalInput) (domain.Principal, error) {
	var email *string
	if in.Email != "" {
		email = &in.Email
	}
	name := in.DisplayName
	if name == "" {
		name = in.Handle
	}
	// Кандидат выбирается по одному, а не условием «или»: человек с уже
	// связанным `sub` и однофамилец с тем же хэндлом — это два разных
	// принципала, и обновлять обоих нельзя.
	p, err := scanPrincipal(q.QueryRow(ctx, `
		WITH target AS (
			SELECT id, 1 AS rank FROM principals WHERE idp_subject = $1
			UNION ALL
			SELECT id, 2 FROM principals
			WHERE idp_subject IS NULL AND handle = $2 AND kind = 'user'
			ORDER BY rank
			LIMIT 1
		), linked AS (
			UPDATE principals SET
				idp_subject = $1,
				display_name = $3,
				email = coalesce($4, email)
			WHERE id IN (SELECT id FROM target)
			RETURNING `+principalCols+`
		), created AS (
			INSERT INTO principals (kind, handle, display_name, email, idp_subject)
			SELECT 'user', $2, $3, $4, $1
			WHERE NOT EXISTS (SELECT 1 FROM linked)
			RETURNING `+principalCols+`
		)
		SELECT * FROM linked UNION ALL SELECT * FROM created`,
		in.Subject, in.Handle, name, email))
	if err != nil {
		return p, translate(err)
	}
	return p, nil
}

// ── сопоставление групп провайдера с командами ──────────────────────────────

// TeamsForIdPGroups — команды, в которые ведут группы человека у провайдера.
func (s *Store) TeamsForIdPGroups(ctx context.Context, q Querier, groups []string) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT g.team_id
		FROM team_idp_groups g
		JOIN teams t ON t.id = g.team_id
		WHERE g.idp_group = ANY ($1) AND t.archived_at IS NULL`, groups)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, translate(err)
		}
		out = append(out, id)
	}
	return out, translate(rows.Err())
}

// SyncedTeamsOf — команды принципала, состав которых ведёт провайдер. Только
// они и снимаются при входе: команду, набранную руками, синхронизация не
// трогает.
func (s *Store) SyncedTeamsOf(ctx context.Context, q Querier, principalID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT m.team_id
		FROM team_members m
		WHERE m.principal_id = $1
		  AND EXISTS (SELECT 1 FROM team_idp_groups g WHERE g.team_id = m.team_id)`,
		principalID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, translate(err)
		}
		out = append(out, id)
	}
	return out, translate(rows.Err())
}

func (s *Store) ListTeamIdPGroups(ctx context.Context, q Querier, teamID uuid.UUID) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT idp_group FROM team_idp_groups WHERE team_id = $1 ORDER BY idp_group`, teamID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, translate(err)
		}
		out = append(out, g)
	}
	return out, translate(rows.Err())
}

// SetTeamIdPGroups заменяет набор групп команды целиком: набор — это
// настройка, а не журнал, и удалять группы по одной незачем.
func (s *Store) SetTeamIdPGroups(ctx context.Context, q Querier, teamID uuid.UUID, groups []string, addedBy uuid.UUID) error {
	if _, err := q.Exec(ctx, `DELETE FROM team_idp_groups WHERE team_id = $1`, teamID); err != nil {
		return translate(err)
	}
	for _, g := range groups {
		if _, err := q.Exec(ctx, `
			INSERT INTO team_idp_groups (team_id, idp_group, added_by)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, teamID, g, addedBy); err != nil {
			return translate(err)
		}
	}
	return nil
}
