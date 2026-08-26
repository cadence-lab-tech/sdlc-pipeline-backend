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

// InlineMaxBytes — предел для содержимого, лежащего прямо в blobs.content.
// Вынос крупного в объектное хранилище запланирован фазой 4; пока превышение
// честно отбивается, а не молча кладёт мегабайты в строку таблицы.
const InlineMaxBytes = 4 << 20

// PutBlob кладёт содержимое в контент-адресуемое хранилище и возвращает sha256.
// Повторная запись того же содержимого не создаёт дубликата — на этом держится
// иммутабельность артефактов: новая версия ссылается на новый blob, а
// одинаковые вложения разных задач физически хранятся один раз.
func (s *Store) PutBlob(ctx context.Context, q Querier, content []byte, mediaType string) (string, error) {
	if len(content) == 0 {
		return "", apperr.InvalidInput("содержимое артефакта пустое")
	}
	if len(content) > InlineMaxBytes {
		return "", apperr.InvalidInput(
			"содержимое %d байт больше предела %d: вынос в объектное хранилище появится в фазе 4",
			len(content), InlineMaxBytes)
	}
	if mediaType == "" {
		mediaType = "text/markdown"
	}

	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	if _, err := q.Exec(ctx, `
		INSERT INTO blobs (sha256, size_bytes, media_type, storage, content)
		VALUES ($1, $2, $3, 'inline', $4)
		ON CONFLICT (sha256) DO NOTHING`,
		sha, len(content), mediaType, content); err != nil {
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
	if storage != "inline" {
		return nil, "", apperr.Internal(nil, "содержимое вынесено во внешнее хранилище, чтение появится в фазе 4")
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
