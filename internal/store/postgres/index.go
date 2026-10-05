package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
)

// IndexTarget — репозиторий, за ветками которого следит индексатор.
type IndexTarget struct {
	RepositoryID  uuid.UUID
	ProjectID     uuid.UUID
	ProjectKey    string
	Owner         string
	Name          string
	Branches      []string
	ExcludeGlobs  []string
	CredentialRef *string
	// Установка, которой читается репозиторий: своя, а без неё — проекта
	// (решение 0034); и имя секрета проекта.
	InstallationID       *int64
	ProjectCredentialRef *string
}

func (t IndexTarget) Repository() string { return t.Owner + "/" + t.Name }

// indexTargetCols — столбцы цели с алиасами r (repositories) и p (projects).
const indexTargetCols = `r.id, r.project_id, p.key, r.owner, r.name,
	       r.watch_branches, r.exclude_globs, r.credential_ref,
	       coalesce(r.installation_id, p.github_installation_id), p.github_credential_ref`

// indexTargetLive — репозиторий читаем: не убран из установки, а его
// установка (алиас gi, LEFT JOIN github_installations) не снята и не
// приостановлена. Снятая установка индексацию останавливает, но собранный
// индекс не трогает.
const indexTargetLive = `r.unavailable_at IS NULL
	  AND (gi.id IS NULL OR (gi.suspended_at IS NULL AND gi.removed_at IS NULL))`

func scanIndexTarget(row interface{ Scan(...any) error }) (IndexTarget, error) {
	var t IndexTarget
	err := row.Scan(&t.RepositoryID, &t.ProjectID, &t.ProjectKey,
		&t.Owner, &t.Name, &t.Branches, &t.ExcludeGlobs, &t.CredentialRef,
		&t.InstallationID, &t.ProjectCredentialRef)
	return t, err
}

// IndexRun — прогон индексации репозитория на конкретном коммите.
type IndexRun struct {
	ID             uuid.UUID  `json:"id"`
	RepositoryID   uuid.UUID  `json:"repository_id"`
	ProjectID      uuid.UUID  `json:"-"`
	Repository     string     `json:"repository"`
	Owner          string     `json:"-"`
	Name           string     `json:"-"`
	Branch         string     `json:"branch"`
	CommitSHA      string     `json:"commit"`
	State          string     `json:"state"`
	Kind           string     `json:"kind"`
	Trigger        string     `json:"trigger"`
	Attempt        int        `json:"attempt"`
	ParentID       *uuid.UUID `json:"parent_index_run_id,omitempty"`
	FilesIndexed   int        `json:"files_indexed"`
	FilesReused    int        `json:"files_reused"`
	ChunksIndexed  int        `json:"chunks_indexed"`
	ChunksReused   int        `json:"chunks_reused"`
	ChunksEmbedded int        `json:"chunks_embedded"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Error          *string    `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ExcludeGlobs   []string   `json:"-"`
	CredentialRef  *string    `json:"-"`
	// Учётные данные проекта: установка приложения и имя секрета (решение
	// 0028). Секрет репозитория, если он задан, частнее и побеждает.
	InstallationID       *int64  `json:"-"`
	ProjectCredentialRef *string `json:"-"`
}

const indexRunSelect = `
	SELECT ir.id, ir.repository_id, r.project_id, r.owner || '/' || r.name,
	       r.owner, r.name, ir.branch, ir.commit_sha, ir.state, ir.kind,
	       ir.trigger, ir.attempt, ir.parent_index_run_id,
	       ir.files_indexed, ir.files_reused, ir.chunks_indexed,
	       ir.chunks_reused, ir.chunks_embedded,
	       ir.started_at, ir.finished_at, ir.error, ir.created_at,
	       r.exclude_globs, r.credential_ref,
	       coalesce(r.installation_id, p.github_installation_id), p.github_credential_ref
	FROM repo_index_runs ir
	JOIN repositories r ON r.id = ir.repository_id
	JOIN projects p ON p.id = r.project_id`

func scanIndexRun(row interface{ Scan(...any) error }) (IndexRun, error) {
	var r IndexRun
	err := row.Scan(&r.ID, &r.RepositoryID, &r.ProjectID, &r.Repository,
		&r.Owner, &r.Name, &r.Branch, &r.CommitSHA, &r.State, &r.Kind,
		&r.Trigger, &r.Attempt, &r.ParentID,
		&r.FilesIndexed, &r.FilesReused, &r.ChunksIndexed,
		&r.ChunksReused, &r.ChunksEmbedded,
		&r.StartedAt, &r.FinishedAt, &r.Error, &r.CreatedAt,
		&r.ExcludeGlobs, &r.CredentialRef, &r.InstallationID, &r.ProjectCredentialRef)
	return r, err
}

// IndexTargets — репозитории, которые индексируются, с их отслеживаемыми
// ветками. Репозиторий может остаться в области задач, но быть выключен из
// индексации: зеркало или архив.
func (s *Store) IndexTargets(ctx context.Context, q Querier) ([]IndexTarget, error) {
	rows, err := q.Query(ctx, `
		SELECT `+indexTargetCols+`
		FROM repositories r
		JOIN projects p ON p.id = r.project_id
		LEFT JOIN github_installations gi ON gi.installation_id = r.installation_id
		WHERE r.index_enabled AND `+indexTargetLive+`
		ORDER BY p.key, r.owner, r.name`)
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

// LatestCompletedRun — актуальный индекс ветки: последний завершённый прогон.
// От него наследуется всё неизменившееся и от него же считается дельта.
func (s *Store) LatestCompletedRun(ctx context.Context, q Querier, repoID uuid.UUID, branch string) (IndexRun, bool, error) {
	row := q.QueryRow(ctx, indexRunSelect+`
		WHERE ir.repository_id = $1 AND ir.branch = $2 AND ir.state = 'completed'
		ORDER BY ir.finished_at DESC LIMIT 1`, repoID, branch)
	run, err := scanIndexRun(row)
	if isNoRows(err) {
		return IndexRun{}, false, nil
	}
	if err != nil {
		return IndexRun{}, false, translate(err)
	}
	return run, true, nil
}

func (s *Store) GetIndexRun(ctx context.Context, q Querier, id uuid.UUID) (IndexRun, error) {
	run, err := scanIndexRun(q.QueryRow(ctx, indexRunSelect+` WHERE ir.id = $1`, id))
	if err != nil {
		return IndexRun{}, notFound(err, "прогон индексации не найден")
	}
	return run, nil
}

func (s *Store) ListIndexRuns(ctx context.Context, q Querier, repoID uuid.UUID, limit int) ([]IndexRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx, indexRunSelect+`
		WHERE ir.repository_id = $1 ORDER BY ir.created_at DESC LIMIT $2`, repoID, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []IndexRun{}
	for rows.Next() {
		run, err := scanIndexRun(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, run)
	}
	return out, translate(rows.Err())
}

type EnqueueIndexRunInput struct {
	RepositoryID uuid.UUID
	Branch       string
	CommitSHA    string
	Trigger      string
	Kind         string
	ParentID     *uuid.UUID
}

// EnqueueIndexRun ставит прогон в очередь.
//
// Повторная постановка того же коммита не заводит второй прогон: индекс
// коммита неизменяем, и переиндексировать его нечем — состояние репозитория
// на этом коммите уже не изменится. Уже завершённый прогон возвращается как
// есть, провалившийся — переоткрывается на новую попытку.
func (s *Store) EnqueueIndexRun(ctx context.Context, q Querier, in EnqueueIndexRunInput) (IndexRun, bool, error) {
	if in.Trigger == "" {
		in.Trigger = "schedule"
	}
	if in.Kind == "" {
		in.Kind = "full"
	}

	var id uuid.UUID
	var created bool
	err := q.QueryRow(ctx, `
		INSERT INTO repo_index_runs
		    (repository_id, branch, commit_sha, state, kind, trigger, parent_index_run_id)
		VALUES ($1, $2, $3, 'queued', $4, $5, $6)
		ON CONFLICT (repository_id, commit_sha) DO UPDATE
		SET state = CASE WHEN repo_index_runs.state = 'failed'
		                 THEN 'queued' ELSE repo_index_runs.state END,
		    trigger = CASE WHEN repo_index_runs.state = 'failed'
		                   THEN excluded.trigger ELSE repo_index_runs.trigger END
		RETURNING id, (xmax = 0)`,
		in.RepositoryID, in.Branch, in.CommitSHA, in.Kind, in.Trigger, in.ParentID,
	).Scan(&id, &created)
	if err != nil {
		return IndexRun{}, false, translate(err)
	}

	run, err := s.GetIndexRun(ctx, q, id)
	return run, created, err
}

// ClaimIndexRun забирает следующий прогон из очереди.
//
// SKIP LOCKED, потому что воркеров может стать несколько, а прогон длинный:
// он ходит в сеть, и держать на нём транзакцию всё это время нельзя. Отсюда
// же состояние running в самой строке, а не блокировка.
func (s *Store) ClaimIndexRun(ctx context.Context) (IndexRun, bool, error) {
	var run IndexRun
	var found bool
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			UPDATE repo_index_runs SET
			    state = 'running',
			    started_at = now(),
			    attempt = attempt + 1,
			    error = NULL
			WHERE id = (
			    SELECT ir.id FROM repo_index_runs ir
			    JOIN repositories r ON r.id = ir.repository_id
			    LEFT JOIN github_installations gi ON gi.installation_id = r.installation_id
			    WHERE ir.state = 'queued' AND ir.next_attempt_at <= now()
			      AND `+indexTargetLive+`
			    ORDER BY ir.created_at
			    FOR UPDATE OF ir SKIP LOCKED
			    LIMIT 1)
			RETURNING id`).Scan(&id)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return translate(err)
		}
		run, err = s.GetIndexRun(ctx, tx, id)
		found = err == nil
		return err
	})
	return run, found, err
}

// RequeueStuckRuns возвращает в очередь прогоны, застрявшие в running:
// воркера убили посреди выкачки, и без этого прогон висел бы вечно, а ветка
// осталась бы без индекса.
func (s *Store) RequeueStuckRuns(ctx context.Context, q Querier, olderThan time.Duration) (int, error) {
	tag, err := q.Exec(ctx, `
		UPDATE repo_index_runs
		SET state = 'queued', error = 'прогон прерван: воркер не завершил его вовремя'
		WHERE state = 'running' AND started_at < now() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

// IndexCounters — во что обошёлся прогон. Отношение reused к indexed — это и
// есть цена инкрементальности: на репозитории, где меняются десятки файлов из
// тысяч, полная переиндексация стоила бы в сто раз дороже.
type IndexCounters struct {
	FilesIndexed   int
	FilesReused    int
	ChunksIndexed  int
	ChunksReused   int
	ChunksEmbedded int
	Kind           string
	ParentID       *uuid.UUID
}

func (s *Store) FinishIndexRun(ctx context.Context, q Querier, id uuid.UUID, c IndexCounters) error {
	_, err := q.Exec(ctx, `
		UPDATE repo_index_runs SET
		    state = 'completed', finished_at = now(), error = NULL,
		    kind = $2, parent_index_run_id = $3,
		    files_indexed = $4, files_reused = $5,
		    chunks_indexed = $6, chunks_reused = $7, chunks_embedded = $8
		WHERE id = $1`,
		id, c.Kind, c.ParentID, c.FilesIndexed, c.FilesReused,
		c.ChunksIndexed, c.ChunksReused, c.ChunksEmbedded)
	if err != nil {
		return translate(err)
	}
	_, err = q.Exec(ctx, `
		UPDATE repositories r SET last_indexed_at = now()
		FROM repo_index_runs ir WHERE ir.id = $1 AND r.id = ir.repository_id`, id)
	return translate(err)
}

// FailIndexRun откладывает прогон на следующую попытку. Строка не удаляется:
// уникальность (repository_id, commit_sha) — это ещё и защита от параллельной
// индексации того же коммита.
func (s *Store) FailIndexRun(ctx context.Context, q Querier, id uuid.UUID, cause string, backoff time.Duration, maxAttempts int) error {
	_, err := q.Exec(ctx, `
		UPDATE repo_index_runs SET
		    state = CASE WHEN attempt >= $4 THEN 'failed' ELSE 'queued' END,
		    error = $2,
		    finished_at = CASE WHEN attempt >= $4 THEN now() ELSE NULL END,
		    next_attempt_at = now() + $3::interval
		WHERE id = $1`, id, cause, backoff.String(), maxAttempts)
	return translate(err)
}

// ParentFile — файл предыдущего прогона: по source_sha решается, изменился ли
// он, а по идентификатору куски копируются без выкачивания и эмбеддинга.
type ParentFile struct {
	ID        uuid.UUID
	SourceSHA string
	BlobSHA   *string
}

func (s *Store) ParentFiles(ctx context.Context, q Querier, runID uuid.UUID) (map[string]ParentFile, error) {
	rows, err := q.Query(ctx, `
		SELECT path, id, coalesce(source_sha, ''), blob_sha
		FROM code_files WHERE index_run_id = $1`, runID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := map[string]ParentFile{}
	for rows.Next() {
		var path string
		var f ParentFile
		if err := rows.Scan(&path, &f.ID, &f.SourceSHA, &f.BlobSHA); err != nil {
			return nil, translate(err)
		}
		out[path] = f
	}
	return out, translate(rows.Err())
}

type CodeFileInput struct {
	RunID        uuid.UUID
	RepositoryID uuid.UUID
	Path         string
	Lang         string
	SizeBytes    int64
	BlobSHA      string
	SourceSHA    string
}

func (s *Store) InsertCodeFile(ctx context.Context, q Querier, in CodeFileInput) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO code_files (index_run_id, repository_id, path, lang, size_bytes, blob_sha, source_sha)
		VALUES ($1, $2, $3, nullif($4, ''), $5, nullif($6, ''), nullif($7, ''))
		RETURNING id`,
		in.RunID, in.RepositoryID, in.Path, in.Lang, in.SizeBytes, in.BlobSHA, in.SourceSHA,
	).Scan(&id)
	return id, translate(err)
}

// CodeChunkInput — кусок файла вместе с вектором. Вектор может быть nil:
// без провайдера эмбеддингов поиск вырождается в полнотекстовый, но индекс
// остаётся полным и досчитать векторы можно позже.
type CodeChunkInput struct {
	Ord        int
	StartLine  int
	EndLine    int
	SymbolFQN  string
	Text       string
	TextSHA    string
	Tokens     int
	Embedding  []float32
	EmbedModel string
}

func (s *Store) InsertCodeChunks(ctx context.Context, q Querier, fileID, projectID uuid.UUID, chunks []CodeChunkInput) error {
	if len(chunks) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, c := range chunks {
		batch.Queue(`
			INSERT INTO code_chunks
			    (code_file_id, project_id, ord, start_line, end_line, symbol_fqn,
			     text, token_count, text_sha, embedding, embedding_model)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9, nullif($10, '')::vector, nullif($11, ''))`,
			fileID, projectID, c.Ord, c.StartLine, c.EndLine, c.SymbolFQN,
			c.Text, c.Tokens, c.TextSHA, vectorLiteral(c.Embedding), c.EmbedModel)
	}
	return sendBatch(ctx, q, batch)
}

type CodeSymbolInput struct {
	Kind      string
	Name      string
	FQN       string
	StartLine int
	EndLine   int
	Exported  bool
}

func (s *Store) InsertCodeSymbols(ctx context.Context, q Querier, fileID, repoID uuid.UUID, symbols []CodeSymbolInput) error {
	if len(symbols) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, sym := range symbols {
		batch.Queue(`
			INSERT INTO code_symbols
			    (code_file_id, repository_id, kind, name, fqn, start_line, end_line, exported)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			fileID, repoID, sym.Kind, sym.Name, sym.FQN, sym.StartLine, sym.EndLine, sym.Exported)
	}
	return sendBatch(ctx, q, batch)
}

// CopyFileFromParent переносит файл и всё производное из предыдущего прогона.
//
// Это и есть инкрементальность: неизменившийся файл не выкачивается, не
// режется заново и не эмбеддится — его куски вместе с уже посчитанными
// векторами копируются одним запросом.
func (s *Store) CopyFileFromParent(ctx context.Context, q Querier, runID, parentFileID uuid.UUID) (int, error) {
	var newFileID uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO code_files (index_run_id, repository_id, path, lang, size_bytes, blob_sha, source_sha)
		SELECT $1, repository_id, path, lang, size_bytes, blob_sha, source_sha
		FROM code_files WHERE id = $2
		RETURNING id`, runID, parentFileID).Scan(&newFileID)
	if err != nil {
		return 0, translate(err)
	}

	tag, err := q.Exec(ctx, `
		INSERT INTO code_chunks
		    (code_file_id, project_id, ord, start_line, end_line, symbol_fqn,
		     text, token_count, text_sha, embedding, embedding_model)
		SELECT $1, project_id, ord, start_line, end_line, symbol_fqn,
		       text, token_count, text_sha, embedding, embedding_model
		FROM code_chunks WHERE code_file_id = $2`, newFileID, parentFileID)
	if err != nil {
		return 0, translate(err)
	}

	if _, err := q.Exec(ctx, `
		INSERT INTO code_symbols
		    (code_file_id, repository_id, kind, name, fqn, signature, doc, start_line, end_line, exported)
		SELECT $1, repository_id, kind, name, fqn, signature, doc, start_line, end_line, exported
		FROM code_symbols WHERE code_file_id = $2`, newFileID, parentFileID); err != nil {
		return 0, translate(err)
	}

	return int(tag.RowsAffected()), nil
}

type FileChangeInput struct {
	RunID          uuid.UUID
	RepositoryID   uuid.UUID
	Path           string
	ChangeType     string
	PreviousPath   string
	ChangedSymbols []string
	Additions      int
	Deletions      int
	CommitSHA      string
}

func (s *Store) InsertFileChanges(ctx context.Context, q Querier, changes []FileChangeInput) error {
	if len(changes) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, c := range changes {
		symbols := c.ChangedSymbols
		if symbols == nil {
			symbols = []string{}
		}
		batch.Queue(`
			INSERT INTO code_file_changes
			    (index_run_id, repository_id, path, change_type, previous_path,
			     changed_symbols, additions, deletions, commit_sha)
			VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7, $8, $9)
			ON CONFLICT (index_run_id, path) DO NOTHING`,
			c.RunID, c.RepositoryID, c.Path, c.ChangeType, c.PreviousPath,
			symbols, c.Additions, c.Deletions, c.CommitSHA)
	}
	return sendBatch(ctx, q, batch)
}

// CachedEmbeddings достаёт уже посчитанные векторы по sha текстов.
//
// Кэш адресуется содержимым, поэтому попадает и на файле, вернувшемся после
// отката, и на куске, переехавшем в другой файл или репозиторий. При
// непрерывной кодогенерации это заметная доля прогона.
func (s *Store) CachedEmbeddings(ctx context.Context, q Querier, model string, shas []string) (map[string][]float32, error) {
	out := map[string][]float32{}
	if len(shas) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT text_sha, embedding::text FROM embedding_cache
		WHERE model = $1 AND text_sha = ANY($2)`, model, shas)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	for rows.Next() {
		var sha, vec string
		if err := rows.Scan(&sha, &vec); err != nil {
			return nil, translate(err)
		}
		parsed, err := parseVector(vec)
		if err != nil {
			// Битую строку кэша проще пересчитать, чем валить прогон.
			continue
		}
		out[sha] = parsed
	}
	return out, translate(rows.Err())
}

// TouchEmbeddings отмечает попадания в кэш: строки живут, пока ими
// пользуются, и вытеснение смотрит именно на эту отметку.
//
// Один UPDATE на всю пачку, ровно тот же набор sha, которым ходил
// CachedEmbeddings. Запрос на чанк здесь запрещён: пачка прогона — это сотни
// sha, и индексация встала бы на отметке, которая никому не видна.
//
// staleAfter — порог, ниже которого строку не переписывают. Без него каждая
// пачка каждого прогона переписывала бы четырёхкилобайтные строки и индекс по
// last_used_at ради времени, которое по существу не изменилось. Ноль означает
// «отмечать всегда» и нужен разве что тесту.
//
// Возвращает число обновлённых строк.
func (s *Store) TouchEmbeddings(ctx context.Context, q Querier, model string, shas []string, staleAfter time.Duration) (int, error) {
	if len(shas) == 0 {
		return 0, nil
	}
	if staleAfter < 0 {
		staleAfter = 0
	}
	tag, err := q.Exec(ctx, `
		UPDATE embedding_cache SET last_used_at = now()
		WHERE model = $1 AND text_sha = ANY($2)
		  AND last_used_at < now() - $3::interval`, model, shas, staleAfter)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) CacheEmbeddings(ctx context.Context, q Querier, model string, vectors map[string][]float32) error {
	if len(vectors) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for sha, vec := range vectors {
		batch.Queue(`
			INSERT INTO embedding_cache (model, text_sha, embedding)
			VALUES ($1, $2, $3::vector)
			ON CONFLICT (model, text_sha) DO NOTHING`, model, sha, vectorLiteral(vec))
	}
	return sendBatch(ctx, q, batch)
}

func sendBatch(ctx context.Context, q Querier, batch *pgx.Batch) error {
	type batchSender interface {
		SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	}
	sender, ok := q.(batchSender)
	if !ok {
		return apperr.Internal(nil, "пакетная запись требует пула или транзакции")
	}
	res := sender.SendBatch(ctx, batch)
	if _, err := res.Exec(); err != nil {
		_ = res.Close()
		return translate(err)
	}
	return translate(res.Close())
}

// vectorLiteral кодирует вектор в литерал pgvector. Отдельного типа в pgx
// нет, а строка с приведением ::vector — то, что понимает расширение.
func vectorLiteral(v []float32) string {
	if len(v) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func parseVector(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil, fmt.Errorf("пустой вектор")
	}
	parts := strings.Split(s, ",")
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, err
		}
		out = append(out, float32(f))
	}
	return out, nil
}

// IndexFailure — репозиторий перестал читаться (CLAB-79). Source: run —
// провалился прогон индексации, head — не читается голова отслеживаемой
// ветки, и прогон поэтому даже не заводится.
type IndexFailure struct {
	Since  time.Time `json:"since"`
	Error  string    `json:"error"`
	Source string    `json:"source"`
}

// IndexStatusRow — состояние индекса одного репозитория для UI и проверки.
type IndexStatusRow struct {
	Repository     string     `json:"repository"`
	Enabled        bool       `json:"index_enabled"`
	Branches       []string   `json:"watch_branches"`
	LastIndexedAt  *time.Time `json:"last_indexed_at,omitempty"`
	Branch         *string    `json:"branch,omitempty"`
	Commit         *string    `json:"commit,omitempty"`
	State          *string    `json:"state,omitempty"`
	Kind           *string    `json:"kind,omitempty"`
	FilesIndexed   *int       `json:"files_indexed,omitempty"`
	FilesReused    *int       `json:"files_reused,omitempty"`
	ChunksIndexed  *int       `json:"chunks_indexed,omitempty"`
	ChunksReused   *int       `json:"chunks_reused,omitempty"`
	ChunksEmbedded *int       `json:"chunks_embedded,omitempty"`
	Attempt        *int       `json:"attempt,omitempty"`
	Error          *string    `json:"error,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	// Failure — репозиторий не читается повторно; nil — читается либо
	// провал пока единичный.
	Failure *IndexFailure `json:"failure,omitempty"`
}

// indexHealthFrom — репозитории проекта ($1) с последним прогоном (ir) и
// серией провалов чтения головы (hf). Общая FROM-часть строки состояния
// индекса и счётчика сводки: правило «репозиторий не читается» обязано быть
// одним на оба ответа, иначе отметка в меню и таблица разойдутся.
//
// hf — серия от двух проходов подряд по ветке, которая всё ещё отслеживается;
// из нескольких веток берётся та, что не читается дольше.
const indexHealthFrom = `
	FROM repositories r
	LEFT JOIN github_installations gi ON gi.installation_id = r.installation_id
	LEFT JOIN LATERAL (
	    SELECT * FROM repo_index_runs
	    WHERE repository_id = r.id
	    ORDER BY created_at DESC LIMIT 1) ir ON true
	LEFT JOIN LATERAL (
	    SELECT first_failed_at, error FROM repository_head_failures
	    WHERE repository_id = r.id AND failures >= 2
	      AND branch = ANY (r.watch_branches)
	    ORDER BY first_failed_at LIMIT 1) hf ON true
	WHERE r.project_id = $1`

// indexFailureSource — правило «репозиторий не читается» (CLAB-79), NULL —
// читается. Повторный провал — это прогон в failed, прогон в очереди после
// второй неудачной попытки либо голова, не прочитанная на двух проходах
// воркера подряд. Одна неудача, за которой идёт успех, сигнала не даёт.
// Репозиторий вне целей индексации (выключен, убран из установки, установка
// снята) сигнала не несёт: его не читают намеренно.
//
// Класс ошибки не фильтруется: 404 на репозитории оставляет знание таким же
// устаревшим, как непринятое приложение.
const indexFailureSource = `CASE
	    WHEN NOT (r.index_enabled AND ` + indexTargetLive + `) THEN NULL
	    WHEN ir.state = 'failed'
	      OR (ir.state = 'queued' AND ir.error IS NOT NULL AND ir.attempt >= 2) THEN 'run'
	    WHEN hf.first_failed_at IS NOT NULL THEN 'head'
	END`

// indexFailureSince — с какого времени не читается. У прогона это начало
// серии: первый неудавшийся прогон после последнего завершённого. Коммиты
// приходят каждый день, и «с» по последнему прогону показывало бы вчера при
// индексе, который стоит третью неделю.
const indexFailureSince = `CASE ` + indexFailureSource + `
	    WHEN 'run' THEN coalesce((
	        SELECT min(x.created_at) FROM repo_index_runs x
	        WHERE x.repository_id = r.id AND x.error IS NOT NULL
	          AND x.created_at > coalesce((
	              SELECT max(c.created_at) FROM repo_index_runs c
	              WHERE c.repository_id = r.id AND c.state = 'completed'), '-infinity')),
	        ir.created_at)
	    WHEN 'head' THEN hf.first_failed_at
	END`

const indexFailureError = `CASE ` + indexFailureSource + `
	    WHEN 'run' THEN ir.error
	    WHEN 'head' THEN hf.error
	END`

// IndexStatus — по последнему прогону на репозиторий. Отношение reused к
// indexed показывает, во что обошлась инкрементальность: на репозитории, где
// меняются десятки файлов из тысяч, разница на два порядка.
func (s *Store) IndexStatus(ctx context.Context, q Querier, projectID uuid.UUID) ([]IndexStatusRow, error) {
	rows, err := q.Query(ctx, `
		SELECT r.owner || '/' || r.name, r.index_enabled, r.watch_branches, r.last_indexed_at,
		       ir.branch, ir.commit_sha, ir.state, ir.kind,
		       ir.files_indexed, ir.files_reused, ir.chunks_indexed,
		       ir.chunks_reused, ir.chunks_embedded, ir.attempt, ir.error, ir.finished_at,
		       `+indexFailureSource+`, `+indexFailureSince+`, `+indexFailureError+
		indexHealthFrom+`
		ORDER BY r.owner, r.name`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []IndexStatusRow{}
	for rows.Next() {
		var r IndexStatusRow
		var source, cause *string
		var since *time.Time
		if err := rows.Scan(&r.Repository, &r.Enabled, &r.Branches, &r.LastIndexedAt,
			&r.Branch, &r.Commit, &r.State, &r.Kind,
			&r.FilesIndexed, &r.FilesReused, &r.ChunksIndexed,
			&r.ChunksReused, &r.ChunksEmbedded, &r.Attempt, &r.Error, &r.FinishedAt,
			&source, &since, &cause); err != nil {
			return nil, translate(err)
		}
		if source != nil && since != nil {
			r.Failure = &IndexFailure{Since: *since, Source: *source}
			if cause != nil {
				r.Failure.Error = *cause
			}
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// CountUnreadableRepositories — сколько репозиториев проекта не читается:
// число строк IndexStatus с failure, по тому же правилу.
func (s *Store) CountUnreadableRepositories(ctx context.Context, q Querier, projectID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE `+indexFailureSource+` IS NOT NULL)`+
		indexHealthFrom, projectID).Scan(&n)
	return n, translate(err)
}

// RecordHeadFailure отмечает, что голова ветки не прочиталась на этом
// проходе воркера. Начало серии повторный провал не двигает.
func (s *Store) RecordHeadFailure(ctx context.Context, q Querier, repoID uuid.UUID, branch, cause string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO repository_head_failures (repository_id, branch, error)
		VALUES ($1, $2, $3)
		ON CONFLICT (repository_id, branch) DO UPDATE
		SET failures = repository_head_failures.failures + 1,
		    last_failed_at = now(),
		    error = excluded.error`, repoID, branch, cause)
	return translate(err)
}

// ClearHeadFailure снимает серию: голова прочиталась. Зовётся на каждом
// удачном чтении, в том числе когда новый прогон не заводится — коммит уже
// проиндексирован.
func (s *Store) ClearHeadFailure(ctx context.Context, q Querier, repoID uuid.UUID, branch string) error {
	_, err := q.Exec(ctx, `
		DELETE FROM repository_head_failures WHERE repository_id = $1 AND branch = $2`, repoID, branch)
	return translate(err)
}
