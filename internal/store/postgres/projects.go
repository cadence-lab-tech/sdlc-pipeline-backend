package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

const projectCols = `id, key, name, description, settings, created_at, updated_at, archived_at`

func scanProject(row interface{ Scan(...any) error }) (domain.Project, error) {
	var p domain.Project
	err := row.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &p.Settings,
		&p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt)
	return p, err
}

func (s *Store) ListProjects(ctx context.Context, q Querier) ([]domain.Project, error) {
	rows, err := q.Query(ctx, `SELECT `+projectCols+`
		FROM projects WHERE archived_at IS NULL ORDER BY key`)
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

func (s *Store) GetProjectByKey(ctx context.Context, q Querier, key string) (domain.Project, error) {
	p, err := scanProject(q.QueryRow(ctx,
		`SELECT `+projectCols+` FROM projects WHERE key = $1`, key))
	if err != nil {
		return p, notFound(err, "проект %q не найден", key)
	}
	return p, nil
}

type CreateProjectInput struct {
	Key         string
	Name        string
	Description *string
	CreatedBy   uuid.UUID
}

func (s *Store) CreateProject(ctx context.Context, q Querier, in CreateProjectInput) (domain.Project, error) {
	p, err := scanProject(q.QueryRow(ctx, `
		INSERT INTO projects (key, name, description, created_by)
		VALUES ($1, $2, $3, $4)
		RETURNING `+projectCols,
		in.Key, in.Name, in.Description, in.CreatedBy))
	if err != nil {
		return p, translate(err)
	}
	return p, nil
}

func (s *Store) ListRepositories(ctx context.Context, q Querier, projectID uuid.UUID) ([]domain.Repository, error) {
	rows, err := q.Query(ctx, `
		SELECT id, project_id, provider, owner, name, default_branch,
		       clone_url, visibility, access_mode, role_hint
		FROM repositories WHERE project_id = $1 ORDER BY owner, name`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Repository{}
	for rows.Next() {
		var r domain.Repository
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Provider, &r.Owner, &r.Name,
			&r.DefaultBranch, &r.CloneURL, &r.Visibility, &r.AccessMode, &r.RoleHint); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// FindRepository ищет репозиторий проекта по owner/name или по role_hint.
// Второй вариант нужен агенту: в задаче он оперирует ролью репозитория
// ("backend"), а не координатами на GitHub.
func (s *Store) FindRepository(ctx context.Context, q Querier, projectID uuid.UUID, ref string) (domain.Repository, error) {
	var r domain.Repository
	err := q.QueryRow(ctx, `
		SELECT id, project_id, provider, owner, name, default_branch,
		       clone_url, visibility, access_mode, role_hint
		FROM repositories
		WHERE project_id = $1 AND (owner || '/' || name = $2 OR name = $2 OR role_hint = $2)
		ORDER BY (owner || '/' || name = $2) DESC, (name = $2) DESC
		LIMIT 1`, projectID, ref,
	).Scan(&r.ID, &r.ProjectID, &r.Provider, &r.Owner, &r.Name,
		&r.DefaultBranch, &r.CloneURL, &r.Visibility, &r.AccessMode, &r.RoleHint)
	if err != nil {
		return r, notFound(err, "репозиторий %q не найден в проекте", ref)
	}
	return r, nil
}

func (s *Store) ListTaskPropertyDefs(ctx context.Context, q Querier, projectID uuid.UUID) ([]domain.TaskPropertyDef, error) {
	rows, err := q.Query(ctx, `
		SELECT id, key, name, description, value_type, default_value,
		       enum_values, is_system, expose_to_agent, position
		FROM task_property_defs WHERE project_id = $1 ORDER BY position, key`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.TaskPropertyDef{}
	for rows.Next() {
		var d domain.TaskPropertyDef
		if err := rows.Scan(&d.ID, &d.Key, &d.Name, &d.Description, &d.ValueType,
			&d.DefaultValue, &d.EnumValues, &d.IsSystem, &d.ExposeToAgent, &d.Position); err != nil {
			return nil, translate(err)
		}
		out = append(out, d)
	}
	return out, translate(rows.Err())
}
