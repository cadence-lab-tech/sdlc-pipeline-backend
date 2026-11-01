package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/importguide"
)

// CLAB-345, AC-1, AC-3, AC-4, AC-5, AC-10 на настоящем сервере: каталог
// связки проходит круг экспорт → правка → --dry-run → импорт, неизменённые
// роли остаются unchanged, правка любого документа, состава, порядка, типа
// или поля роли даёт new_version только у своей роли.

// dirSeed — роли с документами на все случаи побайтного сравнения.
const dirSeed = `{"version": 3,
 "repositories": [{"repository": "acme/api", "role_hint": "backend"}],
 "documents": [{"kind": "guideline", "title": "Соглашения", "format": "markdown", "content": "a -> b & <c>"}],
 "roles": [
  {"key": "writer", "name": "Техписатель", "summary": "Пишет документацию",
   "input_kinds": ["dev.change_summary"], "model_level": "medium",
   "output_contract": [{"kind": "docs.page", "title": "Страница", "format": "markdown", "required": true}],
   "documents": [
     {"name": "main.md", "content": "Главное.\n"},
     {"name": "crlf.md", "kind": "reference", "content": "строка\r\nстрока\r\n"},
     {"name": "bom.md", "content": "\ufeffс BOM"},
     {"name": "nonl.md", "kind": "checklist", "content": "без перевода строки — 日本語"}]},
  {"key": "analyst", "name": "Аналитик", "documents": [{"name": "main.md", "content": "Требования.\n"}]}
 ]}`

type importReport struct {
	Roles        []struct{ Key, Outcome string } `json:"roles"`
	Documents    []struct{ Key, Outcome string } `json:"documents"`
	Repositories []struct{ Key, Outcome string } `json:"repositories"`
	Workflows    []struct{ Key, Outcome string } `json:"workflows"`
	Error        string                          `json:"error"`
}

func (r importReport) role(key string) string {
	for _, e := range r.Roles {
		if e.Key == key {
			return e.Outcome
		}
	}
	return ""
}

func TestBundleDirOnServer(t *testing.T) {
	withJSON(t, false)
	w := newLeaseWorld(t, time.Minute)
	w.call("POST", "/v1/projects", leaseRootToken, map[string]any{"key": "DIRS", "name": "Каталог связки"}, http.StatusCreated)
	c := w.client(leaseRootToken)
	ctx := context.Background()

	run := func(args ...string) importReport {
		t.Helper()
		out, _, err := capture(t, func() error { return cmdImport(ctx, c, append([]string{"--project", "DIRS"}, args...)) })
		if err != nil {
			t.Fatalf("import %v: %v", args, err)
		}
		var r importReport
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("отчёт не JSON: %s", out)
		}
		if r.Error != "" {
			t.Fatalf("import %v: %s", args, r.Error)
		}
		return r
	}
	versions := func() int {
		t.Helper()
		var n int
		if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM role_versions rv JOIN roles r ON r.id = rv.role_id
			JOIN projects p ON p.id = r.project_id WHERE p.key = 'DIRS'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// latest — документы последней версии роли: имя → (позиция, тип, blob_sha).
	type doc struct {
		Position   int
		Kind, Blob string
	}
	latest := func(role string) (int, map[string]doc) {
		t.Helper()
		rows, err := w.store.DB().Query(ctx, `
			SELECT rv.version, d.name, d.position, d.kind, d.blob_sha
			FROM role_versions rv JOIN roles r ON r.id = rv.role_id JOIN projects p ON p.id = r.project_id
			JOIN role_version_documents d ON d.role_version_id = rv.id
			WHERE p.key = 'DIRS' AND r.key = $1
			  AND rv.version = (SELECT max(version) FROM role_versions WHERE role_id = r.id)`, role)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]doc{}
		version := 0
		for rows.Next() {
			var name string
			var d doc
			if err := rows.Scan(&version, &name, &d.Position, &d.Kind, &d.Blob); err != nil {
				t.Fatal(err)
			}
			out[name] = d
		}
		return version, out
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	seed := filepath.Join(t.TempDir(), "seed.json")
	write(seed, dirSeed)
	run("--file", seed)

	// AC-1: файлы каталога побайтно равны content документов в GET роли.
	dir := filepath.Join(t.TempDir(), "rules")
	if err := exportDir(t, c, dir, "--project", "DIRS"); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"writer", "analyst"} {
		_, body := w.call("GET", "/v1/projects/DIRS/roles/"+role, leaseRootToken, nil, http.StatusOK)
		docs := body["latest_version"].(map[string]any)["documents"].([]any)
		for _, item := range docs {
			d := item.(map[string]any)
			file := dirFileText(t, filepath.Join(dir, "roles", role, d["name"].(string)))
			if sha256.Sum256([]byte(file)) != sha256.Sum256([]byte(d["content"].(string))) {
				t.Errorf("%s/%s: файл %q, на сервере %q", role, d["name"], file, d["content"])
			}
		}
	}
	if raw := dirFileText(t, filepath.Join(dir, "bundle.json")); strings.Contains(raw, `"roles"`) || !strings.Contains(raw, `"version": 3`) {
		t.Fatalf("bundle.json:\n%s", raw)
	}

	// AC-3: без правок всё unchanged, исходы остального — как у --file на том
	// же экспорте; повторный импорт версий не прибавляет.
	before := versions()
	dry := run("--dir", dir, "--dry-run")
	if len(dry.Roles) != 2 || dry.role("writer") != "unchanged" || dry.role("analyst") != "unchanged" {
		t.Fatalf("без правок: %+v", dry.Roles)
	}
	jsonOut, _, err := capture(t, func() error { return cmdExport(ctx, c, []string{"--project", "DIRS"}) })
	if err != nil {
		t.Fatal(err)
	}
	exported := filepath.Join(t.TempDir(), "b.json")
	write(exported, jsonOut)
	viaFile := run("--file", exported, "--dry-run")
	if !reflect.DeepEqual(dry, viaFile) {
		t.Fatalf("исходы --dir и --file на одном экспорте разошлись:\n%+v\n%+v", dry, viaFile)
	}
	run("--dir", dir)
	if after := versions(); after != before {
		t.Fatalf("версий было %d, стало %d", before, after)
	}
	again := filepath.Join(t.TempDir(), "again")
	if err := exportDir(t, c, again, "--project", "DIRS"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(treeOf(t, dir), treeOf(t, again)) {
		t.Fatal("повторный экспорт без правок дал другое дерево")
	}

	// AC-4: правка одного файла — new_version только у его роли; у
	// неизменённых документов блоб прежний.
	v0, docs0 := latest("writer")
	analystV, _ := latest("analyst")
	write(filepath.Join(dir, "roles", "writer", "crlf.md"), "строка\r\nновая строка\r\n")
	dry = run("--dir", dir, "--dry-run")
	if dry.role("writer") != "new_version" || dry.role("analyst") != "unchanged" || versions() != before {
		t.Fatalf("правка файла, dry-run: %+v, версий %d", dry.Roles, versions())
	}
	run("--dir", dir)
	v1, docs1 := latest("writer")
	if v1 != v0+1 || versions() != before+1 {
		t.Fatalf("версия writer %d → %d, всего %d", v0, v1, versions())
	}
	for name, d := range docs1 {
		changed := d.Blob != docs0[name].Blob
		if changed != (name == "crlf.md") {
			t.Errorf("%s: блоб %s → %s", name, docs0[name].Blob, d.Blob)
		}
	}
	if v, _ := latest("analyst"); v != analystV {
		t.Fatalf("analyst получил версию %d", v)
	}
	// Только перевод строки в конце — тоже правка.
	write(filepath.Join(dir, "roles", "analyst", "main.md"), "Требования.\n\n")
	if dry = run("--dir", dir, "--dry-run"); dry.role("analyst") != "new_version" || dry.role("writer") != "unchanged" {
		t.Fatalf("перевод строки: %+v", dry.Roles)
	}
	write(filepath.Join(dir, "roles", "analyst", "main.md"), "Требования.\n")

	// AC-5: состав, порядок, тип и поля роли — new_version, сначала видно в
	// dry-run без изменений в базе.
	manifest := filepath.Join(dir, "roles", "writer", "role.json")
	edit := func(name string, change func(m map[string]any), check func(docs map[string]doc)) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(dirFileText(t, manifest)), &m); err != nil {
			t.Fatal(err)
		}
		change(m)
		body, err := encodeIndented(m)
		if err != nil {
			t.Fatal(err)
		}
		write(manifest, string(body))
		count := versions()
		if dry := run("--dir", dir, "--dry-run"); dry.role("writer") != "new_version" || dry.role("analyst") != "unchanged" || versions() != count {
			t.Fatalf("%s, dry-run: %+v, версий %d → %d", name, dry.Roles, count, versions())
		}
		prev, _ := latest("writer")
		run("--dir", dir)
		v, docs := latest("writer")
		if v != prev+1 {
			t.Fatalf("%s: версия %d → %d", name, prev, v)
		}
		check(docs)
	}
	line := func(name, kind string) map[string]any { return map[string]any{"name": name, "kind": kind} }
	write(filepath.Join(dir, "roles", "writer", "checks.md"), "- ссылки открываются\n")
	edit("документ добавлен", func(m map[string]any) {
		docs := m["documents"].([]any)
		m["documents"] = append([]any{docs[0], line("checks.md", "checklist")}, docs[1:]...)
	}, func(docs map[string]doc) {
		if d, ok := docs["checks.md"]; !ok || d.Position != 1 || d.Kind != "checklist" || len(docs) != 5 {
			t.Fatalf("checks.md: %+v", docs)
		}
	})
	edit("строки переставлены", func(m map[string]any) {
		docs := m["documents"].([]any)
		docs[0], docs[1] = docs[1], docs[0]
	}, func(docs map[string]doc) {
		if docs["checks.md"].Position != 0 || docs["main.md"].Position != 1 {
			t.Fatalf("порядок: %+v", docs)
		}
	})
	edit("тип сменён", func(m map[string]any) {
		m["documents"].([]any)[0].(map[string]any)["kind"] = "reference"
	}, func(docs map[string]doc) {
		if docs["checks.md"].Kind != "reference" {
			t.Fatalf("тип: %+v", docs)
		}
	})
	edit("уровень модели", func(m map[string]any) { m["model_level"] = "high" }, func(map[string]doc) {})
	edit("входы роли", func(m map[string]any) { m["input_kinds"] = []any{"dev.change_summary", "analysis.brief"} }, func(map[string]doc) {})

	// AC-10, AC-12: связка формата 1 со строкой instructions (как стартовая
	// связка пакета) и образец справки идут через --file как раньше.
	starter := filepath.Join(t.TempDir(), "starter.json")
	write(starter, `{"version": 1, "roles": [{"key": "qa", "name": "QA", "instructions": "Проверяй по критериям."}]}`)
	if dry := run("--file", starter, "--dry-run"); dry.role("qa") != "created" {
		t.Fatalf("формат 1, dry-run: %+v", dry.Roles)
	}
	run("--file", starter)
	if _, docs := latest("qa"); len(docs) != 1 || docs["main.md"].Kind != "instructions" {
		t.Fatalf("формат 1 — не один main.md: %+v", docs)
	}
	example := filepath.Join(t.TempDir(), "example.json")
	write(example, importguide.Example)
	if dry := run("--file", example, "--dry-run"); dry.role("tech-writer") != "created" {
		t.Fatalf("образец справки: %+v", dry.Roles)
	}

	// Роль из формата 1 тоже выгружается каталогом и возвращается unchanged.
	final := filepath.Join(t.TempDir(), "final")
	if err := exportDir(t, c, final, "--project", "DIRS"); err != nil {
		t.Fatal(err)
	}
	dry = run("--dir", final, "--dry-run")
	keys := []string{}
	for _, e := range dry.Roles {
		if e.Outcome != "unchanged" {
			t.Errorf("%s: %s", e.Key, e.Outcome)
		}
		keys = append(keys, e.Key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"analyst", "qa", "writer"}) {
		t.Fatalf("роли каталога: %v", keys)
	}
}

// AC-10: сервер прежней версии отвечает на формат 3 своим отказом — CLI
// показывает его текст в отчёте, как для --file.
func TestImportDirOldServer(t *testing.T) {
	withJSON(t, false)
	stub, _ := newBundleStub(t, exportFormat3)
	dir := t.TempDir()
	if err := exportDir(t, stub, dir); err != nil {
		t.Fatal(err)
	}
	c, rec := newInviteClient(t, http.StatusBadRequest,
		`{"error":{"code":"invalid_input","message":"связка версии 3 новее поддерживаемой 2"}}`)
	out, err := importDir(t, c, "--project", "CLAB", "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "связка версии 3 новее поддерживаемой 2") || rec.calls != 1 || rec.body["version"] != float64(3) {
		t.Fatalf("отчёт: %s, запросов %d", out, rec.calls)
	}
}
