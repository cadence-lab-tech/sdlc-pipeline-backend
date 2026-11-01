package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Версия роли из документов (CLAB-343, решение 0068): авторинг, чтение,
// дедупликация, атомарность и связка. Номера — критерии приёмки анализа
// CLAB-343.

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type roleCounts struct{ versions, documents, blobs int }

func countRoleRows(t *testing.T, store *postgres.Store) roleCounts {
	t.Helper()
	var c roleCounts
	if err := store.DB().QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM role_versions), (SELECT count(*) FROM role_version_documents),
		       (SELECT count(*) FROM blobs)`).Scan(&c.versions, &c.documents, &c.blobs); err != nil {
		t.Fatal(err)
	}
	return c
}

func columnInstructions(t *testing.T, store *postgres.Store, id any) string {
	t.Helper()
	var s string
	if err := store.DB().QueryRow(context.Background(),
		`SELECT instructions FROM role_versions WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// AC-4, AC-6, AC-7: три документа по порядку, sha и размер от байтов,
// склейка в колонке равна ответу; неизменённые документы новой версии
// делят блобы.
func TestRoleVersionFromDocuments(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)

	three := []RoleDocumentInput{
		{Name: "main.md", Content: "Ты писатель."},
		{Name: "git.md", Kind: "reference", Content: "Ветки clab/<ключ>.\n"},
		{Name: "check.md", Kind: "checklist", Content: "- ссылки живы"},
	}
	created, err := authoring.CreateRole(ctx, "SDLC", system.ID, CreateRoleInput{
		Key: "writer", Name: "Техписатель", Version: &RoleVersionInput{Documents: three},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Latest == nil || created.Latest.Version != 1 || len(created.Latest.Documents) != 3 {
		t.Fatalf("POST roles с version.documents: %+v", created.Latest)
	}

	got, err := authoring.GetRoleVersion(ctx, "SDLC", "writer", 1)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"instructions", "reference", "checklist"}
	for i, d := range got.Documents {
		if d.Position != i || d.Name != three[i].Name || d.Kind != wantKinds[i] ||
			d.SHA256 != shaHex(three[i].Content) || d.SizeBytes != int64(len(three[i].Content)) ||
			d.Content == nil || *d.Content != three[i].Content {
			t.Fatalf("документ %d: %+v", i, d)
		}
	}
	want := "<!-- document: main.md -->\nТы писатель.\n\n<!-- document: git.md -->\nВетки clab/<ключ>.\n\n" +
		"<!-- document: check.md -->\n- ссылки живы\n"
	if got.Instructions != want || columnInstructions(t, store, got.ID) != want {
		t.Fatalf("склейка: ответ %q, колонка %q", got.Instructions, columnInstructions(t, store, got.ID))
	}

	before := countRoleRows(t, store)
	changed := append([]RoleDocumentInput(nil), three...)
	changed[1].Content = "Ветки clab/<ключ>-<slug>.\n"
	rv, err := authoring.AddRoleVersion(ctx, "SDLC", "writer", system.ID, RoleVersionInput{Documents: changed})
	if err != nil {
		t.Fatal(err)
	}
	if rv.Version != 2 {
		t.Fatalf("версия %d, ждали 2", rv.Version)
	}
	after := countRoleRows(t, store)
	if after.blobs != before.blobs+1 || after.documents != before.documents+3 {
		t.Fatalf("дедупликация: было %+v, стало %+v", before, after)
	}
	v1, v2 := got.Documents, rv.Documents
	if v1[0].SHA256 != v2[0].SHA256 || v1[2].SHA256 != v2[2].SHA256 || v1[1].SHA256 == v2[1].SHA256 {
		t.Fatalf("sha документов v1 %+v и v2 %+v", v1, v2)
	}

	// Тот же текст в другой роли — та же строка blobs.
	before = countRoleRows(t, store)
	if _, err := authoring.CreateRole(ctx, "SDLC", system.ID, CreateRoleInput{
		Key: "editor", Name: "Редактор", Version: &RoleVersionInput{Documents: three[:1]},
	}); err != nil {
		t.Fatal(err)
	}
	if after := countRoleRows(t, store); after.blobs != before.blobs {
		t.Fatalf("одинаковый текст в двух ролях — второй блоб: %+v → %+v", before, after)
	}
}

// AC-8: строка instructions — один main.md, колонка и ответ байт в байт;
// пустая — прежний текст отказа.
func TestRoleVersionFromInstructionsString(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)
	for i, text := range []string{"без перевода строки", "хвостовые пробелы  \n  ", "многобайтный: αβγ — ёж 🦔"} {
		rv, err := authoring.AddRoleVersion(ctx, "SDLC", "developer", system.ID, RoleVersionInput{Instructions: text})
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		got, err := authoring.GetRoleVersion(ctx, "SDLC", "developer", rv.Version)
		if err != nil {
			t.Fatal(err)
		}
		if got.Instructions != text || columnInstructions(t, store, rv.ID) != text {
			t.Fatalf("%d: instructions %q", i, got.Instructions)
		}
		if len(got.Documents) != 1 || got.Documents[0].Name != "main.md" || got.Documents[0].Kind != "instructions" ||
			*got.Documents[0].Content != text || got.Documents[0].SHA256 != shaHex(text) {
			t.Fatalf("%d: документы %+v", i, got.Documents)
		}
	}
	_, err := authoring.AddRoleVersion(ctx, "SDLC", "developer", system.ID, RoleVersionInput{Instructions: " \n"})
	if code(err) != apperr.CodeInvalidInput || !strings.Contains(err.Error(), "инструкция роли не может быть пустой") {
		t.Fatalf("пустая строка: %v", err)
	}
}

// AC-5, AC-13: отказ не оставляет ни версии, ни документов, ни блобов; номер
// версии не расходуется. Отказ на третьем документе — предел содержимого.
func TestRoleVersionRejectsLeaveNothing(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)
	store.UseBlobStore(nil, 1024, 64)

	latest := func() int {
		t.Helper()
		r, err := authoring.GetRole(ctx, "SDLC", "developer")
		if err != nil {
			t.Fatal(err)
		}
		return r.Latest.Version
	}
	start := latest()
	before := countRoleRows(t, store)
	bad := []RoleVersionInput{
		{Documents: []RoleDocumentInput{}},
		{},
		{Documents: []RoleDocumentInput{{Name: "", Content: "x"}}},
		{Documents: []RoleDocumentInput{{Name: "Main.md", Content: "x"}}},
		{Documents: []RoleDocumentInput{{Name: "a.md", Content: "x"}, {Name: "a.md", Content: "y"}}},
		{Documents: []RoleDocumentInput{{Name: "a.md", Kind: "Ref", Content: "x"}}},
		{Documents: []RoleDocumentInput{{Name: "a.md", Content: "  \n"}}},
		{Documents: []RoleDocumentInput{{Name: "a.md", Content: "a\x00"}}},
		{Instructions: "x", Documents: []RoleDocumentInput{{Name: "a.md", Content: "x"}}},
		{Documents: []RoleDocumentInput{
			{Name: "a.md", Content: "новый текст a"}, {Name: "b.md", Content: "новый текст b"},
			{Name: "c.md", Content: strings.Repeat("я", 40)}}},
	}
	for i, in := range bad {
		_, err := authoring.AddRoleVersion(ctx, "SDLC", "developer", system.ID, in)
		if code(err) != apperr.CodeInvalidInput {
			t.Fatalf("%d: ждали invalid_input, получили %v", i, err)
		}
	}
	if after := countRoleRows(t, store); after != before {
		t.Fatalf("отказы оставили строки: было %+v, стало %+v", before, after)
	}
	rv, err := authoring.AddRoleVersion(ctx, "SDLC", "developer", system.ID,
		RoleVersionInput{Documents: []RoleDocumentInput{{Name: "a.md", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if rv.Version != start+1 {
		t.Fatalf("номер после отказов %d, ждали %d", rv.Version, start+1)
	}

	// Подставленный отказ вставки документа посреди записи: CHECK схемы
	// (имя, которое валидатор не видит — запись в обход addVersion) откатывает
	// и версию, и блобы этой попытки.
	before = countRoleRows(t, store)
	role, err := store.GetRoleByKey(ctx, store.DB(), projectID(t, store), "developer")
	if err != nil {
		t.Fatal(err)
	}
	err = store.InTx(ctx, func(tx pgx.Tx) error {
		_, err := store.CreateRoleVersion(ctx, tx, postgres.CreateRoleVersionInput{
			RoleID: role.ID, Instructions: "x", CreatedBy: system.ID,
			Documents: []postgres.RoleDocumentInput{
				{Name: "ok.md", Kind: "instructions", Content: []byte("уникальный текст 1")},
				{Name: "BAD.md", Kind: "instructions", Content: []byte("уникальный текст 2")},
			},
		})
		return err
	})
	if err == nil {
		t.Fatal("вставка документа с именем вне формы прошла")
	}
	if after := countRoleRows(t, store); after != before {
		t.Fatalf("сбой посреди записи оставил строки: было %+v, стало %+v", before, after)
	}
}

// AC-9: версия без строк документов (сид, реплика прежней версии) читается
// одним main.md из колонки во всех путях чтения.
func TestRoleVersionWithoutDocumentsReadsMain(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)

	// Сиды default_workflow вставляют role_versions напрямую — документов нет.
	var n int
	if err := store.DB().QueryRow(ctx, `SELECT count(*) FROM role_version_documents`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("у версий сида есть документы: %d %v", n, err)
	}
	role, err := authoring.GetRole(ctx, "SDLC", "analyst")
	if err != nil {
		t.Fatal(err)
	}
	check := func(where string, rv domain.RoleVersion, withContent bool) {
		t.Helper()
		col := columnInstructions(t, store, rv.ID)
		if rv.Instructions != col || len(rv.Documents) != 1 {
			t.Fatalf("%s: %+v", where, rv)
		}
		d := rv.Documents[0]
		if d.Name != "main.md" || d.Kind != "instructions" || d.Position != 0 ||
			d.SHA256 != shaHex(col) || d.SizeBytes != int64(len(col)) {
			t.Fatalf("%s: документ %+v", where, d)
		}
		if withContent != (d.Content != nil) || (withContent && *d.Content != col) {
			t.Fatalf("%s: содержимое %v", where, d.Content)
		}
	}
	check("versions[]", role.Versions[0], false)
	check("latest_version", *role.Latest, true)
	one, err := authoring.GetRoleVersion(ctx, "SDLC", "analyst", role.Latest.Version)
	if err != nil {
		t.Fatal(err)
	}
	check("GET версии", one, true)

	// Импорт той же инструкции строкой — unchanged: main.md из колонки и
	// main.md из строки — один набор.
	report, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{Version: 1, Roles: []BundleRole{{
		Key: "analyst", Name: role.Role.Name, Summary: deref(role.Role.Summary), Instructions: one.Instructions,
		InputKinds: one.InputKinds, OutputContract: one.OutputContract, ContextBudget: one.ContextBudget,
	}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Roles[0].Outcome != "unchanged" {
		t.Fatalf("та же инструкция строкой поверх версии без документов: %+v", report.Roles[0])
	}

	if _, err := authoring.GetRoleVersion(ctx, "SDLC", "analyst", 99); code(err) != apperr.CodeNotFound {
		t.Fatalf("неизвестная версия: %v", err)
	}
}

// AC-19: связка формата 3 — created / unchanged / new_version; форматы 1–2
// дают main.md; оба поля — отказ всей связки; формат 4 — отказ; экспорт —
// только documents, и его импорт — всё unchanged.
func TestBundleRoleDocuments(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)

	docs := []BundleRoleDocument{
		{Name: "main.md", Content: "Ты писатель."},
		{Name: "git.md", Kind: "reference", Content: "Ветки."},
	}
	b := Bundle{Version: 3, Roles: []BundleRole{{Key: "writer", Name: "Техписатель", Documents: docs}}}
	outcome := func(b Bundle) string {
		t.Helper()
		r, err := authoring.Import(ctx, "SDLC", system.ID, b, false)
		if err != nil {
			t.Fatal(err)
		}
		return r.Roles[0].Outcome
	}
	if o := outcome(b); o != "created" {
		t.Fatalf("первый импорт: %s", o)
	}
	if o := outcome(b); o != "unchanged" {
		t.Fatalf("повтор: %s", o)
	}
	edited := b
	edited.Roles = []BundleRole{{Key: "writer", Name: "Техписатель",
		Documents: []BundleRoleDocument{docs[0], {Name: "git.md", Kind: "reference", Content: "Ветки и PR."}}}}
	if o := outcome(edited); o != "new_version" {
		t.Fatalf("правка одного документа: %s", o)
	}

	// Формат 1: строка — один main.md; та же строка повторно — unchanged.
	v1 := Bundle{Version: 1, Roles: []BundleRole{{Key: "reviewer2", Name: "Р", Instructions: "Проверяй."}}}
	if o := outcome(v1); o != "created" {
		t.Fatalf("формат 1: %s", o)
	}
	r, err := authoring.GetRole(ctx, "SDLC", "reviewer2")
	if err != nil {
		t.Fatal(err)
	}
	if d := r.Latest.Documents; len(d) != 1 || d[0].Name != "main.md" || *d[0].Content != "Проверяй." {
		t.Fatalf("формат 1 — не main.md: %+v", d)
	}
	asDocs := Bundle{Version: 3, Roles: []BundleRole{{Key: "reviewer2", Name: "Р",
		Documents: []BundleRoleDocument{{Name: "main.md", Kind: "instructions", Content: "Проверяй."}}}}}
	if o := outcome(asDocs); o != "unchanged" {
		t.Fatalf("тот же main.md форматом 3: %s", o)
	}

	// Оба поля у одной роли — отказ всей связки, и других ролей тоже.
	before := countRoleRows(t, store)
	var roles, workflows int
	countOthers := func() (int, int) {
		var r, w int
		_ = store.DB().QueryRow(ctx, `SELECT (SELECT count(*) FROM roles), (SELECT count(*) FROM workflows)`).Scan(&r, &w)
		return r, w
	}
	roles, workflows = countOthers()
	exported, err := authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	both := Bundle{Version: 3, Workflows: exported.Workflows[:1], Roles: []BundleRole{
		{Key: "fresh", Name: "Новая", Documents: docs},
		{Key: "broken", Name: "Сломанная", Instructions: "x", Documents: docs},
	}}
	for _, dry := range []bool{true, false} {
		_, err = authoring.Import(ctx, "SDLC", system.ID, both, dry)
		if code(err) != apperr.CodeInvalidInput || !strings.Contains(err.Error(), `"broken"`) {
			t.Fatalf("оба поля (dry_run=%v): %v", dry, err)
		}
	}
	if r2, w2 := countOthers(); countRoleRows(t, store) != before || r2 != roles || w2 != workflows {
		t.Fatal("отказ связки что-то применил")
	}
	_, err = authoring.Import(ctx, "SDLC", system.ID, Bundle{Version: 4}, false)
	if code(err) != apperr.CodeInvalidInput || !strings.Contains(err.Error(), "связка версии 4 новее поддерживаемой 3") {
		t.Fatalf("формат 4: %v", err)
	}

	// dry_run ничего не пишет.
	before = countRoleRows(t, store)
	if _, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{Version: 3, Roles: []BundleRole{
		{Key: "dry", Name: "Сухая", Documents: []BundleRoleDocument{{Name: "z.md", Content: "сухой прогон"}}}}}, true); err != nil {
		t.Fatal(err)
	}
	if countRoleRows(t, store) != before {
		t.Fatal("dry_run записал версию")
	}

	// Экспорт — только documents; импорт экспорта — всё unchanged.
	exported, err = authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if exported.Version != 3 {
		t.Fatalf("формат экспорта %d", exported.Version)
	}
	raw, _ := json.Marshal(exported.Roles)
	var keys []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, role := range keys {
		if _, ok := role["instructions"]; ok {
			t.Fatalf("экспорт пишет instructions: %s", raw[:200])
		}
	}
	for _, role := range exported.Roles {
		if len(role.Documents) == 0 {
			t.Fatalf("роль %s без документов в экспорте", role.Key)
		}
	}
	round, err := authoring.Import(ctx, "SDLC", system.ID, Bundle{Version: exported.Version, Roles: exported.Roles}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range round.Roles {
		if e.Outcome != "unchanged" {
			t.Fatalf("импорт экспорта: %s — %s", e.Key, e.Outcome)
		}
	}
}

// AC-11: движок берёт последнюю версию роли из одной точки store — прогон,
// открытый после добавления версии, пинит новую.
func TestPrimaryRolePinsLatestVersion(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)
	rv, err := authoring.AddRoleVersion(ctx, "SDLC", "developer", system.ID,
		RoleVersionInput{Documents: []RoleDocumentInput{{Name: "main.md", Content: "новая версия"}}})
	if err != nil {
		t.Fatal(err)
	}
	var sid uuid.UUID
	if err := store.DB().QueryRow(ctx, `
		SELECT sr.status_id FROM status_roles sr JOIN roles r ON r.id = sr.role_id
		JOIN workflow_statuses ws ON ws.id = sr.status_id JOIN workflows w ON w.id = ws.workflow_id
		WHERE r.key = 'developer' AND sr.is_primary AND w.state = 'active' LIMIT 1`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	_, rvid, err := store.PrimaryRole(ctx, store.DB(), sid)
	if err != nil || rvid == nil || *rvid != rv.ID {
		t.Fatalf("PrimaryRole: %v %v, ждали %s", rvid, err, rv.ID)
	}
	latest, err := store.LatestRoleVersion(ctx, store.DB(), rv.RoleID)
	if err != nil || latest.ID != rv.ID {
		t.Fatalf("LatestRoleVersion: %+v %v", latest, err)
	}
}
