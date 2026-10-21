package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CLAB-232: clab setup. Сервер — подставной httptest, claude — подставной
// скрипт в PATH, который пишет свои аргументы в журнал и помнит адрес
// подключённого сервера в файле состояния.

const setupToken = "sdlc_setup-secret-token-42"

type kitFile struct{ path, content string }

func kitJSON(version string, files ...kitFile) map[string]any {
	out := []map[string]string{}
	for _, f := range files {
		sum := sha256.Sum256([]byte(f.content))
		out = append(out, map[string]string{"path": f.path, "content": f.content, "sha256": hex.EncodeToString(sum[:])})
	}
	return map[string]any{"version": version, "files": out, "skills": []any{}, "agents": []any{}}
}

type setupServer struct {
	kit     map[string]any // nil — сервер без /v1/agent-kit (404)
	version string
	repos   map[string]string // owner/name → ключ проекта
}

func (s *setupServer) start(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent-kit":
			if s.kit == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(s.kit)
		case "/v1/instance":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": s.version})
		case "/v1/projects":
			projects := []map[string]string{}
			if key, ok := s.repos[r.URL.Query().Get("repo")]; ok {
				projects = append(projects, map[string]string{"key": key})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"projects": projects})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: srv.URL, actor: "user:alice", token: setupToken, http: srv.Client()}
}

// fakeClaude ставит подставной claude первым в PATH. state — адрес, под
// которым сервер уже подключён (пусто — не подключён).
type fakeClaude struct{ dir, log, state, cwd string }

func installClaude(t *testing.T, state string) *fakeClaude {
	t.Helper()
	d := t.TempDir()
	fc := &fakeClaude{dir: d, log: filepath.Join(d, "log"), state: filepath.Join(d, "state"), cwd: filepath.Join(d, "cwd")}
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + fc.log + `
case "$2" in
get)
  if [ -f ` + fc.state + ` ]; then
    printf '%s:\n  Scope: Local config (private to you in this project)\n  Status: connected\n  Type: http\n  URL: %s\n  Headers:\n    Authorization: Bearer old\n' "$3" "$(cat ` + fc.state + `)"
    exit 0
  fi
  echo "No MCP server named \"$3\"" >&2
  exit 1;;
remove) rm -f ` + fc.state + `;;
add) printf '%s' "$8" > ` + fc.state + `; pwd > ` + fc.cwd + `;;
esac
`
	if err := os.WriteFile(filepath.Join(d, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		if err := os.WriteFile(fc.state, []byte(state), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", d+":/usr/bin:/bin")
	return fc
}

func (fc *fakeClaude) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(fc.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func (fc *fakeClaude) called(t *testing.T, prefix string) bool {
	for _, c := range fc.calls(t) {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// withRoot — корень проекта без git: временный каталог.
func withRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	prev := projectRoot
	projectRoot = func(context.Context) (string, error) { return root, nil }
	t.Cleanup(func() { projectRoot = prev })
	t.Setenv("SDLC_PROJECT", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	return root
}

func runSetup(t *testing.T, c *Client, args ...string) (string, string, error) {
	t.Helper()
	return capture(t, func() error { return cmdSetup(context.Background(), c, args) })
}

func readText(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

var kitV1 = kitJSON("v1.0.0",
	kitFile{"agents/clab-stage.md", "---\nname: clab-stage\n---\nagent v1\n"},
	kitFile{"skills/clab-work/SKILL.md", "---\nname: clab-work\n---\nwork v1\n"},
	kitFile{"skills/clab-old/SKILL.md", "---\nname: clab-old\n---\nold v1\n"},
)

var kitV2 = kitJSON("v1.1.0",
	kitFile{"agents/clab-stage.md", "---\nname: clab-stage\n---\nagent v1\n"},
	kitFile{"skills/clab-work/SKILL.md", "---\nname: clab-work\n---\nwork v2\n"},
	kitFile{"skills/clab-new/SKILL.md", "---\nname: clab-new\n---\nnew v2\n"},
)

func readKitManifest(t *testing.T, dir string) kitState {
	t.Helper()
	var m kitState
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(dir, kitManifest))), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Критерии 4 и 9: набор ровно с содержимым сервера, манифест с версией и
// суммами, claude mcp add из корня проекта с токеном в заголовке, следующий
// шаг в выводе; токена нет ни в одном записанном файле.
func TestSetupInstallsKitAndMCP(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	fc := installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)

	out, errOut, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatalf("clab setup: %v, stderr %q", err, errOut)
	}
	wantAdd := "mcp add --transport http --scope local sdlc " + c.origin + "/mcp/projects/ACME --header Authorization: Bearer " + setupToken
	if !fc.called(t, wantAdd) {
		t.Fatalf("claude mcp add не вызван как надо:\n%s", strings.Join(fc.calls(t), "\n"))
	}
	cwd, _ := filepath.EvalSymlinks(strings.TrimSpace(readText(t, fc.cwd)))
	if real, _ := filepath.EvalSymlinks(root); cwd != real {
		t.Fatalf("claude mcp add из %q, а корень %q", cwd, real)
	}
	dir := filepath.Join(root, ".claude")
	for _, f := range kitV1["files"].([]map[string]string) {
		if got := readText(t, filepath.Join(dir, f["path"])); got != f["content"] {
			t.Fatalf("%s: %q", f["path"], got)
		}
	}
	m := readKitManifest(t, dir)
	if m.Version != "v1.0.0" || len(m.Files) != 3 {
		t.Fatalf("манифест: %+v", m)
	}
	for _, f := range kitV1["files"].([]map[string]string) {
		if m.Files[f["path"]] != f["sha256"] {
			t.Fatalf("манифест %s: %q", f["path"], m.Files[f["path"]])
		}
	}
	if !strings.Contains(out, "/clab-work ACME-") || !strings.Contains(out, "подключён") || !strings.Contains(out, "v1.0.0") {
		t.Fatalf("вывод:\n%s", out)
	}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.Contains(readText(t, p), setupToken) {
			t.Errorf("токен в файле %s", p)
		}
		return nil
	})
}

// Критерий 5: повторный запуск ничего не меняет и claude mcp add не зовёт.
func TestSetupIsIdempotent(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	fc := installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)
	if _, _, err := runSetup(t, c, "--project", "ACME"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".claude")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	stamp := map[string]string{}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			_ = os.Chtimes(p, old, old)
			stamp[p] = readText(t, p)
		}
		return nil
	})
	if err := os.Remove(fc.log); err != nil {
		t.Fatal(err)
	}

	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if fc.called(t, "mcp add") || fc.called(t, "mcp remove") {
		t.Fatalf("повторный запуск позвал: %v", fc.calls(t))
	}
	for p, content := range stamp {
		info, err := os.Stat(p)
		if err != nil || !info.ModTime().Equal(old) || readText(t, p) != content {
			t.Errorf("%s изменён повторным запуском", p)
		}
	}
	if !strings.Contains(out, "Уже актуально") || !strings.Contains(out, "уже подключён") {
		t.Fatalf("вывод:\n%s", out)
	}
}

// Критерий 6 и 21: новая версия набора — изменённое перезаписано, новое
// добавлено, ушедшее удалено вместе с опустевшим каталогом, манифест с новой
// версией.
func TestSetupUpdatesToNewVersion(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	installClaude(t, "")
	srv := &setupServer{kit: kitV1}
	c := srv.start(t)
	if _, _, err := runSetup(t, c, "--project", "ACME"); err != nil {
		t.Fatal(err)
	}
	srv.kit = kitV2
	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".claude")
	if readText(t, filepath.Join(dir, "skills/clab-work/SKILL.md")) != "---\nname: clab-work\n---\nwork v2\n" ||
		readText(t, filepath.Join(dir, "skills/clab-new/SKILL.md")) != "---\nname: clab-new\n---\nnew v2\n" {
		t.Fatal("набор не обновлён")
	}
	if exists(filepath.Join(dir, "skills/clab-old")) {
		t.Fatal("ушедший из набора скил остался")
	}
	m := readKitManifest(t, dir)
	if m.Version != "v1.1.0" || len(m.Files) != 3 || m.Files["skills/clab-old/SKILL.md"] != "" {
		t.Fatalf("манифест: %+v", m)
	}
	for _, want := range []string{"обновлён", "создан", "удалён"} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
}

// Критерий 7: правка человека не перезаписывается и не удаляется без
// --force — предупреждение с путём; с --force перезаписана.
func TestSetupKeepsUserEdits(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	installClaude(t, "")
	srv := &setupServer{kit: kitV1}
	c := srv.start(t)
	if _, _, err := runSetup(t, c, "--project", "ACME"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".claude")
	work := filepath.Join(dir, "skills/clab-work/SKILL.md")
	old := filepath.Join(dir, "skills/clab-old/SKILL.md")
	for _, p := range []string{work, old} {
		if err := os.WriteFile(p, []byte("my edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv.kit = kitV2
	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if readText(t, work) != "my edit\n" || readText(t, old) != "my edit\n" {
		t.Fatal("правка человека потеряна без --force")
	}
	if !strings.Contains(out, work) || !strings.Contains(out, old) || !strings.Contains(out, "--force") {
		t.Fatalf("нет предупреждения с путём:\n%s", out)
	}
	if !exists(filepath.Join(dir, "skills/clab-new/SKILL.md")) {
		t.Fatal("остальное не обновлено")
	}

	if _, _, err := runSetup(t, c, "--project", "ACME", "--force"); err != nil {
		t.Fatal(err)
	}
	if readText(t, work) != "---\nname: clab-work\n---\nwork v2\n" {
		t.Fatal("--force не перезаписал правленый файл")
	}
	// Ушедший из набора правленый файл выпал из манифеста и остаётся
	// файлом человека.
	if readText(t, old) != "my edit\n" {
		t.Fatal("файл человека, ушедший из набора, тронут")
	}
}

// Критерий 8: --dry-run — перечень действий, на диске ничего, claude не
// вызывается.
func TestSetupDryRun(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	fc := installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)

	out, _, err := runSetup(t, c, "--project", "ACME", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, ".claude")) {
		t.Fatal("--dry-run записал набор")
	}
	if len(fc.calls(t)) != 0 {
		t.Fatalf("--dry-run позвал claude: %v", fc.calls(t))
	}
	for _, want := range []string{"--dry-run", "создать", "skills/clab-work/SKILL.md", "MCP:", "будет проверен"} {
		if !strings.Contains(out, want) {
			t.Errorf("в перечне нет %q:\n%s", want, out)
		}
	}

	withJSON(t, true)
	out, _, err = runSetup(t, c, "--project", "ACME", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep["mcp"] != mcpCheck || rep["dry_run"] != true {
		t.Fatalf("--dry-run --json: %s", out)
	}

	// Без claude --dry-run только говорит, что MCP не подключится, и не
	// падает: он ничего не делает.
	withJSON(t, false)
	t.Setenv("PATH", t.TempDir())
	out, _, err = runSetup(t, c, "--project", "ACME", "--dry-run")
	if err != nil || !strings.Contains(out, "не будет подключён") || exists(filepath.Join(root, ".claude")) {
		t.Fatalf("--dry-run без claude: %v\n%s", err, out)
	}
}

// Критерий 9: --scope project при входе по токену — отказ тем же текстом,
// что у clab mcp install, без запросов и без claude.
func TestSetupRefusesProjectScopeWithToken(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	fc := installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)
	_, _, err := runSetup(t, c, "--project", "ACME", "--scope", "project")
	if err == nil || err.Error() != errProjectScopeToken.Error() {
		t.Fatalf("--scope project: %v", err)
	}
	_, _, mcpErr := capture(t, func() error {
		return cmdMCP(context.Background(), c, []string{"install", "--project", "ACME", "--scope", "project"})
	})
	if mcpErr == nil || mcpErr.Error() != err.Error() {
		t.Fatalf("тексты отказа разошлись: %v / %v", err, mcpErr)
	}
	if exists(filepath.Join(root, ".claude")) || len(fc.calls(t)) != 0 {
		t.Fatal("после отказа что-то сделано")
	}
}

// Критерий 10: --scope user — набор и манифест в ~/.claude, MCP с --scope
// user, в корне проекта файлов нет.
func TestSetupUserScope(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	fc := installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)

	if _, _, err := runSetup(t, c, "--project", "ACME", "--scope", "user"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude")
	if !exists(filepath.Join(dir, "skills/clab-work/SKILL.md")) || !exists(filepath.Join(dir, "agents/clab-stage.md")) ||
		readKitManifest(t, dir).Version != "v1.0.0" {
		t.Fatal("набор не в ~/.claude")
	}
	if exists(filepath.Join(root, ".claude")) {
		t.Fatal("--scope user записал в корень проекта")
	}
	if !fc.called(t, "mcp add --transport http --scope user sdlc ") {
		t.Fatalf("MCP не в области user: %v", fc.calls(t))
	}
}

// Критерий 11: --agent other — без claude и без набора, печатает
// конфигурацию MCP и адрес /docs.
func TestSetupOtherAgent(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	fc := installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)

	out, _, err := runSetup(t, c, "--project", "ACME", "--agent", "other")
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.calls(t)) != 0 || exists(filepath.Join(root, ".claude")) {
		t.Fatal("--agent other позвал claude или записал набор")
	}
	if !strings.Contains(out, `"mcpServers"`) || !strings.Contains(out, c.origin+"/mcp/projects/ACME") ||
		!strings.Contains(out, c.origin+"/docs") {
		t.Fatalf("вывод:\n%s", out)
	}
}

// Критерий 11: claude нет в PATH — набор поставлен, MCP нет, конфигурация для
// ручного подключения и ненулевой код.
func TestSetupWithoutClaude(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	t.Setenv("PATH", t.TempDir())
	c := (&setupServer{kit: kitV1}).start(t)

	out, _, err := runSetup(t, c, "--project", "ACME")
	if err == nil || !strings.Contains(err.Error(), "claude не найден") || !strings.Contains(err.Error(), `"mcpServers"`) {
		t.Fatalf("без claude: %v", err)
	}
	if !exists(filepath.Join(root, ".claude/skills/clab-work/SKILL.md")) {
		t.Fatal("набор не поставлен")
	}
	if !strings.Contains(out, "не подключён") {
		t.Fatalf("вывод:\n%s", out)
	}
}

// Критерий 12: сервер без /v1/agent-kit — MCP подключён, набора нет,
// сообщение с версией сервера, код 0.
func TestSetupOldServer(t *testing.T) {
	withJSON(t, false)
	withVersion(t, "v0.12.0")
	root := withRoot(t)
	fc := installClaude(t, "")
	c := (&setupServer{kit: nil, version: "v0.11.0"}).start(t)

	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if !fc.called(t, "mcp add") {
		t.Fatal("MCP не подключён")
	}
	if exists(filepath.Join(root, ".claude")) {
		t.Fatal("без набора на сервере что-то записано")
	}
	if !strings.Contains(out, "v0.11.0") || !strings.Contains(out, "v0.12.0") || !strings.Contains(out, "не раздаёт набор") {
		t.Fatalf("вывод:\n%s", out)
	}

	withJSON(t, true)
	out, _, _ = runSetup(t, c, "--project", "ACME")
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(out)
	}
	if v, present := rep["kit_version"]; !present || v != nil || rep["server_version"] != "v0.11.0" {
		t.Fatalf("--json без набора: %s", out)
	}
}

// Критерий 13: проект не определён — подсказка --project, ничего не
// сделано; хост не настроен — стандартная подсказка без запросов.
func TestSetupWithoutProjectOrHost(t *testing.T) {
	withJSON(t, false)
	root := withRoot(t)
	t.Chdir(root)
	fc := installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)

	_, _, err := runSetup(t, c)
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("без проекта: %v", err)
	}
	if exists(filepath.Join(root, ".claude")) || len(fc.calls(t)) != 0 {
		t.Fatal("без проекта что-то сделано")
	}

	nc, rt := noHostClient(t, "")
	t.Setenv("SDLC_PROJECT", "ACME")
	_, _, err = capture(t, func() error { return cmdSetup(context.Background(), nc, nil) })
	if err == nil || !strings.HasPrefix(err.Error(), noHostHint) || rt.calls != 0 || len(fc.calls(t)) != 0 {
		t.Fatalf("без хоста: %v, запросов %d", err, rt.calls)
	}
}

// Критерий 14: --json — объект с проектом, адресом, областью, агентом,
// версией набора, действием MCP и файлами.
func TestSetupJSON(t *testing.T) {
	withJSON(t, true)
	withRoot(t)
	installClaude(t, "")
	c := (&setupServer{kit: kitV1}).start(t)

	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Project    string  `json:"project"`
		URL        string  `json:"url"`
		Scope      string  `json:"scope"`
		Agent      string  `json:"agent"`
		KitVersion *string `json:"kit_version"`
		MCP        string  `json:"mcp"`
		Files      []struct {
			Path   string `json:"path"`
			Action string `json:"action"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("не JSON: %s", out)
	}
	if rep.Project != "ACME" || rep.URL != c.origin+"/mcp/projects/ACME" || rep.Scope != "local" || rep.Agent != "claude" ||
		rep.KitVersion == nil || *rep.KitVersion != "v1.0.0" || rep.MCP != mcpAdded || len(rep.Files) != 3 {
		t.Fatalf("--json: %s", out)
	}
	for _, f := range rep.Files {
		if f.Action != fileCreate {
			t.Fatalf("файл %s: %s", f.Path, f.Action)
		}
	}
	if strings.Contains(out, setupToken) {
		t.Fatal("токен в --json")
	}
}

// Под тем же именем другой адрес — сервер заменён, вывод говорит «заменён».
func TestSetupReplacesMCP(t *testing.T) {
	withJSON(t, false)
	withRoot(t)
	fc := installClaude(t, "https://old.example/mcp/projects/OLD")
	c := (&setupServer{kit: kitV1}).start(t)

	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if !fc.called(t, "mcp remove sdlc --scope local") || !fc.called(t, "mcp add ") {
		t.Fatalf("замена: %v", fc.calls(t))
	}
	if !strings.Contains(out, "заменён (был https://old.example/mcp/projects/OLD)") {
		t.Fatalf("вывод:\n%s", out)
	}
}

// Набор с суммой, не сходящейся с содержимым, или с путём наружу — ошибка,
// ничего не записано и claude не вызван.
func TestSetupRejectsBadKit(t *testing.T) {
	withJSON(t, false)
	for name, kit := range map[string]map[string]any{
		"sha256": {"version": "v1", "files": []map[string]string{{"path": "skills/x/SKILL.md", "content": "a", "sha256": "00"}}},
		"path":   kitJSON("v1", kitFile{"../settings.json", "{}"}),
		"hooks":  kitJSON("v1", kitFile{"settings.json", "{}"}),
	} {
		root := withRoot(t)
		fc := installClaude(t, "")
		c := (&setupServer{kit: kit}).start(t)
		_, _, err := runSetup(t, c, "--project", "ACME")
		if err == nil || !strings.Contains(err.Error(), "ничего не записано") {
			t.Errorf("%s: %v", name, err)
		}
		if exists(filepath.Join(root, ".claude")) || len(fc.calls(t)) != 0 {
			t.Errorf("%s: что-то сделано", name)
		}
	}
}

// Критерий 15: clab --help перечисляет setup, clab setup --help — все флаги.
func TestSetupHelp(t *testing.T) {
	_, usageOut, _ := capture(t, func() error { usage(); return nil })
	if !strings.Contains(usageOut, "  setup ") {
		t.Fatalf("clab --help без setup:\n%s", usageOut)
	}
	_, errOut, err := capture(t, func() error { return cmdSetup(context.Background(), &Client{}, []string{"--help"}) })
	if err != flag.ErrHelp {
		t.Fatalf("--help: %v", err)
	}
	for _, f := range []string{"-project", "-scope", "-agent", "-name", "-dry-run", "-force"} {
		if !strings.Contains(errOut, f) {
			t.Errorf("clab setup --help без %s:\n%s", f, errOut)
		}
	}
}

// Критерий 4: корень — верх git-репозитория и из подкаталога, проект — по
// git remote.
func TestSetupGitRootAndRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git нет")
	}
	withJSON(t, false)
	t.Setenv("SDLC_PROJECT", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:acme/api.git"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v %s", args, err, out)
		}
	}
	sub := filepath.Join(root, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	gitPath, _ := exec.LookPath("git")
	installClaude(t, "")
	t.Setenv("PATH", os.Getenv("PATH")+":"+filepath.Dir(gitPath))
	c := (&setupServer{kit: kitV1, repos: map[string]string{"acme/api": "ACME"}}).start(t)

	out, _, err := runSetup(t, c)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(root)
	if !exists(filepath.Join(real, ".claude/skills/clab-work/SKILL.md")) || exists(filepath.Join(sub, ".claude")) {
		t.Fatalf("набор не в корне репозитория %s", real)
	}
	if !strings.Contains(out, "Проект:  ACME") {
		t.Fatalf("проект не по git remote:\n%s", out)
	}
}
