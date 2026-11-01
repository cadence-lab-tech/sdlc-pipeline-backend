package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Библиотека инструкций в CLI (CLAB-351): каталог связки (AC-15) и
// clab instruction (AC-12). Номера — критерии приёмки анализа CLAB-351.

const exportWithLibrary = `{
  "version": 3,
  "project": {"key": "CLAB", "name": "Cadence Lab"},
  "roles": [
    {"key": "dev", "name": "Разработчик",
     "documents": [
       {"name": "main.md", "kind": "instructions", "content": "Ты разработчик.\n"},
       {"name": "git.md", "kind": "instructions", "content": "Ветки v2.\n", "instruction": "git"}
     ]}
  ],
  "instructions": [
    {"key": "git", "name": "Правила git", "summary": "ветки",
     "versions": [
       {"version": 1, "kind": "instructions", "content": "Ветки v1.\n", "changelog": "вынесено"},
       {"version": 2, "kind": "instructions", "content": "Ветки v2.\n"}
     ]}
  ]
}`

// AC-15: export --dir пишет instructions/<ключ>/ и instruction в role.json;
// import --dir отправляет ту же связку; лишний и пропавший файл — отказ.
func TestInstructionsInBundleDir(t *testing.T) {
	withJSON(t, false)
	c, st := newBundleStub(t, exportWithLibrary)
	dir := t.TempDir()
	if err := exportDir(t, c, dir); err != nil {
		t.Fatal(err)
	}
	tree := treeOf(t, dir)
	for _, f := range []string{"bundle.json", "roles/dev/role.json", "roles/dev/git.md",
		"instructions/git/instruction.json", "instructions/git/v1.md", "instructions/git/v2.md"} {
		if _, ok := tree[f]; !ok {
			t.Fatalf("нет %s: %v", f, tree)
		}
	}
	if strings.Contains(dirFileText(t, filepath.Join(dir, "bundle.json")), "instructions") {
		t.Fatal("instructions попали в bundle.json")
	}
	if got := dirFileText(t, filepath.Join(dir, "instructions/git/v1.md")); got != "Ветки v1.\n" {
		t.Fatalf("v1.md: %q", got)
	}
	if role := dirFileText(t, filepath.Join(dir, "roles/dev/role.json")); !strings.Contains(role, `"instruction": "git"`) {
		t.Fatalf("role.json без instruction: %s", role)
	}
	manifest := dirFileText(t, filepath.Join(dir, "instructions/git/instruction.json"))
	if strings.Contains(manifest, "content") || !strings.Contains(manifest, `"changelog": "вынесено"`) {
		t.Fatalf("instruction.json: %s", manifest)
	}

	if _, err := importDir(t, c, "--project", "CLAB", "--dir", dir, "--dry-run"); err != nil {
		t.Fatal(err)
	}
	var want, got struct {
		Roles        any `json:"roles"`
		Instructions any `json:"instructions"`
	}
	_ = json.Unmarshal([]byte(exportWithLibrary), &want)
	raw, _ := json.Marshal(st.imports[0].body)
	_ = json.Unmarshal(raw, &got)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("связка из каталога разошлась с экспортом:\n%v\n%v", want, got)
	}

	sent := len(st.imports)
	extra := filepath.Join(dir, "instructions/git/draft.md")
	if err := os.WriteFile(extra, []byte("черновик"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := importDir(t, c, "--project", "CLAB", "--dir", dir); err == nil || !strings.Contains(err.Error(), "draft.md") {
		t.Fatalf("лишний файл: %v", err)
	}
	_ = os.Remove(extra)
	if err := os.Remove(filepath.Join(dir, "instructions/git/v2.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := importDir(t, c, "--project", "CLAB", "--dir", dir); err == nil || !strings.Contains(err.Error(), "v2.md") {
		t.Fatalf("пропавший файл: %v", err)
	}
	if len(st.imports) != sent {
		t.Fatal("каталог с изъяном ушёл на сервер")
	}
}

// libraryStub — сервер с одной инструкцией git; old — сервер без
// библиотеки (404 маршрута без конверта).
type libraryStub struct {
	mu    sync.Mutex
	old   bool
	posts []string
	body  map[string]any
}

func newLibraryStub(t *testing.T, old bool) (*Client, *libraryStub) {
	t.Helper()
	st := &libraryStub{old: old}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.old {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			st.posts = append(st.posts, r.Method+" "+r.URL.Path)
			raw, _ := io.ReadAll(r.Body)
			st.body = map[string]any{}
			_ = json.Unmarshal(raw, &st.body)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/projects/CLAB/instructions":
			_, _ = w.Write([]byte(`{"instructions":[{"key":"git","name":"Правила git","latest_version":2,"pinned_roles":2,"lagging_roles":1}]}`))
		case "GET /v1/projects/CLAB/instructions/git":
			_, _ = w.Write([]byte(`{"instruction":{"key":"git","name":"Правила git","latest_version":2},
				"versions":[{"version":2},{"version":1}],
				"version":{"version":2,"kind":"instructions","sha256":"a1b2c3d4e5","size_bytes":11,"content":"Ветки v2.\n"},
				"usage":[{"role_key":"dev","role_version":20,"pinned_version":1},{"role_key":"test","role_version":13,"pinned_version":null}]}`))
		case "GET /v1/projects/CLAB/instructions/fmt":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"инструкции \"fmt\" нет в проекте"}}`))
		case "POST /v1/projects/CLAB/instructions":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"instruction":{"key":"fmt"},"version":{"version":1,"kind":"instructions","sha256":"ffff000","size_bytes":3},"created":true}`))
		case "POST /v1/projects/CLAB/instructions/git/versions":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"instruction":{"key":"git"},"version":{"version":3,"kind":"instructions","sha256":"ab12cd3","size_bytes":640}}`))
		case "POST /v1/projects/CLAB/instructions/git/apply":
			_, _ = w.Write([]byte(`{"instruction":"git","version":2,"roles":[
				{"role":"dev","outcome":"created","role_version":21,"from_version":1,"to_version":2},
				{"role":"integ","outcome":"unchanged","role_version":4,"from_version":2,"to_version":2}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"нет"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: "https://ui.example", actor: "user:alice", http: srv.Client()}, st
}

func runInstruction(t *testing.T, c *Client, args ...string) (string, error) {
	t.Helper()
	out, _, err := capture(t, func() error { return cmdInstruction(context.Background(), c, args) })
	return out, err
}

// AC-12: таблицы, put заводит или дописывает версию, apply печатает итог,
// сервер без библиотеки — понятный отказ.
func TestInstructionCommands(t *testing.T) {
	withJSON(t, false)
	t.Setenv("SDLC_PROJECT", "CLAB")
	c, st := newLibraryStub(t, false)

	out, err := runInstruction(t, c, "list")
	if err != nil || !strings.Contains(out, "KEY") || !strings.Contains(out, "ЗАКРЕПЛЕНА") || !strings.Contains(out, "v2") {
		t.Fatalf("list: %q %v", out, err)
	}
	out, err = runInstruction(t, c, "show", "git")
	if err != nil || !strings.Contains(out, "git v1 (есть v2)") || !strings.Contains(out, "не закреплён") ||
		!strings.Contains(out, "--- git v2 (instructions, 11 байт, sha256 a1b2c3d) ---\nВетки v2.") {
		t.Fatalf("show: %q %v", out, err)
	}

	file := filepath.Join(t.TempDir(), "git.md")
	if err := os.WriteFile(file, []byte("новый текст"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runInstruction(t, c, "put", "--key", "git", "--file", file, "--changelog", "правка")
	if err != nil || !strings.Contains(out, "инструкция git: версия 3 создана") ||
		st.posts[len(st.posts)-1] != "POST /v1/projects/CLAB/instructions/git/versions" || st.body["changelog"] != "правка" {
		t.Fatalf("put существующей: %q %v %v", out, err, st.posts)
	}
	out, err = runInstruction(t, c, "put", "--key", "fmt", "--file", file)
	if err != nil || !strings.Contains(out, "инструкция fmt заведена, версия 1") ||
		st.posts[len(st.posts)-1] != "POST /v1/projects/CLAB/instructions" || st.body["name"] != "fmt" {
		t.Fatalf("put новой: %q %v %v", out, err, st.posts)
	}

	if _, err := runInstruction(t, c, "apply", "git"); err == nil || !strings.Contains(err.Error(), "не указана роль") {
		t.Fatalf("apply без роли: %v", err)
	}
	out, err = runInstruction(t, c, "apply", "git", "--role", "dev", "--role", "integ")
	if err != nil || !strings.Contains(out, "создана") || !strings.Contains(out, "без изменений") || !strings.Contains(out, "git v1 → v2") {
		t.Fatalf("apply: %q %v", out, err)
	}
	if roles, _ := st.body["roles"].([]any); len(roles) != 2 {
		t.Fatalf("роли в запросе apply: %v", st.body)
	}

	withJSON(t, true)
	out, err = runInstruction(t, c, "list")
	if err != nil || !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("--json: %q %v", out, err)
	}

	old, _ := newLibraryStub(t, true)
	if _, err := runInstruction(t, old, "list"); err == nil || !strings.Contains(err.Error(), "сервер не знает библиотеку инструкций") {
		t.Fatalf("старый сервер: %v", err)
	}
}
