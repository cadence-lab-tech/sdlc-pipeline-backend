package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/importguide"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Каталог связки: clab export --dir и clab import --dir (CLAB-345). Номера —
// критерии приёмки анализа CLAB-345.

// bundleStub — заглушка сервера: экспорт отдаёт заданную связку, импорт
// записывает тело и отвечает отчётом; проект из failing отвечает 403.
type bundleStub struct {
	mu       sync.Mutex
	export   string
	failing  map[string]bool
	calls    int
	imports  []stubImport
	exported int
}

type stubImport struct {
	path, query string
	body        map[string]json.RawMessage
}

func newBundleStub(t *testing.T, export string) (*Client, *bundleStub) {
	t.Helper()
	st := &bundleStub{export: export, failing: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.calls++
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(r.URL.Path, "/") // "", v1, projects, KEY, export|import
		if len(parts) != 5 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if st.failing[parts[3]] {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"нет доступа к проекту"}}`))
			return
		}
		switch {
		case r.Method == "GET" && parts[4] == "export":
			st.exported++
			_, _ = w.Write([]byte(st.export))
		case r.Method == "POST" && parts[4] == "import":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("тело импорта не JSON: %s", raw)
			}
			st.imports = append(st.imports, stubImport{path: r.URL.Path, query: r.URL.RawQuery, body: body})
			_, _ = w.Write([]byte(`{"dry_run":true,"roles":[],"documents":[],"repositories":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: "https://ui.example", actor: "user:alice", http: srv.Client()}, st
}

// exportFormat3 — ответ экспорта формата 3: две роли, у writer документы с
// содержимым на все случаи побайтного сравнения, цикл с полем, которого CLI
// не знает.
const exportFormat3 = `{
  "version": 3,
  "project": {"key": "CLAB", "name": "Cadence Lab"},
  "repositories": [{"repository": "acme/api", "role_hint": "backend", "index_enabled": true}],
  "roles": [
    {"key": "writer", "name": "Техписатель", "summary": "Пишет <доки> & примеры",
     "documents": [
       {"name": "main.md", "kind": "instructions", "content": "Главное.\n"},
       {"name": "crlf.md", "kind": "reference", "content": "строка\r\nстрока\r\n"},
       {"name": "bom.md", "kind": "instructions", "content": "\ufeffс BOM"},
       {"name": "nonl.md", "kind": "checklist", "content": "без перевода строки — 日本語 <b>&amp;</b>"}
     ],
     "input_kinds": ["dev.change_summary"],
     "output_contract": [{"kind": "docs.page", "title": "Страница", "format": "markdown", "required": true}],
     "tool_policy": {"deny": ["Bash"]},
     "model_hint": "claude-sonnet", "model_level": "medium",
     "repo_scope": ["acme/api"], "context_budget": {"max_tokens": 120000}},
    {"key": "analyst", "name": "Аналитик",
     "documents": [{"name": "main.md", "kind": "instructions", "content": "Требования."}]}
  ],
  "documents": [{"kind": "guideline", "title": "Соглашения", "format": "markdown", "content": "a -> b & c"}],
  "workflows": [{"name": "Simple", "task_types": ["task"],
    "statuses": [{"key": "new", "name": "Новая", "category": "todo", "is_initial": true, "role_documents": ["main.md"], "wip_limit": 3}],
    "transitions": []}]
}`

func exportDir(t *testing.T, c *Client, dir string, extra ...string) error {
	t.Helper()
	_, _, err := capture(t, func() error {
		return cmdExport(context.Background(), c, append([]string{"--project", "CLAB", "--dir", dir}, extra...))
	})
	return err
}

func dirFileText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// treeOf — файлы дерева с sha256 содержимого.
func treeOf(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		sum := sha256.Sum256(raw)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// AC-1, AC-2: раскладка, побайтное содержимое документов, role.json без
// содержимого и с полями роли, детерминированный вывод.
func TestExportDirLayout(t *testing.T) {
	withJSON(t, false)
	c, st := newBundleStub(t, exportFormat3)
	dir := filepath.Join(t.TempDir(), "rules")
	if err := exportDir(t, c, dir); err != nil {
		t.Fatal(err)
	}

	var bundle map[string]json.RawMessage
	if err := json.Unmarshal([]byte(dirFileText(t, filepath.Join(dir, "bundle.json"))), &bundle); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "project", "repositories", "documents", "workflows"} {
		if _, ok := bundle[k]; !ok {
			t.Errorf("в bundle.json нет %s", k)
		}
	}
	if _, ok := bundle["roles"]; ok || string(bundle["version"]) != "3" {
		t.Fatalf("bundle.json: roles или version: %s", bundle["version"])
	}
	text := dirFileText(t, filepath.Join(dir, "bundle.json"))
	if !strings.HasPrefix(text, "{\n  \"version\": 3,\n  \"project\"") || !strings.HasSuffix(text, "}\n") {
		t.Fatalf("порядок ключей или перевод строки в конце:\n%s", text)
	}
	if !strings.Contains(text, `"a -> b & c"`) || !strings.Contains(text, `"role_documents"`) {
		t.Fatalf("содержимое экранировано или неизвестное поле цикла потеряно:\n%s", text)
	}

	var exported struct {
		Roles []struct {
			Key       string `json:"key"`
			Documents []struct{ Name, Content string }
		} `json:"roles"`
	}
	if err := json.Unmarshal([]byte(exportFormat3), &exported); err != nil {
		t.Fatal(err)
	}
	for _, r := range exported.Roles {
		for _, d := range r.Documents {
			got := dirFileText(t, filepath.Join(dir, "roles", r.Key, d.Name))
			if sha256.Sum256([]byte(got)) != sha256.Sum256([]byte(d.Content)) {
				t.Errorf("%s/%s: %q вместо %q", r.Key, d.Name, got, d.Content)
			}
		}
	}

	manifest := dirFileText(t, filepath.Join(dir, "roles", "writer", "role.json"))
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(manifest), &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"key", "name", "summary", "input_kinds", "output_contract", "tool_policy",
		"model_hint", "model_level", "repo_scope", "context_budget", "documents"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("в role.json нет %s", k)
		}
	}
	if _, ok := fields["instructions"]; ok || strings.Contains(manifest, "content") {
		t.Fatalf("в role.json содержимое: %s", manifest)
	}
	if string(fields["documents"]) == "" || !strings.Contains(manifest, `"name": "main.md",`) {
		t.Fatalf("documents: %s", manifest)
	}
	var docs []roleDirDocument
	_ = json.Unmarshal(fields["documents"], &docs)
	want := []roleDirDocument{{Name: "main.md", Kind: "instructions"}, {Name: "crlf.md", Kind: "reference"}, {Name: "bom.md", Kind: "instructions"}, {Name: "nonl.md", Kind: "checklist"}}
	if !reflect.DeepEqual(docs, want) {
		t.Fatalf("documents %v, ждали %v", docs, want)
	}
	if !strings.HasPrefix(manifest, "{\n  \"key\": \"writer\",\n  \"name\"") || !strings.Contains(manifest, "<доки> & примеры") {
		t.Fatalf("порядок полей role.json или экранирование:\n%s", manifest)
	}

	// Второй экспорт в другой каталог — то же дерево побайтно.
	dir2 := filepath.Join(t.TempDir(), "again")
	if err := exportDir(t, c, dir2); err != nil {
		t.Fatal(err)
	}
	if a, b := treeOf(t, dir), treeOf(t, dir2); !reflect.DeepEqual(a, b) || len(a) != 8 {
		t.Fatalf("деревья разошлись или не те файлы:\n%v\n%v", a, b)
	}
	if st.exported != 2 {
		t.Fatalf("экспортов %d", st.exported)
	}
}

// AC-8: непустой каталог без --force, замена с --force, ключи-пути,
// регистр, сервер прежнего формата — ничего не записано.
func TestExportDirRefuses(t *testing.T) {
	withJSON(t, false)
	c, _ := newBundleStub(t, exportFormat3)
	dir := t.TempDir()
	if err := exportDir(t, c, dir); err != nil {
		t.Fatal(err)
	}
	// Устаревшие роль и документ на диске, посторонний файл рядом.
	stale := filepath.Join(dir, "roles", "gone")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(stale, "role.json"):               `{}`,
		filepath.Join(dir, "roles", "writer", "old.md"): "старое",
		filepath.Join(dir, "notes.txt"):                 "заметки",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := treeOf(t, dir)
	err := exportDir(t, c, dir)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("непустой каталог: %v", err)
	}
	if !reflect.DeepEqual(before, treeOf(t, dir)) {
		t.Fatal("каталог изменился без --force")
	}
	if err := exportDir(t, c, dir, "--force"); err != nil {
		t.Fatal(err)
	}
	after := treeOf(t, dir)
	if _, ok := after["roles/gone/role.json"]; ok {
		t.Fatal("устаревшая роль осталась")
	}
	if _, ok := after["roles/writer/old.md"]; ok {
		t.Fatal("устаревший документ остался")
	}
	if after["notes.txt"] != before["notes.txt"] || len(after) != 9 {
		t.Fatalf("посторонний файл или состав: %v", after)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".clab-export-") {
			t.Fatalf("временный каталог остался: %s", e.Name())
		}
	}

	role := func(key string) string {
		return `{"key": "` + key + `", "name": "x", "documents": [{"name": "main.md", "kind": "instructions", "content": "x"}]}`
	}
	for name, tc := range map[string]struct{ export, want string }{
		"../x":       {`{"version": 3, "roles": [` + role("../x") + `]}`, `"../x"`},
		"a/b":        {`{"version": 3, "roles": [` + role("a/b") + `]}`, `"a/b"`},
		"a\\b":       {`{"version": 3, "roles": [` + role(`a\\b`) + `]}`, `разделитель`},
		".hidden":    {`{"version": 3, "roles": [` + role(".hidden") + `]}`, `".hidden"`},
		"Dev и dev":  {`{"version": 3, "roles": [` + role("Dev") + `, ` + role("dev") + `]}`, `"Dev" и "dev"`},
		"формат 2":   {`{"version": 2, "roles": [{"key": "a", "name": "A", "instructions": "x"}]}`, "формата 2, каталог связки требует формата 3"},
		"без версии": {`{"roles": []}`, "формата 0"},
		"instructions в формате 3": {`{"version": 3, "roles": [{"key": "a", "name": "A", "instructions": "x"}]}`,
			"обновите сервер или выгрузите JSON"},
		"неизвестное поле роли": {`{"version": 3, "roles": [{"key": "a", "name": "A", "persona": "x", "documents": [{"name": "main.md", "content": "x"}]}]}`,
			"persona"},
		"роль без документов": {`{"version": 3, "roles": [{"key": "a", "name": "A"}]}`, "нет ни одного документа"},
	} {
		c, _ := newBundleStub(t, tc.export)
		target := filepath.Join(t.TempDir(), "new")
		err := exportDir(t, c, target)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, ждали %q", name, err, tc.want)
		}
		if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
			t.Errorf("%s: каталог создан", name)
		}
	}

	// --force без --dir — ошибка до запроса.
	c, st := newBundleStub(t, exportFormat3)
	if _, _, err := capture(t, func() error {
		return cmdExport(context.Background(), c, []string{"--project", "CLAB", "--force"})
	}); err == nil || st.calls != 0 {
		t.Fatalf("--force без --dir: %v, запросов %d", err, st.calls)
	}
}

func importDir(t *testing.T, c *Client, args ...string) (string, error) {
	t.Helper()
	out, _, err := capture(t, func() error { return cmdImport(context.Background(), c, args) })
	return out, err
}

// AC-3 (на заглушке), AC-13, R2: каталог из экспорта уходит одной связкой
// формата 3 — роли с документами байт в байт, bundle.json как есть, включая
// поле цикла, которого CLI не знает.
func TestImportDirSendsBundle(t *testing.T) {
	withJSON(t, false)
	c, st := newBundleStub(t, exportFormat3)
	dir := t.TempDir()
	if err := exportDir(t, c, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := importDir(t, c, "--project", "CLAB", "--dir", dir, "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if len(st.imports) != 1 || st.imports[0].query != "dry_run=true" || st.imports[0].path != "/v1/projects/CLAB/import" {
		t.Fatalf("запросы импорта: %+v", st.imports)
	}
	sent := st.imports[0].body
	if string(sent["version"]) != "3" {
		t.Fatalf("version %s", sent["version"])
	}

	var want, got struct {
		Workflows    any `json:"workflows"`
		Documents    any `json:"documents"`
		Repositories any `json:"repositories"`
		Project      any `json:"project"`
	}
	_ = json.Unmarshal([]byte(dirFileText(t, filepath.Join(dir, "bundle.json"))), &want)
	raw, _ := json.Marshal(sent)
	_ = json.Unmarshal(raw, &got)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("bundle.json ушёл не как есть:\n%v\n%v", want, got)
	}
	if !strings.Contains(string(sent["workflows"]), `"role_documents"`) {
		t.Fatalf("неизвестное поле цикла потеряно: %s", sent["workflows"])
	}

	// Роли — те же, что в экспорте, по смыслу; документы байт в байт.
	var exported, sentRoles struct {
		Roles []map[string]any `json:"roles"`
	}
	_ = json.Unmarshal([]byte(exportFormat3), &exported)
	_ = json.Unmarshal(raw, &sentRoles)
	sort.Slice(exported.Roles, func(i, j int) bool { return exported.Roles[i]["key"].(string) < exported.Roles[j]["key"].(string) })
	if !reflect.DeepEqual(exported.Roles, sentRoles.Roles) {
		t.Fatalf("роли разошлись с экспортом:\n%v\n%v", exported.Roles, sentRoles.Roles)
	}

	// Каталог только с roles/ — принимается и несёт только роли.
	if err := os.Remove(filepath.Join(dir, "bundle.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := importDir(t, c, "--project", "CLAB", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	only := st.imports[1].body
	if _, has := only["workflows"]; has || string(only["version"]) != "3" || only["roles"] == nil || st.imports[1].query != "" {
		t.Fatalf("каталог только с ролями: %v", only)
	}

	// key в role.json не обязателен: ключ — имя каталога.
	manifest := filepath.Join(dir, "roles", "analyst", "role.json")
	if err := os.WriteFile(manifest, []byte(`{"name": "Аналитик", "documents": [{"name": "main.md"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := importDir(t, c, "--project", "CLAB", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(st.imports[2].body["roles"]), `"key":"analyst"`) {
		t.Fatalf("ключ из имени каталога: %s", st.imports[2].body["roles"])
	}
}

// AC-6, AC-7: изъяны каталога — ошибка с ролью, файлом и причиной, на сервер
// ничего не уходит.
func TestImportDirRejects(t *testing.T) {
	withJSON(t, false)
	c, _ := newBundleStub(t, exportFormat3)
	template := t.TempDir()
	if err := exportDir(t, c, template); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("чужой файл"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer := func(d string) string { return filepath.Join(d, "roles", "writer") }
	setManifest := func(body string) func(string) error {
		return func(d string) error { return os.WriteFile(filepath.Join(writer(d), "role.json"), []byte(body), 0o644) }
	}
	docsLine := `"documents": [{"name": "main.md"}, {"name": "crlf.md"}, {"name": "bom.md"}, {"name": "nonl.md"}]`

	cases := []struct {
		name   string
		break_ func(d string) error
		want   []string
	}{
		{".md без строки", func(d string) error {
			return os.WriteFile(filepath.Join(writer(d), "git.md"), []byte("x"), 0o644)
		}, []string{"роль writer", "git.md", "без строки в role.json"}},
		{"строка без файла", func(d string) error {
			return os.Remove(filepath.Join(writer(d), "bom.md"))
		}, []string{"роль writer", "bom.md", "файла"}},
		{"дубль имени", setManifest(`{"key": "writer", "name": "W", "documents": [{"name": "main.md"}, {"name": "main.md"}]}`),
			[]string{"роль writer", "main.md", "дважды"}},
		{"имя не по форме", setManifest(`{"key": "writer", "name": "W", "documents": [{"name": "Git.md"}]}`),
			[]string{"роль writer", "Git.md"}},
		{"имя с каталогом", setManifest(`{"key": "writer", "name": "W", "documents": [{"name": "a/b.md"}]}`),
			[]string{"роль writer", "a/b.md"}},
		{"нет role.json", func(d string) error { return os.Remove(filepath.Join(writer(d), "role.json")) },
			[]string{"роль writer", "role.json"}},
		{"подкаталог в роли", func(d string) error { return os.Mkdir(filepath.Join(writer(d), "drafts"), 0o755) },
			[]string{"роль writer", "drafts", "подкаталог"}},
		{"instructions", setManifest(`{"key": "writer", "name": "W", "instructions": "x", ` + docsLine + `}`),
			[]string{"роль writer", "instructions", "текст роли лежит документами"}},
		{"неизвестное поле", setManifest(`{"key": "writer", "name": "W", "model_levl": "high", ` + docsLine + `}`),
			[]string{"роль writer", "model_levl", "неизвестное поле"}},
		{"content в строке documents", setManifest(`{"key": "writer", "name": "W", "documents": [{"name": "main.md", "content": "x"}, {"name": "crlf.md"}, {"name": "bom.md"}, {"name": "nonl.md"}]}`),
			[]string{"роль writer", "content"}},
		{"key не равен каталогу", setManifest(`{"key": "editor", "name": "W", ` + docsLine + `}`),
			[]string{"роль writer", `"editor"`, "не равен имени каталога"}},
		{"roles в bundle.json", func(d string) error {
			return os.WriteFile(filepath.Join(d, "bundle.json"), []byte(`{"version": 3, "roles": []}`), 0o644)
		}, []string{"bundle.json", "roles/<ключ>/"}},
		{"ссылка вместо .md за пределы каталога", func(d string) error {
			path := filepath.Join(writer(d), "main.md")
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(outside, path)
		}, []string{"main.md", "символьная ссылка"}},
		{"ссылка вместо role.json", func(d string) error {
			path := filepath.Join(writer(d), "role.json")
			target := filepath.Join(t.TempDir(), "role.json")
			raw, _ := os.ReadFile(path)
			if err := os.WriteFile(target, raw, 0o644); err != nil {
				return err
			}
			_ = os.Remove(path)
			return os.Symlink(target, path)
		}, []string{"role.json", "символьная ссылка"}},
		{"ссылка вместо каталога роли", func(d string) error {
			return os.Symlink(writer(d), filepath.Join(d, "roles", "copy"))
		}, []string{"copy", "только каталоги ролей"}},
		{"файл в roles/", func(d string) error {
			return os.WriteFile(filepath.Join(d, "roles", "README.md"), []byte("x"), 0o644)
		}, []string{"README.md", "только каталоги ролей"}},
		{"нет ни bundle.json, ни roles/", func(d string) error {
			if err := os.RemoveAll(filepath.Join(d, "roles")); err != nil {
				return err
			}
			return os.Remove(filepath.Join(d, "bundle.json"))
		}, []string{"не похоже на каталог связки"}},
		{"version не число", func(d string) error {
			return os.WriteFile(filepath.Join(d, "bundle.json"), []byte(`{"version": "три"}`), 0o644)
		}, []string{"version"}},
	}
	for _, tc := range cases {
		dir := filepath.Join(t.TempDir(), "rules")
		if err := os.CopyFS(dir, os.DirFS(template)); err != nil {
			t.Fatal(err)
		}
		if err := tc.break_(dir); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		c, st := newBundleStub(t, exportFormat3)
		_, err := importDir(t, c, "--project", "CLAB", "--dir", dir, "--dry-run")
		if err == nil {
			t.Errorf("%s: ошибки нет", tc.name)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: в %q нет %q", tc.name, err, w)
			}
		}
		if strings.Contains(err.Error(), "чужой файл") {
			t.Errorf("%s: содержимое по ссылке прочитано", tc.name)
		}
		if st.calls != 0 {
			t.Errorf("%s: запросов к серверу %d", tc.name, st.calls)
		}
	}

	c, st := newBundleStub(t, exportFormat3)
	if _, err := importDir(t, c, "--project", "CLAB", "--dir", filepath.Join(t.TempDir(), "nope")); err == nil ||
		!strings.Contains(err.Error(), "nope") || st.calls != 0 {
		t.Fatalf("несуществующий каталог: %v, запросов %d", err, st.calls)
	}
}

// AC-9: --dir несовместим с --file и --from; --to читает каталог один раз,
// ошибка одного проекта не обрывает остальные, отчёт в том же виде.
func TestImportDirFlags(t *testing.T) {
	withJSON(t, false)
	c, _ := newBundleStub(t, exportFormat3)
	dir := t.TempDir()
	if err := exportDir(t, c, dir); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(file, []byte(`{"version": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"--dir и --file": {"--project", "CLAB", "--dir", dir, "--file", file},
		"--dir и --from": {"--project", "CLAB", "--dir", dir, "--from", "TPL"},
	} {
		c, st := newBundleStub(t, exportFormat3)
		if _, err := importDir(t, c, args...); err == nil || !strings.Contains(err.Error(), "вместе не задаются") || st.calls != 0 {
			t.Errorf("%s: %v, запросов %d", name, err, st.calls)
		}
	}

	c, st := newBundleStub(t, exportFormat3)
	st.failing["B"] = true
	out, err := importDir(t, c, "--dir", dir, "--to", "A,B,C", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.imports) != 2 || st.calls != 3 {
		t.Fatalf("импортов %d, запросов %d", len(st.imports), st.calls)
	}
	var report struct {
		Imports []map[string]any `json:"imports"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("отчёт не JSON: %s", out)
	}
	if len(report.Imports) != 3 || report.Imports[1]["project"] != "B" || report.Imports[1]["error"] == nil ||
		report.Imports[2]["project"] != "C" || report.Imports[2]["error"] != nil {
		t.Fatalf("отчёт: %v", report.Imports)
	}
	if string(st.imports[0].body["roles"]) != string(st.imports[1].body["roles"]) {
		t.Fatal("проекты получили разные связки")
	}
}

// AC-10: JSON-путь прежний — export без --dir печатает ответ сервера, import
// --file шлёт файл как есть, в том числе связку формата 1 со строкой
// instructions (стартовая связка пакета).
func TestJSONPathUnchanged(t *testing.T) {
	withJSON(t, false)
	c, st := newBundleStub(t, exportFormat3)
	out, _, err := capture(t, func() error { return cmdExport(context.Background(), c, []string{"--project", "CLAB"}) })
	if err != nil {
		t.Fatal(err)
	}
	var printed, served map[string]any
	_ = json.Unmarshal([]byte(out), &printed)
	_ = json.Unmarshal([]byte(exportFormat3), &served)
	if !reflect.DeepEqual(printed, served) {
		t.Fatalf("export без --dir изменил ответ:\n%s", out)
	}

	starter := `{"version": 1, "roles": [{"key": "analyst", "name": "Аналитик", "instructions": "Требования."}]}`
	file := filepath.Join(t.TempDir(), "starter.json")
	if err := os.WriteFile(file, []byte(starter), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := importDir(t, c, "--project", "CLAB", "--file", file, "--dry-run"); err != nil {
		t.Fatal(err)
	}
	sent := st.imports[0].body
	if string(sent["version"]) != "1" || !strings.Contains(string(sent["roles"]), `"instructions":"Требования."`) {
		t.Fatalf("--file изменил связку: %v", sent)
	}
}

// AC-14: role.json несёт все поля роли связки, кроме instructions и
// содержимого документов. Добавят поле в workflow.BundleRole — тест красный,
// пока экспорт и импорт каталогом его не знают.
func TestRoleManifestCoversBundleRole(t *testing.T) {
	tags := func(typ reflect.Type, skip ...string) []string {
		var out []string
		for i := range typ.NumField() {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			skipped := false
			for _, s := range skip {
				skipped = skipped || s == name
			}
			if !skipped {
				out = append(out, name)
			}
		}
		sort.Strings(out)
		return out
	}
	want := tags(reflect.TypeFor[workflow.BundleRole](), "instructions")
	got := tags(reflect.TypeFor[roleManifest]())
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("поля role.json %v, у workflow.BundleRole %v", got, want)
	}
	// instruction_version — ссылка номером без содержимого (sdlc_put_role):
	// в каталоге содержимое лежит файлом, и версия находится по его sha.
	if want, got := tags(reflect.TypeFor[workflow.BundleRoleDocument](), "content", "instruction_version"), tags(reflect.TypeFor[roleDirDocument]()); !reflect.DeepEqual(want, got) {
		t.Fatalf("строка documents %v, у workflow.BundleRoleDocument %v", got, want)
	}
	// Библиотека (CLAB-351): instruction.json несёт все поля инструкции и
	// версии связки, кроме содержимого версии — оно лежит файлом v<N>.md.
	if want, got := tags(reflect.TypeFor[workflow.BundleInstruction]()), tags(reflect.TypeFor[instructionManifest]()); !reflect.DeepEqual(want, got) {
		t.Fatalf("поля instruction.json %v, у workflow.BundleInstruction %v", got, want)
	}
	if want, got := tags(reflect.TypeFor[workflow.BundleInstructionVersion](), "content"), tags(reflect.TypeFor[instructionDirVersion]()); !reflect.DeepEqual(want, got) {
		t.Fatalf("версия в instruction.json %v, у workflow.BundleInstructionVersion %v", got, want)
	}
}

// AC-11, AC-12: clab import-guide печатает тот же текст, что MCP, и своей
// копии справки в cmd/clab нет; справка — про документы роли и каталог.
func TestImportGuideSingleSource(t *testing.T) {
	out, _, err := capture(t, func() error { return cmdImportGuide(context.Background(), nil, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if out != importguide.Text {
		t.Fatal("clab import-guide печатает не importguide.Text")
	}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if strings.Contains(dirFileText(t, f), "Перенос правил команды в проект") {
			t.Errorf("%s: своя копия справки импорта", f)
		}
	}
}

// AC-12: справка CLI показывает флаги каталога.
func TestHelpShowsDirFlags(t *testing.T) {
	usage := map[string]string{}
	for _, c := range commands() {
		usage[c.name] = c.usage
	}
	if !strings.Contains(usage["export"], "--dir") || !strings.Contains(usage["export"], "--force") ||
		!strings.Contains(usage["import"], "--dir") {
		t.Fatalf("export: %s\nimport: %s", usage["export"], usage["import"])
	}
}
