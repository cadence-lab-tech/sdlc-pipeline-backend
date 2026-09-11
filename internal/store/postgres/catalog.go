package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Каталог проекта: репозитории и проектные документы. До фазы 4 они
// заводились только сидом; перенос правил из существующих репозиториев
// (решение 0022) требует писать их через API.

type RepositoryInput struct {
	ProjectID     uuid.UUID
	Provider      string
	Owner         string
	Name          string
	DefaultBranch string
	CloneURL      string
	Visibility    string
	RoleHint      *string
	CredentialRef *string
	WatchBranches []string
	ExcludeGlobs  []string
	IndexEnabled  *bool
	// Установка, которой репозиторий читается (решение 0034). nil при
	// обновлении — оставить прежнюю.
	InstallationID *int64
}

const repositoryCols = `id, project_id, provider, owner, name, default_branch,
	clone_url, visibility, access_mode, role_hint, credential_ref, installation_id, unavailable_at`

func scanRepository(row interface{ Scan(...any) error }) (domain.Repository, error) {
	var r domain.Repository
	err := row.Scan(&r.ID, &r.ProjectID, &r.Provider, &r.Owner, &r.Name, &r.DefaultBranch,
		&r.CloneURL, &r.Visibility, &r.AccessMode, &r.RoleHint, &r.CredentialRef,
		&r.InstallationID, &r.UnavailableAt)
	return r, err
}

// UpsertRepository заводит репозиторий или обновляет настройки уже
// привязанного. Ключ — (project, provider, owner, name): повторный импорт
// того же набора не должен плодить дубли.
func (s *Store) UpsertRepository(ctx context.Context, q Querier, in RepositoryInput) (domain.Repository, bool, error) {
	if in.Provider == "" {
		in.Provider = "github"
	}
	if in.Owner == "" || in.Name == "" {
		return domain.Repository{}, false, apperr.InvalidInput("у репозитория нужны owner и name")
	}
	if in.DefaultBranch == "" {
		in.DefaultBranch = "main"
	}
	if in.CloneURL == "" {
		in.CloneURL = "https://github.com/" + in.Owner + "/" + in.Name + ".git"
	}
	if in.Visibility == "" {
		in.Visibility = "private"
	}
	if in.WatchBranches == nil {
		in.WatchBranches = []string{in.DefaultBranch}
	}
	if in.ExcludeGlobs == nil {
		in.ExcludeGlobs = []string{}
	}

	var r domain.Repository
	var created bool
	// Повторная привязка через установку снимает пометку «недоступен»:
	// человек заново выбрал репозиторий на стороне GitHub.
	err := q.QueryRow(ctx, `
		INSERT INTO repositories (project_id, provider, owner, name, default_branch, clone_url,
		                          visibility, role_hint, credential_ref, watch_branches,
		                          exclude_globs, index_enabled, installation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, coalesce($12, true), $13)
		ON CONFLICT (project_id, provider, owner, name) DO UPDATE SET
		    default_branch  = EXCLUDED.default_branch,
		    clone_url       = EXCLUDED.clone_url,
		    visibility      = EXCLUDED.visibility,
		    role_hint       = coalesce(EXCLUDED.role_hint, repositories.role_hint),
		    credential_ref  = coalesce(EXCLUDED.credential_ref, repositories.credential_ref),
		    watch_branches  = EXCLUDED.watch_branches,
		    exclude_globs   = EXCLUDED.exclude_globs,
		    index_enabled   = coalesce($12, repositories.index_enabled),
		    installation_id = coalesce($13, repositories.installation_id),
		    unavailable_at  = CASE WHEN $13::bigint IS NULL THEN repositories.unavailable_at END
		RETURNING `+repositoryCols+`, (xmax = 0)`,
		in.ProjectID, in.Provider, in.Owner, in.Name, in.DefaultBranch, in.CloneURL,
		in.Visibility, in.RoleHint, in.CredentialRef, in.WatchBranches,
		in.ExcludeGlobs, in.IndexEnabled, in.InstallationID,
	).Scan(&r.ID, &r.ProjectID, &r.Provider, &r.Owner, &r.Name, &r.DefaultBranch,
		&r.CloneURL, &r.Visibility, &r.AccessMode, &r.RoleHint, &r.CredentialRef,
		&r.InstallationID, &r.UnavailableAt, &created)
	if err != nil {
		return r, false, translate(err)
	}
	return r, created, nil
}

// RepositorySettings — то, что не входит в domain.Repository, но нужно
// экспорту: ветки под индексацией, исключения, флаг индексации.
type RepositorySettings struct {
	WatchBranches []string
	ExcludeGlobs  []string
	IndexEnabled  bool
	CredentialRef *string
}

func (s *Store) RepositorySettings(ctx context.Context, q Querier, id uuid.UUID) (RepositorySettings, error) {
	var out RepositorySettings
	err := q.QueryRow(ctx, `
		SELECT watch_branches, exclude_globs, index_enabled, credential_ref
		FROM repositories WHERE id = $1`, id,
	).Scan(&out.WatchBranches, &out.ExcludeGlobs, &out.IndexEnabled, &out.CredentialRef)
	if err != nil {
		return out, notFound(err, "репозиторий не найден")
	}
	return out, nil
}

// RepositoriesByCoordinates — привязки репозитория owner/name, читаемые
// установкой installationID, по всем проектам. Вебхук приходит от
// провайдера и проекта не знает; установка — первый ключ маршрутизации
// (решение 0034): репозиторий без установки событий не получает, и
// одноимённый репозиторий другого арендатора событием не задевается.
func (s *Store) RepositoriesByCoordinates(ctx context.Context, q Querier, provider, owner, name string, installationID int64) ([]IndexTarget, error) {
	rows, err := q.Query(ctx, `
		SELECT `+indexTargetCols+`
		FROM repositories r
		JOIN projects p ON p.id = r.project_id
		LEFT JOIN github_installations gi ON gi.installation_id = r.installation_id
		WHERE r.provider = $1 AND lower(r.owner) = lower($2) AND lower(r.name) = lower($3)
		  AND coalesce(r.installation_id, p.github_installation_id) = $4
		  AND r.index_enabled AND `+indexTargetLive, provider, owner, name, installationID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []IndexTarget{}
	for rows.Next() {
		t, err := scanIndexTarget(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, t)
	}
	return out, translate(rows.Err())
}

// ── документы ───────────────────────────────────────────────────────────────

type DocumentInput struct {
	ProjectID    uuid.UUID
	Kind         string
	Title        string
	Source       string
	URL          *string
	RepositoryID *uuid.UUID
	RepoPath     *string
	BlobSHA      *string
	Meta         json.RawMessage
	CreatedBy    uuid.UUID
}

// UpsertDocument заводит документ или обновляет содержимое существующего.
// Ключ — (проект, kind, title): у документов нет естественного ключа кроме
// названия, а импорт обязан быть повторяемым. Обновление двигает updated_at,
// и воркер переразбивает документ на куски сам.
func (s *Store) UpsertDocument(ctx context.Context, q Querier, in DocumentInput) (domain.ProjectDocument, string, error) {
	if in.Kind == "" || in.Title == "" {
		return domain.ProjectDocument{}, "", apperr.InvalidInput("у документа нужны kind и title")
	}
	if in.Source == "" {
		switch {
		case in.BlobSHA != nil:
			in.Source = "inline"
		case in.URL != nil:
			in.Source = "url"
		case in.RepoPath != nil:
			in.Source = "repo_path"
		}
	}
	if len(in.Meta) == 0 {
		in.Meta = json.RawMessage(`{}`)
	}

	var existing struct {
		id      uuid.UUID
		source  string
		url     *string
		repo    *uuid.UUID
		path    *string
		blobSHA *string
	}
	err := q.QueryRow(ctx, `
		SELECT id, source, url, repository_id, repo_path, blob_sha
		FROM project_documents WHERE project_id = $1 AND kind = $2 AND title = $3`,
		in.ProjectID, in.Kind, in.Title,
	).Scan(&existing.id, &existing.source, &existing.url, &existing.repo,
		&existing.path, &existing.blobSHA)

	switch {
	case isNoRows(err):
		var id uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO project_documents (project_id, kind, title, source, url,
			                               repository_id, repo_path, blob_sha, meta, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
			in.ProjectID, in.Kind, in.Title, in.Source, in.URL,
			in.RepositoryID, in.RepoPath, in.BlobSHA, in.Meta, in.CreatedBy).Scan(&id); err != nil {
			return domain.ProjectDocument{}, "", translate(err)
		}
		doc, err := s.GetDocument(ctx, q, id)
		return doc, "created", err
	case err != nil:
		return domain.ProjectDocument{}, "", translate(err)
	}

	same := existing.source == in.Source &&
		eqStr(existing.url, in.URL) && eqStr(existing.path, in.RepoPath) &&
		eqStr(existing.blobSHA, in.BlobSHA) && eqUUID(existing.repo, in.RepositoryID)
	if same {
		doc, err := s.GetDocument(ctx, q, existing.id)
		return doc, "unchanged", err
	}
	if _, err := q.Exec(ctx, `
		UPDATE project_documents
		SET source = $2, url = $3, repository_id = $4, repo_path = $5, blob_sha = $6,
		    meta = $7, updated_at = now()
		WHERE id = $1`,
		existing.id, in.Source, in.URL, in.RepositoryID, in.RepoPath, in.BlobSHA, in.Meta); err != nil {
		return domain.ProjectDocument{}, "", translate(err)
	}
	doc, err := s.GetDocument(ctx, q, existing.id)
	return doc, "updated", err
}

func (s *Store) GetDocument(ctx context.Context, q Querier, id uuid.UUID) (domain.ProjectDocument, error) {
	var d domain.ProjectDocument
	err := q.QueryRow(ctx, `
		SELECT d.id, d.project_id, d.kind, d.title, d.source, d.url,
		       r.owner || '/' || r.name, d.repo_path, d.blob_sha, d.meta
		FROM project_documents d
		LEFT JOIN repositories r ON r.id = d.repository_id
		WHERE d.id = $1`, id,
	).Scan(&d.ID, &d.ProjectID, &d.Kind, &d.Title, &d.Source,
		&d.URL, &d.Repository, &d.RepoPath, &d.BlobSHA, &d.Meta)
	if err != nil {
		return d, notFound(err, "документ не найден")
	}
	return d, nil
}

func (s *Store) DeleteDocument(ctx context.Context, q Querier, id uuid.UUID) error {
	tag, err := q.Exec(ctx, `DELETE FROM project_documents WHERE id = $1`, id)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("документ не найден")
	}
	return nil
}

func eqStr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
