package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

// Аутентификация и права. В базе лежит только хеш токена: сам токен
// показывается один раз при выдаче, и утечка дампа базы не даёт войти.

const principalCols = `id, kind, handle, display_name, email, is_admin, created_at`

func scanPrincipal(row interface{ Scan(...any) error }) (domain.Principal, error) {
	var p domain.Principal
	err := row.Scan(&p.ID, &p.Kind, &p.Handle, &p.DisplayName, &p.Email, &p.IsAdmin, &p.CreatedAt)
	return p, err
}

func (s *Store) GetPrincipal(ctx context.Context, q Querier, id uuid.UUID) (domain.Principal, error) {
	p, err := scanPrincipal(q.QueryRow(ctx,
		`SELECT `+principalCols+` FROM principals WHERE id = $1`, id))
	if err != nil {
		return p, notFound(err, "принципал не найден")
	}
	return p, nil
}

func (s *Store) GetPrincipalByHandle(ctx context.Context, q Querier, handle string) (domain.Principal, error) {
	p, err := scanPrincipal(q.QueryRow(ctx,
		`SELECT `+principalCols+` FROM principals WHERE handle = $1`, handle))
	if err != nil {
		return p, notFound(err, "принципал %q не найден", handle)
	}
	return p, nil
}

// ListPrincipals — все, кто заведён в системе: люди, агенты, сервисы.
// Нужен странице администратора, чтобы добавить человека в команду до того,
// как он впервые придёт с токеном. Фильтр по подстроке идёт и по хэндлу, и по
// имени: администратор ищет по тому, что помнит.
func (s *Store) ListPrincipals(ctx context.Context, q Querier, search string) ([]domain.Principal, error) {
	rows, err := q.Query(ctx, `SELECT `+principalCols+` FROM principals
		WHERE $1 = '' OR handle ILIKE '%' || $1 || '%' OR display_name ILIKE '%' || $1 || '%'
		ORDER BY is_admin DESC, handle`, search)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Principal{}
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, p)
	}
	return out, translate(rows.Err())
}

// SetAdmin выдаёт или снимает глобальные права администратора.
func (s *Store) SetAdmin(ctx context.Context, q Querier, id uuid.UUID, admin bool) error {
	_, err := q.Exec(ctx, `UPDATE principals SET is_admin = $2 WHERE id = $1`, id, admin)
	return translate(err)
}

// ── токены ──────────────────────────────────────────────────────────────────

const tokenSelect = `
	SELECT t.id, t.principal_id, p.handle, p.kind, t.kind, t.project_id, pr.key,
	       t.name, t.token_prefix, t.role_keys, t.created_at,
	       t.expires_at, t.last_used_at, t.revoked_at
	FROM agent_tokens t
	JOIN principals p ON p.id = t.principal_id
	LEFT JOIN projects pr ON pr.id = t.project_id`

func scanToken(row interface{ Scan(...any) error }) (domain.AccessToken, error) {
	var t domain.AccessToken
	err := row.Scan(&t.ID, &t.PrincipalID, &t.Handle, &t.Kind, &t.TokenKind, &t.ProjectID, &t.ProjectKey,
		&t.Name, &t.Prefix, &t.RoleKeys, &t.CreatedAt,
		&t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt)
	if t.RoleKeys == nil {
		t.RoleKeys = []string{}
	}
	return t, err
}

type CreateTokenInput struct {
	PrincipalID uuid.UUID
	ProjectID   *uuid.UUID
	Name        string
	// Вид токена: api (выдан руками) или session (вход через провайдера).
	// Пусто — api.
	Kind      string
	TokenHash string
	Prefix    string
	RoleKeys  []string
	ExpiresAt *time.Time
	CreatedBy uuid.UUID
}

func (s *Store) CreateToken(ctx context.Context, q Querier, in CreateTokenInput) (domain.AccessToken, error) {
	if in.RoleKeys == nil {
		in.RoleKeys = []string{}
	}
	if in.Kind == "" {
		in.Kind = domain.TokenAPI
	}
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO agent_tokens (principal_id, project_id, name, token_hash, token_prefix,
		                          role_keys, expires_at, created_by, kind)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		in.PrincipalID, in.ProjectID, in.Name, in.TokenHash, in.Prefix,
		in.RoleKeys, in.ExpiresAt, in.CreatedBy, in.Kind).Scan(&id)
	if err != nil {
		return domain.AccessToken{}, translate(err)
	}
	return s.GetToken(ctx, q, id)
}

func (s *Store) GetToken(ctx context.Context, q Querier, id uuid.UUID) (domain.AccessToken, error) {
	t, err := scanToken(q.QueryRow(ctx, tokenSelect+` WHERE t.id = $1`, id))
	if err != nil {
		return t, notFound(err, "токен не найден")
	}
	return t, nil
}

// TokenByHash — резолв предъявленного токена. Отозванный и протухший не
// отдаются вовсе: для middleware это одно и то же «токен недействителен».
func (s *Store) TokenByHash(ctx context.Context, q Querier, hash string) (domain.AccessToken, domain.Principal, error) {
	var t domain.AccessToken
	var p domain.Principal
	err := q.QueryRow(ctx, `
		SELECT t.id, t.principal_id, p.handle, p.kind, t.kind, t.project_id, pr.key,
		       t.name, t.token_prefix, t.role_keys, t.created_at,
		       t.expires_at, t.last_used_at, t.revoked_at,
		       p.id, p.kind, p.handle, p.display_name, p.email, p.is_admin, p.created_at
		FROM agent_tokens t
		JOIN principals p ON p.id = t.principal_id
		LEFT JOIN projects pr ON pr.id = t.project_id
		WHERE t.token_hash = $1
		  AND t.revoked_at IS NULL
		  AND (t.expires_at IS NULL OR t.expires_at > now())
		  AND p.disabled_at IS NULL`, hash,
	).Scan(&t.ID, &t.PrincipalID, &t.Handle, &t.Kind, &t.TokenKind, &t.ProjectID, &t.ProjectKey,
		&t.Name, &t.Prefix, &t.RoleKeys, &t.CreatedAt,
		&t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt,
		&p.ID, &p.Kind, &p.Handle, &p.DisplayName, &p.Email, &p.IsAdmin, &p.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return t, p, apperr.New(apperr.CodeUnauthenticated, "токен недействителен")
		}
		return t, p, translate(err)
	}
	if t.RoleKeys == nil {
		t.RoleKeys = []string{}
	}
	return t, p, nil
}

// TouchToken отмечает использование. Раз в минуту, не на каждый запрос:
// иначе каждый heartbeat агента превращался бы в запись в таблицу токенов.
func (s *Store) TouchToken(ctx context.Context, q Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE agent_tokens SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, id)
	return translate(err)
}

// ListTokens — токены принципала либо все токены проекта.
func (s *Store) ListTokens(ctx context.Context, q Querier, principalID *uuid.UUID, projectID *uuid.UUID) ([]domain.AccessToken, error) {
	rows, err := q.Query(ctx, tokenSelect+`
		WHERE ($1::uuid IS NULL OR t.principal_id = $1)
		  AND ($2::uuid IS NULL OR t.project_id = $2)
		ORDER BY t.revoked_at NULLS FIRST, t.created_at DESC`, principalID, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.AccessToken{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, t)
	}
	return out, translate(rows.Err())
}

func (s *Store) RevokeToken(ctx context.Context, q Querier, id uuid.UUID) error {
	tag, err := q.Exec(ctx, `
		UPDATE agent_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("токен не найден или уже отозван")
	}
	return nil
}

// UpsertBootstrapToken заводит токен из окружения. Хеш — ключ: тот же
// токен в окружении при рестарте не плодит записи, другой — добавляет новую,
// а прежняя остаётся действующей, пока её не отзовут.
func (s *Store) UpsertBootstrapToken(ctx context.Context, q Querier, principalID uuid.UUID, hash, prefix string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO agent_tokens (principal_id, name, token_hash, token_prefix, created_by)
		VALUES ($1, 'bootstrap', $2, $3, $1)
		ON CONFLICT (token_hash) DO UPDATE SET revoked_at = NULL`, principalID, hash, prefix)
	return translate(err)
}

// ── участники проекта ───────────────────────────────────────────────────────

const memberSelect = `
	SELECT m.project_id, pr.key, m.principal_id, p.handle, p.kind, p.display_name,
	       m.role, m.role_keys, m.created_at
	FROM project_members m
	JOIN principals p ON p.id = m.principal_id
	JOIN projects pr ON pr.id = m.project_id`

func scanMember(row interface{ Scan(...any) error }) (domain.ProjectMember, error) {
	var m domain.ProjectMember
	err := row.Scan(&m.ProjectID, &m.ProjectKey, &m.PrincipalID, &m.Handle, &m.Kind,
		&m.DisplayName, &m.Role, &m.RoleKeys, &m.CreatedAt)
	return m, err
}

func (s *Store) ListMembers(ctx context.Context, q Querier, projectID uuid.UUID) ([]domain.ProjectMember, error) {
	rows, err := q.Query(ctx, memberSelect+`
		WHERE m.project_id = $1 ORDER BY m.role DESC, p.handle`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.ProjectMember{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

// Memberships — во всех каких проектах состоит принципал. Нужно списку
// проектов и ответу /auth/me.
func (s *Store) Memberships(ctx context.Context, q Querier, principalID uuid.UUID) ([]domain.ProjectMember, error) {
	rows, err := q.Query(ctx, memberSelect+`
		WHERE m.principal_id = $1 ORDER BY pr.key`, principalID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.ProjectMember{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

// MemberRole отдаёт уровень прав принципала в проекте; пустая строка —
// не участник.
func (s *Store) MemberRole(ctx context.Context, q Querier, projectID, principalID uuid.UUID) (domain.MemberRole, error) {
	var role domain.MemberRole
	err := q.QueryRow(ctx, `
		SELECT role FROM project_members WHERE project_id = $1 AND principal_id = $2`,
		projectID, principalID).Scan(&role)
	if isNoRows(err) {
		return "", nil
	}
	return role, translate(err)
}

func (s *Store) SetMember(ctx context.Context, q Querier, projectID, principalID uuid.UUID, role domain.MemberRole, addedBy uuid.UUID) (domain.ProjectMember, error) {
	_, err := q.Exec(ctx, `
		INSERT INTO project_members (project_id, principal_id, role, added_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (project_id, principal_id) DO UPDATE SET role = EXCLUDED.role`,
		projectID, principalID, role, addedBy)
	if err != nil {
		return domain.ProjectMember{}, translate(err)
	}
	m, err := scanMember(q.QueryRow(ctx, memberSelect+`
		WHERE m.project_id = $1 AND m.principal_id = $2`, projectID, principalID))
	if err != nil {
		return m, translate(err)
	}
	return m, nil
}

// SetMemberRoles ограничивает участника набором ролей специалистов
// (решение 0030). nil снимает ограничение; уровень не трогается.
func (s *Store) SetMemberRoles(ctx context.Context, q Querier, projectID, principalID uuid.UUID, roleKeys []string) error {
	tag, err := q.Exec(ctx, `
		UPDATE project_members SET role_keys = $3
		WHERE project_id = $1 AND principal_id = $2`, projectID, principalID, roleKeys)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("участник не найден")
	}
	return nil
}

// RoleGrant — один путь, которым принципал получил уровень в проекте, вместе
// с ролями специалистов, разрешёнными на этом пути.
type RoleGrant struct {
	Level    domain.MemberRole
	RoleKeys []string
}

// RoleGrants — все пути принципала в проект: поимённое участие, команды,
// лидерство в команде-владельце. По ним считается, какие роли специалистов
// он может забирать (решение 0030); сами уровни складывает EffectiveRole.
func (s *Store) RoleGrants(ctx context.Context, q Querier, projectID, principalID uuid.UUID) ([]RoleGrant, error) {
	rows, err := q.Query(ctx, `
		SELECT role, role_keys FROM project_members
		WHERE project_id = $1 AND principal_id = $2
		UNION ALL
		SELECT pt.level, pt.role_keys FROM project_teams pt
		JOIN team_members tm ON tm.team_id = pt.team_id
		JOIN teams t ON t.id = pt.team_id
		WHERE pt.project_id = $1 AND tm.principal_id = $2 AND t.archived_at IS NULL
		UNION ALL
		SELECT 'admin', NULL FROM projects pr
		JOIN team_members tm ON tm.team_id = pr.owner_team_id
		WHERE pr.id = $1 AND tm.principal_id = $2 AND tm.role = 'lead'`,
		projectID, principalID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []RoleGrant{}
	for rows.Next() {
		var g RoleGrant
		if err := rows.Scan(&g.Level, &g.RoleKeys); err != nil {
			return nil, translate(err)
		}
		out = append(out, g)
	}
	return out, translate(rows.Err())
}

func (s *Store) RemoveMember(ctx context.Context, q Querier, projectID, principalID uuid.UUID) error {
	tag, err := q.Exec(ctx, `
		DELETE FROM project_members WHERE project_id = $1 AND principal_id = $2`,
		projectID, principalID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("участник не найден")
	}
	return nil
}

// CountAdmins — сколько у проекта администраторов, считая полученных через
// команду и лида команды-владельца. Последнего снять нельзя: проект без
// администратора никто не сможет починить. Считать только поимённых значило
// бы запрещать снятие там, где права всё равно есть у команды.
func (s *Store) CountAdmins(ctx context.Context, q Querier, projectID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT principal_id FROM project_members
			WHERE project_id = $1 AND role = 'admin'
			UNION
			SELECT tm.principal_id FROM project_teams pt
			JOIN team_members tm ON tm.team_id = pt.team_id
			JOIN teams t ON t.id = pt.team_id
			WHERE pt.project_id = $1 AND pt.level = 'admin' AND t.archived_at IS NULL
			UNION
			SELECT tm.principal_id FROM projects pr
			JOIN team_members tm ON tm.team_id = pr.owner_team_id
			WHERE pr.id = $1 AND tm.role = 'lead'
		) admins`, projectID).Scan(&n)
	return n, translate(err)
}

// ListProjectsFor — проекты, доступные принципалу: где он участвует сам, где
// участвует его команда и где его команда — владелец (решение 0027).
// Администратор видит все.
func (s *Store) ListProjectsFor(ctx context.Context, q Querier, p domain.Principal) ([]domain.Project, error) {
	if p.IsAdmin {
		return s.ListProjects(ctx, q)
	}
	rows, err := q.Query(ctx, `SELECT `+projectCols+`
		FROM projects
		WHERE archived_at IS NULL
		  AND (id IN (SELECT project_id FROM project_members WHERE principal_id = $1)
		    OR id IN (SELECT pt.project_id FROM project_teams pt
		              JOIN team_members tm ON tm.team_id = pt.team_id
		              WHERE tm.principal_id = $1)
		    OR owner_team_id IN (SELECT team_id FROM team_members
		                         WHERE principal_id = $1 AND role = 'lead'))
		ORDER BY key`, p.ID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Project{}
	for rows.Next() {
		pr, err := scanProject(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, pr)
	}
	return out, translate(rows.Err())
}

// ── вебхуки ─────────────────────────────────────────────────────────────────

// RecordDelivery запоминает доставку; false — такая уже была, и повтор от
// провайдера обрабатывать не нужно.
func (s *Store) RecordDelivery(ctx context.Context, q Querier, provider, deliveryID, event string) (bool, error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO webhook_deliveries (provider, delivery_id, event)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, provider, deliveryID, event)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() == 1, nil
}
