package main

// Файловый слой CLI (CLAB-298, решение 0064): байты файлов идут через clab,
// а не через MCP. Здесь загрузка — clab attach и бинарная сдача clab submit
// --file; скачивание — в artifact.go.
//
// Правила слоя:
//   - Загрузка потоковая: multipart собирается поверх io.Pipe из файла на
//     диске, файл в памяти не держится.
//   - Размер файла сверяется с пределом из GET /v1/instance до отправки;
//     сервер без limits — старый, вложений не умеет, и это говорится сразу.
//   - Права и область считает сервер, CLI показывает его ошибку как пришла.
//   - Файловые вызовы идут клиентом без общего срока: файл в десятки МиБ на
//     медленном канале оборвался бы клиентом раньше срока сервера
//     (SDLC_UPLOAD_TIMEOUT, SDLC_DOWNLOAD_TIMEOUT).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// sniffBytes — сколько байт начала файла смотрит классификация: NUL или не
// UTF-8 в этих байтах делают файл бинарным.
const sniffBytes = 8 << 10

// errNoAttachments — сервер прежней версии: вложений и пределов в
// /v1/instance у него нет.
var errNoAttachments = errors.New("сервер не поддерживает вложения: обновите сервер до версии с вложениями (версия сервера — clab version)")

// binaryExtensions — расширения файлов, которые считаются бинарными при любом
// содержимом (скриншот, документ, архив, модель). Одна таблица: список
// «и подобные» из требований дополняется здесь.
var binaryExtensions = map[string]bool{
	// изображения
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true,
	".ico": true, ".tif": true, ".tiff": true, ".heic": true, ".avif": true, ".psd": true,
	// документы
	".pdf": true, ".docx": true, ".xlsx": true, ".pptx": true, ".odt": true, ".ods": true,
	// архивы
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true, ".zst": true,
	".7z": true, ".rar": true, ".tar": true,
	// модели и печать
	".glb": true, ".3mf": true, ".stl": true, ".fbx": true,
	// звук и видео
	".mp3": true, ".wav": true, ".ogg": true, ".flac": true, ".mp4": true, ".mov": true,
	".webm": true, ".mkv": true, ".avi": true,
	// шрифты и прочее
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".wasm": true,
	".bin": true, ".dat": true, ".sqlite": true, ".db": true, ".parquet": true,
}

// looksBinary — по началу содержимого: NUL или невалидный UTF-8. truncated —
// голова обрезана на sniffBytes, и последняя руна может быть разрезана.
func looksBinary(head []byte, truncated bool) bool {
	if bytes.IndexByte(head, 0) >= 0 {
		return true
	}
	if truncated {
		// Разрезанная посередине руна — не признак бинарного файла.
		for i := 0; i < utf8.UTFMax && len(head) > 0; i++ {
			if r, size := utf8.DecodeLastRune(head); r == utf8.RuneError && size <= 1 {
				head = head[:len(head)-1]
				continue
			}
			break
		}
	}
	return !utf8.Valid(head)
}

// readHead читает до sniffBytes байт начала потока; truncated — упёрлись в
// предел, значит, дальше может быть ещё.
func readHead(r io.Reader) (head []byte, truncated bool, err error) {
	head = make([]byte, sniffBytes)
	n, err := io.ReadFull(r, head)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return head[:n], n == sniffBytes, err
}

// isBinaryFile — бинарный ли файл: по расширению, а иначе по первым 8 КиБ.
// Не найденный файл — ошибка здесь же, до любой сети.
//
// Не обычный файл (канал, /dev/stdin, подстановка процесса) читать заранее
// нельзя: начало потока пропало бы для сдачи. Он считается текстом, как до
// вложений, а читается сдачей целиком.
func isBinaryFile(path string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, nil
	}
	if binaryExtensions[strings.ToLower(filepath.Ext(path))] {
		return true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head, truncated, err := readHead(f)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return looksBinary(head, truncated), nil
}

// regularFile — обычный файл: размер известен заранее, читать его можно
// потоком. Каталог, канал и устройство отбиваются до сети.
func regularFile(path string) (os.FileInfo, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s не обычный файл: приложить можно только файл с диска", path)
	}
	return st, nil
}

// readStdinText читает stdin целиком как текст. Бинарное начало — отказ с
// подсказкой: поток без имени и размера не отправить файлом, а в JSON-строку
// байты не помещаются без порчи.
func readStdinText(in io.Reader) ([]byte, error) {
	br := bufio.NewReaderSize(in, sniffBytes)
	head, err := br.Peek(sniffBytes)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, err
	}
	if looksBinary(head, len(head) == sniffBytes) {
		return nil, errors.New("stdin содержит бинарные данные: укажите путь к файлу (--file shot.png)")
	}
	return io.ReadAll(br)
}

// fileClient — клиент для файловых вызовов: тот же транспорт, но без общего
// срока клиента. Срок держит сервер.
func (c *Client) fileClient() *http.Client {
	hc := *c.http
	hc.Timeout = 0
	return &hc
}

// fileLimit — предел размера файла из GET /v1/instance; 0 — предела нет.
// Сервер без поля limits или без /v1/instance — прежней версии.
func (c *Client) fileLimit(ctx context.Context) (int64, error) {
	var info struct {
		Limits *struct {
			MaxFileBytes int64 `json:"max_file_bytes"`
		} `json:"limits"`
	}
	if err := c.get(ctx, "/instance", nil, &info); err != nil {
		if routeMissing(err) {
			return 0, errNoAttachments
		}
		return 0, err
	}
	if info.Limits == nil {
		return 0, errNoAttachments
	}
	return info.Limits.MaxFileBytes, nil
}

// routeMissing — ручки нет совсем, а не «нет такой задачи»: ошибка домена
// приходит конвертом с кодом, а отказ роутера старого сервера — без него.
func routeMissing(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == "" &&
		(apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusMethodNotAllowed)
}

// checkFileSize отказывает до отправки, если файл больше предела сервера.
func checkFileSize(path string, size, limit int64) error {
	if limit > 0 && size > limit {
		return fmt.Errorf("файл %s: %d байт, а сервер принимает не больше %d байт", path, size, limit)
	}
	return nil
}

// uploadField — поле формы. Поля идут до части file: сервер проверяет их до
// приёма файла и поле после файла отвергает.
type uploadField struct{ name, value string }

// uploadFile отправляет файл multipart-потоком. Тело строится поверх io.Pipe:
// из файла на диске в сеть, без чтения файла в память.
func (c *Client) uploadFile(ctx context.Context, path string, fields []uploadField, f *os.File, out any) error {
	if err := c.requireHost(); err != nil {
		return err
	}
	mediaType := fileMediaType(f)
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(writeMultipart(mw, fields, f, mediaType))
	}()
	// Сервер мог ответить, не дочитав тело (403, 400 по полю): закрываем
	// читающий конец, чтобы писатель не завис, и ждём его, пока файл открыт.
	defer func() {
		_ = pr.Close()
		<-done
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.authorize(req)
	resp, err := c.fileClient().Do(req)
	if err != nil {
		return err
	}
	if err := c.finish(resp, out); err != nil {
		if routeMissing(err) {
			return errNoAttachments
		}
		return err
	}
	return nil
}

func writeMultipart(mw *multipart.Writer, fields []uploadField, f *os.File, mediaType string) error {
	for _, field := range fields {
		if err := mw.WriteField(field.name, field.value); err != nil {
			return err
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, quoteFormValue(filepath.Base(f.Name()))))
	h.Set("Content-Type", mediaType)
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	return mw.Close()
}

// quoteFormValue экранирует значение в кавычках заголовка части. Управляющие
// символы убираются: перевод строки в имени дописал бы заголовок части.
func quoteFormValue(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// fileMediaType — тип файла для части file: по расширению, а иначе по
// сигнатуре начала. Сервер очищает и перепроверяет его сам.
func fileMediaType(f *os.File) string {
	if mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(f.Name()))); mt != "" {
		return mt
	}
	head := make([]byte, 512)
	n, _ := f.ReadAt(head, 0)
	return http.DetectContentType(head[:n])
}

// openForUpload — проверки до сети (файл, предел сервера) и открытый файл.
// Порядок из сценария: имя и размер файла, затем /v1/instance, и только
// после этого файл открывается.
func (c *Client) openForUpload(ctx context.Context, path string) (*os.File, error) {
	if path == "-" || path == "" {
		return nil, errors.New("укажите путь к файлу: байты со stdin не прикладываются")
	}
	st, err := regularFile(path)
	if err != nil {
		return nil, err
	}
	if err := c.requireHost(); err != nil {
		return nil, err
	}
	limit, err := c.fileLimit(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkFileSize(path, st.Size(), limit); err != nil {
		return nil, err
	}
	return os.Open(path)
}

// cmdAttach — clab attach <ключ> <файл> [--title] [--supersedes <id>]:
// вложение к задаче, не меняющее ни прогон, ни гейты. Флаги разбираются и
// после файла.
func cmdAttach(ctx context.Context, c *Client, args []string) error {
	var title, supersedes string
	fs, pos, err := flagsInterspersed("attach", args, func(fs *flag.FlagSet) {
		fs.StringVar(&title, "title", "", "заголовок; без него сервер ставит имя файла")
		fs.StringVar(&supersedes, "supersedes", "", "id вложения этой задачи, новая версия которого прикладывается")
	})
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		fs.Usage()
		return errors.New("нужны ключ задачи и путь к файлу: clab attach SDLC-42 shot.png")
	}
	task, path := pos[0], pos[1]

	f, err := c.openForUpload(ctx, path)
	if err != nil {
		return err
	}
	defer f.Close()

	var fields []uploadField
	if title != "" {
		fields = append(fields, uploadField{"title", title})
	}
	if supersedes != "" {
		fields = append(fields, uploadField{"supersedes_id", supersedes})
	}
	var out json.RawMessage
	if err := c.uploadFile(ctx, "/tasks/"+url.PathEscape(task)+"/attachments", fields, f, &out); err != nil {
		return err
	}
	return emit(out, render(renderAttached, out))
}

// submitBinary — бинарная сдача по контракту: multipart на POST …/artifacts,
// сервер ставит format: binary.
func submitBinary(ctx context.Context, c *Client, task, kind, title, path string) (json.RawMessage, error) {
	f, err := c.openForUpload(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fields := []uploadField{{"kind", kind}, {"title", title}}
	var out json.RawMessage
	if err := c.uploadFile(ctx, "/tasks/"+url.PathEscape(task)+"/artifacts", fields, f, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// renderAttached — что напечатал clab attach: id, имя, размер.
func renderAttached(raw json.RawMessage) error {
	var a struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		FileName string `json:"file_name"`
		Size     int64  `json:"size_bytes"`
		Version  int    `json:"version"`
	}
	if err := decode(raw, &a); err != nil {
		return err
	}
	fmt.Printf("Приложено %s  %s  %s  v%d\n", a.ID, printable(dash(a.FileName)), byteSize(a.Size), a.Version)
	return nil
}

// byteSize — «15 байт» или «3407872 байт (3.3M)»: точное число остаётся, а
// короткая форма добавляется, когда она короче.
func byteSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d байт", n)
	}
	return fmt.Sprintf("%d байт (%s)", n, humanSize(n))
}

// printable — строка, пришедшая от того, кто приложил файл, для вывода в
// терминал: управляющие символы и невидимые знаки направления текста
// убираются, длина ограничена.
func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	return trunc(s, 120)
}
