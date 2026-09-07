package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

// Ключ команды-владельца берётся подзапросом, а не джойном: projectCols
// подставляется и в RETURNING, где джойна нет.
const projectCols = `id, key, name, description, settings, owner_team_id,
	(SELECT t.key FROM teams t WHERE t.id = projects.owner_team_id) AS owner_team_key,
	created_at, updated_at, archived_at`

func scanProject(row interface{ Scan(...any) error }) (domain.Project, error) {
	var p domain.Project
	err := row.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &p.Settings,
		&p.OwnerTeamID, &p.OwnerTeamKey, &p.CreatedAt, &p.UpdatedAt, &p.ArchivedAt)
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
	// Команда-владелец. Пусто у проекта, заведённого глобальным
	// администратором вне команд.
	OwnerTeamID *uuid.UUID
}

func (s *Store) CreateProject(ctx context.Context, q Querier, in CreateProjectInput) (domain.Project, error) {
	p, err := scanProject(q.QueryRow(ctx, `
		INSERT INTO projects (key, name, description, created_by, owner_team_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+projectCols,
		in.Key, in.Name, in.Description, in.CreatedBy, in.OwnerTeamID))
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
		if isNoRows(err) {
			return r, apperr.New(apperr.CodeUnknownRepository,
				"репозиторий %q не привязан к проекту", ref)
		}
		return r, translate(err)
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

// ListProjectDocuments — материалы проекта вне задач. Агенту они приезжают
// списком в контексте захвата: содержимое он вытянет отдельно, если нужно.
func (s *Store) ListProjectDocuments(ctx context.Context, q Querier, projectID uuid.UUID) ([]domain.ProjectDocument, error) {
	rows, err := q.Query(ctx, `
		SELECT d.id, d.project_id, d.kind, d.title, d.source, d.url,
		       r.owner || '/' || r.name, d.repo_path, d.blob_sha, d.meta
		FROM project_documents d
		LEFT JOIN repositories r ON r.id = d.repository_id
		WHERE d.project_id = $1
		ORDER BY d.kind, d.title`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.ProjectDocument{}
	for rows.Next() {
		var d domain.ProjectDocument
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Kind, &d.Title, &d.Source,
			&d.URL, &d.Repository, &d.RepoPath, &d.BlobSHA, &d.Meta); err != nil {
			return nil, translate(err)
		}
		out = append(out, d)
	}
	return out, translate(rows.Err())
}
