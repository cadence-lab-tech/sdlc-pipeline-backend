package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// attachFixture — сервер, администратор и вызов тулов от его имени.
type attachFixture struct {
	store  *postgres.Store
	engine *workflow.Engine
	admin  workflow.Identity
	call   mcpCaller
	srv    *Server
}

func newAttachFixture(t *testing.T) attachFixture {
	t.Helper()
	ctx := context.Background()
	store, _ := testdb.Store(t)
	// Внешнее хранилище, как у настоящего инстанса: без него файл крупнее
	// 4 МиБ не принять, а zip на 5 МиБ — случай из критериев приёмки.
	blobs, err := blobstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.UseBlobStore(blobs, 1<<20, 64<<20)
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := NewServer(log, store, engine, workflow.NewAuthoring(store, log, engine), auth, "test")
	if err := auth.Bootstrap(ctx, "root", "bootstrap-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, "bootstrap-token-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	return attachFixture{store: store, engine: engine, admin: admin, call: connectAs(t, srv, admin), srv: srv}
}

func (f attachFixture) attach(t *testing.T, name, mediaType string, body []byte) domain.Artifact {
	t.Helper()
	return f.attachTo(t, "SDLC-1", name, mediaType, body)
}

func (f attachFixture) attachTo(t *testing.T, key, name, mediaType string, body []byte) domain.Artifact {
	t.Helper()
	file, err := f.store.SpoolBlob(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.engine.AttachFile(context.Background(), key, f.admin.Principal.ID, workflow.AttachInput{
		FileName: name, MediaType: mediaType, File: file})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// rows — число строк в таблицах, которые трогает сдача артефакта.
func (f attachFixture) rows(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"artifacts", "blobs", "task_events"} {
		var n int
		if err := f.store.DB().QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

// CLAB-297, AC-18: sdlc_get_artifact на файле отдаёт метаданные и способ взять
// байты, без содержимого — для картинки, текстового вложения и zip на 5 МиБ,
// а также для файла по контракту (format: binary); блоб не читается; размер
// ответа не зависит от размера файла. Текстовый артефакт этапа — как раньше.
func TestGetArtifactOnFilesReturnsMetadata(t *testing.T) {
	ctx := context.Background()
	f := newAttachFixture(t)

	const secret = "секретные байты файла"
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte(secret)...)
	zip := bytes.Repeat([]byte("Z"), 5<<20)
	copy(zip, "PK\x03\x04")
	small := bytes.Repeat([]byte("Z"), 1<<10)
	copy(small, "PK\x03\x04")

	shot := f.attach(t, "shot.png", "image/png", png)
	notes := f.attach(t, "notes.md", "text/markdown", []byte("# "+secret))
	big := f.attach(t, "dump.zip", "application/zip", zip)
	tiny := f.attach(t, "tiny.zip", "application/zip", small)

	var binary, text domain.Artifact
	if err := f.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := f.store.GetTaskByKey(ctx, tx, "SDLC-1")
		if err != nil {
			return err
		}
		if binary, err = f.store.CreateArtifact(ctx, tx, postgres.CreateArtifactInput{
			Task: task, Kind: "analysis.brief", Title: "файл", Format: "binary",
			Content: []byte(secret + " по контракту"), MediaType: "image/png", ProducedBy: f.admin.Principal.ID,
		}); err != nil {
			return err
		}
		text, err = f.store.CreateArtifact(ctx, tx, postgres.CreateArtifactInput{
			Task: task, Kind: "analysis.acceptance_criteria", Title: "критерии",
			Content: []byte("# критерии"), ProducedBy: f.admin.Principal.ID,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Блоб файлов делаем нечитаемым: внешний адрес, которого нет. Любая
	// попытка прочитать содержимое упала бы, а метаданные от блоба не зависят.
	for _, a := range []domain.Artifact{shot, notes, big, tiny, binary} {
		if _, err := f.store.DB().Exec(ctx, `
			UPDATE blobs SET storage = 'external', content = NULL, external_uri = 'file:///no/such/blob'
			WHERE sha256 = $1`, a.BlobSHA); err != nil {
			t.Fatal(err)
		}
	}

	lens := map[string]int{}
	for _, a := range []domain.Artifact{shot, notes, big, tiny, binary} {
		out, isErr := f.call("sdlc_get_artifact", map[string]any{"artifact_id": a.ID.String()})
		if isErr {
			t.Fatalf("%s: %v", a.Title, out)
		}
		raw := mustJSON(t, out)
		for _, banned := range []string{`"content"`, "секретные", "UEsD", "iVBOR"} {
			if bytes.Contains(raw, []byte(banned)) {
				t.Fatalf("%s: в ответе %s: %s", a.Title, banned, raw)
			}
		}
		file, _ := out["file"].(map[string]any)
		if file["sha256"] != a.BlobSHA || file["size_bytes"] != float64(a.SizeBytes) ||
			file["version"] != float64(1) || file["is_attachment"] != a.IsAttachment {
			t.Fatalf("%s: метаданные файла: %v", a.Title, file)
		}
		if a.FileName != nil && file["file_name"] != *a.FileName {
			t.Fatalf("%s: имя файла: %v", a.Title, file["file_name"])
		}
		if a.MediaType != nil && file["media_type"] != *a.MediaType {
			t.Fatalf("%s: media_type: %v", a.Title, file)
		}
		fetch, _ := out["fetch"].(map[string]any)
		clab, _ := fetch["clab"].([]any)
		if len(clab) != 2 ||
			clab[0] != "clab artifact "+a.ID.String()+" -o <путь>" ||
			clab[1] != "clab artifact "+a.ID.String()+" -o <каталог>/" ||
			fetch["http"] != "GET "+workflow.ContentPath(a.ID) {
			t.Fatalf("%s: способ взять байты: %v", a.Title, fetch)
		}
		if !strings.Contains(fetch["note"].(string), "инструментом чтения файлов") {
			t.Fatalf("%s: нет подсказки про чтение файла: %v", a.Title, fetch["note"])
		}
		lens[a.Title] = len(raw)
	}

	// Размер ответа не зависит от размера файла: zip в 5 МиБ и в 1 КиБ
	// отвечают почти одинаково, разница — цифры размера и sha.
	if d := lens["dump.zip"] - lens["tiny.zip"]; d < -64 || d > 64 {
		t.Fatalf("ответ растёт с размером файла: %d против %d байт", lens["dump.zip"], lens["tiny.zip"])
	}
	if lens["dump.zip"] > 4<<10 {
		t.Fatalf("ответ на zip в 5 МиБ — %d байт", lens["dump.zip"])
	}

	out, isErr := f.call("sdlc_get_artifact", map[string]any{"artifact_id": text.ID.String()})
	if isErr || out["content"] != "# критерии" || out["media_type"] == nil || out["fetch"] != nil {
		t.Fatalf("текстовый артефакт: %v", out)
	}
}

// CLAB-296, AC-15: sdlc_get_task несёт список вложений и (CLAB-297) строку о
// байтах; вложения в prior_artifacts не попадают, содержимого в ответе нет.
func TestGetTaskCarriesAttachmentsAndHint(t *testing.T) {
	f := newAttachFixture(t)

	out, isErr := f.call("sdlc_get_task", map[string]any{"task_key": "SDLC-1"})
	if isErr {
		t.Fatalf("sdlc_get_task: %v", out)
	}
	ctxOut, _ := out["context"].(map[string]any)
	if _, has := ctxOut["attachments_hint"]; has {
		t.Fatalf("строка о байтах без вложений: %v", ctxOut["attachments_hint"])
	}

	att := f.attach(t, "notes.txt", "text/plain", []byte("секретные байты файла"))
	out, isErr = f.call("sdlc_get_task", map[string]any{"task_key": "SDLC-1"})
	if isErr {
		t.Fatalf("sdlc_get_task: %v", out)
	}
	ctxOut, _ = out["context"].(map[string]any)
	list, _ := ctxOut["attachments"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["content_path"] != workflow.ContentPath(att.ID) {
		t.Fatalf("вложения в sdlc_get_task: %v", ctxOut["attachments"])
	}
	if ctxOut["attachments_hint"] != workflow.AttachmentsHint {
		t.Fatalf("строка о байтах: %v", ctxOut["attachments_hint"])
	}
	for _, want := range []string{"clab artifact <id> -o <каталог>/", "GET <content_path>", "инструментом чтения файлов"} {
		if !strings.Contains(workflow.AttachmentsHint, want) {
			t.Errorf("в строке о байтах нет %q", want)
		}
	}
	if raw := mustJSON(t, out); bytes.Contains(raw, []byte("секретные")) {
		t.Fatalf("байты файла в ответе sdlc_get_task: %s", raw)
	}
	for _, p := range ctxOut["prior_artifacts"].([]any) {
		if p.(map[string]any)["id"] == att.ID.String() {
			t.Fatal("вложение в prior_artifacts")
		}
	}
}

// CLAB-297, AC-15: тот же список и та же строка приезжают в ответе захвата;
// без вложений строки нет.
func TestClaimCarriesAttachmentsHint(t *testing.T) {
	f := newAttachFixture(t)

	created, isErr := f.call("sdlc_create_task", map[string]any{"title": "Захват с файлом"})
	if isErr {
		t.Fatalf("sdlc_create_task: %v", created)
	}
	key, _ := created["key"].(string)
	if out, isErr := f.call("sdlc_request_transition", map[string]any{"task_key": key, "transition": "start"}); isErr {
		t.Fatalf("start: %v", out)
	}

	out, isErr := f.call("sdlc_claim_task", map[string]any{"task_key": key})
	if isErr {
		t.Fatalf("sdlc_claim_task: %v", out)
	}
	if c, _ := out["context"].(map[string]any); c["attachments_hint"] != nil {
		t.Fatalf("строка о байтах без вложений: %v", c["attachments_hint"])
	}

	att := f.attachTo(t, key, "shot.png", "image/png", []byte("\x89PNG секретные"))
	out, isErr = f.call("sdlc_claim_task", map[string]any{"task_key": key})
	if isErr {
		t.Fatalf("sdlc_claim_task: %v", out)
	}
	c, _ := out["context"].(map[string]any)
	list, _ := c["attachments"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != att.ID.String() ||
		c["attachments_hint"] != workflow.AttachmentsHint {
		t.Fatalf("захват: %v", c)
	}
	b, _ := out["boundary"].(map[string]any)
	if !strings.Contains(string(mustJSON(t, b["data"])), "context.attachments_hint") {
		t.Fatalf("строка не размечена как данные: %v", b["data"])
	}
	if raw := mustJSON(t, out); bytes.Contains(raw, []byte("секретные")) {
		t.Fatalf("байты файла в ответе захвата: %s", raw)
	}
}

// CLAB-297, AC-17: sdlc_submit_artifact с format: binary — invalid_input с
// тремя путями сдачи файла; в artifacts, blobs и task_events ничего не
// появилось. Текстовая сдача проходит, как раньше.
func TestSubmitArtifactRefusesBinary(t *testing.T) {
	f := newAttachFixture(t)
	before := f.rows(t)

	for _, format := range []string{"binary", "BINARY"} {
		out, isErr := f.call("sdlc_submit_artifact", map[string]any{
			"task_key": "SDLC-1", "kind": "analysis.brief", "title": "картинка",
			"format": format, "content": "aGVsbG8=",
		})
		msg := errorMessage(out)
		if !isErr {
			t.Fatalf("%s: сдача принята: %v", format, out)
		}
		for _, want := range []string{
			"clab attach SDLC-1 <файл>",
			"clab submit SDLC-1 --kind <kind> --file <файл>",
			"POST /v1/tasks/SDLC-1/artifacts",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: в отказе нет %q: %s", format, want, msg)
			}
		}
		if e, _ := out["error"].(map[string]any); e["code"] != "invalid_input" {
			t.Errorf("%s: код отказа: %v", format, out)
		}
	}
	after := f.rows(t)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s: было %d строк, стало %d", table, n, after[table])
		}
	}

	// Текстовая сдача — как до Ч4.
	out, isErr := f.call("sdlc_submit_artifact", map[string]any{
		"task_key": "SDLC-1", "kind": "analysis.brief", "title": "текст",
		"format": "markdown", "content": "# текст",
	})
	if isErr {
		t.Fatalf("текстовая сдача: %v", out)
	}
	for _, key := range []string{"artifact_id", "kind", "version", "sha256", "size_bytes", "status"} {
		if _, ok := out[key]; !ok {
			t.Errorf("в ответе сдачи нет %s: %v", key, out)
		}
	}
}

// CLAB-297, AC-17: набор тулов не пополнился и ни один вход не несёт
// содержимого файла.
func TestToolSetHasNoFileContentTools(t *testing.T) {
	cs := connectPrompts(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		raw := strings.ToLower(string(mustJSON(t, tool.InputSchema)))
		for _, banned := range []string{"base64", "file_content", "bytes"} {
			if strings.Contains(raw, banned) {
				t.Errorf("%s: в схеме входа %q", tool.Name, banned)
			}
		}
	}
	sort.Strings(names)
	want := []string{
		"sdlc_add_repository", "sdlc_assign_task", "sdlc_check_gates", "sdlc_claim_task",
		"sdlc_create_task", "sdlc_export_bundle", "sdlc_get_artifact", "sdlc_get_document",
		"sdlc_get_drift", "sdlc_get_role", "sdlc_get_task", "sdlc_heartbeat",
		"sdlc_import_bundle", "sdlc_import_guide", "sdlc_link_repo_ref", "sdlc_link_tasks",
		"sdlc_list_documents", "sdlc_list_tasks", "sdlc_put_document", "sdlc_put_role",
		"sdlc_release", "sdlc_report_usage", "sdlc_request_transition", "sdlc_resolve_drift",
		"sdlc_search_context", "sdlc_submit_artifact", "sdlc_update_task",
		"sdlc_update_task_scope", "sdlc_whoami",
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("набор тулов изменился:\n%v\nждали\n%v", names, want)
	}
}

// CLAB-297, AC-19: описания тулов и промпт называют путь байтов через clab и
// HTTP и то, что картинку открывают инструментом чтения файлов.
func TestToolDescriptionsNameFilePath(t *testing.T) {
	cs := connectPrompts(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool.Description
	}
	for _, name := range []string{"sdlc_get_artifact", "sdlc_claim_task", "sdlc_get_task"} {
		for _, want := range []string{
			"clab artifact <id> -o <путь>", "-o <каталог>/",
			"/v1/artifacts/<id>/content", "инструментом чтения файлов",
		} {
			if !strings.Contains(byName[name], want) {
				t.Errorf("%s: в описании нет %q", name, want)
			}
		}
	}
	for _, want := range []string{"clab attach <ключ> <файл>", "clab submit <ключ> --kind <kind> --file <файл>", "multipart", "format: binary"} {
		if !strings.Contains(byName["sdlc_submit_artifact"], want) {
			t.Errorf("sdlc_submit_artifact: в описании нет %q", want)
		}
	}
	if !strings.Contains(byName["sdlc_get_artifact"], "fetch") {
		t.Error("sdlc_get_artifact: описание не называет поле fetch")
	}

	flat := strings.Join(strings.Fields(promptText(t, cs, "work_on_task", map[string]string{"task": "SDLC-42"})), " ")
	for _, want := range []string{
		"clab artifact <id> -o <каталог>/", "/v1/artifacts/<id>/content",
		"инструментом чтения файлов", "clab attach <ключ> <файл>",
		"clab submit <ключ> --kind <kind> --file <файл>",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("work_on_task: в промпте нет %q", want)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
