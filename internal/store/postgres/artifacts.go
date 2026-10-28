package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// BlobStore — внешнее хранилище крупного содержимого. Объявлен здесь, а не
// взят из blobstore, чтобы store не зависел от реализации; из blobstore взят
// только потолок inline — одно число на конфигурацию и хранилище.
//
// Содержимое идёт через него потоком (CLAB-295): запись — из файла спула с
// уже посчитанным sha, чтение — io.ReadCloser. Контракт методов — у
// blobstore.Store.
type BlobStore interface {
	PutFile(ctx context.Context, sha, path string, size int64, mediaType string) (string, error)
	Open(ctx context.Context, uri string) (io.ReadCloser, error)
	// Delete зовёт сборщик мусора — после того, как удалил строку blobs.
	Delete(ctx context.Context, uri string) error
	// TempDir — где спулить содержимое; пусто — os.TempDir().
	TempDir() string
}

// UseBlobStore задаёт пределы содержимого и, если bs не nil, включает вынос:
// содержимое больше inlineMax уходит во внешнее хранилище, в базе остаётся
// адрес. maxBytes — предел принимаемого вовсе, на любом пути, inline тоже.
//
// Без хранилища (bs == nil) всё лежит inline, и принимается не больше
// min(blobstore.InlineMaxBytes, maxBytes); inlineMax тогда не действует.
// Порог inline выше потолка blobstore.InlineMaxBytes срезается до него:
// config.Load такую пару не пропускает, а здесь — охрана для вызовов в обход
// конфигурации.
func (s *Store) UseBlobStore(bs BlobStore, inlineMax, maxBytes int) {
	s.blobs = bs
	s.inlineMax = inlineMax
	s.blobMax = maxBytes
}

func (s *Store) inlineLimit() int {
	if s.blobs == nil || s.inlineMax <= 0 || s.inlineMax > blobstore.InlineMaxBytes {
		return blobstore.InlineMaxBytes
	}
	return s.inlineMax
}

// BlobLimit — сколько байт содержимого PutBlob примет: общий предел, а без
// внешнего хранилища — не больше потолка blobstore.InlineMaxBytes. Ноль —
// общего предела нет, но потолок без хранилища действует и тогда. Правило
// живёт здесь, рядом с PutBlob, чтобы тот, кто отсеивает содержимое заранее
// (индексатор), не повторял его у себя.
func (s *Store) BlobLimit() int {
	if s.blobs != nil {
		return s.blobMax
	}
	if s.blobMax > 0 && s.blobMax < blobstore.InlineMaxBytes {
		return s.blobMax
	}
	return blobstore.InlineMaxBytes
}

// BlobInfo — записанное содержимое: адрес и размер.
type BlobInfo struct {
	SHA  string
	Size int64
}

// PutBlob кладёт содержимое в контент-адресуемое хранилище и возвращает sha256.
// Это обёртка над PutBlobFrom для содержимого, уже лежащего в памяти: путь
// записи у них один, а отказы по пределу — прежние, 400 с прежним текстом:
// его ждут писатели связки, документов и индексатора.
func (s *Store) PutBlob(ctx context.Context, q Querier, content []byte, mediaType string) (string, error) {
	if len(content) == 0 {
		return "", apperr.InvalidInput("содержимое артефакта пустое")
	}
	// Общий предел — до выбора пути: inline-содержимое ему подчиняется так же,
	// как вынесенное.
	if s.blobMax > 0 && len(content) > s.blobMax {
		return "", apperr.InvalidInput("содержимое %d байт больше предела %d (SDLC_BLOB_MAX_BYTES)",
			len(content), s.blobMax)
	}
	if len(content) <= s.inlineLimit() {
		info, err := s.putInline(ctx, q, content, mediaType)
		return info.SHA, err
	}
	if s.blobs == nil {
		return "", apperr.InvalidInput(
			"содержимое %d байт больше предела %d, а внешнее хранилище не настроено (SDLC_BLOB_DIR или SDLC_BLOB_S3_BUCKET)",
			len(content), blobstore.InlineMaxBytes)
	}
	info, err := s.putExternal(ctx, q, content, bytes.NewReader(nil), mediaType)
	return info.SHA, err
}

// PutBlobFrom кладёт содержимое из потока и возвращает его sha256 и размер.
//
// Это приём (SpoolBlob) и фиксация (CommitBlob) одним вызовом — для
// писателей, у которых поток уже под рукой и транзакция их не держит.
// Транспорт, принимающий файл от клиента, зовёт их порознь: приём — до
// транзакции, фиксацию — в ней (CLAB-296).
//
// Общий предел (BlobLimit) считается по потоку, без доверия к заявленной
// длине: превышение — payload_too_large, и ни строки, ни объекта, ни спула
// после него не остаётся. Ошибка читателя (клиент ушёл, предел тела
// запроса) приходит обёрнутой, с причиной в цепочке.
func (s *Store) PutBlobFrom(ctx context.Context, q Querier, r io.Reader, mediaType string) (BlobInfo, error) {
	b, err := s.SpoolBlob(r)
	if err != nil {
		return BlobInfo{}, err
	}
	defer b.Close()
	return s.CommitBlob(ctx, q, b, mediaType)
}

// SpooledBlob — содержимое, принятое из потока, но ещё не записанное:
// sha256 и размер посчитаны, байты лежат в памяти (не больше порога inline)
// или в файле спула. Закрывает его тот, кто принял, — после фиксации или
// вместо неё: Close убирает спул.
type SpooledBlob struct {
	SHA  string
	Size int64
	// Ровно одно из двух: содержимое inline или путь спула.
	inline []byte
	path   string
}

// Close убирает спул. Забранный хранилищем переименованием уже не на месте
// — это не ошибка. Повторный вызов безвреден.
func (b *SpooledBlob) Close() error {
	if b == nil || b.path == "" {
		return nil
	}
	err := os.Remove(b.path)
	b.path = ""
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// SpoolBlob принимает содержимое из потока, не трогая базу.
//
// Первые байты, до порога inline, читаются в память: кончился поток раньше
// — содержимое ляжет в строку blobs. Нет — весь поток спулится во временный
// файл (каталог — TempDir хранилища) с подсчётом sha256 на лету; сверх
// порога inline память на содержимое не тратится.
//
// Здесь нет ни соединения, ни транзакции, ни замка: медленный клиент,
// который шлёт файл десять минут, не держит ни строку задачи, ни соединение
// пула (R-1 CLAB-296). Пустое содержимое — отказ: блоб из нуля байт не
// заводится.
func (s *Store) SpoolBlob(r io.Reader) (*SpooledBlob, error) {
	inline := s.inlineLimit()
	var head bytes.Buffer
	if _, err := head.ReadFrom(io.LimitReader(r, int64(inline)+1)); err != nil {
		return nil, readFailed(err)
	}
	if head.Len() == 0 {
		return nil, apperr.InvalidInput("содержимое артефакта пустое")
	}
	if head.Len() <= inline {
		if limit := s.BlobLimit(); limit > 0 && head.Len() > limit {
			return nil, blobTooLarge(int64(limit))
		}
		return inlineBlob(head.Bytes()), nil
	}
	if s.blobs == nil {
		// Без хранилища крупнее порога inline не принимается вовсе: дальше
		// читать незачем.
		return nil, blobTooLarge(int64(s.BlobLimit()))
	}
	return s.spoolExternal(head.Bytes(), r)
}

func inlineBlob(content []byte) *SpooledBlob {
	sum := sha256.Sum256(content)
	return &SpooledBlob{SHA: hex.EncodeToString(sum[:]), Size: int64(len(content)), inline: content}
}

// CommitBlob записывает принятое содержимое в транзакции q: строку blobs, а
// вынесенное — сперва объектом во внешнее хранилище.
//
// Перед записью берётся advisory-замок по sha, тот же, что берёт сборщик
// мусора (CollectBlobs). Он держится до конца этой транзакции — то есть и
// после того, как ссылающаяся строка вставлена, — и потому сборщик не удалит
// блоб, на который ссылка вот-вот появится.
//
// Замок по sha берётся до похода в хранилище, а не после, — иначе PutFile
// успевал бы отработать вне замка. Обратную гонку — объект, унесённый
// из-под только что вставленной строки, — этот замок сам по себе не
// закрывает: между коммитом удаляющей транзакции сборщика и удалением
// объекта из хранилища он свободен. Её закрывает сборщик со своей стороны,
// держа тот же ключ сессионным замком на оба шага (withBlobLock в blobs.go).
//
// Запись, заставшая ключ занятым проходом сборщика, ждёт его — не дольше
// одного удаления объекта из хранилища. Обратной задержки нет: сборщик
// занятый замок не ждёт, а блоб пропускает. Спул к этому моменту уже
// записан: замок не держится на время приёма содержимого от клиента.
func (s *Store) CommitBlob(ctx context.Context, q Querier, b *SpooledBlob, mediaType string) (BlobInfo, error) {
	if b == nil || (b.inline == nil && b.path == "") {
		return BlobInfo{}, apperr.Internal(nil, "содержимое не принято или уже записано")
	}
	if mediaType == "" {
		mediaType = "text/markdown"
	}
	if err := s.lockBlob(ctx, q, b.SHA); err != nil {
		return BlobInfo{}, err
	}
	if b.inline != nil {
		if _, err := q.Exec(ctx, `
			INSERT INTO blobs (sha256, size_bytes, media_type, storage, content)
			VALUES ($1, $2, $3, 'inline', $4)
			ON CONFLICT (sha256) DO NOTHING`,
			b.SHA, b.Size, mediaType, b.inline); err != nil {
			return BlobInfo{}, translate(err)
		}
		return BlobInfo{SHA: b.SHA, Size: b.Size}, nil
	}
	uri, err := s.blobs.PutFile(ctx, b.SHA, b.path, b.Size, mediaType)
	if err != nil {
		return BlobInfo{}, apperr.Internal(err, "запись во внешнее хранилище")
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO blobs (sha256, size_bytes, media_type, storage, external_uri)
		VALUES ($1, $2, $3, 'external', $4)
		ON CONFLICT (sha256) DO NOTHING`,
		b.SHA, b.Size, mediaType, uri); err != nil {
		return BlobInfo{}, translate(err)
	}
	return BlobInfo{SHA: b.SHA, Size: b.Size}, nil
}

// putInline пишет содержимое из памяти в строку blobs.
func (s *Store) putInline(ctx context.Context, q Querier, content []byte, mediaType string) (BlobInfo, error) {
	if len(content) == 0 {
		return BlobInfo{}, apperr.InvalidInput("содержимое артефакта пустое")
	}
	if limit := s.BlobLimit(); limit > 0 && len(content) > limit {
		return BlobInfo{}, blobTooLarge(int64(limit))
	}
	return s.CommitBlob(ctx, q, inlineBlob(content), mediaType)
}

// putExternal спулит head и остаток потока и выносит их во внешнее
// хранилище; спул убирается всегда.
func (s *Store) putExternal(ctx context.Context, q Querier, head []byte, rest io.Reader, mediaType string) (BlobInfo, error) {
	b, err := s.spoolExternal(head, rest)
	if err != nil {
		return BlobInfo{}, err
	}
	defer b.Close()
	return s.CommitBlob(ctx, q, b, mediaType)
}

// spoolExternal спулит head и остаток потока во временный файл, считая
// sha256 и размер. После отказа и ошибки спула не остаётся.
func (s *Store) spoolExternal(head []byte, rest io.Reader) (_ *SpooledBlob, err error) {
	limit := int64(s.BlobLimit())
	if limit > 0 && int64(len(head)) > limit {
		return nil, blobTooLarge(limit)
	}

	spool, err := os.CreateTemp(s.blobs.TempDir(), ".spool-*")
	if err != nil {
		return nil, apperr.Internal(err, "временный файл содержимого")
	}
	defer func() {
		_ = spool.Close()
		if err != nil {
			_ = os.Remove(spool.Name())
		}
	}()

	h := sha256.New()
	w := io.MultiWriter(spool, h)
	if _, err := w.Write(head); err != nil {
		return nil, apperr.Internal(err, "запись временного файла содержимого")
	}
	src := &readTap{r: rest}
	var body io.Reader = src
	if limit > 0 {
		// Байт сверх предела дочитывается, чтобы отличить «ровно предел»
		// от «больше».
		body = io.LimitReader(src, limit-int64(len(head))+1)
	}
	n, err := io.Copy(w, body)
	if err != nil {
		if src.err != nil {
			return nil, readFailed(src.err)
		}
		return nil, apperr.Internal(err, "запись временного файла содержимого")
	}
	size := int64(len(head)) + n
	if limit > 0 && size > limit {
		return nil, blobTooLarge(limit)
	}
	if err := spool.Close(); err != nil {
		return nil, apperr.Internal(err, "запись временного файла содержимого")
	}
	return &SpooledBlob{SHA: hex.EncodeToString(h.Sum(nil)), Size: size, path: spool.Name()}, nil
}

// readTap запоминает ошибку читателя: io.Copy возвращает и ошибки чтения, и
// ошибки записи в спул, а это разные исходы — клиент ушёл или кончился диск.
type readTap struct {
	r   io.Reader
	err error
}

func (t *readTap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF {
		t.err = err
	}
	return n, err
}

// blobTooLarge — содержимое больше общего предела (или потолка inline без
// внешнего хранилища).
func blobTooLarge(limit int64) *apperr.Error {
	return apperr.New(apperr.CodePayloadTooLarge,
		"содержимое больше предела %d байт (SDLC_BLOB_MAX_BYTES; без SDLC_BLOB_DIR и SDLC_BLOB_S3_BUCKET — не больше %d)",
		limit, blobstore.InlineMaxBytes).
		WithDetails(map[string]any{"limit_bytes": limit})
}

// readFailed — поток содержимого оборвался. Причина остаётся в цепочке:
// транспорт узнаёт по ней свой предел тела запроса и отвечает 413.
func readFailed(err error) *apperr.Error {
	return apperr.Wrap(apperr.CodeInvalidInput, err, "содержимое не дочитано: %v", err)
}

// Blob — содержимое, открытое на чтение: поток, размер и тип из строки blobs.
// Закрывает его тот, кто открыл.
type Blob struct {
	io.ReadCloser
	Size      int64
	MediaType string
}

// OpenBlob открывает содержимое потоком. Строка blobs читается одним
// запросом, и соединение с базой не держится, пока поток отдают клиенту.
// Пропавший объект внешнего хранилища — ошибка здесь, а не посреди потока.
func (s *Store) OpenBlob(ctx context.Context, q Querier, sha string) (Blob, error) {
	var content []byte
	var size int64
	var mediaType, storage string
	var externalURI *string
	err := q.QueryRow(ctx, `
		SELECT content, size_bytes, media_type, storage, external_uri FROM blobs WHERE sha256 = $1`, sha,
	).Scan(&content, &size, &mediaType, &storage, &externalURI)
	if err != nil {
		return Blob{}, notFound(err, "содержимое %s не найдено", sha)
	}
	if storage == "inline" {
		return Blob{ReadCloser: io.NopCloser(bytes.NewReader(content)), Size: size, MediaType: mediaType}, nil
	}
	if s.blobs == nil || externalURI == nil {
		return Blob{}, apperr.Internal(nil,
			"содержимое %s вынесено во внешнее хранилище, а оно не настроено", sha)
	}
	r, err := s.blobs.Open(ctx, *externalURI)
	if err != nil {
		return Blob{}, apperr.Internal(err, "чтение %s из внешнего хранилища", sha)
	}
	return Blob{ReadCloser: r, Size: size, MediaType: mediaType}, nil
}

// BlobContent читает содержимое целиком — для текстовых читателей (разбор,
// сдача, MCP, индексатор): им нужен текст, а не поток.
func (s *Store) BlobContent(ctx context.Context, q Querier, sha string) ([]byte, string, error) {
	b, err := s.OpenBlob(ctx, q, sha)
	if err != nil {
		return nil, "", err
	}
	defer b.Close()
	var buf bytes.Buffer
	if b.Size > 0 {
		buf.Grow(int(b.Size))
	}
	if _, err := buf.ReadFrom(b); err != nil {
		return nil, "", apperr.Internal(err, "чтение %s из внешнего хранилища", sha)
	}
	return buf.Bytes(), b.MediaType, nil
}

const artifactSelect = `
	SELECT a.id, a.task_id, a.project_id, a.stage_run_id, a.kind, a.title, a.format,
	       a.blob_sha, b.size_bytes, a.version, a.supersedes_id, a.status,
	       a.produced_by, a.approved_by, a.meta, a.created_at,
	       a.is_attachment, a.file_name, a.media_type
	FROM artifacts a
	JOIN blobs b ON b.sha256 = a.blob_sha`

func scanArtifact(row interface{ Scan(...any) error }) (domain.Artifact, error) {
	var a domain.Artifact
	err := row.Scan(&a.ID, &a.TaskID, &a.ProjectID, &a.StageRunID, &a.Kind, &a.Title,
		&a.Format, &a.BlobSHA, &a.SizeBytes, &a.Version, &a.SupersedesID, &a.Status,
		&a.ProducedBy, &a.ApprovedBy, &a.Meta, &a.CreatedAt,
		&a.IsAttachment, &a.FileName, &a.MediaType)
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
	Task       domain.Task
	StageRunID *uuid.UUID
	Kind       string
	Title      string
	Format     string
	Content    []byte
	MediaType  string
	// Blob — файл, принятый до транзакции (SpoolBlob); тогда Content не
	// читается, а FileName и MediaType ложатся и на артефакт.
	Blob     *SpooledBlob
	FileName string
	// IsAttachment — вложение (решение 0064): без прогона, заменяет только
	// вложение той же задачи и только актуальную версию.
	IsAttachment bool
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
	// Заменяемая версия проверяется до записи содержимого: отказ не должен
	// оставлять ни строки blobs, ни объекта во внешнем хранилище.
	version := 1
	if in.SupersedesID != nil {
		var prevTask uuid.UUID
		var prevVersion int
		var prevAttachment bool
		var prevStatus string
		if err := tx.QueryRow(ctx,
			`SELECT task_id, version, is_attachment, status FROM artifacts WHERE id = $1 FOR UPDATE`,
			*in.SupersedesID).Scan(&prevTask, &prevVersion, &prevAttachment, &prevStatus); err != nil {
			return domain.Artifact{}, notFound(err, "артефакт, который заменяется, не найден")
		}
		if prevTask != in.Task.ID {
			return domain.Artifact{}, apperr.InvalidInput(
				"нельзя заменить артефакт из другой задачи")
		}
		// Вложение и результат этапа друг друга не заменяют: иначе файл от
		// viewer перекрыл бы сданный этапом артефакт, а сдача этапа — файл.
		switch {
		case in.IsAttachment && !prevAttachment:
			return domain.Artifact{}, apperr.InvalidInput(
				"вложение заменяет только вложение, а %s — артефакт этапа", in.SupersedesID)
		case !in.IsAttachment && prevAttachment:
			return domain.Artifact{}, apperr.InvalidInput(
				"%s — вложение; новая версия вложения — POST /v1/tasks/<ключ>/attachments", in.SupersedesID)
		}
		// Две замены одной версии вложения: вторая, дождавшись FOR UPDATE,
		// видит её перекрытой и не плодит две «вторые версии».
		if in.IsAttachment && prevStatus == domain.ArtifactSuperseded {
			return domain.Artifact{}, apperr.Conflict(
				"версия %d вложения уже заменена — заменять надо актуальную", prevVersion).
				WithDetails(map[string]any{"supersedes_id": in.SupersedesID.String()})
		}
		version = prevVersion + 1
		if _, err := tx.Exec(ctx,
			`UPDATE artifacts SET status = 'superseded' WHERE id = $1`, *in.SupersedesID); err != nil {
			return domain.Artifact{}, translate(err)
		}
	}

	var sha string
	var fileName, fileType *string
	if in.Blob != nil {
		info, err := s.CommitBlob(ctx, tx, in.Blob, in.MediaType)
		if err != nil {
			return domain.Artifact{}, err
		}
		sha = info.SHA
		fileName, fileType = &in.FileName, &in.MediaType
	} else {
		var err error
		if sha, err = s.PutBlob(ctx, tx, in.Content, in.MediaType); err != nil {
			return domain.Artifact{}, err
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
		                       blob_sha, version, supersedes_id, status, produced_by, meta,
		                       is_attachment, file_name, media_type)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id`,
		in.Task.ID, in.Task.ProjectID, in.StageRunID, in.Kind, in.Title, format,
		sha, version, in.SupersedesID, status, in.ProducedBy, meta,
		in.IsAttachment, fileName, fileType,
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
//
// Вложений здесь нет (R-5 CLAB-296): эту выборку читают гейты, перепроверка,
// вход этапа и карточка, и вложение не должно засчитываться ни одним из
// них — ни нынешним, ни будущим. Вложения читает ListAttachments.
func (s *Store) ListArtifacts(ctx context.Context, q Querier, taskID uuid.UUID, includeSuperseded bool) ([]domain.Artifact, error) {
	rows, err := q.Query(ctx, artifactSelect+`
		WHERE a.task_id = $1 AND NOT a.is_attachment AND ($2 OR a.status <> 'superseded')
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

// Attachment — вложение задачи с хэндлом того, кто его приложил: список
// вложений показывают людям и агенту, и «кто» в нём — имя, а не UUID.
type Attachment struct {
	domain.Artifact
	ProducedByHandle string
}

// ListAttachments — вложения задачи; includeSuperseded=false оставляет
// актуальные версии. Отдельная выборка, а не флаг ListArtifacts: потребитель
// артефактов этапа не может получить вложение по ошибке.
func (s *Store) ListAttachments(ctx context.Context, q Querier, taskID uuid.UUID, includeSuperseded bool) ([]Attachment, error) {
	rows, err := q.Query(ctx, `
		SELECT x.*, coalesce(p.handle, '') FROM (`+artifactSelect+`
		WHERE a.task_id = $1 AND a.is_attachment AND ($2 OR a.status <> 'superseded')) x
		LEFT JOIN principals p ON p.id = x.produced_by
		ORDER BY x.created_at, x.version`, taskID, includeSuperseded)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []Attachment{}
	for rows.Next() {
		var at Attachment
		a := &at.Artifact
		if err := rows.Scan(&a.ID, &a.TaskID, &a.ProjectID, &a.StageRunID, &a.Kind, &a.Title,
			&a.Format, &a.BlobSHA, &a.SizeBytes, &a.Version, &a.SupersedesID, &a.Status,
			&a.ProducedBy, &a.ApprovedBy, &a.Meta, &a.CreatedAt,
			&a.IsAttachment, &a.FileName, &a.MediaType, &at.ProducedByHandle); err != nil {
			return nil, translate(err)
		}
		out = append(out, at)
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
		WHERE task_id = $1 AND kind = $2 AND status <> 'superseded' AND NOT is_attachment
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
