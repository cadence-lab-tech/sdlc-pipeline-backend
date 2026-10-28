package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Вложения и файлы по контракту в REST (CLAB-296, решение 0064): AC-6–AC-13,
// AC-15, AC-16, AC-33–AC-35, AC-37, AC-38, AC-41.

// Сигнатуры форматов — первые байты, по которым их узнаёт
// http.DetectContentType.
var (
	pngBytes  = append([]byte("\x89PNG\x0D\x0A\x1A\x0A"), bytes.Repeat([]byte{1}, 64)...)
	jpegBytes = append([]byte("\xFF\xD8\xFF\xE0"), bytes.Repeat([]byte{2}, 64)...)
	gifBytes  = append([]byte("GIF89a"), bytes.Repeat([]byte{3}, 64)...)
	webpBytes = append([]byte("RIFF\x00\x00\x00\x00WEBPVP"), bytes.Repeat([]byte{4}, 64)...)
	pdfBytes  = append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{'x'}, 64)...)
	zipBytes  = append([]byte("PK\x03\x04"), bytes.Repeat([]byte{5}, 64)...)
)

type formPart struct {
	name, value string
	file        bool
	fname       string
	ctype       string
	data        []byte
}

func field(name, value string) formPart { return formPart{name: name, value: value} }

func filePart(fname, ctype string, data []byte) formPart {
	return formPart{name: "file", file: true, fname: fname, ctype: ctype, data: data}
}

// multipartBody собирает форму. Имя файла идёт через filename* — так в
// него проходят кавычки, CR/LF и управляющие символы, которые клиент вправе
// прислать.
func multipartBody(t *testing.T, parts ...formPart) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		if p.file {
			h.Set("Content-Disposition", `form-data; name="`+p.name+`"; filename*=UTF-8''`+rfc5987(p.fname))
			if p.ctype != "" {
				h.Set("Content-Type", p.ctype)
			}
		} else {
			h.Set("Content-Disposition", `form-data; name="`+p.name+`"`)
		}
		pw, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if p.file {
			pw.Write(p.data)
		} else {
			pw.Write([]byte(p.value))
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

func (f *limitsFixture) uploadAs(token, path string, parts ...formPart) *httptest.ResponseRecorder {
	f.t.Helper()
	body, ctype := multipartBody(f.t, parts...)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+token)
	return f.serve(req)
}

func (f *limitsFixture) upload(path string, parts ...formPart) *httptest.ResponseRecorder {
	return f.uploadAs(f.root, path, parts...)
}

func (f *limitsFixture) getAs(token, path string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return f.serve(req)
}

func (f *limitsFixture) jsonAs(token, method, path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return f.serve(req)
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("ответ не разбирается: %v: %s", err, rec.Body)
	}
	return v
}

func want(t *testing.T, rec *httptest.ResponseRecorder, code int, what string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("%s: ждали %d, получили %d: %s", what, code, rec.Code, rec.Body)
	}
}

func (f *limitsFixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *limitsFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

// moveTo ставит задаче статус по ключу без цикла: вложению статус не важен,
// и проверка не должна зависеть от гейтов.
func (f *limitsFixture) moveTo(key, status string) {
	f.exec(`UPDATE tasks SET status_id = (SELECT id FROM workflow_statuses
	        WHERE workflow_id = tasks.workflow_id AND key = $2) WHERE key = $1`, key, status)
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func newAttachFixture(t *testing.T) *limitsFixture {
	return newLimitsFixture(t, config.Config{BlobMaxBytes: 8 << 20, BlobInlineMaxBytes: 256 << 10})
}

// AC-6, AC-8, AC-12, AC-37: вложение к задаче любого типа и статуса, inline
// и во внешнем хранилище, версии, дедупликация блоба, выборки.
func TestAttachmentLifecycle(t *testing.T) {
	f := newAttachFixture(t)
	// Типы — данные проекта: вложению их значение безразлично.
	f.exec(`UPDATE tasks SET properties = properties || '{"type":"epic"}' WHERE key = 'SDLC-2'`)
	f.exec(`UPDATE tasks SET properties = properties || '{"type":"release"}' WHERE key = 'SDLC-4'`)
	f.exec(`UPDATE tasks SET properties = properties || '{"type":"bugfix"}' WHERE key = 'SDLC-3'`)
	f.moveTo("SDLC-4", "done")
	f.moveTo("SDLC-3", "cancelled")

	for _, key := range []string{"SDLC-1", "SDLC-2", "SDLC-3", "SDLC-4"} {
		rec := f.upload("/v1/tasks/"+key+"/attachments", field("title", "скрин "+key),
			filePart("shot.png", "image/png", pngBytes))
		want(t, rec, http.StatusCreated, "вложение к "+key)
		a := decode[domain.Artifact](t, rec)
		if !a.IsAttachment || a.Version != 1 || a.FileName == nil || *a.FileName != "shot.png" ||
			a.MediaType == nil || *a.MediaType != "image/png" || a.SizeBytes != int64(len(pngBytes)) ||
			a.BlobSHA != sha(pngBytes) || a.StageRunID != nil || a.Status != domain.ArtifactSubmitted {
			t.Fatalf("вложение %s: %+v", key, a)
		}
	}

	// Крупнее порога inline — во внешнее хранилище; оба скачиваются с тем же sha.
	big := make([]byte, 300<<10)
	rand.Read(big)
	rec := f.upload("/v1/tasks/SDLC-1/attachments", filePart("dump.bin", "application/octet-stream", big))
	want(t, rec, http.StatusCreated, "крупное вложение")
	bigArt := decode[domain.Artifact](t, rec)
	var storage string
	if err := f.pool.QueryRow(context.Background(), `SELECT storage FROM blobs WHERE sha256 = $1`, bigArt.BlobSHA).Scan(&storage); err != nil || storage != "external" {
		t.Fatalf("крупное вложение не вынесено: %q %v", storage, err)
	}
	got := f.getAs(f.root, "/v1/artifacts/"+bigArt.ID.String()+"/content")
	if got.Code != http.StatusOK || sha(got.Body.Bytes()) != sha(big) {
		t.Fatalf("скачивание крупного: %d", got.Code)
	}

	// AC-12: те же байты под другим именем и типом — блоб один, артефакта два.
	blobs := f.blobs()
	rec = f.upload("/v1/tasks/SDLC-1/attachments", filePart("notes.txt", "text/plain", pngBytes))
	want(t, rec, http.StatusCreated, "те же байты")
	twin := decode[domain.Artifact](t, rec)
	if f.blobs() != blobs {
		t.Fatalf("те же байты завели второй блоб")
	}
	got = f.getAs(f.root, "/v1/artifacts/"+twin.ID.String()+"/content")
	if ct := got.Header().Get("Content-Type"); ct != "text/plain" || !strings.Contains(got.Header().Get("Content-Disposition"), "notes.txt") {
		t.Fatalf("отдача двойника: %s / %s", ct, got.Header().Get("Content-Disposition"))
	}

	// AC-8: два вложения одного типа — оба актуальны; замена — версия 2.
	list := decode[struct {
		Attachments []workflow.AttachmentRef `json:"attachments"`
	}](t, f.getAs(f.root, "/v1/tasks/SDLC-1/attachments")).Attachments
	if len(list) != 3 {
		t.Fatalf("актуальных вложений SDLC-1: %d", len(list))
	}
	first := list[0]
	rec = f.upload("/v1/tasks/SDLC-1/attachments", field("supersedes_id", first.ID.String()),
		filePart("shot-v2.png", "image/png", append(pngBytes, 9)))
	want(t, rec, http.StatusCreated, "новая версия")
	v2 := decode[domain.Artifact](t, rec)
	if v2.Version != 2 || v2.SupersedesID == nil || *v2.SupersedesID != first.ID {
		t.Fatalf("вторая версия: %+v", v2)
	}

	// Замена артефакта этапа и вложения чужой задачи — 400, ничего не создано.
	jsonArt := f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-1/artifacts",
		map[string]any{"kind": "analysis.brief", "title": "разбор", "content": "# разбор"})
	want(t, jsonArt, http.StatusCreated, "сдача строкой")
	stageArt := decode[domain.Artifact](t, jsonArt)
	otherList := decode[struct {
		Attachments []workflow.AttachmentRef `json:"attachments"`
	}](t, f.getAs(f.root, "/v1/tasks/SDLC-2/attachments")).Attachments
	before := [3]int{f.count(`SELECT count(*) FROM artifacts`), f.count(`SELECT count(*) FROM task_events`), f.blobs()}
	for _, target := range []uuid.UUID{stageArt.ID, otherList[0].ID} {
		rec = f.upload("/v1/tasks/SDLC-1/attachments", field("supersedes_id", target.String()),
			filePart("new.bin", "application/octet-stream", []byte("новые байты "+target.String())))
		want(t, rec, http.StatusBadRequest, "замена не своего вложения")
	}
	after := [3]int{f.count(`SELECT count(*) FROM artifacts`), f.count(`SELECT count(*) FROM task_events`), f.blobs()}
	if before != after || len(f.spools()) != 0 {
		t.Fatalf("отказ что-то оставил: %v → %v, спулы %v", before, after, f.spools())
	}

	// AC-35: замена перекрытой версии — 409; две замены одной версии — одна
	// 201, вторая 409.
	rec = f.upload("/v1/tasks/SDLC-1/attachments", field("supersedes_id", first.ID.String()),
		filePart("late.png", "image/png", pngBytes))
	want(t, rec, http.StatusConflict, "замена перекрытой версии")
	var codes []int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.upload("/v1/tasks/SDLC-1/attachments", field("supersedes_id", v2.ID.String()),
				filePart("race.png", "image/png", append(pngBytes, byte(10+i))))
			mu.Lock()
			codes = append(codes, r.Code)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if !(codes[0] == http.StatusCreated && codes[1] == http.StatusConflict ||
		codes[1] == http.StatusCreated && codes[0] == http.StatusConflict) {
		t.Fatalf("гонка замен: %v", codes)
	}
	if n := f.count(`SELECT count(*) FROM artifacts WHERE supersedes_id = $1`, v2.ID); n != 1 {
		t.Fatalf("у версии 2 наследников %d", n)
	}
	// AC-35: вложение через сдачу по контракту не заменяется.
	rec = f.upload("/v1/tasks/SDLC-1/artifacts", field("kind", "analysis.brief"), field("title", "файл"),
		field("supersedes_id", twin.ID.String()), filePart("f.md", "text/markdown", []byte("# файл")))
	want(t, rec, http.StatusBadRequest, "замена вложения файлом по контракту")

	// AC-37: выборки.
	arts := decode[struct {
		Artifacts []domain.Artifact `json:"artifacts"`
	}](t, f.getAs(f.root, "/v1/tasks/SDLC-1/artifacts?include_superseded=true")).Artifacts
	for _, a := range arts {
		if a.IsAttachment {
			t.Fatalf("вложение в списке артефактов: %+v", a)
		}
	}
	card := decode[map[string]json.RawMessage](t, f.getAs(f.root, "/v1/tasks/SDLC-1"))
	if bytes.Contains(card["artifacts"], []byte(`"is_attachment":true`)) {
		t.Fatalf("вложение в artifacts карточки: %s", card["artifacts"])
	}
	var cardAtt []workflow.AttachmentRef
	if err := json.Unmarshal(card["attachments"], &cardAtt); err != nil || len(cardAtt) != 3 {
		t.Fatalf("вложения карточки: %s", card["attachments"])
	}
	all := decode[struct {
		Attachments []workflow.AttachmentRef `json:"attachments"`
	}](t, f.getAs(f.root, "/v1/tasks/SDLC-1/attachments?include_superseded=true")).Attachments
	if len(all) != 5 {
		t.Fatalf("история вложений: %d", len(all))
	}
	for _, a := range all {
		if a.ContentPath != "/v1/artifacts/"+a.ID.String()+"/content" || a.AddedBy == "" || a.SHA256 == "" {
			t.Fatalf("метаданные вложения: %+v", a)
		}
	}
	if rec := f.getAs(f.root, "/v1/projects/SDLC"); rec.Code != http.StatusOK {
		t.Fatalf("проект со вложениями: %d %s", rec.Code, rec.Body)
	}
	if rec := f.getAs(f.root, "/v1/projects/SDLC/metrics"); rec.Code != http.StatusOK {
		t.Fatalf("метрики со вложениями: %d %s", rec.Code, rec.Body)
	}
}

// spools — файлы спула, оставшиеся в каталоге хранилища.
func (f *limitsFixture) spools() []string {
	var out []string
	for _, p := range f.storedFiles() {
		if strings.HasPrefix(filepath.Base(p), ".spool-") {
			out = append(out, p)
		}
	}
	return out
}

// AC-7, AC-15, AC-37: вложение не трогает прогон, гейты и вход этапа даже
// при kind, совпавшем с техническим kind вложения.
func TestAttachmentDoesNotMoveCycle(t *testing.T) {
	f := newAttachFixture(t)
	want(t, f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-1/transitions/start", nil), http.StatusOK, "start")
	// Роль объявляет kind attachment, гейты требуют его на задаче и этапе и
	// проверяют схемой.
	f.exec(`UPDATE role_versions SET output_contract = output_contract ||
	        '[{"kind":"attachment","title":"Вложение роли","format":"markdown"}]'::jsonb
	        WHERE role_id = (SELECT id FROM roles WHERE key = 'analyst')`)
	for i, cfg := range []string{
		`{"kinds":["attachment"],"scope":"task"}`,
		`{"kinds":["attachment"],"scope":"stage"}`,
	} {
		f.exec(`INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
		        SELECT tr.id, $1, 'вложение', 'artifacts_required', $2, true, $3
		        FROM workflow_transitions tr JOIN tasks t ON t.workflow_id = tr.workflow_id
		        WHERE t.key = 'SDLC-1' AND tr.key = 'analysis_done'`, "att_"+strconv.Itoa(i), cfg, 20+i)
	}
	f.exec(`INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
	        SELECT tr.id, 'att_schema', 'схема', 'schema_validation',
	               '{"artifact_kind":"attachment","scope":"task","schema":{"type":"object"}}', true, 30
	        FROM workflow_transitions tr JOIN tasks t ON t.workflow_id = tr.workflow_id
	        WHERE t.key = 'SDLC-1' AND tr.key = 'analysis_done'`)

	runs := func() string {
		var s string
		if err := f.pool.QueryRow(context.Background(), `
			SELECT string_agg(id::text || ':' || state || ':' || coalesce(finished_at::text, '-'), ',' ORDER BY entered_at)
			FROM stage_runs WHERE task_id = (SELECT id FROM tasks WHERE key = 'SDLC-1')`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	transitions := func() string {
		rec := f.getAs(f.root, "/v1/tasks/SDLC-1/transitions")
		want(t, rec, http.StatusOK, "переходы")
		return rec.Body.String()
	}
	runsBefore, trBefore := runs(), transitions()

	rec := f.upload("/v1/tasks/SDLC-1/attachments", filePart("brief.md", "text/markdown", []byte("{}")))
	want(t, rec, http.StatusCreated, "вложение")
	att := decode[domain.Artifact](t, rec)
	if runs() != runsBefore {
		t.Fatalf("прогоны изменились: %s → %s", runsBefore, runs())
	}
	if tr := transitions(); tr != trBefore {
		t.Fatalf("гейты изменились:\n%s\n%s", trBefore, tr)
	}
	var stageRun *uuid.UUID
	var payload map[string]any
	if err := f.pool.QueryRow(context.Background(), `
		SELECT stage_run_id, payload FROM task_events WHERE type = 'attachment.added'
		AND payload->>'artifact_id' = $1`, att.ID.String()).Scan(&stageRun, &payload); err != nil {
		t.Fatalf("события attachment.added нет: %v", err)
	}
	if stageRun != nil || payload["file_name"] != "brief.md" || payload["version"] != float64(1) || payload["size_bytes"] != float64(2) {
		t.Fatalf("событие: прогон %v, %v", stageRun, payload)
	}

	// Сдача kind attachment от роли — отдельный артефакт этапа: вложение не
	// перекрыто, и наоборот.
	rec = f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-1/artifacts",
		map[string]any{"kind": "attachment", "title": "роль", "content": "# роль"})
	want(t, rec, http.StatusCreated, "сдача kind attachment")
	roleArt := decode[domain.Artifact](t, rec)
	if roleArt.SupersedesID != nil || roleArt.IsAttachment {
		t.Fatalf("сдача роли заменила вложение: %+v", roleArt)
	}
	ctx := context.Background()
	task, _ := f.store.GetTaskByKey(ctx, f.store.DB(), "SDLC-1")
	latest, err := f.store.LatestArtifactOfKind(ctx, f.store.DB(), task.ID, "attachment")
	if err != nil || latest == nil || *latest != roleArt.ID {
		t.Fatalf("LatestArtifactOfKind: %v %v", latest, err)
	}
	if n := f.count(`SELECT count(*) FROM artifacts WHERE id = $1 AND status = 'submitted'`, att.ID); n != 1 {
		t.Fatal("вложение перекрыто сдачей роли")
	}

	// AC-15: ответ state несёт вложения под boundary, prior_artifacts — нет,
	// и при пустом input_kinds, и при непустом.
	for _, inputs := range []string{"{}", "{attachment,analysis.brief}"} {
		f.exec(`UPDATE role_versions SET input_kinds = $1::text[]
		        WHERE role_id = (SELECT id FROM roles WHERE key = 'analyst')`, inputs)
		rec = f.getAs(f.root, "/v1/tasks/SDLC-1/state")
		want(t, rec, http.StatusOK, "state")
		var state workflow.ClaimResult
		if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		for _, p := range state.Context.PriorArtifacts {
			if p.ID == att.ID {
				t.Fatalf("вложение в prior_artifacts (input_kinds %s)", inputs)
			}
		}
		if len(state.Context.Attachments) != 1 || state.Context.Attachments[0].ID != att.ID ||
			state.Context.Attachments[0].ContentPath != workflow.ContentPath(att.ID) {
			t.Fatalf("вложения в state: %+v", state.Context.Attachments)
		}
		if !strings.Contains(strings.Join(state.Boundary.Data, ","), "context.attachments") {
			t.Fatalf("boundary.data без вложений: %v", state.Boundary.Data)
		}
	}
}

// AC-9, AC-35: файл по контракту.
func TestContractFile(t *testing.T) {
	f := newAttachFixture(t)
	want(t, f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-1/transitions/start", nil), http.StatusOK, "start")
	post := func(kind string, data []byte) *httptest.ResponseRecorder {
		return f.upload("/v1/tasks/SDLC-1/artifacts", field("kind", kind), field("title", "скрин"),
			filePart("скрин.png", "image/png", data))
	}
	rec := post("analysis.brief", pngBytes)
	want(t, rec, http.StatusCreated, "файл по контракту")
	a := decode[domain.Artifact](t, rec)
	if a.Format != "binary" || a.IsAttachment || a.StageRunID == nil || a.FileName == nil || *a.FileName != "скрин.png" {
		t.Fatalf("файл по контракту: %+v", a)
	}
	if n := f.count(`SELECT count(*) FROM stage_runs WHERE id = $1 AND state = 'submitted'`, *a.StageRunID); n != 1 {
		t.Fatal("прогон не в submitted")
	}
	rec = post("analysis.brief", append(pngBytes, 1))
	want(t, rec, http.StatusCreated, "повторная сдача")
	if v := decode[domain.Artifact](t, rec); v.Version != 2 || v.SupersedesID == nil || *v.SupersedesID != a.ID {
		t.Fatalf("повторная сдача не новая версия: %+v", v)
	}

	// Отказы до чтения файла: схема, вне контракта, этап завершён.
	f.exec(`UPDATE role_versions SET output_contract = output_contract ||
	        '[{"kind":"analysis.verdict","title":"Вердикт","format":"json","schema":{"type":"object"}}]'::jsonb
	        WHERE role_id = (SELECT id FROM roles WHERE key = 'analyst')`)
	cases := []struct {
		kind  string
		code  int
		check func(map[string]any) bool
	}{
		{"analysis.verdict", http.StatusBadRequest, func(map[string]any) bool { return true }},
		{"dev.plan", http.StatusBadRequest, func(b map[string]any) bool {
			d, _ := b["error"].(map[string]any)["details"].(map[string]any)
			return d["allowed_kinds"] != nil
		}},
	}
	for _, c := range cases {
		read, code, body := f.uploadCounting("/v1/tasks/SDLC-1/artifacts", f.root, c.kind)
		if code != c.code || !c.check(body) || read > 1<<20 {
			t.Fatalf("%s: %d %v, прочитано до ответа %d байт", c.kind, code, body, read)
		}
	}
	f.exec(`UPDATE stage_runs SET finished_at = now()
	        WHERE task_id = (SELECT id FROM tasks WHERE key = 'SDLC-1') AND finished_at IS NULL`)
	if read, code, body := f.uploadCounting("/v1/tasks/SDLC-1/artifacts", f.root, "analysis.brief"); code != http.StatusConflict || read > 1<<20 {
		t.Fatalf("закрытый этап: %d %v, прочитано %d", code, body, read)
	}

	// viewer файл по контракту не сдаёт.
	viewer := f.member("user:vera", domain.MemberViewer, nil)
	rec = f.uploadAs(viewer, "/v1/tasks/SDLC-3/artifacts", field("kind", "analysis.brief"),
		field("title", "x"), filePart("x.md", "text/markdown", []byte("x")))
	want(t, rec, http.StatusForbidden, "viewer по контракту")
}

// countingWriter запоминает, сколько тела запроса было прочитано к моменту,
// когда ручка начала отвечать: журнал дочитывает тело уже после ответа.
type countingWriter struct {
	http.ResponseWriter
	read *int64
	at   int64
	once sync.Once
}

func (w *countingWriter) WriteHeader(code int) {
	w.once.Do(func() { w.at = *w.read })
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { w.at = *w.read })
	return w.ResponseWriter.Write(b)
}

type countingReader struct {
	r io.Reader
	n *int64
}

func (r countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	*r.n += int64(n)
	return n, err
}

// uploadCounting шлёт файл по контракту с файлом в 4 МиБ и отвечает, сколько
// тела ручка прочла до ответа.
func (f *limitsFixture) uploadCounting(path, token, kind string) (int64, int, map[string]any) {
	f.t.Helper()
	big := bytes.Repeat([]byte("a"), 4<<20)
	body, ctype := multipartBody(f.t, field("kind", kind), field("title", "t"), filePart("big.md", "text/markdown", big))
	var n int64
	req := httptest.NewRequest(http.MethodPost, path, countingReader{r: bytes.NewReader(body), n: &n})
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	w := &countingWriter{ResponseWriter: rec, read: &n}
	f.h.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return w.at, rec.Code, out
}

// member заводит участника проекта и выдаёт ему личный токен на проект.
func (f *limitsFixture) member(handle string, level domain.MemberRole, roleKeys []string) string {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.store.ResolveActor(ctx, f.store.DB(), strings.TrimPrefix(handle, "user:"), domain.PrincipalUser); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.srv.auth.SetMember(ctx, f.admin, "SDLC", handle, level, nil); err != nil {
		f.t.Fatal(err)
	}
	issued, err := f.srv.auth.IssueToken(ctx, f.admin, workflow.IssueTokenInput{
		Name: "t-" + handle + strconv.Itoa(len(roleKeys)), Principal: handle, ProjectKey: "SDLC", RoleKeys: roleKeys,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return issued.Token
}

// AC-10, AC-11: заголовки отдачи.
func TestContentHeaders(t *testing.T) {
	f := newAttachFixture(t)
	cases := []struct {
		name, ctype string
		data        []byte
		disp        string
	}{
		{"a.png", "image/png", pngBytes, "inline"},
		{"a.jpg", "image/jpeg", jpegBytes, "inline"},
		{"a.gif", "image/gif", gifBytes, "inline"},
		{"a.webp", "image/webp", webpBytes, "inline"},
		{"a.pdf", "application/pdf", pdfBytes, "inline"},
		{"a.txt", "text/plain", []byte("текст"), "inline"},
		{"a.md", "text/markdown", []byte("# md"), "inline"},
		{"a.csv", "text/csv", []byte("a,b"), "inline"},
		{"a.json", "application/json", []byte(`{"a":1}`), "inline"},
		{"a.html", "text/html", []byte("<script>alert(1)</script>"), "attachment"},
		{"a.svg", "image/svg+xml", []byte(`<svg onload="alert(1)"/>`), "attachment"},
		{"a.bin", "application/octet-stream", []byte{0, 1, 2}, "attachment"},
		{"a.zip", "application/zip", zipBytes, "attachment"},
		{"fake.png", "image/png", jpegBytes, "attachment"},
		{"Отчёт тестирования.pdf", "application/pdf", pdfBytes, "inline"},
	}
	for _, c := range cases {
		rec := f.upload("/v1/tasks/SDLC-1/attachments", filePart(c.name, c.ctype, c.data))
		want(t, rec, http.StatusCreated, c.name)
		a := decode[domain.Artifact](t, rec)
		got := f.getAs(f.root, "/v1/artifacts/"+a.ID.String()+"/content")
		want(t, got, http.StatusOK, "скачивание "+c.name)
		h := got.Header()
		cd := h.Get("Content-Disposition")
		if !strings.HasPrefix(cd, c.disp+";") || h.Get("Content-Type") != c.ctype ||
			h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") ||
			!strings.Contains(cd, "filename*=UTF-8''"+rfc5987(c.name)) || !bytes.Equal(got.Body.Bytes(), c.data) {
			t.Fatalf("%s: %v", c.name, h)
		}
	}

	// Артефакт этапа, сданный строкой до вложений: тот же набор заголовков,
	// тип с блоба.
	rec := f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-3/artifacts",
		map[string]any{"kind": "analysis.brief", "title": "старый", "content": "# старый", "media_type": "text/html"})
	want(t, rec, http.StatusCreated, "сдача строкой")
	old := decode[domain.Artifact](t, rec)
	h := f.getAs(f.root, "/v1/artifacts/"+old.ID.String()+"/content").Header()
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "attachment;") || h.Get("Content-Type") != "text/html" {
		t.Fatalf("старый артефакт: %v", h)
	}

	// AC-11: враждебные имена сохраняются и отдаются очищенными.
	for raw, wantName := range map[string]string{
		"../../etc/passwd":                "passwd",
		`a"b.txt`:                         "ab.txt",
		"evil\r\nX-Injected: 1.txt":       "evilX-Injected: 1.txt",
		"ctl\x00\x07\x1b.txt":             "ctl.txt",
		"..  . ..":                        defaultFileName,
		strings.Repeat("я", 200) + ".txt": strings.Repeat("я", 125) + ".txt",
	} {
		rec := f.upload("/v1/tasks/SDLC-1/attachments", filePart(raw, "text/plain", []byte("x "+raw)))
		want(t, rec, http.StatusCreated, "имя "+strconv.Quote(raw))
		a := decode[domain.Artifact](t, rec)
		if a.FileName == nil || *a.FileName != wantName {
			t.Fatalf("имя %q сохранено как %v, ждали %q", raw, a.FileName, wantName)
		}
		cd := f.getAs(f.root, "/v1/artifacts/"+a.ID.String()+"/content").Header().Get("Content-Disposition")
		if strings.ContainsAny(cd, "\r\n") || strings.Count(cd, `"`) != 2 {
			t.Fatalf("заголовок для %q: %q", raw, cd)
		}
	}
	for _, p := range f.storedFiles() {
		if strings.Contains(p, "passwd") || strings.Contains(p, ".txt") {
			t.Fatalf("путь в хранилище зависит от имени: %s", p)
		}
	}
}

// AC-13: права.
func TestAttachmentPermissions(t *testing.T) {
	f := newAttachFixture(t)
	ctx := context.Background()
	viewer := f.member("user:vera", domain.MemberViewer, nil)
	narrowed := f.member("user:vera", domain.MemberViewer, []string{"analyst"})

	rec := f.uploadAs(viewer, "/v1/tasks/SDLC-1/attachments", filePart("v.png", "image/png", pngBytes))
	want(t, rec, http.StatusCreated, "viewer прикрепляет")
	v1 := decode[domain.Artifact](t, rec)
	rec = f.uploadAs(narrowed, "/v1/tasks/SDLC-1/attachments", field("supersedes_id", v1.ID.String()),
		filePart("v2.png", "image/png", append(pngBytes, 7)))
	want(t, rec, http.StatusCreated, "суженный токен viewer выпускает версию")
	want(t, f.getAs(viewer, "/v1/artifacts/"+v1.ID.String()+"/content"), http.StatusOK, "viewer скачивает")
	want(t, f.getAs(viewer, "/v1/tasks/SDLC-1/attachments"), http.StatusOK, "viewer читает список")

	// Без доступа к проекту — отказ на прикрепление и скачивание.
	if _, err := f.store.ResolveActor(ctx, f.store.DB(), "outsider", domain.PrincipalUser); err != nil {
		t.Fatal(err)
	}
	issued, err := f.srv.auth.IssueToken(ctx, f.admin, workflow.IssueTokenInput{Name: "o", Principal: "user:outsider"})
	if err != nil {
		t.Fatal(err)
	}
	outsider := issued.Token
	for _, r := range []*httptest.ResponseRecorder{
		f.uploadAs(outsider, "/v1/tasks/SDLC-1/attachments", filePart("o.png", "image/png", pngBytes)),
		f.getAs(outsider, "/v1/artifacts/"+v1.ID.String()+"/content"),
	} {
		if r.Code != http.StatusForbidden && r.Code != http.StatusNotFound {
			t.Fatalf("без доступа: %d %s", r.Code, r.Body)
		}
	}

	// Токен задачи: своя задача и её прямой ребёнок — да, чужая — нет.
	want(t, f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-1/transitions/start", nil), http.StatusOK, "start")
	want(t, f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-1/claim", nil), http.StatusOK, "claim")
	rec = f.jsonAs(f.root, http.MethodPost, "/v1/projects/SDLC/tasks", map[string]any{"title": "ребёнок", "parent_key": "SDLC-1"})
	want(t, rec, http.StatusCreated, "дочерняя задача")
	child := decode[domain.Task](t, rec).Key
	rec = f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-1/task-token", nil)
	want(t, rec, http.StatusCreated, "токен задачи")
	taskToken := decode[map[string]any](t, rec)["token"].(string)
	for _, key := range []string{"SDLC-1", child} {
		want(t, f.uploadAs(taskToken, "/v1/tasks/"+key+"/attachments", filePart("t.png", "image/png", pngBytes)),
			http.StatusCreated, "токен задачи → "+key)
	}
	rec = f.uploadAs(taskToken, "/v1/tasks/SDLC-3/attachments", filePart("t.png", "image/png", pngBytes))
	want(t, rec, http.StatusForbidden, "токен задачи → чужая")
	want(t, f.getAs(taskToken, "/v1/tasks/SDLC-1/attachments"), http.StatusOK, "токен задачи читает список")

	var actor uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT actor_id FROM task_events WHERE type = 'attachment.added'
	        AND task_id = (SELECT id FROM tasks WHERE key = $1) AND stage_run_id IS NULL`, child).Scan(&actor); err != nil {
		t.Fatalf("событие вложения токеном задачи: %v", err)
	}
	if actor != f.admin.Principal.ID {
		t.Fatalf("актор события %s, ждали принципала root", actor)
	}
	// Журнал пишется после ответа — ждём записи.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok := f.count(`SELECT count(*) FROM audit_log WHERE action = 'POST /v1/tasks/:taskKey/attachments'
		        AND token_kind = 'task' AND outcome = 'ok' AND task_id IS NOT NULL`)
		denied := f.count(`SELECT count(*) FROM audit_log WHERE action = 'POST /v1/tasks/:taskKey/attachments'
		        AND token_kind = 'task' AND outcome = 'forbidden' AND http_status = 403`)
		if ok == 2 && denied == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("журнал: ok %d, отказов %d", ok, denied)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// AC-16, AC-38: предел файла в /v1/instance — действующий.
func TestInstanceFileLimit(t *testing.T) {
	f := newAttachFixture(t)
	inst := decode[workflow.Instance](t, f.getAs("", "/v1/instance"))
	if inst.Limits == nil || inst.Limits.MaxFileBytes != 8<<20 {
		t.Fatalf("предел с хранилищем: %+v", inst.Limits)
	}

	// Без внешнего хранилища — потолок inline, и вложение больше него — 413
	// с тем же числом.
	f.store.UseBlobStore(nil, 256<<10, 64<<20)
	inst = decode[workflow.Instance](t, f.getAs("", "/v1/instance"))
	if inst.Limits == nil || inst.Limits.MaxFileBytes != 4<<20 {
		t.Fatalf("предел без хранилища: %+v", inst.Limits)
	}
	five := bytes.Repeat([]byte("z"), 5<<20)
	for _, chunked := range []bool{false, true} {
		body, ctype := multipartBody(t, filePart("five.bin", "application/octet-stream", five))
		var r io.Reader = bytes.NewReader(body)
		if chunked {
			r = struct{ io.Reader }{r}
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/tasks/SDLC-1/attachments", r)
		if chunked {
			req.ContentLength = -1
		}
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Authorization", "Bearer "+f.root)
		wantTooLarge(t, f.serve(req), 4<<20)
	}
}

// AC-34: форма.
func TestUploadForm(t *testing.T) {
	f := newAttachFixture(t)
	path := "/v1/tasks/SDLC-1/attachments"
	snapshot := func() [3]int {
		return [3]int{f.count(`SELECT count(*) FROM artifacts`), f.blobs(), len(f.storedFiles())}
	}
	before := snapshot()
	big := bytes.Repeat([]byte("b"), 300<<10)
	for name, parts := range map[string][]formPart{
		"поле после файла": {filePart("a.bin", "application/octet-stream", big), field("title", "поздно")},
		"два файла":        {filePart("a.bin", "", big), filePart("b.bin", "", []byte("b"))},
		"нет файла":        {field("title", "без файла")},
		"пустой файл":      {filePart("empty.txt", "text/plain", nil)},
		"большое поле":     {field("title", strings.Repeat("т", formFieldMax)), filePart("a.bin", "", big)},
		"незнакомое поле":  {field("kind", "x"), filePart("a.bin", "", big)},
		"неверный uuid":    {field("supersedes_id", "nope"), filePart("a.bin", "", big)},
	} {
		rec := f.upload(path, parts...)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if after := snapshot(); after != before {
			t.Fatalf("%s: что-то создано: %v → %v", name, before, after)
		}
	}
	// Не multipart.
	want(t, f.jsonAs(f.root, http.MethodPost, path, map[string]any{"title": "x"}), http.StatusBadRequest, "JSON на вложение")

	// Тип: из Content-Type части, невалидный — octet-stream, параметры
	// отброшены.
	for ctype, wantType := range map[string]string{
		"foo":                       octetStream,
		"":                          octetStream,
		"text/plain; charset=utf-8": "text/plain",
		"Image/PNG":                 "image/png",
		"text/plain; charset":       "text/plain",
	} {
		rec := f.upload(path, filePart("t.bin", ctype, []byte("тип "+ctype)))
		want(t, rec, http.StatusCreated, "тип "+ctype)
		if a := decode[domain.Artifact](t, rec); a.MediaType == nil || *a.MediaType != wantType {
			t.Fatalf("тип %q сохранён как %v", ctype, a.MediaType)
		}
	}
	if got := sanitizeMediaType("text/plain\r\nX-Evil: 1"); got != octetStream {
		t.Fatalf("тип с CR/LF: %q", got)
	}

	// Тело больше предела файла с запасом формы — 413 без чтения (по длине) и
	// на чтении (chunked), без строк, объектов и спула.
	before = snapshot()
	over := bytes.Repeat([]byte("o"), 8<<20+1)
	for _, chunked := range []bool{false, true} {
		body, ctype := multipartBody(t, filePart("over.bin", "", over))
		var r io.Reader = bytes.NewReader(body)
		if chunked {
			r = struct{ io.Reader }{r}
		}
		req := httptest.NewRequest(http.MethodPost, path, r)
		if chunked {
			req.ContentLength = -1
		}
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Authorization", "Bearer "+f.root)
		wantTooLarge(t, f.serve(req), 8<<20)
		if after := snapshot(); after != before {
			t.Fatalf("после 413 (chunked %v): %v → %v", chunked, before, after)
		}
	}
	// Заявленная длина больше предела файла с запасом формы — 413 прежде,
	// чем прочитан хоть байт.
	body, ctype := multipartBody(t, filePart("over.bin", "", bytes.Repeat([]byte("o"), 8<<20+bodyLimitSlack)))
	var n int64
	req := httptest.NewRequest(http.MethodPost, path, countingReader{r: bytes.NewReader(body), n: &n})
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+f.root)
	rec := httptest.NewRecorder()
	w := &countingWriter{ResponseWriter: rec, read: &n}
	f.h.ServeHTTP(w, req)
	if w.at != 0 {
		t.Fatalf("по заявленной длине прочитано %d байт", w.at)
	}
	wantTooLarge(t, rec, 8<<20)
}

// AC-41: маршруты с файлом — под сроком загрузки и пределом файла; в
// таблице маршрутов нет ручки файла без них.
func TestUploadRoutes(t *testing.T) {
	f := newLimitsFixture(t, config.Config{BlobMaxBytes: 1 << 20, BlobInlineMaxBytes: 1024,
		RequestTimeout: shortRequest, UploadTimeout: 10 * time.Second})
	want(t, f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-1/transitions/start", nil), http.StatusOK, "start")

	fileHandlers := map[string]bool{"attachFile": false, "submitArtifact": false}
	var routes []gin.RouteInfo
	for _, r := range f.h.(*gin.Engine).Routes() {
		for name := range fileHandlers {
			if strings.HasSuffix(r.Handler, "."+name+"-fm") {
				fileHandlers[name] = true
				routes = append(routes, r)
			}
		}
		if strings.Contains(r.Path, "attachments") && r.Method == http.MethodPost &&
			!strings.HasSuffix(r.Handler, ".attachFile-fm") {
			t.Fatalf("маршрут вложений без ручки файла: %+v", r)
		}
	}
	for name, seen := range fileHandlers {
		if !seen {
			t.Fatalf("ручки %s нет в таблице маршрутов", name)
		}
	}
	ts := f.liveServer()
	over := bytes.Repeat([]byte("o"), 1<<20+bodyLimitSlack)
	for _, r := range routes {
		path := strings.ReplaceAll(r.Path, ":taskKey", "SDLC-1")
		parts := []formPart{}
		if strings.HasSuffix(r.Handler, ".submitArtifact-fm") {
			parts = append(parts, field("kind", "analysis.brief"), field("title", "медленно"))
		}
		// Предел файла: тело больше — 413 с пределом файла.
		wantTooLarge(t, f.upload(path, append(parts, filePart("over.bin", "", over))...), 1<<20)
		// Срок загрузки: дольше SDLC_REQUEST_TIMEOUT, короче SDLC_UPLOAD_TIMEOUT — 201.
		body, ctype := multipartBody(t, append(parts, filePart("slow.md", "text/markdown",
			bytes.Repeat([]byte("м"), 64<<10)))...)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, slowBody(body, slowUpload))
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Authorization", "Bearer "+f.root)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s медленно: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s медленно: %d", path, resp.StatusCode)
		}
	}
}

// AC-33: медленная загрузка не держит ни строку задачи, ни соединение пула.
func TestSlowUploadDoesNotHoldTask(t *testing.T) {
	f := newLimitsFixture(t, config.Config{BlobMaxBytes: 16 << 20, BlobInlineMaxBytes: 256 << 10,
		RequestTimeout: 30 * time.Second, UploadTimeout: time.Minute})
	want(t, f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-1/transitions/start", nil), http.StatusOK, "start")
	want(t, f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-1/claim", nil), http.StatusOK, "claim")
	// Переход берёт задачу под FOR UPDATE и отбивается гейтами; перепроверку
	// он заводить не должен — её прогон увёл бы этап в gating.
	f.exec(`DELETE FROM gates WHERE type = 'agent_review'`)
	ts := f.liveServer()

	type slow struct {
		pw   *io.PipeWriter
		rest []byte
		done chan int
	}
	start := func(path string, parts ...formPart) *slow {
		body, ctype := multipartBody(t, parts...)
		pr, pw := io.Pipe()
		s := &slow{pw: pw, rest: body[len(body)/2:], done: make(chan int, 1)}
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, pr)
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Authorization", "Bearer "+f.root)
		go func() {
			resp, err := ts.Client().Do(req)
			if err != nil {
				s.done <- 0
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			s.done <- resp.StatusCode
		}()
		go pw.Write(body[:len(body)/2])
		return s
	}
	// Двадцать загрузок больше порога inline (спул, а не память) и две по
	// 8 МиБ — вложение и файл по контракту; все встают на полпути.
	eight := make([]byte, 8<<20)
	rand.Read(eight)
	var uploads []*slow
	for i := range 20 {
		uploads = append(uploads, start("/v1/tasks/SDLC-1/attachments",
			filePart("big"+strconv.Itoa(i)+".bin", "", append(eight[:512<<10:512<<10], byte(i)))))
	}
	uploads = append(uploads,
		start("/v1/tasks/SDLC-1/attachments", filePart("eight.bin", "", eight)),
		start("/v1/tasks/SDLC-1/artifacts", field("kind", "analysis.brief"),
			field("title", "файл"), filePart("brief.bin", "", append(eight[:8<<20-1:8<<20-1], 1))))
	time.Sleep(time.Second)

	quick := func(method, path string, body any) int {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, method, ts.URL+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.root)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s во время загрузки: %v", method, path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := quick(http.MethodPost, "/v1/tasks/SDLC-1/heartbeat", nil); code != http.StatusOK {
		t.Fatalf("heartbeat во время загрузки: %d", code)
	}
	if code := quick(http.MethodPost, "/v1/tasks/SDLC-1/transitions/analysis_done", nil); code != http.StatusConflict {
		t.Fatalf("переход во время загрузки: %d", code)
	}
	if code := quick(http.MethodGet, "/v1/tasks/SDLC-1", nil); code != http.StatusOK {
		t.Fatalf("карточка во время загрузки: %d", code)
	}
	if n := f.count(`SELECT count(*) FROM pg_stat_activity
	        WHERE datname = current_database() AND state LIKE 'idle in transaction%'`); n != 0 {
		t.Fatalf("сеансов в транзакции во время загрузки: %d", n)
	}

	// Одну загрузку обрываем — ни артефакта, ни блоба, ни спула.
	broken := uploads[0]
	uploads = uploads[1:]
	broken.pw.CloseWithError(io.ErrUnexpectedEOF)
	<-broken.done
	for _, s := range uploads {
		go func() {
			s.pw.Write(s.rest)
			s.pw.Close()
		}()
	}
	for _, s := range uploads {
		if code := <-s.done; code != http.StatusCreated {
			t.Fatalf("загрузка после паузы: %d", code)
		}
	}
	if n := f.count(`SELECT count(*) FROM artifacts WHERE file_name = 'big0.bin'`); n != 0 {
		t.Fatal("оборванная загрузка оставила артефакт")
	}
	if len(f.spools()) != 0 {
		t.Fatalf("спулы после загрузок: %v", f.spools())
	}
	var objects int
	for _, p := range f.storedFiles() {
		if !strings.HasPrefix(filepath.Base(p), ".") {
			objects++
		}
	}
	if blobs := f.count(`SELECT count(*) FROM blobs WHERE storage = 'external'`); objects != blobs {
		t.Fatalf("объектов %d, строк external %d", objects, blobs)
	}
}

// AC-39: миграция аддитивна — down на базе с вложениями снимает колонки, не
// трогая строк, up возвращает их с умолчанием; ограничения держат форму
// вложения.
func TestAttachmentsMigration(t *testing.T) {
	f := newAttachFixture(t)
	ctx := context.Background()
	want(t, f.upload("/v1/tasks/SDLC-1/attachments", filePart("a.png", "image/png", pngBytes)),
		http.StatusCreated, "вложение")
	rec := f.jsonAs(f.root, http.MethodPost, "/v1/tasks/SDLC-3/artifacts",
		map[string]any{"kind": "analysis.brief", "title": "этап", "content": "# этап"})
	want(t, rec, http.StatusCreated, "сдача строкой")
	stage := decode[domain.Artifact](t, rec)

	// Ограничения: вложение без имени, с прогоном, имя длиннее 255 байт.
	for name, sql := range map[string]string{
		"без имени": `INSERT INTO artifacts (task_id, project_id, kind, title, blob_sha, is_attachment, media_type)
		              SELECT task_id, project_id, 'attachment', 'x', blob_sha, true, 'text/plain' FROM artifacts WHERE id = $1`,
		"с прогоном": `INSERT INTO artifacts (task_id, project_id, stage_run_id, kind, title, blob_sha, is_attachment, file_name, media_type)
		              SELECT task_id, project_id, stage_run_id, 'attachment', 'x', blob_sha, true, 'x', 'text/plain' FROM artifacts WHERE id = $1`,
		"длинное имя": `INSERT INTO artifacts (task_id, project_id, kind, title, blob_sha, file_name)
		              SELECT task_id, project_id, kind, 'x', blob_sha, repeat('я', 128) FROM artifacts WHERE id = $1`,
	} {
		if _, err := f.pool.Exec(ctx, sql, stage.ID); err == nil {
			t.Fatalf("%s: ограничение не сработало", name)
		}
	}

	read := func(name string) string {
		body, err := migrations.Schema.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	columns := func() int {
		return f.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'artifacts'
		        AND column_name IN ('is_attachment', 'file_name', 'media_type', 'indexed_at')`)
	}
	before := f.count(`SELECT count(*) FROM artifacts`)
	f.exec(read("000033_attachments.down.sql"))
	if columns() != 0 || f.count(`SELECT count(*) FROM artifacts`) != before {
		t.Fatalf("down: колонок %d, артефактов %d → %d", columns(), before, f.count(`SELECT count(*) FROM artifacts`))
	}
	f.exec(read("000033_attachments.up.sql"))
	if columns() != 4 || f.count(`SELECT count(*) FROM artifacts WHERE is_attachment`) != 0 {
		t.Fatal("up после down: колонки или умолчание")
	}
	got := f.getAs(f.root, "/v1/artifacts/"+stage.ID.String()+"/content")
	if got.Code != http.StatusOK || got.Body.String() != "# этап" || got.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("артефакт этапа после up: %d %v", got.Code, got.Header())
	}
}
