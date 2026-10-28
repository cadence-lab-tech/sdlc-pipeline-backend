package httpapi

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Файлы в REST (CLAB-296, решение 0064): вложение к задаче, файл по
// контракту и отдача содержимого с безопасными заголовками. Ручки не держат
// правил: вложение создаёт Engine.AttachFile, файл этапа — SubmitArtifact;
// здесь — разбор формы, имя и тип файла и заголовки ответа.

// formFieldMax — предел одного поля формы: заголовок, идентификаторы. Поля
// читаются в память, файл — нет.
const formFieldMax = 64 << 10

// defaultFileName — имя файла, от которого после очистки ничего не осталось.
const defaultFileName = "attachment"

// maxFileNameBytes — предел имени файла, тот же, что держит схема.
const maxFileNameBytes = 255

// ── вложения ────────────────────────────────────────────────────────────────

// attachFile — POST /v1/tasks/:taskKey/attachments. Уровень viewer (guard),
// токен задачи — только в круге записи: своя задача и её прямые дети.
//
// Порядок — ради R-1: форма и файл принимаются до транзакции и без
// блокировки задачи, внутри неё — только запись блоба, строки и события.
func (s *Server) attachFile(c *gin.Context) {
	ctx, taskKey := c.Request.Context(), c.Param("taskKey")
	if err := s.auth.RequireWriteCircle(ctx, identity(c), taskKey, "вложение"); err != nil {
		fail(c, s.log, err)
		return
	}
	form, err := readUploadForm(c, "title", "supersedes_id")
	if err != nil {
		s.uploadFail(c, err)
		return
	}
	supersedes, err := form.uuid("supersedes_id")
	if err != nil {
		fail(c, s.log, err)
		return
	}
	file, err := s.store.SpoolBlob(form.file)
	if err != nil {
		s.uploadFail(c, err)
		return
	}
	defer file.Close()
	if err := form.finish(); err != nil {
		s.uploadFail(c, err)
		return
	}
	art, err := s.engine.AttachFile(ctx, taskKey, actor(c).ID, workflow.AttachInput{
		Title: form.fields["title"], FileName: form.fileName, MediaType: form.mediaType,
		SupersedesID: supersedes, File: file,
	})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, art)
}

// listAttachments — GET /v1/tasks/:taskKey/attachments: вложения задачи
// метаданными; include_superseded=true — с перекрытыми версиями (история).
func (s *Server) listAttachments(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()
	task, err := s.store.GetTaskByKey(ctx, q, c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	list, err := s.engine.Attachments(ctx, q, task.ID, c.Query("include_superseded") == "true")
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"attachments": list})
}

// submitFile — файл по контракту: multipart на POST …/artifacts. Прогон,
// контракт и схема kind проверяются до чтения файла: неверный kind или
// закрытый этап не стоят загрузки.
func (s *Server) submitFile(c *gin.Context) {
	ctx, taskKey := c.Request.Context(), c.Param("taskKey")
	form, err := readUploadForm(c, "kind", "title", "stage_run_id", "supersedes_id")
	if err != nil {
		s.uploadFail(c, err)
		return
	}
	kind, title := strings.TrimSpace(form.fields["kind"]), strings.TrimSpace(form.fields["title"])
	if kind == "" || title == "" {
		fail(c, s.log, apperr.InvalidInput("поля kind и title обязательны и идут до части file"))
		return
	}
	run, err := form.uuid("stage_run_id")
	if err != nil {
		fail(c, s.log, err)
		return
	}
	supersedes, err := form.uuid("supersedes_id")
	if err != nil {
		fail(c, s.log, err)
		return
	}
	// Токен задачи сдаёт только в свой прогон.
	run, err = s.auth.OwnRun(identity(c), taskKey, run)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	if err := s.engine.CheckFileSubmission(ctx, taskKey, actor(c).ID, kind, run); err != nil {
		fail(c, s.log, err)
		return
	}
	file, err := s.store.SpoolBlob(form.file)
	if err != nil {
		s.uploadFail(c, err)
		return
	}
	defer file.Close()
	if err := form.finish(); err != nil {
		s.uploadFail(c, err)
		return
	}
	art, err := s.engine.SubmitArtifact(ctx, taskKey, actor(c).ID, workflow.SubmitArtifactInput{
		Kind: kind, Title: title, Format: "binary", MediaType: form.mediaType,
		FileName: form.fileName, File: file, SupersedesID: supersedes, StageRunID: run,
	})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, art)
}

// ── предел и разбор формы ───────────────────────────────────────────────────

func isMultipart(c *gin.Context) bool {
	return strings.EqualFold(c.ContentType(), "multipart/form-data")
}

// fileLimit — сколько байт файла сервер примет (Store.BlobLimit): то же
// число, что в GET /v1/instance. Ноль — без предела.
func (s *Server) fileLimit() int64 { return int64(s.store.BlobLimit()) }

// fileTooLarge — отказ по пределу файла с самим пределом в сообщении: его
// клиент видел в /v1/instance, а запас на поля формы — деталь транспорта.
func fileTooLarge(limit int64) *apperr.Error {
	return apperr.PayloadTooLarge("файл", limit)
}

// limitUpload — предел тела multipart на маршрутах с файлом: предел файла
// плюс запас на поля формы, уже общего предела тела. Заявленная длина
// больше — 413 без чтения тела; без длины — предел на чтении. Тело JSON на
// том же маршруте (сдача строкой) живёт по общему пределу, как раньше.
func (s *Server) limitUpload() gin.HandlerFunc {
	return func(c *gin.Context) {
		file := s.fileLimit()
		if !isMultipart(c) || file <= 0 {
			c.Next()
			return
		}
		if wrapBody(c, file+bodyLimitSlack) {
			fail(c, s.log, fileTooLarge(file))
			return
		}
		c.Next()
	}
}

// uploadFail — отказ на приёме формы: обрыв по пределу тела отвечает тем же
// пределом файла, что и отказ по заявленной длине.
func (s *Server) uploadFail(c *gin.Context, err error) {
	if _, ok := asTooLarge(err); ok {
		err = fileTooLarge(s.fileLimit())
	}
	fail(c, s.log, err)
}

// uploadForm — форма с файлом, прочитанная до части file. Тело читается
// потоком (MultipartReader), а не ParseMultipartForm: тот буферизует файл в
// память и TMPDIR и sha на лету не считает.
type uploadForm struct {
	reader    *multipart.Reader
	fields    map[string]string
	file      *multipart.Part
	fileName  string
	mediaType string
}

// readUploadForm читает поля формы до части file и останавливается на ней.
// Поля обязаны стоять до файла (R-2 CLAB-296): проверки, которым они нужны,
// идут до приёма файла. Незнакомое поле, повтор поля и поле больше
// formFieldMax — отказ.
func readUploadForm(c *gin.Context, allowed ...string) (*uploadForm, error) {
	if !isMultipart(c) {
		return nil, apperr.InvalidInput("ожидается multipart/form-data: поля формы, затем часть file")
	}
	mr, err := c.Request.MultipartReader()
	if err != nil {
		return nil, apperr.InvalidInput("форма multipart не разбирается: %v", err)
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	f := &uploadForm{reader: mr, fields: map[string]string{}}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, apperr.InvalidInput("в форме нет части file")
		}
		if err != nil {
			return nil, formBroken(err)
		}
		name := part.FormName()
		if name == "file" {
			f.file = part
			f.fileName = sanitizeFileName(partFileName(part))
			f.mediaType = sanitizeMediaType(part.Header.Get("Content-Type"))
			return f, nil
		}
		if !ok[name] {
			sort.Strings(allowed)
			return nil, apperr.InvalidInput("поле формы %q не принимается", name).
				WithDetails(map[string]any{"allowed_fields": append(allowed, "file")})
		}
		if _, dup := f.fields[name]; dup {
			return nil, apperr.InvalidInput("поле формы %q повторяется", name)
		}
		val, err := io.ReadAll(io.LimitReader(part, formFieldMax+1))
		if err != nil {
			return nil, formBroken(err)
		}
		if len(val) > formFieldMax {
			return nil, apperr.InvalidInput("поле формы %q больше %d байт", name, formFieldMax)
		}
		f.fields[name] = string(val)
	}
}

// finish проверяет, что после части file формы нет: поле после файла не
// прошло бы проверок, сделанных до его приёма, а вторая часть file — это
// второй файл в одном вложении.
func (f *uploadForm) finish() error {
	part, err := f.reader.NextPart()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return formBroken(err)
	}
	if part.FormName() == "file" {
		return apperr.InvalidInput("часть file в форме ровно одна")
	}
	return apperr.InvalidInput("поле формы %q после части file: поля идут до файла", part.FormName())
}

func (f *uploadForm) uuid(name string) (*uuid.UUID, error) {
	raw := strings.TrimSpace(f.fields[name])
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, apperr.InvalidInput("%s не UUID", name)
	}
	return &id, nil
}

// formBroken — форма оборвалась или не разбирается. Предел тела остаётся в
// цепочке: на него ответ — 413.
func formBroken(err error) error {
	return apperr.Wrap(apperr.CodeInvalidInput, err, "форма multipart не дочитана: %v", err)
}

// partFileName — имя файла как его прислал клиент, без очистки: Part.FileName
// сам срезает каталоги, а очистка у нас одна — sanitizeFileName.
func partFileName(p *multipart.Part) string {
	if _, params, err := mime.ParseMediaType(p.Header.Get("Content-Disposition")); err == nil {
		if name, ok := params["filename"]; ok {
			return name
		}
	}
	return p.FileName()
}

// sanitizeFileName — имя файла, которое безопасно хранить и отдавать в
// заголовке (R-3): без каталогов, управляющих и невидимых символов
// направления текста, без кавычек, не длиннее 255 байт с сохранением
// расширения. Пустое или из одних точек и пробелов — defaultFileName. Путь в
// хранилище от имени не зависит: блоб адресуется sha256.
func sanitizeFileName(raw string) string {
	name := strings.ToValidUTF8(raw, "")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '"' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if strings.Trim(name, ". ") == "" {
		return defaultFileName
	}
	if len(name) <= maxFileNameBytes {
		return name
	}
	ext := filepath.Ext(name)
	if len(ext) > 32 || ext == name {
		ext = ""
	}
	stem := truncateUTF8(name[:len(name)-len(ext)], maxFileNameBytes-len(ext))
	if stem = strings.TrimSpace(stem); stem == "" {
		return truncateUTF8(name, maxFileNameBytes)
	}
	return stem + ext
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// sanitizeMediaType — тип файла из Content-Type части: type/subtype в нижнем
// регистре без параметров, иначе application/octet-stream. Тип приходит от
// клиента и попадает в заголовок ответа только в таком виде.
func sanitizeMediaType(raw string) string {
	mt, _, err := mime.ParseMediaType(raw)
	if err != nil && !errors.Is(err, mime.ErrInvalidMediaParameter) {
		return octetStream
	}
	major, minor, ok := strings.Cut(mt, "/")
	if !ok || !isToken(major) || !isToken(minor) {
		return octetStream
	}
	return mt
}

const octetStream = "application/octet-stream"

// isToken — token по RFC 7230: видимые ASCII без разделителей.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune(`()<>@,;:\"/[]?={}`, r) {
			return false
		}
	}
	return true
}

// ── отдача ──────────────────────────────────────────────────────────────────

// inlineTypes — что можно показать в браузере (решение 0064, п. 6); true —
// заявленный тип сверяется с сигнатурой содержимого. Всё остальное, в том
// числе text/html и image/svg+xml, отдаётся вложением для скачивания.
var inlineTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"application/pdf": true,
	"text/plain":      false, "text/markdown": false, "text/csv": false, "application/json": false,
}

// sniffBytes — сколько байт смотрит http.DetectContentType.
const sniffBytes = 512

// contentHeaders — заголовки ответа с содержимым: тип, disposition с
// санитизированным именем, nosniff и CSP sandbox. Ставятся на любой ответ
// содержимого, в том числе артефактов этапов, созданных до вложений: их тип
// приходил строкой в JSON и тоже от клиента.
func contentHeaders(h http.Header, art domain.Artifact, blobType string, head []byte) {
	mt := artifactType(art, blobType)
	name := ""
	if art.FileName != nil {
		name = sanitizeFileName(*art.FileName)
	} else {
		name = sanitizeFileName(art.Kind + extensionFor(mt))
	}
	h.Set("Content-Type", mt)
	h.Set("Content-Disposition", contentDisposition(dispositionFor(mt, head), name))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
}

// artifactType — тип отдачи: с артефакта, а у артефактов без файла — с
// блоба; в обоих случаях санитизированный.
func artifactType(art domain.Artifact, blobType string) string {
	if art.MediaType != nil {
		return sanitizeMediaType(*art.MediaType)
	}
	return sanitizeMediaType(blobType)
}

// needsSniff — нужна ли сигнатура содержимого, чтобы решить про inline.
func needsSniff(mt string) bool { return inlineTypes[mt] }

func dispositionFor(mt string, head []byte) string {
	sniff, ok := inlineTypes[mt]
	if !ok {
		return "attachment"
	}
	if sniff && http.DetectContentType(head) != mt {
		return "attachment"
	}
	return "inline"
}

// extensionFor — расширение имени по умолчанию у артефакта без имени файла.
func extensionFor(mt string) string {
	switch mt {
	case "text/markdown":
		return ".md"
	case "text/plain":
		return ".txt"
	case "text/csv":
		return ".csv"
	case "application/json":
		return ".json"
	}
	return ""
}

// contentDisposition — disposition с именем дважды: filename* по RFC 5987
// (UTF-8, кириллица) и ASCII-запасной filename для старых клиентов. Имя уже
// санитизировано; в заголовок попадают только экранированные байты.
func contentDisposition(disp, name string) string {
	return disp + `; filename="` + asciiFileName(name) + `"; filename*=UTF-8''` + rfc5987(name)
}

func asciiFileName(name string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r >= 0x7f || r == '"' || r == '\\' || r == '%' {
			return '_'
		}
		return r
	}, name)
}

// rfc5987 кодирует значение ext-value: attr-char как есть, остальное — %XX.
func rfc5987(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}
