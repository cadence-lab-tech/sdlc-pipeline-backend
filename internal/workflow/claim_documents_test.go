package workflow

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Доставка роли документами (CLAB-344, решение 0068, п. 4): захват и
// /state отдают все документы версии прогона и склейку instructions.
// Номера — критерии приёмки анализа CLAB-344.

// prevRoleKeys — ключи role в ответе захвата до CLAB-344 (model_hint и
// context_budget — omitempty, у сидовой роли analyst их нет).
var prevRoleKeys = []string{
	"key", "name", "version", "instructions", "input_kinds", "output_contract", "tool_policy", "model_level",
}

// claimAnalysis заводит задачу, переводит её на статус роли analyst и
// отдаёт захват и состояние этого прогона.
func claimAnalysis(t *testing.T, engine *Engine, system domain.Principal, title string) (ClaimResult, ClaimResult) {
	t.Helper()
	ctx := context.Background()
	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: title},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(ctx, task.Key, "start", system.ID, "поехали"); err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.Claim(ctx, task.Key, system.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.State(ctx, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Role == nil || state.Role == nil || claimed.Role.Key != "analyst" {
		t.Fatalf("у прогона нет роли analyst: %+v", claimed.Role)
	}
	return claimed, state
}

func roleJSON(t *testing.T, r *RoleView) map[string]any {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func checkDocument(t *testing.T, where string, got domain.RoleDocument, pos int, name, kind, text string) {
	t.Helper()
	if got.Position != pos || got.Name != name || got.Kind != kind || got.Content == nil || *got.Content != text ||
		got.SHA256 != shaHex(text) || got.SizeBytes != int64(len(text)) {
		t.Fatalf("%s: документ %d %+v, ждали %s/%s, %d байт", where, pos, got, name, kind, len(text))
	}
}

// AC-1: три документа по position с sha и размером от байтов; склейка равна
// колонке и GET версии; захват и /state — одни байты; прежние ключи role на
// месте. Роль из одного документа — его текст байт в байт.
func TestClaimCarriesRoleDocuments(t *testing.T) {
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)

	three := []RoleDocumentInput{
		{Name: "main.md", Content: "Ты аналитик.\n"},
		{Name: "git.md", Kind: "reference", Content: "Ветки clab/<ключ> — ёж 🦔"},
		{Name: "selfhosted.md", Kind: "checklist", Content: "- пакет учтён  \n  "},
	}
	rv, err := authoring.AddRoleVersion(ctx, "SDLC", "analyst", system.ID, RoleVersionInput{Documents: three})
	if err != nil {
		t.Fatal(err)
	}
	claimed, state := claimAnalysis(t, engine, system, "Три документа")
	if claimed.Role.Version != rv.Version {
		t.Fatalf("захват на версии %d, ждали %d", claimed.Role.Version, rv.Version)
	}
	got, err := authoring.GetRoleVersion(ctx, "SDLC", "analyst", rv.Version)
	if err != nil {
		t.Fatal(err)
	}
	col := columnInstructions(t, store, rv.ID)
	for name, res := range map[string]ClaimResult{"claim": claimed, "state": state} {
		docs := res.Role.Documents
		if len(docs) != 3 {
			t.Fatalf("%s: документов %d", name, len(docs))
		}
		for i, in := range three {
			kind := in.Kind
			if kind == "" {
				kind = domain.DefaultRoleDocumentKind
			}
			checkDocument(t, name, docs[i], i, in.Name, kind, in.Content)
		}
		if res.Role.Instructions != col || res.Role.Instructions != got.Instructions ||
			!strings.Contains(col, "<!-- document: git.md -->") {
			t.Fatalf("%s: склейка %q, колонка %q", name, res.Role.Instructions, col)
		}
		keys := roleJSON(t, res.Role)
		for _, k := range append(prevRoleKeys, "documents") {
			if _, ok := keys[k]; !ok {
				t.Errorf("%s: в role нет ключа %q", name, k)
			}
		}
	}
	if !reflect.DeepEqual(roleJSON(t, claimed.Role), roleJSON(t, state.Role)) {
		t.Fatal("role захвата и /state расходятся")
	}

	for _, text := range []string{"без перевода строки", "хвостовые пробелы  \n  ", "многобайтный: αβγ — ёж 🦔"} {
		one, err := authoring.AddRoleVersion(ctx, "SDLC", "analyst", system.ID, RoleVersionInput{Instructions: text})
		if err != nil {
			t.Fatal(err)
		}
		claimed, state := claimAnalysis(t, engine, system, "Один документ")
		for name, res := range map[string]ClaimResult{"claim": claimed, "state": state} {
			if res.Role.Version != one.Version || len(res.Role.Documents) != 1 {
				t.Fatalf("%s: %+v", name, res.Role)
			}
			checkDocument(t, name, res.Role.Documents[0], 0, domain.MainRoleDocument, domain.DefaultRoleDocumentKind, text)
			if res.Role.Instructions != text || columnInstructions(t, store, one.ID) != text {
				t.Fatalf("%s: instructions %q, ждали %q байт в байт", name, res.Role.Instructions, text)
			}
		}
	}
}

// AC-2 (а): версия без строк документов — сидовая, вставленная прямым
// INSERT INTO role_versions, — отдаётся одним main.md из колонки.
func TestClaimRoleWithoutDocumentRows(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	claimed, state := claimAnalysis(t, engine, system, "Сидовая роль")
	var rows int
	if err := store.DB().QueryRow(ctx, `
		SELECT count(*) FROM role_version_documents d
		JOIN role_versions v ON v.id = d.role_version_id JOIN roles r ON r.id = v.role_id
		WHERE r.key = 'analyst' AND v.version = $1`, claimed.Role.Version).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("у сидовой версии есть строки документов: %d %v", rows, err)
	}
	var col string
	if err := store.DB().QueryRow(ctx, `
		SELECT v.instructions FROM role_versions v JOIN roles r ON r.id = v.role_id
		WHERE r.key = 'analyst' AND v.version = $1`, claimed.Role.Version).Scan(&col); err != nil {
		t.Fatal(err)
	}
	for name, res := range map[string]ClaimResult{"claim": claimed, "state": state} {
		if len(res.Role.Documents) != 1 || res.Role.Instructions != col {
			t.Fatalf("%s: %+v", name, res.Role)
		}
		checkDocument(t, name, res.Role.Documents[0], 0, domain.MainRoleDocument, domain.DefaultRoleDocumentKind, col)
	}
}

// AC-2 (б): документ крупнее порога inline лежит во внешнем хранилище и
// приходит в захвате целиком, с верными sha256 и размером.
func TestClaimRoleDocumentFromBlobStore(t *testing.T) {
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	blobs, err := blobstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.UseBlobStore(blobs, 1024, 64<<20)

	big := strings.Repeat("Длинный пункт инструкции роли, который не влезает в базу.\n", 200)
	if _, err := authoring.AddRoleVersion(ctx, "SDLC", "analyst", system.ID, RoleVersionInput{Documents: []RoleDocumentInput{
		{Name: "main.md", Content: "Коротко."}, {Name: "big.md", Kind: "reference", Content: big},
	}}); err != nil {
		t.Fatal(err)
	}
	var storage string
	if err := store.DB().QueryRow(ctx, `SELECT storage FROM blobs WHERE sha256 = $1`, shaHex(big)).Scan(&storage); err != nil {
		t.Fatal(err)
	}
	if storage == "inline" {
		t.Fatalf("документ %d байт остался в базе: порог не сработал", len(big))
	}
	claimed, state := claimAnalysis(t, engine, system, "Большой документ")
	for name, res := range map[string]ClaimResult{"claim": claimed, "state": state} {
		if len(res.Role.Documents) != 2 {
			t.Fatalf("%s: документов %d", name, len(res.Role.Documents))
		}
		checkDocument(t, name, res.Role.Documents[1], 1, "big.md", "reference", big)
		if !strings.HasSuffix(res.Role.Instructions, big) {
			t.Fatalf("%s: в склейке нет большого документа", name)
		}
	}
}

// Ключи role ответа — прежние плюс documents, и ничего больше: новое поле
// роли (CLAB-348 добавит document_warnings) — осознанное решение, а не
// случайность.
func TestRoleViewKeys(t *testing.T) {
	r := fullClaimResult().Role
	r.Documents = []domain.RoleDocument{{Name: "main.md"}}
	var keys []string
	for k := range roleJSON(t, r) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := append(append([]string{}, prevRoleKeys...), "model_hint", "context_budget", "documents")
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("ключи role %v, ждали %v", keys, want)
	}
}
