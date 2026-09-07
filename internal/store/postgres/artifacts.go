package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

// InlineMaxBytes — предел содержимого, которое лежит прямо в blobs.content,
// когда внешнее хранилище не настроено. С хранилищем порог задаётся
// настройкой (SDLC_BLOB_INLINE_MAX_BYTES), а этот — верхняя граница того,
// что вообще принимается без выноса.
const InlineMaxBytes = 4 << 20

// BlobStore — внешнее хранилище крупного содержимого. Объявлен здесь, а не
// импортирован из blobstore, чтобы store не зависел от реализации.
type BlobStore interface {
	Put(ctx context.Context, sha string, content []byte, mediaType string) (string, error)
	Get(ctx context.Context, uri string) ([]byte, error)
}

// UseBlobStore включает вынос: содержимое больше inlineMax уходит во внешнее
// хранилище, в базе остаётся адрес. maxBytes — предел принимаемого вовсе.
func (s *Store) UseBlobStore(bs BlobStore, inlineMax, maxBytes int) {
	s.blobs = bs
	s.inlineMax = inlineMax
	s.blobMax = maxBytes
}

func (s *Store) inlineLimit() int {
	if s.blobs == nil || s.inlineMax <= 0 {
		return InlineMaxBytes
	}
	return s.inlineMax
}

// PutBlob кладёт содержимое в контент-адресуемое хранилище и возвращает sha256.
//
// Дедупликация бесплатна: одинаковое содержимое даёт одинаковый sha, и второй
// INSERT ничего не делает. Крупное уходит во внешнее хранилище до записи
// строки — иначе строка с адресом появилась бы раньше содержимого.
func (s *Store) PutBlob(ctx context.Context, q Querier, content []byte, mediaType string) (string, error) {
	if len(content) == 0 {
		return "", apperr.InvalidInput("содержимое артефакта пустое")
	}
	if mediaType == "" {
		mediaType = "text/markdown"
	}

	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	if len(content) <= s.inlineLimit() {
		if _, err := q.Exec(ctx, `
			INSERT INTO blobs (sha256, size_bytes, media_type, storage, content)
			VALUES ($1, $2, $3, 'inline', $4)
			ON CONFLICT (sha256) DO NOTHING`,
			sha, len(content), mediaType, content); err != nil {
			return "", translate(err)
		}
		return sha, nil
	}

	if s.blobs == nil {
		return "", apperr.InvalidInput(
			"содержимое %d байт больше предела %d, а внешнее хранилище не настроено (SDLC_BLOB_DIR)",
			len(content), InlineMaxBytes)
	}
	if s.blobMax > 0 && len(content) > s.blobMax {
		return "", apperr.InvalidInput("содержимое %d байт больше предела %d", len(content), s.blobMax)
	}
	uri, err := s.blobs.Put(ctx, sha, content, mediaType)
	if err != nil {
		return "", apperr.Internal(err, "запись во внешнее хранилище")
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO blobs (sha256, size_bytes, media_type, storage, external_uri)
		VALUES ($1, $2, $3, 'external', $4)
		ON CONFLICT (sha256) DO NOTHING`,
		sha, len(content), mediaType, uri); err != nil {
		return "", translate(err)
	}
	return sha, nil
}

func (s *Store) BlobContent(ctx context.Context, q Querier, sha string) ([]byte, string, error) {
	var content []byte
	var mediaType, storage string
	var externalURI *string
	err := q.QueryRow(ctx, `
		SELECT content, media_type, storage, external_uri FROM blobs WHERE sha256 = $1`, sha,
	).Scan(&content, &mediaType, &storage, &externalURI)
	if err != nil {
		return nil, "", notFound(err, "содержимое %s не найдено", sha)
	}
	if storage == "inline" {
		return content, mediaType, nil
	}
	if s.blobs == nil || externalURI == nil {
		return nil, "", apperr.Internal(nil,
			"содержимое %s вынесено во внешнее хранилище, а оно не настроено", sha)
	}
	content, err = s.blobs.Get(ctx, *externalURI)
	if err != nil {
		return nil, "", apperr.Internal(err, "чтение %s из внешнего хранилища", sha)
	}
	return content, mediaType, nil
}

const artifactSelect = `
	SELECT a.id, a.task_id, a.project_id, a.stage_run_id, a.kind, a.title, a.format,
	       a.blob_sha, b.size_bytes, a.version, a.supersedes_id, a.status,
	       a.produced_by, a.approved_by, a.meta, a.created_at
	FROM artifacts a
	JOIN blobs b ON b.sha256 = a.blob_sha`

func scanArtifact(row interface{ Scan(...any) error }) (domain.Artifact, error) {
	var a domain.Artifact
	err := row.Scan(&a.ID, &a.TaskID, &a.ProjectID, &a.StageRunID, &a.Kind, &a.Title,
		&a.Format, &a.BlobSHA, &a.SizeBytes, &a.Version, &a.SupersedesID, &a.Status,
		&a.ProducedBy, &a.ApprovedBy, &a.Meta, &a.CreatedAt)
	return a, err
}

// ArtifactLink — связь артефакта с другим артефактом, документом проекта
// или местом в коде.
type ArtifactLink struct {
	Rel              string     `json:"rel"`
	TargetArtifactID *uuid.UUID `json:"target_artifact_id,omitempty"`
	TargetDocumentID *uuid.UUID `json:"target_document_id,omitempty"`
	TargetURI        string     `json:"target_uri,omitempty"`
	Note             string     `json:"note,omitempty"`
}

// ArtifactCodeRef — объявленная зона задачи в коде.
//
// Дублирует artifact_links.target_uri намеренно: пересечение зоны задачи с
// изменениями в main должно быть индексируемым джойном, а не разбором строк
// URI на каждом прогоне детектора дрейфа.
type ArtifactCodeRef struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	SymbolFQN  string `json:"symbol_fqn,omitempty"`
	StartLine  *int   `json:"start_line,omitempty"`
	EndLine    *int   `json:"end_line,omitempty"`
	CommitSHA  string `json:"commit,omitempty"`
	RefKind    string `json:"ref_kind"`
}

type CreateArtifactInput struct {
	Task         domain.Task
	StageRunID   *uuid.UUID
	Kind         string
	Title        string
	Format       string
	Content      []byte
	MediaType    string
	SupersedesID *uuid.UUID
	Status       string
	Meta         json.RawMessage
	Links        []ArtifactLink
	CodeRefs     []ArtifactCodeRef
	ProducedBy   uuid.UUID
}

// CreateArtifact создаёт новую запись, а не правит существующую.
//
// Артефакты иммутабельны (решение 0004): правка — это новая версия, связанная
// с прежней через supersedes_id, а прежняя переводится в superseded. Так
// история этапа остаётся воспроизводимой.
func (s *Store) CreateArtifact(ctx context.Context, tx pgx.Tx, in CreateArtifactInput) (domain.Artifact, error) {
	sha, err := s.PutBlob(ctx, tx, in.Content, in.MediaType)
	if err != nil {
		return domain.Artifact{}, err
	}

	version := 1
	if in.SupersedesID != nil {
		var prevTask uuid.UUID
		var prevVersion int
		if err := tx.QueryRow(ctx,
			`SELECT task_id, version FROM artifacts WHERE id = $1 FOR UPDATE`,
			*in.SupersedesID).Scan(&prevTask, &prevVersion); err != nil {
			return domain.Artifact{}, notFound(err, "артефакт, который заменяется, не найден")
		}
		if prevTask != in.Task.ID {
			return domain.Artifact{}, apperr.InvalidInput(
				"нельзя заменить артефакт из другой задачи")
		}
		version = prevVersion + 1
		if _, err := tx.Exec(ctx,
			`UPDATE artifacts SET status = 'superseded' WHERE id = $1`, *in.SupersedesID); err != nil {
			return domain.Artifact{}, translate(err)
		}
	}

	format := in.Format
	if format == "" {
		format = "markdown"
	}
	status := in.Status
	if status == "" {
		status = domain.ArtifactSubmitted
	}
	meta := in.Meta
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}

	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO artifacts (task_id, project_id, stage_run_id, kind, title, format,
		                       blob_sha, version, supersedes_id, status, produced_by, meta)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id`,
		in.Task.ID, in.Task.ProjectID, in.StageRunID, in.Kind, in.Title, format,
		sha, version, in.SupersedesID, status, in.ProducedBy, meta,
	).Scan(&id); err != nil {
		return domain.Artifact{}, translate(err)
	}

	for _, l := range in.Links {
		if _, err := tx.Exec(ctx, `
			INSERT INTO artifact_links (artifact_id, rel, target_artifact_id,
			                            target_document_id, target_uri, note)
			VALUES ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''))`,
			id, l.Rel, l.TargetArtifactID, l.TargetDocumentID, l.TargetURI, l.Note); err != nil {
			return domain.Artifact{}, translate(err)
		}
	}

	for _, r := range in.CodeRefs {
		repo, err := s.FindRepository(ctx, tx, in.Task.ProjectID, r.Repository)
		if err != nil {
			return domain.Artifact{}, err
		}
		refKind := r.RefKind
		if refKind == "" {
			refKind = "reads"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO artifact_code_refs (artifact_id, task_id, repository_id, path,
			                                symbol_fqn, start_line, end_line, commit_sha, ref_kind)
			VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7, nullif($8, ''), $9)`,
			id, in.Task.ID, repo.ID, r.Path, r.SymbolFQN, r.StartLine, r.EndLine,
			r.CommitSHA, refKind); err != nil {
			return domain.Artifact{}, translate(err)
		}
	}

	a, err := scanArtifact(tx.QueryRow(ctx, artifactSelect+` WHERE a.id = $1`, id))
	if err != nil {
		return a, translate(err)
	}
	return a, nil
}

// ListArtifacts отдаёт артефакты задачи. includeSuperseded=false оставляет
// только актуальные версии — именно их видит агент и проверяют гейты.
func (s *Store) ListArtifacts(ctx context.Context, q Querier, taskID uuid.UUID, includeSuperseded bool) ([]domain.Artifact, error) {
	rows, err := q.Query(ctx, artifactSelect+`
		WHERE a.task_id = $1 AND ($2 OR a.status <> 'superseded')
		ORDER BY a.created_at`, taskID, includeSuperseded)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Artifact{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, a)
	}
	return out, translate(rows.Err())
}

func (s *Store) GetArtifact(ctx context.Context, q Querier, id uuid.UUID) (domain.Artifact, error) {
	a, err := scanArtifact(q.QueryRow(ctx, artifactSelect+` WHERE a.id = $1`, id))
	if err != nil {
		return a, notFound(err, "артефакт не найден")
	}
	return a, nil
}

func (s *Store) ApproveArtifact(ctx context.Context, tx pgx.Tx, id, actor uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE artifacts SET status = 'approved', approved_by = $2, approved_at = now()
		WHERE id = $1 AND status IN ('draft', 'submitted')`, id, actor)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("артефакт нельзя заапрувить в его текущем состоянии")
	}
	return nil
}

// LatestArtifactOfKind — актуальная версия артефакта этого kind в задаче.
// Возвращает nil, если такого ещё не сдавали.
func (s *Store) LatestArtifactOfKind(ctx context.Context, q Querier, taskID uuid.UUID, kind string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT id FROM artifacts
		WHERE task_id = $1 AND kind = $2 AND status <> 'superseded'
		ORDER BY version DESC, created_at DESC
		LIMIT 1`, taskID, kind).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, translate(err)
	}
	return &id, nil
}
