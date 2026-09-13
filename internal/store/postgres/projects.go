package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Ключ команды-владельца берётся подзапросом, а не джойном: projectCols
// подставляется и в RETURNING, где джойна нет.
const projectCols = `id, key, name, description, settings, owner_team_id,
	(SELECT t.key FROM teams t WHERE t.id = projects.owner_team_id) AS owner_team_key,
	github_installation_id, github_credential_ref,
	created_at, updated_at, archived_at`

func scanProject(row interface{ Scan(...any) error }) (domain.Project, error) {
	var p domain.Project
	err := row.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &p.Settings,
		&p.OwnerTeamID, &p.OwnerTeamKey, &p.GitHubInstallationID, &p.GitHubCredentialRef,
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
	// Команда-владелец: проект без неё не бывает (решение 0033).
	OwnerTeamID uuid.UUID
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

// SetProjectGitHub привязывает проект к установке приложения либо к
// именованному секрету. nil-поля не меняются, пустая строка в ref снимает
// привязку.
func (s *Store) SetProjectGitHub(ctx context.Context, q Querier, projectID uuid.UUID, installationID *int64, credentialRef *string) (domain.Project, error) {
	p, err := scanProject(q.QueryRow(ctx, `
		UPDATE projects SET
			github_installation_id = coalesce($2, github_installation_id),
			github_credential_ref = coalesce(nullif($3, ''), github_credential_ref),
			updated_at = now()
		WHERE id = $1
		RETURNING `+projectCols, projectID, installationID, credentialRef))
	if err != nil {
		return p, notFound(err, "проект не найден")
	}
	return p, nil
}

// ProjectByInstallation — проект, привязанный к установке приложения. Нужно
// маршрутизации вебхуков: доставка несёт installation.id.
func (s *Store) ProjectByInstallation(ctx context.Context, q Querier, installationID int64) (domain.Project, error) {
	p, err := scanProject(q.QueryRow(ctx, `SELECT `+projectCols+`
		FROM projects WHERE github_installation_id = $1`, installationID))
	if err != nil {
		return p, notFound(err, "установка %d не привязана ни к одному проекту", installationID)
	}
	return p, nil
}

// ProjectIDsByRepository — проекты, к которым привязан репозиторий с такими
// координатами. Один репозиторий может стоять в нескольких проектах, поэтому
// множество, а не один идентификатор.
func (s *Store) ProjectIDsByRepository(ctx context.Context, q Querier, owner, name string) (map[uuid.UUID]struct{}, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT project_id FROM repositories
		WHERE lower(owner) = lower($1) AND lower(name) = lower($2)`, owner, name)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := map[uuid.UUID]struct{}{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, translate(err)
		}
		out[id] = struct{}{}
	}
	return out, translate(rows.Err())
}

func (s *Store) ListRepositories(ctx context.Context, q Querier, projectID uuid.UUID) ([]domain.Repository, error) {
	rows, err := q.Query(ctx, `
		SELECT `+repositoryCols+`
		FROM repositories WHERE project_id = $1 ORDER BY owner, name`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Repository{}
	for rows.Next() {
		r, err := scanRepository(rows)
		if err != nil {
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
	r, err := scanRepository(q.QueryRow(ctx, `
		SELECT `+repositoryCols+`
		FROM repositories
		WHERE project_id = $1 AND (owner || '/' || name = $2 OR name = $2 OR role_hint = $2)
		ORDER BY (owner || '/' || name = $2) DESC, (name = $2) DESC
		LIMIT 1`, projectID, ref))
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

// ProjectRemovalCounters — что ушло вместе с проектом. Как и у отвязки
// репозитория, считается до удаления: после него считать нечего, а человеку
// важно видеть цену действия.
type ProjectRemovalCounters struct {
	Tasks        int `json:"tasks"`
	StageRuns    int `json:"stage_runs"`
	Artifacts    int `json:"artifacts"`
	Repositories int `json:"repositories"`
	IndexRuns    int `json:"index_runs"`
	CodeFiles    int `json:"code_files"`
	CodeChunks   int `json:"code_chunks"`
	Documents    int `json:"documents"`
	Members      int `json:"members"`
	Tokens       int `json:"tokens"`
	Roles        int `json:"roles"`
	Workflows    int `json:"workflows"`
	Invites      int `json:"invites"`
	// Расход прогонов проекта: он исчезает вместе с задачами, а бюджет команды
	// считается по живым строкам. Пусть хотя бы останется в событии аудита.
	CostUSD float64 `json:"cost_usd"`
}

// CountProjectContents считает содержимое проекта одним запросом.
func (s *Store) CountProjectContents(ctx context.Context, q Querier, projectID uuid.UUID) (ProjectRemovalCounters, error) {
	var c ProjectRemovalCounters
	err := q.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM tasks              WHERE project_id = $1),
		       (SELECT count(*) FROM stage_runs sr
		          JOIN tasks t ON t.id = sr.task_id     WHERE t.project_id = $1),
		       (SELECT count(*) FROM artifacts          WHERE project_id = $1),
		       (SELECT count(*) FROM repositories       WHERE project_id = $1),
		       (SELECT count(*) FROM repo_index_runs ir
		          JOIN repositories r ON r.id = ir.repository_id
		                                             WHERE r.project_id = $1),
		       (SELECT count(*) FROM code_files cf
		          JOIN repositories r ON r.id = cf.repository_id
		                                             WHERE r.project_id = $1),
		       (SELECT count(*) FROM code_chunks ch
		          JOIN code_files cf ON cf.id = ch.code_file_id
		          JOIN repositories r ON r.id = cf.repository_id
		                                             WHERE r.project_id = $1),
		       (SELECT count(*) FROM project_documents  WHERE project_id = $1),
		       (SELECT count(*) FROM project_members    WHERE project_id = $1),
		       (SELECT count(*) FROM agent_tokens       WHERE project_id = $1),
		       (SELECT count(*) FROM roles              WHERE project_id = $1),
		       (SELECT count(*) FROM workflows          WHERE project_id = $1),
		       (SELECT count(*) FROM invites            WHERE project_id = $1),
		       (SELECT coalesce(sum(u.cost_usd), 0)::float8 FROM stage_run_usage u
		          JOIN stage_runs sr ON sr.id = u.run_id
		          JOIN tasks t ON t.id = sr.task_id     WHERE t.project_id = $1)`, projectID,
	).Scan(&c.Tasks, &c.StageRuns, &c.Artifacts, &c.Repositories, &c.IndexRuns,
		&c.CodeFiles, &c.CodeChunks, &c.Documents, &c.Members, &c.Tokens,
		&c.Roles, &c.Workflows, &c.Invites, &c.CostUSD)
	if err != nil {
		return c, translate(err)
	}
	return c, nil
}

// LockProjectForRemoval берёт строку проекта под FOR UPDATE. Без этого задача
// или репозиторий успеют появиться между подсчётом и удалением: их вставка
// проверяет внешний ключ и на время блокировки ждёт. Заодно два одновременных
// удаления выстраиваются в очередь.
func (s *Store) LockProjectForRemoval(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id = $1 FOR UPDATE`, projectID).Scan(&id)
	if err != nil {
		return notFound(err, "проект не найден")
	}
	return nil
}

// DeleteProject удаляет проект целиком. Каскада схемы для этого недостаточно:
// задачи и прогоны ссылаются на цикл и роли того же проекта связями без
// каскада (tasks.workflow_id, tasks.status_id, stage_runs.role_id,
// role_version_id, status_id), и прямой DELETE FROM projects падает на
// stage_runs_role_id_fkey, как только у проекта был хоть один захваченный
// этап. Поэтому задачи убираются первыми — они уносят каскадом прогоны,
// артефакты, гейты, события и область, — и только потом уходит сам проект,
// после чего роли, версии цикла, статусы, репозитории с индексом, участники,
// токены и приглашения удаляются каскадом без конфликтов.
//
// Документы убираются вторым шагом, тоже явно: project_documents.repository_id
// — ON DELETE SET NULL, а project_documents_source_shape требует у source =
// 'repo_path' непустой repository_id, и обнуление ломает документ. Само по
// себе удаление проекта до этого не доводит — каскад документов и каскад
// репозиториев висят на projects одним уровнем, а обнуление приходит вторым,
// когда документов уже нет, — но это дисциплина очереди после-триггеров, а не
// обещание схемы. Строка ниже делает порядок обещанием кода, как в
// DeleteRepository.
//
// Блобы и кэш эмбеддингов не трогаются: они общие и уходят сборкой мусора.
func (s *Store) DeleteProject(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM tasks WHERE project_id = $1`, projectID); err != nil {
		return translate(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM project_documents WHERE project_id = $1`, projectID); err != nil {
		return translate(err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projectID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("проект уже удалён")
	}
	return nil
}
