package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Команды: группы принципалов, участвующие в проектах целиком (решение 0027).
//
// Действующий уровень в проекте считается здесь одним запросом — максимум из
// прямого участия и участия через команды. Считать его в Go означало бы два
// похода в базу на каждую проверку прав, а проверка идёт на каждый запрос.

const teamCols = `id, key, name, description, kind, personal_of, can_create_projects, created_at, updated_at, archived_at`

func scanTeam(row interface{ Scan(...any) error }) (domain.Team, error) {
	var t domain.Team
	err := row.Scan(&t.ID, &t.Key, &t.Name, &t.Description, &t.Kind, &t.PersonalOf,
		&t.CanCreateProjects, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt)
	return t, err
}

type CreateTeamInput struct {
	Key         string
	Name        string
	Description *string
	// Пусто — организация. Личная команда несёт PersonalOf (решение 0033).
	Kind              domain.TeamKind
	PersonalOf        *uuid.UUID
	CanCreateProjects bool
	CreatedBy         uuid.UUID
}

func (s *Store) CreateTeam(ctx context.Context, q Querier, in CreateTeamInput) (domain.Team, error) {
	kind := in.Kind
	if kind == "" {
		kind = domain.TeamOrganization
	}
	t, err := scanTeam(q.QueryRow(ctx, `
		INSERT INTO teams (key, name, description, kind, personal_of, can_create_projects, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+teamCols,
		in.Key, in.Name, in.Description, kind, in.PersonalOf, in.CanCreateProjects, in.CreatedBy))
	if err != nil {
		return t, translate(err)
	}
	return t, nil
}

// UpdateTeamInput — nil-поля не меняются.
type UpdateTeamInput struct {
	Name              *string
	Description       *string
	CanCreateProjects *bool
}

func (s *Store) UpdateTeam(ctx context.Context, q Querier, id uuid.UUID, in UpdateTeamInput) (domain.Team, error) {
	t, err := scanTeam(q.QueryRow(ctx, `
		UPDATE teams SET
			name = coalesce($2, name),
			description = coalesce($3, description),
			can_create_projects = coalesce($4, can_create_projects),
			updated_at = now()
		WHERE id = $1
		RETURNING `+teamCols, id, in.Name, in.Description, in.CanCreateProjects))
	if err != nil {
		return t, notFound(err, "команда не найдена")
	}
	return t, nil
}

func (s *Store) GetTeamByKey(ctx context.Context, q Querier, key string) (domain.Team, error) {
	t, err := scanTeam(q.QueryRow(ctx, `SELECT `+teamCols+` FROM teams WHERE key = $1`, key))
	if err != nil {
		return t, notFound(err, "команда %q не найдена", key)
	}
	return t, nil
}

// ListTeams — все команды либо только те, где состоит принципал.
func (s *Store) ListTeams(ctx context.Context, q Querier, principalID *uuid.UUID) ([]domain.Team, error) {
	rows, err := q.Query(ctx, `SELECT `+teamCols+`
		FROM teams
		WHERE archived_at IS NULL
		  AND ($1::uuid IS NULL OR id IN (
		      SELECT team_id FROM team_members WHERE principal_id = $1))
		ORDER BY key`, principalID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Team{}
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, t)
	}
	return out, translate(rows.Err())
}

// ── удаление арендатора ─────────────────────────────────────────────────────

// TeamRemovalCounters — что ушло вместе с командой. Как у проекта и отвязки
// репозитория, считается до удаления: после него считать нечего, а человеку
// важно видеть цену действия.
//
// Проекты заполняет вызывающий: он и так держит их список под блокировкой,
// и второй счёт той же строки разошёлся бы с первым при первой же правке.
type TeamRemovalCounters struct {
	Projects    int      `json:"projects"`
	ProjectKeys []string `json:"project_keys"`
	Members     int      `json:"members"`
	// Участие команды в чужих проектах: эти проекты остаются жить, а доступ,
	// который команда давала своим людям, исчезает. Участие в собственных
	// проектах сюда не входит — они уходят целиком и считаются отдельно.
	ProjectGrants int `json:"project_grants"`
	Invites       int `json:"invites"`
	Installations int `json:"installations"`
	Budgets       int `json:"budgets"`
	IdPGroups     int `json:"idp_groups"`
}

// CountTeamContents считает содержимое команды одним запросом.
func (s *Store) CountTeamContents(ctx context.Context, q Querier, teamID uuid.UUID) (TeamRemovalCounters, error) {
	c := TeamRemovalCounters{ProjectKeys: []string{}}
	err := q.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM team_members         WHERE team_id = $1),
		       (SELECT count(*) FROM project_teams pt
		          JOIN projects p ON p.id = pt.project_id
		                                                  WHERE pt.team_id = $1
		                                                    AND p.owner_team_id <> $1),
		       (SELECT count(*) FROM invites              WHERE team_id = $1),
		       (SELECT count(*) FROM github_installations WHERE owner_team_id = $1),
		       (SELECT count(*) FROM team_budgets         WHERE team_id = $1),
		       (SELECT count(*) FROM team_idp_groups      WHERE team_id = $1)`, teamID,
	).Scan(&c.Members, &c.ProjectGrants, &c.Invites, &c.Installations, &c.Budgets, &c.IdPGroups)
	if err != nil {
		return c, translate(err)
	}
	return c, nil
}

// LockTeamForRemoval берёт строку команды под FOR UPDATE. Этим же держатся
// её проекты и установки: вставка проверяет внешний ключ на teams и на время
// блокировки ждёт, так что между подсчётом и удалением у команды не появится
// ни проекта, ни установки. Заодно два одновременных удаления одного ключа
// выстраиваются в очередь, и второе видит уже удалённую команду.
func (s *Store) LockTeamForRemoval(ctx context.Context, tx pgx.Tx, teamID uuid.UUID) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM teams WHERE id = $1 FOR UPDATE`, teamID).Scan(&id)
	if err != nil {
		return notFound(err, "команда не найдена")
	}
	return nil
}

// ProjectsOfTeam — проекты, которыми команда владеет, под FOR UPDATE и в
// порядке ключа: удаление идёт по ним по одному, и порядок держит очередь
// блокировок предсказуемой.
func (s *Store) ProjectsOfTeam(ctx context.Context, tx pgx.Tx, teamID uuid.UUID) ([]domain.Project, error) {
	rows, err := tx.Query(ctx, `SELECT `+projectCols+`
		FROM projects WHERE owner_team_id = $1 ORDER BY key FOR UPDATE`, teamID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, p)
	}
	return out, translate(rows.Err())
}

// DeleteTeam удаляет команду. Установки GitHub убираются явно: у
// github_installations.owner_team_id стоит ON DELETE RESTRICT
// (migrations/000019_github_installations.up.sql:21), и без этой строки
// удаление команды с подключённым GitHub падало бы на внешнем ключе.
// Потерять чужое этим нельзя: установка видна только проектам своей
// команды-владельца (workflow/installations.go, installationOf), а доставки
// вебхуков по ней дальше идут штатным путём «установка не подключена».
// Остальное — состав, участие в проектах, приглашения, бюджеты, группы
// провайдера, подписка и лента team_events — уходит каскадом схемы.
//
// Проекты команды должны быть удалены до вызова: на projects.owner_team_id
// тоже RESTRICT, и это намеренно — проект удаляется кодом DeleteProject, а не
// каскадом.
func (s *Store) DeleteTeam(ctx context.Context, tx pgx.Tx, teamID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM github_installations WHERE owner_team_id = $1`, teamID); err != nil {
		return translate(err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM teams WHERE id = $1`, teamID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("команда уже удалена")
	}
	return nil
}

// ── состав команды ──────────────────────────────────────────────────────────

const teamMemberSelect = `
	SELECT m.team_id, t.key, t.kind, m.principal_id, p.handle, p.kind, p.display_name,
	       m.role, m.created_at
	FROM team_members m
	JOIN principals p ON p.id = m.principal_id
	JOIN teams t ON t.id = m.team_id`

func scanTeamMember(row interface{ Scan(...any) error }) (domain.TeamMember, error) {
	var m domain.TeamMember
	err := row.Scan(&m.TeamID, &m.TeamKey, &m.TeamKind, &m.PrincipalID, &m.Handle, &m.Kind,
		&m.DisplayName, &m.Role, &m.CreatedAt)
	return m, err
}

func (s *Store) ListTeamMembers(ctx context.Context, q Querier, teamID uuid.UUID) ([]domain.TeamMember, error) {
	rows, err := q.Query(ctx, teamMemberSelect+`
		WHERE m.team_id = $1 ORDER BY m.role DESC, p.handle`, teamID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.TeamMember{}
	for rows.Next() {
		m, err := scanTeamMember(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

// TeamsOf — команды принципала вместе с его ролью в них. Нужно ответу
// /auth/me и проверке «лид ли он».
func (s *Store) TeamsOf(ctx context.Context, q Querier, principalID uuid.UUID) ([]domain.TeamMember, error) {
	rows, err := q.Query(ctx, teamMemberSelect+`
		WHERE m.principal_id = $1 AND t.archived_at IS NULL ORDER BY t.key`, principalID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.TeamMember{}
	for rows.Next() {
		m, err := scanTeamMember(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

func (s *Store) SetTeamMember(ctx context.Context, q Querier, teamID, principalID uuid.UUID, role domain.TeamRole, addedBy uuid.UUID) (domain.TeamMember, error) {
	_, err := q.Exec(ctx, `
		INSERT INTO team_members (team_id, principal_id, role, added_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (team_id, principal_id) DO UPDATE SET role = EXCLUDED.role`,
		teamID, principalID, role, addedBy)
	if err != nil {
		return domain.TeamMember{}, translate(err)
	}
	m, err := scanTeamMember(q.QueryRow(ctx, teamMemberSelect+`
		WHERE m.team_id = $1 AND m.principal_id = $2`, teamID, principalID))
	return m, translate(err)
}

func (s *Store) RemoveTeamMember(ctx context.Context, q Querier, teamID, principalID uuid.UUID) error {
	tag, err := q.Exec(ctx, `
		DELETE FROM team_members WHERE team_id = $1 AND principal_id = $2`, teamID, principalID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("в команде нет такого участника")
	}
	return nil
}

// TeamRoleOf — роль принципала в команде; пустая строка — не состоит.
func (s *Store) TeamRoleOf(ctx context.Context, q Querier, teamID, principalID uuid.UUID) (domain.TeamRole, error) {
	var role domain.TeamRole
	err := q.QueryRow(ctx, `
		SELECT role FROM team_members WHERE team_id = $1 AND principal_id = $2`,
		teamID, principalID).Scan(&role)
	if isNoRows(err) {
		return "", nil
	}
	return role, translate(err)
}

// CountTeamLeads — сколько у команды лидов. Последнего не снять: команда без
// лида не сможет ни принять человека, ни завести проект.
func (s *Store) CountTeamLeads(ctx context.Context, q Querier, teamID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM team_members WHERE team_id = $1 AND role = 'lead'`,
		teamID).Scan(&n)
	return n, translate(err)
}

// ── участие команды в проекте ───────────────────────────────────────────────

const projectTeamSelect = `
	SELECT pt.project_id, pr.key, pt.team_id, t.key, t.name, pt.level, pt.role_keys, pt.created_at
	FROM project_teams pt
	JOIN teams t ON t.id = pt.team_id
	JOIN projects pr ON pr.id = pt.project_id`

func scanProjectTeam(row interface{ Scan(...any) error }) (domain.ProjectTeam, error) {
	var pt domain.ProjectTeam
	err := row.Scan(&pt.ProjectID, &pt.ProjectKey, &pt.TeamID, &pt.TeamKey, &pt.TeamName,
		&pt.Level, &pt.RoleKeys, &pt.CreatedAt)
	return pt, err
}

// ListProjectTeams — команды проекта либо проекты команды.
func (s *Store) ListProjectTeams(ctx context.Context, q Querier, projectID, teamID *uuid.UUID) ([]domain.ProjectTeam, error) {
	rows, err := q.Query(ctx, projectTeamSelect+`
		WHERE ($1::uuid IS NULL OR pt.project_id = $1)
		  AND ($2::uuid IS NULL OR pt.team_id = $2)
		ORDER BY t.key`, projectID, teamID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.ProjectTeam{}
	for rows.Next() {
		pt, err := scanProjectTeam(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, pt)
	}
	return out, translate(rows.Err())
}

// SetProjectTeam выдаёт команде уровень и набор ролей специалистов
// (решение 0030); roleKeys nil — любые роли.
func (s *Store) SetProjectTeam(ctx context.Context, q Querier, projectID, teamID uuid.UUID, level domain.MemberRole, roleKeys []string, addedBy uuid.UUID) (domain.ProjectTeam, error) {
	_, err := q.Exec(ctx, `
		INSERT INTO project_teams (project_id, team_id, level, role_keys, added_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (project_id, team_id) DO UPDATE
		SET level = EXCLUDED.level, role_keys = EXCLUDED.role_keys`,
		projectID, teamID, level, roleKeys, addedBy)
	if err != nil {
		return domain.ProjectTeam{}, translate(err)
	}
	pt, err := scanProjectTeam(q.QueryRow(ctx, projectTeamSelect+`
		WHERE pt.project_id = $1 AND pt.team_id = $2`, projectID, teamID))
	return pt, translate(err)
}

func (s *Store) RemoveProjectTeam(ctx context.Context, q Querier, projectID, teamID uuid.UUID) error {
	tag, err := q.Exec(ctx, `
		DELETE FROM project_teams WHERE project_id = $1 AND team_id = $2`, projectID, teamID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("команда не участвует в проекте")
	}
	return nil
}

// EffectiveRole — действующий уровень принципала в проекте: максимум из
// прямого участия и участия через команды (решение 0027). Пустая строка —
// доступа нет.
//
// Уровни лежат текстом, и сравнивать их приходится по порядку, заданному
// здесь: в базе он тот же, что в domain.MemberRole.Rank.
func (s *Store) EffectiveRole(ctx context.Context, q Querier, projectID, principalID uuid.UUID) (domain.MemberRole, error) {
	var role *string
	err := q.QueryRow(ctx, `
		WITH levels AS (
			SELECT role AS level FROM project_members
			WHERE project_id = $1 AND principal_id = $2
			UNION ALL
			SELECT pt.level FROM project_teams pt
			JOIN team_members tm ON tm.team_id = pt.team_id
			JOIN teams t ON t.id = pt.team_id
			WHERE pt.project_id = $1 AND tm.principal_id = $2 AND t.archived_at IS NULL
			UNION ALL
			-- Лид команды-владельца — администратор её проекта по умолчанию:
			-- иначе, заведя проект команде, он потерял бы управление им, стоило
			-- вывести себя из участников.
			SELECT 'admin' FROM projects pr
			JOIN team_members tm ON tm.team_id = pr.owner_team_id
			WHERE pr.id = $1 AND tm.principal_id = $2 AND tm.role = 'lead'
		)
		SELECT level FROM levels
		ORDER BY array_position(ARRAY['viewer','agent','maintainer','admin'], level) DESC NULLS LAST
		LIMIT 1`, projectID, principalID).Scan(&role)
	if isNoRows(err) {
		return "", nil
	}
	if err != nil {
		return "", translate(err)
	}
	if role == nil {
		return "", nil
	}
	return domain.MemberRole(*role), nil
}

// ── квоты по стоимости ──────────────────────────────────────────────────────

// Расход команды — это стоимость прогонов в проектах, которыми она владеет
// (`projects.owner_team_id`). Проект, где команда просто участвует, платит его
// владелец: иначе один и тот же доллар считался бы дважды.
//
// Бюджет считает только деньги (решение 0062): строки api и без признака.
// Условная стоимость на подписке в расход не входит никогда — её сумма за то
// же окно идёт рядом, для сведения.
const teamSpendQuery = `
	SELECT coalesce(sum(u.cost_usd) FILTER (WHERE ` + moneyPredicate + `), 0)::float8,
	       coalesce(sum(u.cost_usd) FILTER (WHERE ` + subscriptionPredicate + `), 0)::float8
	FROM stage_run_usage u
	JOIN stage_runs sr ON sr.id = u.run_id
	JOIN tasks t ON t.id = sr.task_id
	JOIN projects p ON p.id = t.project_id
	WHERE p.owner_team_id = $1 AND u.reported_at >= date_trunc($2, now())`

// TeamSpend — сколько команда потратила денег с начала окна и сколько за то
// же окно прошло условной стоимостью на подписке.
func (s *Store) TeamSpend(ctx context.Context, q Querier, teamID uuid.UUID, period string) (spent, subscription float64, err error) {
	err = q.QueryRow(ctx, teamSpendQuery, teamID, period).Scan(&spent, &subscription)
	return spent, subscription, translate(err)
}

// ListTeamBudgets — пределы команды по окнам.
func (s *Store) ListTeamBudgets(ctx context.Context, q Querier, teamID uuid.UUID) ([]domain.TeamBudget, error) {
	rows, err := q.Query(ctx, `
		SELECT team_id, period, cost_usd_limit::float8, created_at, updated_at
		FROM team_budgets WHERE team_id = $1 ORDER BY period`, teamID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.TeamBudget{}
	for rows.Next() {
		var b domain.TeamBudget
		if err := rows.Scan(&b.TeamID, &b.Period, &b.LimitUSD, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, translate(err)
		}
		out = append(out, b)
	}
	return out, translate(rows.Err())
}

func (s *Store) SetTeamBudget(ctx context.Context, q Querier, teamID uuid.UUID, period string, limit float64, actor uuid.UUID) (domain.TeamBudget, error) {
	var b domain.TeamBudget
	err := q.QueryRow(ctx, `
		INSERT INTO team_budgets (team_id, period, cost_usd_limit, created_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (team_id, period) DO UPDATE
			SET cost_usd_limit = EXCLUDED.cost_usd_limit, updated_at = now()
		RETURNING team_id, period, cost_usd_limit::float8, created_at, updated_at`,
		teamID, period, limit, actor,
	).Scan(&b.TeamID, &b.Period, &b.LimitUSD, &b.CreatedAt, &b.UpdatedAt)
	return b, translate(err)
}

func (s *Store) DeleteTeamBudget(ctx context.Context, q Querier, teamID uuid.UUID, period string) error {
	tag, err := q.Exec(ctx, `DELETE FROM team_budgets WHERE team_id = $1 AND period = $2`,
		teamID, period)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("у команды нет предела на это окно")
	}
	return nil
}

// ExceededBudget отдаёт превышенный предел команды-владельца проекта; пустой
// Period означает, что превышения нет.
//
// Одним запросом: проверка стоит на пути каждого захвата этапа, и три похода
// в базу там дороже, чем выглядят. Считаются только деньги — тем же
// предикатом, что TeamSpend (решение 0062).
func (s *Store) ExceededBudget(ctx context.Context, q Querier, projectID uuid.UUID) (domain.BudgetState, error) {
	var out domain.BudgetState
	err := q.QueryRow(ctx, `
		WITH owner AS (
			SELECT t.id AS team_id, t.key
			FROM projects p JOIN teams t ON t.id = p.owner_team_id
			WHERE p.id = $1
		), spend AS (
			SELECT o.key, b.period, b.cost_usd_limit::float8 AS lim,
			       coalesce((
			           SELECT sum(u.cost_usd)
			           FROM stage_run_usage u
			           JOIN stage_runs sr ON sr.id = u.run_id
			           JOIN tasks t ON t.id = sr.task_id
			           JOIN projects p ON p.id = t.project_id
			           WHERE p.owner_team_id = o.team_id
			             AND u.reported_at >= date_trunc(b.period, now())
			             AND `+moneyPredicate+`
			       ), 0)::float8 AS spent
			FROM owner o
			JOIN team_budgets b ON b.team_id = o.team_id
		)
		SELECT key, period, lim, spent FROM spend
		WHERE spent >= lim
		ORDER BY spent - lim DESC
		LIMIT 1`, projectID).Scan(&out.TeamKey, &out.Period, &out.LimitUSD, &out.SpentUSD)
	if isNoRows(err) {
		return domain.BudgetState{}, nil
	}
	return out, translate(err)
}
