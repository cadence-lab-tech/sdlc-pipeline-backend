package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

const roleVersionCols = `id, role_id, version, instructions, input_kinds, output_contract,
	       tool_policy, model_hint, repo_scope, context_budget, changelog, created_at`

func scanRoleVersion(row interface{ Scan(...any) error }) (domain.RoleVersion, error) {
	var rv domain.RoleVersion
	err := row.Scan(&rv.ID, &rv.RoleID, &rv.Version, &rv.Instructions, &rv.InputKinds,
		&rv.OutputContract, &rv.ToolPolicy, &rv.ModelHint, &rv.RepoScope,
		&rv.ContextBudget, &rv.Changelog, &rv.CreatedAt)
	return rv, err
}

const roleCols = `r.id, r.project_id, r.key, r.name, r.summary, r.archived_at,
	       (SELECT max(version) FROM role_versions v WHERE v.role_id = r.id)`

func scanRole(row interface{ Scan(...any) error }) (domain.Role, error) {
	var r domain.Role
	err := row.Scan(&r.ID, &r.ProjectID, &r.Key, &r.Name, &r.Summary,
		&r.ArchivedAt, &r.LatestVersion)
	return r, err
}

func (s *Store) ListRoles(ctx context.Context, q Querier, projectID uuid.UUID, includeArchived bool) ([]domain.Role, error) {
	rows, err := q.Query(ctx, `SELECT `+roleCols+`
		FROM roles r
		WHERE r.project_id = $1 AND ($2 OR r.archived_at IS NULL)
		ORDER BY r.key`, projectID, includeArchived)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Role{}
	for rows.Next() {
		role, err := scanRole(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, role)
	}
	return out, translate(rows.Err())
}

func (s *Store) GetRoleByKey(ctx context.Context, q Querier, projectID uuid.UUID, key string) (domain.Role, error) {
	role, err := scanRole(q.QueryRow(ctx, `SELECT `+roleCols+`
		FROM roles r WHERE r.project_id = $1 AND r.key = $2`, projectID, key))
	if err != nil {
		return role, notFound(err, "роль %q не найдена в проекте", key)
	}
	return role, nil
}

type CreateRoleInput struct {
	ProjectID uuid.UUID
	Key       string
	Name      string
	Summary   *string
	CreatedBy uuid.UUID
}

func (s *Store) CreateRole(ctx context.Context, q Querier, in CreateRoleInput) (domain.Role, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO roles (project_id, key, name, summary, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		in.ProjectID, in.Key, in.Name, in.Summary, in.CreatedBy).Scan(&id)
	if err != nil {
		return domain.Role{}, translate(err)
	}
	return s.GetRoleByKey(ctx, q, in.ProjectID, in.Key)
}

// UpdateRole меняет только вывеску роли. Поведение роли живёт в версиях и
// правится добавлением версии, а не редактированием.
func (s *Store) UpdateRole(ctx context.Context, q Querier, roleID uuid.UUID, name *string, summary *string) error {
	_, err := q.Exec(ctx, `
		UPDATE roles SET name = coalesce($2, name), summary = coalesce($3, summary)
		WHERE id = $1`, roleID, name, summary)
	return translate(err)
}

// ArchiveRole убирает роль из выбора, не трогая историю. archived = false
// возвращает её обратно.
func (s *Store) ArchiveRole(ctx context.Context, q Querier, roleID uuid.UUID, archived bool) error {
	_, err := q.Exec(ctx, `
		UPDATE roles SET archived_at = CASE WHEN $2 THEN now() ELSE NULL END
		WHERE id = $1`, roleID, archived)
	return translate(err)
}

// RoleUsage — статусы, на которых роль стоит исполнителем или проверяющим.
// Нужен, чтобы не архивировать роль молча из-под работающего цикла.
type RoleUsage struct {
	WorkflowID    uuid.UUID `json:"workflow_id"`
	WorkflowState string    `json:"workflow_state"`
	StatusKey     string    `json:"status_key"`
	IsPrimary     bool      `json:"is_primary"`
}

func (s *Store) RoleUsage(ctx context.Context, q Querier, roleID uuid.UUID) ([]RoleUsage, error) {
	rows, err := q.Query(ctx, `
		SELECT w.id, w.state, ws.key, sr.is_primary
		FROM status_roles sr
		JOIN workflow_statuses ws ON ws.id = sr.status_id
		JOIN workflows w ON w.id = ws.workflow_id
		WHERE sr.role_id = $1 AND w.state <> 'retired'
		ORDER BY w.state, ws.position`, roleID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []RoleUsage{}
	for rows.Next() {
		var u RoleUsage
		if err := rows.Scan(&u.WorkflowID, &u.WorkflowState, &u.StatusKey, &u.IsPrimary); err != nil {
			return nil, translate(err)
		}
		out = append(out, u)
	}
	return out, translate(rows.Err())
}

func (s *Store) ListRoleVersions(ctx context.Context, q Querier, roleID uuid.UUID) ([]domain.RoleVersion, error) {
	rows, err := q.Query(ctx, `SELECT `+roleVersionCols+`
		FROM role_versions WHERE role_id = $1 ORDER BY version DESC`, roleID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.RoleVersion{}
	for rows.Next() {
		rv, err := scanRoleVersion(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, rv)
	}
	return out, translate(rows.Err())
}

func (s *Store) GetRoleVersionByNumber(ctx context.Context, q Querier, roleID uuid.UUID, version int) (domain.RoleVersion, error) {
	rv, err := scanRoleVersion(q.QueryRow(ctx, `SELECT `+roleVersionCols+`
		FROM role_versions WHERE role_id = $1 AND version = $2`, roleID, version))
	if err != nil {
		return rv, notFound(err, "версия %d роли не найдена", version)
	}
	return rv, nil
}

type CreateRoleVersionInput struct {
	RoleID         uuid.UUID
	Instructions   string
	InputKinds     []string
	OutputContract json.RawMessage
	ToolPolicy     json.RawMessage
	ModelHint      *string
	RepoScope      []string
	ContextBudget  json.RawMessage
	Changelog      *string
	CreatedBy      uuid.UUID
}

// CreateRoleVersion добавляет новую версию инструкции роли.
//
// Версия иммутабельна и номер выдаётся из максимума в этой же транзакции:
// stage_runs пинят конкретную версию при заходе на статус, поэтому правка
// роли не должна менять то, по чему уже работали.
func (s *Store) CreateRoleVersion(ctx context.Context, q Querier, in CreateRoleVersionInput) (domain.RoleVersion, error) {
	if in.InputKinds == nil {
		in.InputKinds = []string{}
	}
	if in.RepoScope == nil {
		in.RepoScope = []string{}
	}
	if len(in.OutputContract) == 0 {
		in.OutputContract = json.RawMessage(`[]`)
	}
	if len(in.ToolPolicy) == 0 {
		in.ToolPolicy = json.RawMessage(`{}`)
	}
	if len(in.ContextBudget) == 0 {
		in.ContextBudget = json.RawMessage(`{}`)
	}

	rv, err := scanRoleVersion(q.QueryRow(ctx, `
		INSERT INTO role_versions (role_id, version, instructions, input_kinds,
		            output_contract, tool_policy, model_hint, repo_scope,
		            context_budget, changelog, created_by)
		SELECT $1, coalesce(max(version), 0) + 1, $2, $3, $4, $5, $6, $7, $8, $9, $10
		FROM role_versions WHERE role_id = $1
		RETURNING `+roleVersionCols,
		in.RoleID, in.Instructions, in.InputKinds, in.OutputContract, in.ToolPolicy,
		in.ModelHint, in.RepoScope, in.ContextBudget, in.Changelog, in.CreatedBy))
	if err != nil {
		return rv, translate(err)
	}
	return rv, nil
}
