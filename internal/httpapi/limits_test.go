package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Предел тела запроса и сроки маршрутов с содержимым (CLAB-295, AC-2.6,
// AC-3, AC-4, AC-5).

type limitsFixture struct {
	t     *testing.T
	cfg   config.Config
	srv   *Server
	h     http.Handler
	store *postgres.Store
	pool  *pgxpool.Pool
	dir   string
	root  string
	admin workflow.Identity
}

const limitsRoot = "bootstrap-token-limits-0123456789"

func newLimitsFixture(t *testing.T, cfg config.Config) *limitsFixture {
	t.Helper()
	ctx := context.Background()
	store, pool := testdb.Store(t)
	dir := t.TempDir()
	fs, err := blobstore.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BlobInlineMaxBytes == 0 {
		cfg.BlobInlineMaxBytes = 256 << 10
	}
	store.UseBlobStore(fs, cfg.BlobInlineMaxBytes, cfg.BlobMaxBytes)
	cfg.Env = "test"
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	authoring := workflow.NewAuthoring(store, log, engine)
	mcpServer := mcp.NewServer(log, store, engine, authoring, auth, "test")
	srv := NewServer(cfg, log, store, engine, authoring, auth, mcpServer)
	if err := auth.Bootstrap(ctx, "root", limitsRoot); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, limitsRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &limitsFixture{t: t, cfg: cfg, srv: srv, h: srv.Handler(), store: store, pool: pool,
		dir: dir, root: limitsRoot, admin: admin}
}

// request — запрос с телом; chunked — без заявленной длины, как тело,
// которое клиент шлёт потоком.
func (f *limitsFixture) request(method, path string, body []byte, chunked bool) *http.Request {
	var r io.Reader = bytes.NewReader(body)
	if chunked {
		r = struct{ io.Reader }{r}
	}
	req := httptest.NewRequest(method, path, r)
	if chunked {
		req.ContentLength = -1
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.root)
	return req
}

func (f *limitsFixture) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *limitsFixture) blobs() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM blobs`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *limitsFixture) storedFiles() []string {
	f.t.Helper()
	var out []string
	_ = filepath.WalkDir(f.dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// submitBody — JSON-сдача текстового артефакта, как её шлёт clab.
func submitBody(t *testing.T, content string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"kind": "analysis.brief", "title": "крупный", "content": content})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func wantTooLarge(t *testing.T, rec *httptest.ResponseRecorder, limit int64) {
	t.Helper()
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("ждали 413, получили %d: %s", rec.Code, rec.Body)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело отказа не ошибка сервиса: %s", rec.Body)
	}
	if body.Error.Code != apperr.CodePayloadTooLarge || !strings.Contains(body.Error.Message, strconv.FormatInt(limit, 10)) {
		t.Fatalf("отказ: %+v", body.Error)
	}
}

// AC-3.1, AC-3.2, AC-3.3: тело больше предела — 413 с кодом и пределом на
// ручке группы v1, на открытой ручке вне группы и на MCP; и по заявленной
// длине, и без неё. После отказа нет ни строки blobs, ни файла, а журнал
// пишет отказ как обычный отклонённый запрос.
func TestBodyLimitRejects(t *testing.T) {
	f := newLimitsFixture(t, config.Config{BlobMaxBytes: 256 << 10})
	limit := f.srv.bodyLimit()
	if limit != 2*(256<<10)+1<<20 {
		t.Fatalf("предел тела %d", limit)
	}
	over := submitBody(t, strings.Repeat("a", int(limit)))

	for _, chunked := range []bool{false, true} {
		name := map[bool]string{false: "по Content-Length", true: "chunked"}[chunked]
		t.Run(name, func(t *testing.T) {
			blobsBefore := f.blobs()
			rec := f.serve(f.request(http.MethodPost, "/v1/tasks/SDLC-3/artifacts", over, chunked))
			wantTooLarge(t, rec, limit)
			if f.blobs() != blobsBefore || len(f.storedFiles()) != 0 {
				t.Fatalf("после отказа: строк blobs %d → %d, файлы %v", blobsBefore, f.blobs(), f.storedFiles())
			}
			var outcome string
			var status int
			if err := f.pool.QueryRow(context.Background(), `
				SELECT outcome, http_status FROM audit_log
				WHERE action = 'POST /v1/tasks/:taskKey/artifacts' ORDER BY created_at DESC LIMIT 1`).
				Scan(&outcome, &status); err != nil {
				t.Fatalf("отказа нет в журнале: %v", err)
			}
			if outcome != string(apperr.CodePayloadTooLarge) || status != http.StatusRequestEntityTooLarge {
				t.Fatalf("журнал: %s %d", outcome, status)
			}

			// Открытая ручка вне группы v1.
			wantTooLarge(t, f.serve(f.request(http.MethodPost, "/v1/auth/cli", over, chunked)), limit)
		})
	}

	// MCP: заявленная длина отбивается до SDK — ответом сервиса.
	req := f.request(http.MethodPost, "/mcp/projects/SDLC", over, false)
	req.Header.Set("Accept", "application/json, text/event-stream")
	wantTooLarge(t, f.serve(req), limit)

	// Без заявленной длины тело читает SDK через тот же предел и отвечает
	// 413 сам — статусом, а не ошибкой JSON-RPC.
	req = f.request(http.MethodPost, "/mcp/projects/SDLC", over, true)
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := f.serve(req)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), strconv.FormatInt(limit, 10)) {
		t.Fatalf("MCP chunked: %d %s", rec.Code, rec.Body)
	}
}

// AC-3.4: предел не ниже законной JSON-сдачи — содержимое ровно
// SDLC_BLOB_MAX_BYTES с переводами строк, кавычками и <, >, & (кодировщик
// Go пишет их как <) принимается; тело ровно в предел проходит
// проверку предела, на байт больше — 413.
func TestBodyLimitAdmitsLegalSubmission(t *testing.T) {
	const blobMax = 512 << 10
	f := newLimitsFixture(t, config.Config{BlobMaxBytes: blobMax})
	limit := f.srv.bodyLimit()

	var b strings.Builder
	pattern := "Строка разбора: \"кавычки\", <тег> & ссылка\n"
	for b.Len() < blobMax {
		b.WriteString(pattern)
	}
	content := b.String()[:blobMax]
	content = strings.ToValidUTF8(content, "x")
	for len(content) < blobMax {
		content += "x"
	}
	body := submitBody(t, content)
	if int64(len(body)) <= int64(blobMax) {
		t.Fatalf("тело %d байт не раздуто экранированием", len(body))
	}
	rec := f.serve(f.request(http.MethodPost, "/v1/tasks/SDLC-3/artifacts", body, false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("законная сдача %d байт (тело %d) отвергнута: %d %s", len(content), len(body), rec.Code, rec.Body)
	}
	var art struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &art)
	got := f.serve(f.request(http.MethodGet, "/v1/artifacts/"+art.ID+"/content", nil, false))
	if got.Body.String() != content {
		t.Fatal("прочитано не то, что сдано")
	}

	// Тело ровно в предел пределом не отбивается — его содержимое больше
	// SDLC_BLOB_MAX_BYTES, и отказ прежний, 400 из PutBlob; на байт больше —
	// 413. Длину добирает само содержимое: JSON-декодер дочитывает тело
	// только до конца значения, и хвост после него предел не проверил бы.
	overhead := len(submitBody(t, ""))
	exact := submitBody(t, strings.Repeat("a", int(limit)-overhead))
	over := submitBody(t, strings.Repeat("a", int(limit)-overhead+1))
	if int64(len(exact)) != limit || int64(len(over)) != limit+1 {
		t.Fatalf("тела %d и %d при пределе %d", len(exact), len(over), limit)
	}
	for _, chunked := range []bool{false, true} {
		if rec := f.serve(f.request(http.MethodPost, "/v1/tasks/SDLC-3/artifacts", exact, chunked)); rec.Code != http.StatusBadRequest {
			t.Fatalf("тело ровно в предел (chunked %v): %d %s", chunked, rec.Code, rec.Body)
		}
		wantTooLarge(t, f.serve(f.request(http.MethodPost, "/v1/tasks/SDLC-3/artifacts", over, chunked)), limit)
	}
}

// AC-4.2: содержимое больше SDLC_BLOB_MAX_BYTES в теле меньше общего
// предела — прежний 400 из PutBlob с прежним текстом.
func TestContentOverBlobMaxStillBadRequest(t *testing.T) {
	const blobMax = 64 << 10
	f := newLimitsFixture(t, config.Config{BlobMaxBytes: blobMax, BlobInlineMaxBytes: 1024})
	rec := f.serve(f.request(http.MethodPost, "/v1/tasks/SDLC-3/artifacts",
		submitBody(t, strings.Repeat("a", blobMax+1)), false))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "(SDLC_BLOB_MAX_BYTES)") {
		t.Fatalf("ждали прежний 400: %d %s", rec.Code, rec.Body)
	}
}

// AC-3.5, AC-3.6: более узкий предел навешивается на маршрут тем же
// механизмом; ошибка чтения тела любым способом — тот же 413.
func TestNarrowRouteLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{cfg: config.Config{BlobMaxBytes: 1 << 20}, log: slog.New(slog.DiscardHandler)}
	r := gin.New()
	r.Use(s.limitRequestBody())
	r.POST("/v1/narrow", s.limitBody(100), func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			badRequest(c, s.log, err)
			return
		}
		c.Status(http.StatusOK)
	})
	r.POST("/v1/bind", func(c *gin.Context) {
		var v any
		if err := c.ShouldBindJSON(&v); err != nil {
			fail(c, s.log, err)
			return
		}
		c.Status(http.StatusOK)
	})
	do := func(path string, n int, chunked bool) *httptest.ResponseRecorder {
		body := []byte(`"` + strings.Repeat("a", n-2) + `"`)
		var rd io.Reader = bytes.NewReader(body)
		if chunked {
			rd = struct{ io.Reader }{rd}
		}
		req := httptest.NewRequest(http.MethodPost, path, rd)
		if chunked {
			req.ContentLength = -1
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	for _, chunked := range []bool{false, true} {
		if rec := do("/v1/narrow", 100, chunked); rec.Code != http.StatusOK {
			t.Fatalf("узкий предел, ровно 100: %d", rec.Code)
		}
		// Между узким и общим пределом — 413 с узким пределом.
		wantTooLarge(t, do("/v1/narrow", 150, chunked), 100)
		limit := s.bodyLimit()
		if rec := do("/v1/bind", 150, chunked); rec.Code != http.StatusOK {
			t.Fatalf("общий предел, 150 байт: %d", rec.Code)
		}
		wantTooLarge(t, do("/v1/bind", int(limit)+1, chunked), limit)
	}
}

// ── сроки на настоящем сервере ──────────────────────────────────────────────

// liveServer — сервер с ReadTimeout и WriteTimeout, как в cmd/api: сроки
// соединения рекордер не умеет.
func (f *limitsFixture) liveServer() *httptest.Server {
	f.t.Helper()
	ts := httptest.NewUnstartedServer(f.h)
	ts.Config.ReadTimeout = f.cfg.RequestTimeout
	ts.Config.WriteTimeout = f.cfg.RequestTimeout
	ts.Start()
	f.t.Cleanup(ts.Close)
	return ts
}

// slowBody отдаёт body кусками за время about — медленный канал клиента.
func slowBody(body []byte, about time.Duration) io.Reader {
	const pieces = 10
	pr, pw := io.Pipe()
	go func() {
		step := (len(body) + pieces - 1) / pieces
		for i := 0; i < len(body); i += step {
			time.Sleep(about / pieces)
			if _, err := pw.Write(body[i:min(i+step, len(body))]); err != nil {
				return
			}
		}
		pw.Close()
	}()
	return pr
}

func (f *limitsFixture) slowPost(client *http.Client, url string, body []byte, about time.Duration) (int, error) {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, slowBody(body, about))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.root)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	return resp.StatusCode, err
}

const (
	shortRequest = 300 * time.Millisecond
	slowUpload   = 1200 * time.Millisecond
)

// AC-5.2, AC-5.3, AC-5.5, AC-5.6: загрузка дольше SDLC_REQUEST_TIMEOUT и
// короче SDLC_UPLOAD_TIMEOUT доходит до 201; дольше срока загрузки —
// обрыв без следов; обычная ручка с тем же телом по-прежнему отбивается
// по SDLC_REQUEST_TIMEOUT; ноль — без срока; срок маршрута не сокращает
// серверного.
func TestUploadDeadline(t *testing.T) {
	body := func(t *testing.T) []byte { return submitBody(t, "медленная сдача "+t.Name()) }

	t.Run("короче срока загрузки", func(t *testing.T) {
		f := newLimitsFixture(t, config.Config{BlobMaxBytes: 1 << 20, RequestTimeout: shortRequest, UploadTimeout: 10 * time.Second})
		ts := f.liveServer()
		code, err := f.slowPost(ts.Client(), ts.URL+"/v1/tasks/SDLC-3/artifacts", body(t), slowUpload)
		if err != nil || code != http.StatusCreated {
			t.Fatalf("медленная загрузка: %d %v", code, err)
		}
	})

	t.Run("дольше срока загрузки", func(t *testing.T) {
		f := newLimitsFixture(t, config.Config{BlobMaxBytes: 1 << 20, BlobInlineMaxBytes: 1024,
			RequestTimeout: shortRequest, UploadTimeout: 600 * time.Millisecond})
		ts := f.liveServer()
		blobsBefore := f.blobs()
		code, err := f.slowPost(ts.Client(), ts.URL+"/v1/tasks/SDLC-3/artifacts",
			submitBody(t, strings.Repeat("б", 64<<10)), 2*time.Second)
		if err == nil && code == http.StatusCreated {
			t.Fatal("загрузка дольше срока принята")
		}
		if f.blobs() != blobsBefore || len(f.storedFiles()) != 0 {
			t.Fatalf("после обрыва: строк blobs %d → %d, файлы %v", blobsBefore, f.blobs(), f.storedFiles())
		}
	})

	t.Run("обычная ручка по SDLC_REQUEST_TIMEOUT", func(t *testing.T) {
		f := newLimitsFixture(t, config.Config{BlobMaxBytes: 1 << 20, RequestTimeout: shortRequest, UploadTimeout: 10 * time.Second})
		ts := f.liveServer()
		bundle, _ := json.Marshal(map[string]any{"bundle": map[string]any{}, "dry_run": true})
		code, err := f.slowPost(ts.Client(), ts.URL+"/v1/projects/SDLC/import", bundle, slowUpload)
		if err == nil && code < 400 {
			t.Fatalf("медленный импорт прошёл мимо SDLC_REQUEST_TIMEOUT: %d", code)
		}
		// Тот же импорт быстро — проходит: отказ выше — от срока, а не от тела.
		code, err = f.slowPost(ts.Client(), ts.URL+"/v1/projects/SDLC/import", bundle, 0)
		if err != nil || code >= 500 {
			t.Fatalf("быстрый импорт: %d %v", code, err)
		}
	})

	t.Run("ноль — без срока, и не дальше своего запроса", func(t *testing.T) {
		f := newLimitsFixture(t, config.Config{BlobMaxBytes: 1 << 20, RequestTimeout: shortRequest})
		ts := f.liveServer()
		// Одно соединение на всё: следующий запрос идёт тем же keep-alive.
		client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1}}
		t.Cleanup(client.CloseIdleConnections)
		code, err := f.slowPost(client, ts.URL+"/v1/tasks/SDLC-3/artifacts", body(t), slowUpload)
		if err != nil || code != http.StatusCreated {
			t.Fatalf("загрузка без срока: %d %v", code, err)
		}

		var reused bool
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
		bundle, _ := json.Marshal(map[string]any{"bundle": map[string]any{}, "dry_run": true})
		req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace),
			http.MethodPost, ts.URL+"/v1/projects/SDLC/import", slowBody(bundle, slowUpload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.root)
		resp, err := client.Do(req)
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		if !reused {
			t.Fatal("второй запрос пришёл новым соединением — проверка ни о чём")
		}
		if err == nil && resp.StatusCode < 400 {
			t.Fatalf("срок загрузки достался следующему запросу соединения: %d", resp.StatusCode)
		}
	})

	t.Run("срок маршрута короче серверного не сокращает его", func(t *testing.T) {
		f := newLimitsFixture(t, config.Config{BlobMaxBytes: 1 << 20, RequestTimeout: 3 * time.Second, UploadTimeout: 100 * time.Millisecond})
		ts := f.liveServer()
		code, err := f.slowPost(ts.Client(), ts.URL+"/v1/tasks/SDLC-3/artifacts", body(t), 600*time.Millisecond)
		if err != nil || code != http.StatusCreated {
			t.Fatalf("загрузка в пределах серверного срока: %d %v", code, err)
		}
	})
}

// bigArtifact сдаёт артефакт напрямую в хранилище: быстрый путь, чтобы
// проверять только отдачу.
func (f *limitsFixture) bigArtifact(content []byte) string {
	f.t.Helper()
	ctx := context.Background()
	var id string
	err := f.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := f.store.GetTaskByKey(ctx, tx, "SDLC-3")
		if err != nil {
			return err
		}
		a, err := f.store.CreateArtifact(ctx, tx, postgres.CreateArtifactInput{
			Task: task, Kind: "analysis.brief", Title: "крупный", Content: content,
			MediaType: "application/octet-stream", ProducedBy: f.admin.Principal.ID,
		})
		id = a.ID.String()
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// slowGet читает ответ кусками с паузами: медленный канал клиента.
func (f *limitsFixture) slowGet(url string, about time.Duration, size int) ([]byte, int64, error) {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+f.root)
	client := &http.Client{Transport: &http.Transport{ReadBufferSize: 64 << 10,
		DisableCompression: true}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	const pieces = 20
	buf := make([]byte, size/pieces+1)
	var out bytes.Buffer
	for {
		time.Sleep(about / pieces)
		n, err := io.ReadFull(resp.Body, buf)
		out.Write(buf[:n])
		if err != nil {
			if (err == io.EOF || err == io.ErrUnexpectedEOF) && int64(out.Len()) == resp.ContentLength {
				return out.Bytes(), resp.ContentLength, nil
			}
			return out.Bytes(), resp.ContentLength, err
		}
	}
}

// AC-2.6, AC-5.4: отдача идёт потоком с Content-Length, равным size_bytes;
// медленный читатель дольше SDLC_REQUEST_TIMEOUT и короче
// SDLC_DOWNLOAD_TIMEOUT получает всё, дольше срока отдачи — обрыв.
// Содержимое крупнее буферов сокетов: иначе ядро приняло бы его целиком, и
// срок записи было бы нечем проверить.
func TestDownloadDeadline(t *testing.T) {
	const size = 48 << 20
	content := make([]byte, size)
	rand.Read(content)
	sum := sha256.Sum256(content)

	t.Run("короче срока отдачи", func(t *testing.T) {
		f := newLimitsFixture(t, config.Config{BlobMaxBytes: 64 << 20, RequestTimeout: shortRequest, DownloadTimeout: 30 * time.Second})
		id := f.bigArtifact(content)
		ts := f.liveServer()
		got, length, err := f.slowGet(ts.URL+"/v1/artifacts/"+id+"/content", 2*time.Second, size)
		if err != nil {
			t.Fatalf("медленное скачивание: %v (получено %d)", err, len(got))
		}
		gotSum := sha256.Sum256(got)
		if length != size || hex.EncodeToString(gotSum[:]) != hex.EncodeToString(sum[:]) {
			t.Fatalf("Content-Length %d, получено %d байт, sha совпал: %v", length, len(got), gotSum == sum)
		}
	})

	t.Run("дольше срока отдачи", func(t *testing.T) {
		f := newLimitsFixture(t, config.Config{BlobMaxBytes: 64 << 20, RequestTimeout: shortRequest, DownloadTimeout: 700 * time.Millisecond})
		id := f.bigArtifact(content)
		ts := f.liveServer()
		got, length, err := f.slowGet(ts.URL+"/v1/artifacts/"+id+"/content", 3*time.Second, size)
		if err == nil && len(got) == size {
			t.Fatal("скачивание дольше срока дошло целиком")
		}
		if length != size {
			t.Fatalf("Content-Length %d, ждали %d", length, size)
		}
	})

	t.Run("пропавший объект — 500 до заголовков", func(t *testing.T) {
		f := newLimitsFixture(t, config.Config{BlobMaxBytes: 64 << 20})
		id := f.bigArtifact(content[:1<<20])
		for _, p := range f.storedFiles() {
			os.Remove(p)
		}
		rec := f.serve(f.request(http.MethodGet, "/v1/artifacts/"+id+"/content", nil, false))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("пропавший объект: %d, %d байт тела", rec.Code, rec.Body.Len())
		}
	})
}
