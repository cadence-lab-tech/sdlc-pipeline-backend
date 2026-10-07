package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
)

// PendingChunkTarget — артефакт или документ, который ещё не разложен на куски.
//
// Векторизация вынесена из сдачи артефакта в воркер намеренно: агент не должен
// ждать чужой API, чтобы сдать работу, и падение провайдера эмбеддингов не
// должно превращаться в невозможность закрыть этап.
type PendingChunkTarget struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	TaskID    *uuid.UUID
	BlobSHA   string
	Kind      string
	Title     string
}

func (s *Store) PendingArtifacts(ctx context.Context, q Querier, limit int) ([]PendingChunkTarget, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT a.id, a.project_id, a.task_id, a.blob_sha, a.kind, a.title
		FROM artifacts a
		WHERE NOT EXISTS (SELECT 1 FROM artifact_chunks c WHERE c.artifact_id = a.id)
		ORDER BY a.created_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []PendingChunkTarget{}
	for rows.Next() {
		var t PendingChunkTarget
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.TaskID, &t.BlobSHA, &t.Kind, &t.Title); err != nil {
			return nil, translate(err)
		}
		out = append(out, t)
	}
	return out, translate(rows.Err())
}

// PendingDocuments — проектные документы с содержимым внутри системы.
// Документ вида repo_path отдельно не векторизуется: этот файл уже приезжает
// в индекс репозитория, и второй его копией поиск только зашумило бы.
func (s *Store) PendingDocuments(ctx context.Context, q Querier, limit int) ([]PendingChunkTarget, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT d.id, d.project_id, NULL::uuid, d.blob_sha, d.kind, d.title
		FROM project_documents d
		WHERE d.source = 'inline' AND d.blob_sha IS NOT NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM document_chunks c
		      WHERE c.document_id = d.id AND c.created_at >= d.updated_at)
		ORDER BY d.updated_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []PendingChunkTarget{}
	for rows.Next() {
		var t PendingChunkTarget
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.TaskID, &t.BlobSHA, &t.Kind, &t.Title); err != nil {
			return nil, translate(err)
		}
		out = append(out, t)
	}
	return out, translate(rows.Err())
}

// TextChunkInput — кусок текстового источника (артефакта или документа).
type TextChunkInput struct {
	Ord        int
	Text       string
	TextSHA    string
	Tokens     int
	Embedding  []float32
	EmbedModel string
}

// ReplaceArtifactChunks перекладывает артефакт заново.
//
// Артефакт иммутабелен, поэтому в норме это разовая операция; удаление перед
// вставкой нужно на случай повторного прохода воркера после сбоя на середине.
func (s *Store) ReplaceArtifactChunks(ctx context.Context, q Querier, artifactID, projectID, taskID uuid.UUID, chunks []TextChunkInput) error {
	if _, err := q.Exec(ctx, `DELETE FROM artifact_chunks WHERE artifact_id = $1`, artifactID); err != nil {
		return translate(err)
	}
	if len(chunks) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, c := range chunks {
		batch.Queue(`
			INSERT INTO artifact_chunks
			    (artifact_id, project_id, task_id, ord, text, token_count, text_sha, embedding, embedding_model)
			VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, '')::vector, nullif($9, ''))`,
			artifactID, projectID, taskID, c.Ord, c.Text, c.Tokens, c.TextSHA,
			vectorLiteral(c.Embedding), c.EmbedModel)
	}
	return sendBatch(ctx, q, batch)
}

func (s *Store) ReplaceDocumentChunks(ctx context.Context, q Querier, documentID, projectID uuid.UUID, chunks []TextChunkInput) error {
	if _, err := q.Exec(ctx, `DELETE FROM document_chunks WHERE document_id = $1`, documentID); err != nil {
		return translate(err)
	}
	if len(chunks) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, c := range chunks {
		batch.Queue(`
			INSERT INTO document_chunks
			    (document_id, project_id, ord, text, token_count, text_sha, embedding, embedding_model)
			VALUES ($1, $2, $3, $4, $5, $6, nullif($7, '')::vector, nullif($8, ''))`,
			documentID, projectID, c.Ord, c.Text, c.Tokens, c.TextSHA,
			vectorLiteral(c.Embedding), c.EmbedModel)
	}
	return sendBatch(ctx, q, batch)
}

// MissingEmbedding — кусок, записанный без вектора.
//
// Так бывает штатно: провайдер эмбеддингов может быть выключен или недоступен,
// и прогон в этом случае не валится — индекс без векторов всё ещё работает на
// полнотекстовом поиске и полностью питает детектор дрейфа. Досчитывает
// векторы отдельный проход, а не переиндексация.
type MissingEmbedding struct {
	Source  string
	ID      uuid.UUID
	Text    string
	TextSHA string
}

// latestRuns — последний завершённый прогон каждой ветки, без фильтра по
// проекту: досчёт векторов идёт по всему инстансу.
const latestRuns = `
	WITH runs AS (
	    SELECT DISTINCT ON (repository_id, branch) id
	    FROM repo_index_runs WHERE state = 'completed'
	    ORDER BY repository_id, branch, finished_at DESC
	)`

// ChunksMissingEmbeddings отдаёт куски без вектора текущей модели: записанные
// без вектора или посчитанные другой моделью. Куски кода берутся только из
// актуальных прогонов: платить за векторизацию истории коммитов незачем, поиск
// по ней всё равно не ходит.
//
// Store о провайдерах не знает: model — это Model() эмбеддера вызывающего.
func (s *Store) ChunksMissingEmbeddings(ctx context.Context, q Querier, model string, limit int) ([]MissingEmbedding, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, latestRuns+`
		(SELECT 'code', cc.id, cc.text, coalesce(cc.text_sha, '')
		   FROM code_chunks cc
		   JOIN code_files cf ON cf.id = cc.code_file_id
		   JOIN runs ON runs.id = cf.index_run_id
		  WHERE cc.embedding IS NULL OR cc.embedding_model IS DISTINCT FROM $2 LIMIT $1)
		UNION ALL
		(SELECT 'artifact', ac.id, ac.text, coalesce(ac.text_sha, '')
		   FROM artifact_chunks ac
		  WHERE ac.embedding IS NULL OR ac.embedding_model IS DISTINCT FROM $2 LIMIT $1)
		UNION ALL
		(SELECT 'document', dc.id, dc.text, coalesce(dc.text_sha, '')
		   FROM document_chunks dc
		  WHERE dc.embedding IS NULL OR dc.embedding_model IS DISTINCT FROM $2 LIMIT $1)`, limit, model)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []MissingEmbedding{}
	for rows.Next() {
		var m MissingEmbedding
		if err := rows.Scan(&m.Source, &m.ID, &m.Text, &m.TextSHA); err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

// ClearStaleRunEmbeddings снимает векторы других моделей с кусков кода
// неактуальных прогонов — тех, что не последние завершённые в своей ветке.
//
// Пересчитывать их незачем: поиск ходит только по актуальному индексу, а
// прогоны истории держатся ради базовых линий и детектора дрейфа, которому
// векторы не нужны. Одним UPDATE, без вызова модели. Без этого строки прежней
// модели оставались бы в базе до вычистки прогона.
func (s *Store) ClearStaleRunEmbeddings(ctx context.Context, q Querier, model string) (int, error) {
	tag, err := q.Exec(ctx, latestRuns+`
		UPDATE code_chunks cc
		   SET embedding = NULL, embedding_model = NULL
		 WHERE cc.embedding IS NOT NULL
		   AND cc.embedding_model IS DISTINCT FROM $1
		   AND NOT EXISTS (
		       SELECT 1 FROM code_files cf JOIN runs ON runs.id = cf.index_run_id
		        WHERE cf.id = cc.code_file_id)`, model)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) SetChunkEmbedding(ctx context.Context, q Querier, source string, id uuid.UUID, vec []float32, model string) error {
	table := map[string]string{
		"code":     "code_chunks",
		"artifact": "artifact_chunks",
		"document": "document_chunks",
	}[source]
	if table == "" {
		return apperr.Internal(nil, "неизвестный источник куска %q", source)
	}
	_, err := q.Exec(ctx, `UPDATE `+table+`
		SET embedding = nullif($2, '')::vector, embedding_model = $3,
		    text_sha = coalesce(text_sha, encode(sha256(text::bytea), 'hex'))
		WHERE id = $1`, id, vectorLiteral(vec), model)
	return translate(err)
}
