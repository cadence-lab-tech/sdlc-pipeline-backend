package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// CLAB-298: clab attach, clab submit --file для бинарного, классификация
// файла и предел сервера. Сервер подменяет httptest.

// uploadServer запоминает запросы и отвечает на /v1/instance и на загрузку.
type uploadServer struct {
	mu sync.Mutex
	// limits: nil — сервер прежней версии (поля limits нет).
	limits *int64
	// instanceStatus — статус ответа на /v1/instance (0 — 200).
	instanceStatus int
	// postStatus и postReply — ответ на POST; по умолчанию 201 и объект артефакта.
	postStatus int
	postReply  string

	posts    []recordedUpload
	jsonBody map[string]any
	gets     int
}

type recordedUpload struct {
	path        string
	contentType string
	chunked     bool
	fields      []string // имена частей по порядку, включая file
	values      map[string]string
	fileName    string
	fileType    string
	fileSHA     string
	fileSize    int64
}

func i64(n int64) *int64 { return &n }

func newUploadClient(t *testing.T, s *uploadServer) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/instance":
			s.gets++
			if s.instanceStatus != 0 {
				w.WriteHeader(s.instanceStatus)
				_, _ = w.Write([]byte("404 page not found"))
				return
			}
			out := map[string]any{"version": "v0.18.0"}
			if s.limits != nil {
				out["limits"] = map[string]any{"max_file_bytes": *s.limits}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost:
			rec := s.record(t, r)
			if rec == nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_input","message":"тело не разобрано"}}`))
				return
			}
			status, reply := s.postStatus, s.postReply
			if status == 0 {
				status = http.StatusCreated
			}
			if reply == "" {
				reply = `{"id":"a1b2c3d4-0000-4000-8000-000000000001","kind":"test.shot","title":"Скриншот","version":1,"size_bytes":` +
					strings.TrimSpace(string(mustJSON(rec.fileSize))) + `,"file_name":"` + rec.fileName + `","is_attachment":true}`
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(reply))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: srv.URL, actor: "user:alice", http: srv.Client()}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// record разбирает тело POST: multipart — потоком, по частям, с порядком
// частей; JSON — в jsonBody.
func (s *uploadServer) record(t *testing.T, r *http.Request) *recordedUpload {
	rec := recordedUpload{path: r.URL.Path, contentType: r.Header.Get("Content-Type"),
		chunked: r.ContentLength < 0, values: map[string]string{}}
	mt, params, err := mime.ParseMediaType(rec.contentType)
	if err != nil || mt != "multipart/form-data" {
		raw, _ := io.ReadAll(r.Body)
		s.jsonBody = nil
		_ = json.Unmarshal(raw, &s.jsonBody)
		s.posts = append(s.posts, rec)
		return &rec
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Errorf("часть формы: %v", err)
			return nil
		}
		rec.fields = append(rec.fields, part.FormName())
		if part.FormName() == "file" {
			h := sha256.New()
			n, _ := io.Copy(h, part)
			rec.fileName, rec.fileType = part.FileName(), part.Header.Get("Content-Type")
			rec.fileSHA, rec.fileSize = hex.EncodeToString(h.Sum(nil)), n
			continue
		}
		val, _ := io.ReadAll(part)
		rec.values[part.FormName()] = string(val)
	}
	s.posts = append(s.posts, rec)
	return &rec
}

func putFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// AC-20.1: флаги разбираются и после файла; поля формы идут до части file.
func TestAttachFieldsBeforeFile(t *testing.T) {
	withJSON(t, false)
	s := &uploadServer{limits: i64(1 << 20)}
	c := newUploadClient(t, s)
	data := []byte("\x89PNG\r\n\x1a\n-------")
	file := putFile(t, t.TempDir(), "shot.png", data)

	out, _, err := capture(t, func() error {
		return cmdAttach(context.Background(), c, []string{"CLAB-1", file, "--title", "Скрин", "--supersedes", "5b3a2f1c-0000-4000-8000-000000000009"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.posts) != 1 {
		t.Fatalf("запросов POST: %d", len(s.posts))
	}
	p := s.posts[0]
	if p.path != "/v1/tasks/CLAB-1/attachments" {
		t.Errorf("путь %s", p.path)
	}
	if got := strings.Join(p.fields, ","); got != "title,supersedes_id,file" {
		t.Errorf("порядок частей: %s", got)
	}
	if p.values["title"] != "Скрин" || p.values["supersedes_id"] != "5b3a2f1c-0000-4000-8000-000000000009" {
		t.Errorf("поля: %v", p.values)
	}
	if p.fileName != "shot.png" || p.fileType != "image/png" || p.fileSHA != sha256Hex(data) || p.fileSize != int64(len(data)) {
		t.Errorf("файл: %+v", p)
	}
	for _, want := range []string{"a1b2c3d4-0000-4000-8000-000000000001", "shot.png", "15 байт"} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
}

// Без --title поля title нет: имя файла ставит сервер. Флаги перед файлом
// и «--» тоже работают.
func TestAttachWithoutTitleAndFlagOrder(t *testing.T) {
	withJSON(t, false)
	s := &uploadServer{limits: i64(1 << 20)}
	c := newUploadClient(t, s)
	file := putFile(t, t.TempDir(), "log.txt", []byte("строка\n"))

	for _, args := range [][]string{{"CLAB-1", file}, {"--", "CLAB-1", file}} {
		if _, _, err := capture(t, func() error { return cmdAttach(context.Background(), c, args) }); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range s.posts {
		if got := strings.Join(p.fields, ","); got != "file" {
			t.Errorf("части без --title: %s", got)
		}
	}
	_, _, err := capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-1"}) })
	if err == nil || !strings.Contains(err.Error(), "путь к файлу") {
		t.Errorf("без файла: %v", err)
	}
}

// --json — объект артефакта как пришёл.
func TestAttachJSON(t *testing.T) {
	withJSON(t, true)
	s := &uploadServer{limits: i64(1 << 20)}
	c := newUploadClient(t, s)
	file := putFile(t, t.TempDir(), "log.txt", []byte("x"))
	out, _, err := capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-1", file}) })
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["id"] != "a1b2c3d4-0000-4000-8000-000000000001" {
		t.Fatalf("вывод --json: %s %v", out, err)
	}
}

// AC-20.2: файл больше предела — отказ до отправки с обоими числами.
func TestAttachOverLimitSendsNothing(t *testing.T) {
	withJSON(t, false)
	s := &uploadServer{limits: i64(10)}
	c := newUploadClient(t, s)
	file := putFile(t, t.TempDir(), "big.bin", bytes.Repeat([]byte("a"), 25))
	_, _, err := capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-1", file}) })
	if err == nil || !strings.Contains(err.Error(), "25 байт") || !strings.Contains(err.Error(), "10 байт") {
		t.Fatalf("ошибка: %v", err)
	}
	if len(s.posts) != 0 {
		t.Fatalf("POST при превышении предела: %d", len(s.posts))
	}
	// Предел 0 — предела нет.
	s.limits = i64(0)
	if _, _, err := capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-1", file}) }); err != nil {
		t.Fatalf("предел 0: %v", err)
	}
}

// AC-20.4: сервер без limits или без ручки — понятный отказ вместо 404.
func TestAttachOldServer(t *testing.T) {
	withJSON(t, false)
	file := putFile(t, t.TempDir(), "log.txt", []byte("x"))

	noLimits := &uploadServer{}
	c := newUploadClient(t, noLimits)
	_, _, err := capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-1", file}) })
	if err == nil || !strings.Contains(err.Error(), "сервер не поддерживает вложения") {
		t.Fatalf("без limits: %v", err)
	}
	if len(noLimits.posts) != 0 {
		t.Fatalf("POST на старом сервере: %d", len(noLimits.posts))
	}

	noInstance := &uploadServer{instanceStatus: http.StatusNotFound}
	c = newUploadClient(t, noInstance)
	_, _, err = capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-1", file}) })
	if err == nil || !strings.Contains(err.Error(), "сервер не поддерживает вложения") {
		t.Fatalf("без /instance: %v", err)
	}

	// limits есть, а маршрута вложений нет: 404 роутера без конверта ошибки.
	noRoute := &uploadServer{limits: i64(100), postStatus: http.StatusNotFound, postReply: "404 page not found"}
	c = newUploadClient(t, noRoute)
	_, _, err = capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-1", file}) })
	if err == nil || !strings.Contains(err.Error(), "сервер не поддерживает вложения") {
		t.Fatalf("404 на POST: %v", err)
	}

	// 404 с конвертом — это «нет такой задачи», его текст показывается как пришёл.
	noTask := &uploadServer{limits: i64(100), postStatus: http.StatusNotFound,
		postReply: `{"error":{"code":"not_found","message":"задача CLAB-9 не найдена"}}`}
	c = newUploadClient(t, noTask)
	_, _, err = capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-9", file}) })
	if err == nil || !strings.Contains(err.Error(), "not_found: задача CLAB-9 не найдена") {
		t.Fatalf("404 домена: %v", err)
	}
}

// Файла нет, каталог, stdin — ошибка до сети.
func TestAttachBadFileNoNetwork(t *testing.T) {
	withJSON(t, false)
	s := &uploadServer{limits: i64(100)}
	c := newUploadClient(t, s)
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "нет.png"), dir, "-"} {
		if _, _, err := capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-1", path}) }); err == nil {
			t.Errorf("%s: ошибки нет", path)
		}
	}
	if s.gets != 0 || len(s.posts) != 0 {
		t.Fatalf("сеть при плохом файле: gets=%d posts=%d", s.gets, len(s.posts))
	}
}

// Ошибка сервера на загрузке показывается как пришла.
func TestAttachServerErrorAsIs(t *testing.T) {
	withJSON(t, false)
	s := &uploadServer{limits: i64(100), postStatus: http.StatusForbidden,
		postReply: `{"error":{"code":"forbidden","message":"задача вне круга записи токена"}}`}
	c := newUploadClient(t, s)
	file := putFile(t, t.TempDir(), "log.txt", []byte("x"))
	_, _, err := capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-7", file}) })
	if err == nil || err.Error() != "forbidden: задача вне круга записи токена" {
		t.Fatalf("ошибка: %v", err)
	}
}

// AC-20.3: загрузка потоковая — файл в десятки МиБ не прибавляет к
// выделенной памяти размер файла.
func TestAttachStreamsLargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("большой файл")
	}
	withJSON(t, false)
	const size = 48 << 20
	s := &uploadServer{limits: i64(1 << 30)}
	c := newUploadClient(t, s)
	path := filepath.Join(t.TempDir(), "big.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, _, err := capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-1", path}) }); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > size/4 {
		t.Errorf("загрузка выделила %d МиБ при файле %d МиБ", got>>20, size>>20)
	}
	if len(s.posts) != 1 || s.posts[0].fileSize != size {
		t.Fatalf("дошло: %+v", s.posts)
	}
	if !s.posts[0].chunked {
		t.Error("тело должно уходить потоком без длины")
	}
}

// AC-20.5: клиент файловых вызовов без срока, обычный — 60 с.
func TestFileClientHasNoTimeout(t *testing.T) {
	t.Setenv("CLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("SDLC_API_URL", "https://sdlc.example.com/v1")
	t.Setenv("SDLC_TOKEN", "")
	c, err := NewClient(globals{})
	if err != nil {
		t.Fatal(err)
	}
	if c.http.Timeout != 60*time.Second {
		t.Errorf("обычный клиент: %s", c.http.Timeout)
	}
	if got := c.fileClient().Timeout; got != 0 {
		t.Errorf("файловый клиент: %s", got)
	}
	if c.http.Timeout != 60*time.Second {
		t.Error("файловый клиент изменил обычный")
	}
}

// AC-21.3: классификация файла.
func TestIsBinaryFile(t *testing.T) {
	dir := t.TempDir()
	// Кириллица, разрезанная границей 8 КиБ посередине руны.
	cut := strings.Repeat("a", sniffBytes-1) + "я" + strings.Repeat("б", 10)
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"log.txt", []byte("строка UTF-8\n"), false},
		{"notes.md", []byte("# Заголовок\n"), false},
		{"data.json", []byte(`{"a":1}`), false},
		{"empty.txt", nil, false},
		{"cut.txt", []byte(cut), false},
		{"nul.txt", []byte("abc\x00def"), true},
		{"nul-late.txt", append(bytes.Repeat([]byte("a"), 100), 0), true},
		{"latin1.txt", []byte("caf\xe9 au lait"), true},
		{"shot.png", []byte("просто текст"), true},
		{"MODEL.GLB", []byte("text"), true},
		{"doc.pdf", []byte("text"), true},
		{"pack.3mf", []byte("text"), true},
		{"noext", []byte("\xff\xfe\xfd"), true},
		{"noext-text", []byte("hello"), false},
	}
	for _, tc := range cases {
		path := putFile(t, dir, tc.name, tc.data)
		got, err := isBinaryFile(path)
		if err != nil || got != tc.want {
			t.Errorf("%s: %v %v, нужно %v", tc.name, got, err, tc.want)
		}
	}
	if _, err := isBinaryFile(filepath.Join(dir, "нет.txt")); err == nil {
		t.Error("несуществующий файл не отвергнут")
	}
	if _, err := isBinaryFile(dir); err == nil {
		t.Error("каталог не отвергнут")
	}
}

// AC-21.1: бинарный файл — multipart с kind и title до file.
func TestSubmitBinaryMultipart(t *testing.T) {
	withJSON(t, false)
	s := &uploadServer{limits: i64(1 << 20)}
	c := newUploadClient(t, s)
	data := []byte("\x89PNG\r\n\x1a\nданные")
	file := putFile(t, t.TempDir(), "shot.png", data)

	out, _, err := capture(t, func() error {
		return cmdSubmit(context.Background(), c, []string{"CLAB-1", "--kind", "test.shot", "--title", "Скриншот", "--file", file})
	})
	if err != nil {
		t.Fatal(err)
	}
	p := s.posts[0]
	if p.path != "/v1/tasks/CLAB-1/artifacts" || strings.Join(p.fields, ",") != "kind,title,file" {
		t.Fatalf("запрос: %s %v", p.path, p.fields)
	}
	if p.values["kind"] != "test.shot" || p.values["title"] != "Скриншот" || p.fileSHA != sha256Hex(data) {
		t.Errorf("поля или файл: %+v", p)
	}
	if !strings.Contains(out, "Сдан test.shot v1") {
		t.Errorf("вывод: %s", out)
	}

	// Без --title заголовок — kind, как у текстовой сдачи.
	s.posts = nil
	if _, _, err := capture(t, func() error {
		return cmdSubmit(context.Background(), c, []string{"CLAB-1", "--kind", "test.shot", "--file", file})
	}); err != nil {
		t.Fatal(err)
	}
	if s.posts[0].values["title"] != "test.shot" {
		t.Errorf("заголовок по умолчанию: %v", s.posts[0].values)
	}
}

// AC-21.2, AC-20.6: текст уходит JSON-ом, как до Ч5, и /v1/instance не
// спрашивается.
func TestSubmitTextUnchanged(t *testing.T) {
	withJSON(t, false)
	dir := t.TempDir()
	cases := []struct {
		file, format string
	}{{"notes.md", "markdown"}, {"verdict.json", "json"}, {"log.txt", "text"}, {"c.yaml", "yaml"}, {"x.patch", "diff"}}
	for _, tc := range cases {
		s := &uploadServer{}
		c := newUploadClient(t, s)
		file := putFile(t, dir, tc.file, []byte("# текст\n"))
		if _, _, err := capture(t, func() error {
			return cmdSubmit(context.Background(), c, []string{"CLAB-1", "--kind", "dev.change_summary", "--title", "Разбор", "--file", file})
		}); err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		want := map[string]any{"kind": "dev.change_summary", "title": "Разбор", "format": tc.format, "content": "# текст\n"}
		if len(s.posts) != 1 || !jsonEqual(s.jsonBody, want) || s.gets != 0 {
			t.Errorf("%s: body=%v posts=%d gets=%d", tc.file, s.jsonBody, len(s.posts), s.gets)
		}
	}
	// --content строкой.
	s := &uploadServer{}
	c := newUploadClient(t, s)
	if _, _, err := capture(t, func() error {
		return cmdSubmit(context.Background(), c, []string{"CLAB-1", "--kind", "k", "--content", "привет"})
	}); err != nil {
		t.Fatal(err)
	}
	if s.jsonBody["content"] != "привет" || s.jsonBody["format"] != "markdown" || s.jsonBody["title"] != "k" {
		t.Errorf("--content: %v", s.jsonBody)
	}
}

func jsonEqual(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// AC-21.4: предел и старый сервер для бинарной сдачи — до отправки.
func TestSubmitBinaryLimitAndOldServer(t *testing.T) {
	withJSON(t, false)
	file := putFile(t, t.TempDir(), "shot.png", bytes.Repeat([]byte("a"), 50))
	args := []string{"CLAB-1", "--kind", "test.shot", "--file", file}

	over := &uploadServer{limits: i64(20)}
	_, _, err := capture(t, func() error { return cmdSubmit(context.Background(), newUploadClient(t, over), args) })
	if err == nil || !strings.Contains(err.Error(), "50 байт") || !strings.Contains(err.Error(), "20 байт") || len(over.posts) != 0 {
		t.Errorf("предел: %v posts=%d", err, len(over.posts))
	}

	old := &uploadServer{}
	_, _, err = capture(t, func() error { return cmdSubmit(context.Background(), newUploadClient(t, old), args) })
	if err == nil || !strings.Contains(err.Error(), "сервер не поддерживает вложения") || len(old.posts) != 0 {
		t.Errorf("старый сервер: %v posts=%d", err, len(old.posts))
	}
}

// AC-21.5: ошибки сервера на бинарной сдаче печатаются как пришли.
func TestSubmitBinaryServerErrors(t *testing.T) {
	withJSON(t, false)
	file := putFile(t, t.TempDir(), "shot.png", []byte("x"))
	for status, reply := range map[int]string{
		400: `{"error":{"code":"invalid_input","message":"kind \"x\" не из контракта; допустимы: test.report"}}`,
		409: `{"error":{"code":"conflict","message":"этап завершён"}}`,
	} {
		s := &uploadServer{limits: i64(100), postStatus: status, postReply: reply}
		c := newUploadClient(t, s)
		_, _, err := capture(t, func() error {
			return cmdSubmit(context.Background(), c, []string{"CLAB-1", "--kind", "x", "--file", file})
		})
		var apiErr *APIError
		if err == nil || !asAPIError(err, &apiErr) || apiErr.Status != status {
			t.Errorf("%d: %v", status, err)
		}
	}
}

func asAPIError(err error, target **APIError) bool {
	e, ok := err.(*APIError)
	if ok {
		*target = e
	}
	return ok
}

// Бинарный файл с чужим --format — отказ, а не молчаливая подмена.
func TestSubmitBinaryRejectsTextFormat(t *testing.T) {
	withJSON(t, false)
	s := &uploadServer{limits: i64(100)}
	c := newUploadClient(t, s)
	file := putFile(t, t.TempDir(), "shot.png", []byte("x"))
	_, _, err := capture(t, func() error {
		return cmdSubmit(context.Background(), c, []string{"CLAB-1", "--kind", "x", "--format", "markdown", "--file", file})
	})
	if err == nil || !strings.Contains(err.Error(), "format: binary") || len(s.posts) != 0 {
		t.Errorf("%v posts=%d", err, len(s.posts))
	}
	// Явный binary допустим.
	if _, _, err := capture(t, func() error {
		return cmdSubmit(context.Background(), c, []string{"CLAB-1", "--kind", "x", "--format", "binary", "--file", file})
	}); err != nil {
		t.Errorf("--format binary: %v", err)
	}
}

// AC-21.6: --artifact с бинарным файлом — отказ до первой отправки.
func TestTransitionArtifactBinaryRefused(t *testing.T) {
	withJSON(t, false)
	dir := t.TempDir()
	text := putFile(t, dir, "report.md", []byte("# отчёт"))
	bin := putFile(t, dir, "shot.png", []byte("x"))

	s := &uploadServer{}
	c := newUploadClient(t, s)
	_, _, err := capture(t, func() error {
		return cmdTransition(context.Background(), c, []string{"CLAB-1", "--to", "dev_done",
			"--artifact", "dev.report=" + text, "--artifact", "test.shot=" + bin})
	})
	if err == nil || !strings.Contains(err.Error(), "clab submit CLAB-1 --kind test.shot --file "+bin) {
		t.Fatalf("ошибка: %v", err)
	}
	if len(s.posts) != 0 {
		t.Fatalf("часть артефактов ушла до отказа: %d", len(s.posts))
	}

	// Текст — как раньше: сдача JSON-ом, затем переход.
	if _, _, err := capture(t, func() error {
		return cmdTransition(context.Background(), c, []string{"CLAB-1", "--to", "dev_done", "--artifact", "dev.report=" + text})
	}); err != nil {
		t.Fatal(err)
	}
	if len(s.posts) != 2 || s.jsonBody == nil {
		t.Fatalf("запросы текстового перехода: %d", len(s.posts))
	}
}

// AC-21.7: stdin — только текст.
func TestReadStdinText(t *testing.T) {
	got, err := readStdinText(strings.NewReader("текст из stdin\n"))
	if err != nil || string(got) != "текст из stdin\n" {
		t.Fatalf("текст: %q %v", got, err)
	}
	// Тело длиннее окна читается целиком.
	long := strings.Repeat("я", sniffBytes)
	if got, err := readStdinText(strings.NewReader(long)); err != nil || string(got) != long {
		t.Fatalf("длинный текст: %d %v", len(got), err)
	}
	if _, err := readStdinText(strings.NewReader("PNG\x00\x01\x02")); err == nil || !strings.Contains(err.Error(), "путь к файлу") {
		t.Fatalf("бинарный stdin: %v", err)
	}
	if got, err := readStdinText(strings.NewReader("")); err != nil || len(got) != 0 {
		t.Fatalf("пустой stdin: %q %v", got, err)
	}
}

// Разбор флагов после позиционных: порядок позиционных сохраняется.
func TestFlagsInterspersed(t *testing.T) {
	var title string
	fs, pos, err := flagsInterspersed("attach", []string{"A", "--title", "T", "B", "--supersedes", "S"}, func(f *flag.FlagSet) {
		f.StringVar(&title, "title", "", "")
		f.String("supersedes", "", "")
	})
	if err != nil || strings.Join(pos, ",") != "A,B" || title != "T" || fs.Lookup("supersedes").Value.String() != "S" {
		t.Fatalf("pos=%v title=%q err=%v", pos, title, err)
	}
	_, pos, err = flagsInterspersed("attach", []string{"A", "--", "--title", "B"}, func(f *flag.FlagSet) { f.String("title", "", "") })
	if err != nil || strings.Join(pos, ",") != "A,--title,B" {
		t.Fatalf("после --: %v %v", pos, err)
	}
}
