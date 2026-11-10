package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// CLAB-418: clab setup и clab mcp install по привязкам агентов. Агенты —
// подставные скрипты в PATH: они пишут свои аргументы в журнал, а codex
// ещё и ведёт $CODEX_HOME/config.toml в форме реального Codex 0.167.0
// (проверено руками: `codex mcp add` пишет [mcp_servers.<имя>] с url и
// bearer_token_env_var, `codex mcp get --json` отвечает 1, если сервера нет).
// Сервер — httptest с набором в формате GET /v1/agent-kit (core, bindings).

type fakeCodex struct{ dir, log string }

func installCodex(t *testing.T, version string) *fakeCodex {
	t.Helper()
	d := t.TempDir()
	fc := &fakeCodex{dir: d, log: filepath.Join(d, "log")}
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + fc.log + `
CFG="${CODEX_HOME:-$HOME/.codex}/config.toml"
if [ "$1" = "--version" ]; then echo "codex-cli ` + version + `"; exit 0; fi
[ "$1" = mcp ] || exit 0
name="$3"
hdr="[mcp_servers.$name]"
case "$2" in
get)
  url=$(awk -v h="$hdr" '$0==h{f=1;next} /^\[/{f=0} f&&$1=="url"{gsub(/"/,"",$3);print $3}' "$CFG" 2>/dev/null)
  env=$(awk -v h="$hdr" '$0==h{f=1;next} /^\[/{f=0} f&&$1=="bearer_token_env_var"{gsub(/"/,"",$3);print $3}' "$CFG" 2>/dev/null)
  if [ -z "$url" ]; then echo "Error: No MCP server named '$name' found." >&2; exit 1; fi
  printf '{"name":"%s","transport":{"type":"streamable_http","url":"%s","bearer_token_env_var":"%s","http_headers":null}}\n' "$name" "$url" "$env";;
remove)
  if [ -f "$CFG" ]; then awk -v h="$hdr" '/^\[/{skip=($0==h)} !skip{print}' "$CFG" > "$CFG.tmp" && mv "$CFG.tmp" "$CFG"; fi;;
add)
  shift 3
  url=""; env=""
  while [ $# -gt 0 ]; do
    case "$1" in --url) url="$2"; shift 2;; --bearer-token-env-var) env="$2"; shift 2;; *) shift;; esac
  done
  mkdir -p "$(dirname "$CFG")"
  printf '\n%s\nurl = "%s"\nbearer_token_env_var = "%s"\n' "$hdr" "$url" "$env" >> "$CFG";;
esac
`
	if err := os.WriteFile(filepath.Join(d, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", d+":"+os.Getenv("PATH"))
	return fc
}

func readLog(t *testing.T, p string) []string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func (fc *fakeCodex) calls(t *testing.T) []string { return readLog(t, fc.log) }

func (fc *fakeCodex) called(t *testing.T, prefix string) bool {
	for _, c := range fc.calls(t) {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// setClaudeVersion учит подставной claude отвечать на --version.
func setClaudeVersion(t *testing.T, fc *fakeClaude, out string) {
	t.Helper()
	p := filepath.Join(fc.dir, "claude")
	orig := strings.TrimPrefix(readText(t, p), "#!/bin/sh\n")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '%s\\n' \"$*\" >> " + fc.log + "; echo '" + out + "'; exit 0; fi\n" + orig
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// setupEnv — корень проекта без git, чистые HOME и CODEX_HOME.
func setupEnv(t *testing.T) (root, home string) {
	t.Helper()
	root = withRoot(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	return root, home
}

func entry(p, content string) map[string]string {
	sum := sha256.Sum256([]byte(content))
	return map[string]string{"path": p, "content": content, "sha256": hex.EncodeToString(sum[:])}
}

func entries(files []kitFile) []map[string]string {
	out := []map[string]string{}
	for _, f := range files {
		out = append(out, entry(f.path, f.content))
	}
	return out
}

func skillBody(name, rev string) string {
	return "---\nname: " + name + "\ndescription: " + name + "\n---\n" + name + " " + rev + "\n"
}

func stageToml(rev string) string {
	return "name = \"clab-stage\"\ndescription = \"stage\"\ndeveloper_instructions = '''\nstage " + rev + "\n'''\n"
}

// codexFilesOf — файлы привязки codex для набора скилов.
func codexFilesOf(rev string, skills ...string) []kitFile {
	var out []kitFile
	for _, s := range skills {
		out = append(out, kitFile{".agents/skills/" + s + "/SKILL.md", skillBody(s, rev)})
	}
	return append(out, kitFile{".codex/agents/clab-stage.toml", stageToml(rev)})
}

var defaultSkills = []string{"clab-guide", "clab-work"}

// bindingsKit — ответ /v1/agent-kit с ядром и привязками; codex — файлы
// привязки codex (nil — обычные для набора скилов).
func agentKitFx(version, rev string, codex []kitFile, skills ...string) map[string]any {
	if len(skills) == 0 {
		skills = defaultSkills
	}
	if codex == nil {
		codex = codexFilesOf(rev, skills...)
	}
	var core, legacy, claude []kitFile
	for _, s := range skills {
		core = append(core, kitFile{"skills/" + s + "/SKILL.md", skillBody(s, rev)})
	}
	legacy = append(append(legacy, core...), kitFile{"agents/clab-stage.md", "---\nname: clab-stage\ndescription: stage\n---\nstage " + rev + "\n"})
	for _, f := range legacy {
		claude = append(claude, kitFile{".claude/" + f.path, f.content})
	}
	m := kitJSON(version, legacy...)
	m["core"] = map[string]any{"skills": entries(core), "agents": []any{}, "guide": entry("guide.md", "guide")}
	m["bindings"] = []any{
		map[string]any{
			"name": "claude", "kit": map[string]string{"manifest": ".claude/clab-kit.json", "skills_dir": ".claude/skills", "agents_dir": ".claude/agents"},
			"files": entries(claude), "detect": []map[string]any{{"env": "CLAUDECODE", "equals": "1"}},
			"levels": map[string]string{"low": "haiku", "medium": "sonnet", "high": "opus"},
		},
		map[string]any{
			"name": "codex", "kit": map[string]string{"manifest": ".agents/clab-kit.json", "skills_dir": ".agents/skills", "agents_dir": ".codex/agents"},
			"files": entries(codex), "detect": []map[string]any{{"env": "CODEX_THREAD_ID", "non_empty": true}}, "levels": nil,
		},
		map[string]any{"name": "generic", "kit": nil, "files": []any{}, "detect": nil, "levels": nil},
	}
	return m
}

// snapshot — содержимое и режимы всех файлов под каталогами.
func snapshot(t *testing.T, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range dirs {
		_ = filepath.Walk(d, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				l, _ := os.Readlink(p)
				out[p] = "link:" + l
			} else if info.IsDir() {
				out[p] = "dir:" + info.Mode().String()
			} else {
				out[p] = info.Mode().String() + ":" + readText(t, p)
			}
			return nil
		})
	}
	return out
}

func sameSnapshot(t *testing.T, before, after map[string]string, what string) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		var diff []string
		for p, v := range after {
			if before[p] != v {
				diff = append(diff, p)
			}
		}
		for p := range before {
			if _, ok := after[p]; !ok {
				diff = append(diff, p)
			}
		}
		t.Fatalf("%s: дерево изменилось: %v", what, diff)
	}
}

// noToken — токена нет ни в тексте, ни в журналах, ни в файлах под каталогами.
func noToken(t *testing.T, texts []string, dirs ...string) {
	t.Helper()
	for i, s := range texts {
		if strings.Contains(s, setupToken) {
			t.Errorf("токен в тексте #%d:\n%s", i, s)
		}
	}
	for _, d := range dirs {
		_ = filepath.Walk(d, func(p string, info os.FileInfo, err error) error {
			if err == nil && info.Mode().IsRegular() && strings.Contains(readText(t, p), setupToken) {
				t.Errorf("токен в файле %s", p)
			}
			return nil
		})
	}
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("в выводе нет %q:\n%s", w, out)
		}
	}
}

func readHomeConfig(t *testing.T, home string) map[string]any {
	t.Helper()
	doc := map[string]any{}
	if err := toml.Unmarshal([]byte(readText(t, filepath.Join(home, ".codex", "config.toml"))), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// AC-1, AC-5, AC-6: только codex — набор и MCP Codex, байт в байт по
// привязке, токен нигде, claude не нужен.
func TestSetupAutoOnlyCodex(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	cx := installCodex(t, "0.167.0")
	srv := &setupServer{kit: agentKitFx("v1.0.0", "1", nil)}
	c := srv.start(t)

	out, errOut, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, errOut)
	}
	for _, f := range srv.kit["bindings"].([]any)[1].(map[string]any)["files"].([]map[string]string) {
		if got := readText(t, filepath.Join(root, f["path"])); got != f["content"] {
			t.Errorf("%s: %q", f["path"], got)
		}
	}
	var m kitState
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(root, ".agents/clab-kit.json"))), &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "v1.0.0" || len(m.Files) != 3 || m.Files[".codex/agents/clab-stage.toml"] != entry("x", stageToml("1"))["sha256"] {
		t.Fatalf("манифест: %+v", m)
	}
	if exists(filepath.Join(root, ".claude")) {
		t.Fatal("записан набор Claude")
	}
	wantAdd := "mcp add sdlc-acme --url " + c.origin + "/mcp/projects/ACME --bearer-token-env-var SDLC_TOKEN"
	if !cx.called(t, wantAdd) {
		t.Fatalf("codex mcp add:\n%s", strings.Join(cx.calls(t), "\n"))
	}
	cfg := readText(t, filepath.Join(home, ".codex", "config.toml"))
	mustContain(t, cfg, "[mcp_servers.sdlc-acme]", "bearer_token_env_var = \"SDLC_TOKEN\"")
	mustContain(t, out, "АГЕНТ", "codex", "$clab-work ACME-", "SDLC_TOKEN", "clab auth token", "модель Codex по умолчанию", "таблица привязки по умолчанию",
		"claude auth status", "codex login status", "--billing", "trust_level")
	noToken(t, []string{out, errOut, strings.Join(cx.calls(t), "\n")}, root, home)
	for _, c := range cx.calls(t) {
		if strings.Contains(c, "login") || strings.Contains(c, "auth") {
			t.Errorf("setup позвал %q: оплату он не трогает", c)
		}
	}
}

// AC-1, AC-2, AC-11: оба агента — оба набора и две строки таблицы; --agent
// codex при обоих не зовёт claude и не пишет в .claude.
func TestSetupAutoBothAndExplicit(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	cl := installClaude(t, "")
	cx := installCodex(t, "0.167.0")
	srv := &setupServer{kit: agentKitFx("v1.0.0", "1", nil)}
	c := srv.start(t)

	if _, _, err := runSetup(t, c, "--project", "ACME", "--agent", "codex"); err != nil {
		t.Fatal(err)
	}
	if len(cl.calls(t)) != 0 || exists(filepath.Join(root, ".claude")) {
		t.Fatalf("--agent codex тронул claude: %v", cl.calls(t))
	}
	if !cx.called(t, "mcp add sdlc-acme") {
		t.Fatal("codex не подключён")
	}

	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(root, ".claude/skills/clab-work/SKILL.md")) || !exists(filepath.Join(root, ".agents/skills/clab-work/SKILL.md")) {
		t.Fatal("поставлен не каждый набор")
	}
	if !cl.called(t, "mcp add --transport http --scope local sdlc ") {
		t.Fatalf("claude: %v", cl.calls(t))
	}
	mustContain(t, out, "Агент:   claude", "Агент:   codex", "/clab-work ACME-", "$clab-work ACME-", "claude (таблица привязки по умолчанию): low haiku, medium sonnet, high opus")
	lines := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "claude ") || strings.HasPrefix(l, "codex ") {
			lines++
		}
	}
	if lines < 2 {
		t.Errorf("в таблице не по строке на агента:\n%s", out)
	}
	_ = home

	withJSON(t, true)
	out, _, err = runSetup(t, c, "--project", "ACME")
	var rep struct {
		Agent  string           `json:"agent"`
		Agents []map[string]any `json:"agents"`
		MCP    string           `json:"mcp"`
	}
	if err != nil || json.Unmarshal([]byte(out), &rep) != nil || len(rep.Agents) != 2 || rep.Agent != "claude,codex" || rep.MCP != mcpUnchanged {
		t.Fatalf("--json: %v\n%s", err, out)
	}
}

// AC-2: неизвестное имя — ошибка с перечнем, ничего не записано, даже блок
// привязки; явный агент без программы — набор есть, MCP нет, ошибка.
func TestSetupAgentErrors(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)

	before := snapshot(t, root, home)
	_, _, err := runSetup(t, c, "--project", "ACME", "--agent", "bogus")
	if err == nil || !strings.Contains(err.Error(), "auto, claude, codex, generic") {
		t.Fatalf("неизвестное имя: %v", err)
	}
	sameSnapshot(t, before, snapshot(t, root, home), "неизвестный агент")

	out, _, err := runSetup(t, c, "--project", "ACME", "--agent", "codex")
	if err == nil || !strings.Contains(err.Error(), "codex не найден") || !strings.Contains(err.Error(), "--bearer-token-env-var SDLC_TOKEN") {
		t.Fatalf("codex без программы: %v", err)
	}
	if !exists(filepath.Join(root, ".agents/skills/clab-work/SKILL.md")) || exists(filepath.Join(home, ".codex")) {
		t.Fatal("набор не записан или MCP записан без codex")
	}
	mustContain(t, out, "не подключён")
}

// AC-3: --dry-run — журналы пусты, дерево не изменилось, вывод показывает
// файлы, действие над MCP, разницу блока и строки --trust; то же с --json.
func TestSetupDryRunAgents(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	cl := installClaude(t, "")
	cx := installCodex(t, "0.167.0")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)

	before := snapshot(t, root, home)
	for _, js := range []bool{false, true} {
		withJSON(t, js)
		out, _, err := runSetup(t, c, "--project", "ACME", "--dry-run", "--trust")
		if err != nil {
			t.Fatal(err)
		}
		if len(cl.calls(t)) != 0 || len(cx.calls(t)) != 0 {
			t.Fatalf("--dry-run позвал агентов: %v %v", cl.calls(t), cx.calls(t))
		}
		sameSnapshot(t, before, snapshot(t, root, home), "--dry-run")
		if js {
			var rep struct {
				DryRun bool `json:"dry_run"`
				Trust  struct {
					Action string `json:"action"`
				} `json:"trust"`
				Agents []struct {
					MCP string `json:"mcp"`
				} `json:"agents"`
			}
			if json.Unmarshal([]byte(out), &rep) != nil || !rep.DryRun || len(rep.Agents) != 2 || rep.Agents[1].MCP != mcpCheck || rep.Trust.Action != trustAdded {
				t.Fatalf("--dry-run --json:\n%s", out)
			}
			continue
		}
		mustContain(t, out, "--dry-run", ".agents/skills/clab-work/SKILL.md", ".codex/agents/clab-stage.toml", "будет проверен", "AGENTS.md", "+ <!-- cadence-lab", "--trust добавит", "trust_level")
	}
}

// AC-5: повторный запуск ничего не пишет; правка не затирается без --force;
// ушедший из набора файл удаляется, правленный остаётся и выпадает из
// манифеста; неверная сумма — ничего не записано.
func TestSetupCodexKitLifecycle(t *testing.T) {
	withJSON(t, false)
	root, _ := setupEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	installCodex(t, "0.167.0")
	srv := &setupServer{kit: agentKitFx("v1.0.0", "1", nil, "clab-extra", "clab-guide", "clab-work")}
	c := srv.start(t)
	if _, _, err := runSetup(t, c, "--project", "ACME"); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	var files []string
	_ = filepath.Walk(filepath.Join(root, ".agents"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, p)
			_ = os.Chtimes(p, old, old)
		}
		return nil
	})
	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range files {
		if info, err := os.Stat(p); err != nil || !info.ModTime().Equal(old) {
			t.Errorf("%s переписан повторным запуском", p)
		}
	}
	mustContain(t, out, "файлы набора актуальны", "Уже актуально")

	// Правка человека и новая версия набора без clab-extra и clab-guide.
	work := filepath.Join(root, ".agents/skills/clab-work/SKILL.md")
	guide := filepath.Join(root, ".agents/skills/clab-guide/SKILL.md")
	for _, p := range []string{work, guide} {
		if err := os.WriteFile(p, []byte("my edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv.kit = agentKitFx("v1.1.0", "2", nil, "clab-work")
	out, _, err = runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if readText(t, work) != "my edit\n" || readText(t, guide) != "my edit\n" {
		t.Fatal("правка человека потеряна без --force")
	}
	if exists(filepath.Join(root, ".agents/skills/clab-extra")) {
		t.Fatal("ушедший непрaвленный скил остался")
	}
	mustContain(t, out, "--force", work, guide)
	var m kitState
	_ = json.Unmarshal([]byte(readText(t, filepath.Join(root, ".agents/clab-kit.json"))), &m)
	if _, ok := m.Files[".agents/skills/clab-guide/SKILL.md"]; ok || m.Version != "v1.1.0" {
		t.Fatalf("правленый ушедший файл остался в манифесте: %+v", m)
	}
	if got := readText(t, filepath.Join(root, ".codex/agents/clab-stage.toml")); got != stageToml("2") {
		t.Fatalf("агент не обновлён: %q", got)
	}

	if _, _, err := runSetup(t, c, "--project", "ACME", "--force"); err != nil {
		t.Fatal(err)
	}
	if readText(t, work) != skillBody("clab-work", "2") {
		t.Fatal("--force не перезаписал")
	}

	// Неверная сумма — отказ, дерево то же.
	bad := agentKitFx("v1.2.0", "3", nil, "clab-work")
	bad["bindings"].([]any)[1].(map[string]any)["files"].([]map[string]string)[0]["sha256"] = "00"
	srv.kit = bad
	before := snapshot(t, root)
	if _, _, err := runSetup(t, c, "--project", "ACME"); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("неверная сумма: %v", err)
	}
	sameSnapshot(t, before, snapshot(t, root), "неверная сумма")
}

// AC-6: сервер уже подключён — unchanged без add; другой адрес — remove и
// add, прежний назван; --name задаёт имя.
func TestSetupCodexMCPStates(t *testing.T) {
	withJSON(t, false)
	_, home := setupEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	cx := installCodex(t, "0.167.0")
	kit := agentKitFx("v1.0.0", "1", nil)
	c1 := (&setupServer{kit: kit}).start(t)
	if _, _, err := runSetup(t, c1, "--project", "ACME"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cx.log); err != nil {
		t.Fatal(err)
	}
	out, _, err := runSetup(t, c1, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if cx.called(t, "mcp add") || cx.called(t, "mcp remove") {
		t.Fatalf("повторный запуск: %v", cx.calls(t))
	}
	mustContain(t, out, "уже подключён")

	c2 := (&setupServer{kit: kit}).start(t) // другой адрес сервера
	out, _, err = runSetup(t, c2, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if !cx.called(t, "mcp remove sdlc-acme") || !cx.called(t, "mcp add sdlc-acme --url "+c2.origin+"/mcp/projects/ACME") {
		t.Fatalf("замена: %v", cx.calls(t))
	}
	mustContain(t, out, "заменён (был "+c1.origin+"/mcp/projects/ACME)")
	// Ячейка таблицы — действие без адреса: двоеточие в «http://» не должно
	// обрезать её на полуслове (CLAB-418, замечание тестировщика).
	mustContain(t, out, "sdlc-acme: заменён")
	if strings.Contains(out, "заменён (был http\n") || strings.Contains(out, "sdlc-acme: заменён (") {
		t.Fatalf("ячейка таблицы с адресом:\n%s", out)
	}
	servers := readHomeConfig(t, home)["mcp_servers"].(map[string]any)
	if len(servers) != 1 || servers["sdlc-acme"].(map[string]any)["url"] != c2.origin+"/mcp/projects/ACME" {
		t.Fatalf("конфиг: %v", servers)
	}

	if _, _, err := runSetup(t, c2, "--project", "ACME", "--name", "mine"); err != nil {
		t.Fatal(err)
	}
	if !cx.called(t, "mcp add mine --url ") {
		t.Fatalf("--name: %v", cx.calls(t))
	}
}

// AC-7: два проекта на одном HOME не перетирают друг друга.
func TestSetupCodexTwoProjects(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	installCodex(t, "0.167.0")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)

	if _, _, err := runSetup(t, c, "--project", "ABC", "--no-marker"); err != nil {
		t.Fatal(err)
	}
	first := readText(t, filepath.Join(home, ".codex", "config.toml"))
	if _, _, err := runSetup(t, c, "--project", "XYZ", "--no-marker"); err != nil {
		t.Fatal(err)
	}
	servers := readHomeConfig(t, home)["mcp_servers"].(map[string]any)
	if len(servers) != 2 || servers["sdlc-abc"].(map[string]any)["url"] != c.origin+"/mcp/projects/ABC" ||
		servers["sdlc-xyz"].(map[string]any)["url"] != c.origin+"/mcp/projects/XYZ" {
		t.Fatalf("конфиг: %v", servers)
	}
	if !strings.HasPrefix(readText(t, filepath.Join(home, ".codex", "config.toml")), first) {
		t.Fatal("запись первого проекта изменилась")
	}
	before := snapshot(t, root, home)
	for _, k := range []string{"ABC", "XYZ"} {
		if _, _, err := runSetup(t, c, "--project", k, "--no-marker"); err != nil {
			t.Fatal(err)
		}
	}
	sameSnapshot(t, before, snapshot(t, root, home), "повторные запуски")
}

// AC-8: чужие настройки не меняются, в пользовательский конфиг Codex
// добавляется только mcp_servers.sdlc-*.
func TestSetupForeignSettingsUntouched(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	installCodex(t, "0.167.0")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)

	foreign := map[string]string{
		filepath.Join(root, ".claude/settings.json"):       `{"hooks":{}}`,
		filepath.Join(root, ".claude/settings.local.json"): `{"env":{}}`,
		filepath.Join(root, ".codex/config.toml"):          "model = \"proj\"\n",
	}
	for p, content := range foreign {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	userCfg := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(userCfg), 0o700); err != nil {
		t.Fatal(err)
	}
	userBefore := "# мой комментарий\nmodel = \"x\"\n\n[projects.\"/some/where\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(userCfg, []byte(userBefore), 0o600); err != nil {
		t.Fatal(err)
	}
	var docBefore map[string]any
	_ = toml.Unmarshal([]byte(userBefore), &docBefore)

	if _, _, err := runSetup(t, c, "--project", "ACME"); err != nil {
		t.Fatal(err)
	}
	for p, content := range foreign {
		if readText(t, p) != content {
			t.Errorf("%s изменён", p)
		}
	}
	after := readText(t, userCfg)
	if !strings.HasPrefix(after, userBefore) {
		t.Fatalf("прежние байты конфига изменились:\n%s", after)
	}
	doc := readHomeConfig(t, home)
	if _, ok := doc["mcp_servers"].(map[string]any)["sdlc-acme"]; !ok {
		t.Fatal("записи MCP нет")
	}
	delete(doc, "mcp_servers")
	if !reflect.DeepEqual(doc, docBefore) {
		t.Fatalf("в конфиге изменено не только mcp_servers: %v", doc)
	}
}

// AC-8: привязка с путём в чужие настройки, наружу, через ссылку, вне
// каталогов — отказ, дерево не тронуто.
func TestSetupRejectsBindingPaths(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	cx := installCodex(t, "0.167.0")

	for _, p := range []string{
		".codex/config.toml", ".claude/settings.json", ".claude/settings.local.json", "../x/SKILL.md", "/tmp/x/SKILL.md",
		".agents\\skills\\x", ".agents/skills/../../.codex/config.toml", "docs/x.md", ".agents/skills/x/settings.json", ".codex/agents/config.toml",
	} {
		kit := agentKitFx("v1.0.0", "1", []kitFile{{p, "x"}})
		c := (&setupServer{kit: kit}).start(t)
		before := snapshot(t, root, home)
		_, _, err := runSetup(t, c, "--project", "ACME")
		if err == nil || !strings.Contains(err.Error(), "недопустимый путь") || !strings.Contains(err.Error(), "ничего не записано") {
			t.Errorf("путь %q: %v", p, err)
		}
		sameSnapshot(t, before, snapshot(t, root, home), p)
	}

	// Каталоги и манифест привязки — только из списка clab.
	for name, mod := range map[string]func(map[string]string){
		"skills_dir": func(k map[string]string) { k["skills_dir"] = "docs" },
		"agents_dir": func(k map[string]string) { k["agents_dir"] = ".codex" },
		"manifest":   func(k map[string]string) { k["manifest"] = ".codex/config.toml" },
	} {
		kit := agentKitFx("v1.0.0", "1", nil)
		mod(kit["bindings"].([]any)[1].(map[string]any)["kit"].(map[string]string))
		c := (&setupServer{kit: kit}).start(t)
		before := snapshot(t, root, home)
		if _, _, err := runSetup(t, c, "--project", "ACME"); err == nil || !strings.Contains(err.Error(), "недопустимый путь") {
			t.Errorf("%s: %v", name, err)
		}
		sameSnapshot(t, before, snapshot(t, root, home), name)
	}

	// Ссылка наружу в каталоге набора.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".agents")); err != nil {
		t.Fatal(err)
	}
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)
	if _, _, err := runSetup(t, c, "--project", "ACME"); err == nil || !strings.Contains(err.Error(), "недопустимый путь") {
		t.Fatalf("ссылка наружу: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("запись ушла через ссылку: %v", entries)
	}
	if len(cx.calls(t)) != 0 {
		t.Fatalf("после отказа позван codex: %v", cx.calls(t))
	}
}

func trustLines(path string) string {
	return "[projects." + tomlQuote(path) + "]\ntrust_level = \"trusted\"\n"
}

// AC-9: доверие только по флагу, одна таблица текстом в конец, остальные
// байты те же, повтор безвреден, чужое значение не правится.
func TestSetupTrust(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	installCodex(t, "0.167.0")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)
	realRoot, _ := filepath.EvalSymlinks(root)
	cfg := filepath.Join(home, ".codex", "config.toml")

	out, _, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readText(t, cfg), "[projects.") {
		t.Fatal("доверие записано без --trust")
	}
	mustContain(t, out, "читает только у доверенного проекта", "[projects.\""+realRoot+"\"]", "trust_level = \"trusted\"", "--trust")

	before := readText(t, cfg)
	out, _, err = runSetup(t, c, "--project", "ACME", "--trust")
	if err != nil {
		t.Fatal(err)
	}
	after := readText(t, cfg)
	if after != before+"\n"+trustLines(realRoot) {
		t.Fatalf("добавлено не то:\n%s", after[len(before):])
	}
	mustContain(t, out, cfg, "trust_level")
	if strings.Count(after, "[projects.") != 1 {
		t.Fatal("таблиц доверия не одна")
	}

	// Повтор.
	if _, _, err := runSetup(t, c, "--project", "ACME", "--trust"); err != nil || readText(t, cfg) != after {
		t.Fatalf("повтор: %v", err)
	}

	// Чужой уровень не правится.
	other := "[projects." + tomlQuote(realRoot) + "]\ntrust_level = \"untrusted\"\n"
	if err := os.WriteFile(cfg, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = runSetup(t, c, "--project", "ACME", "--trust", "--no-marker")
	if got := readText(t, cfg); err != nil || !strings.HasPrefix(got, other) || strings.Count(got, "[projects.") != 1 || strings.Contains(got, "\"trusted\"") {
		t.Fatalf("чужое значение: %v\n%s", err, got)
	}
	mustContain(t, out, "untrusted", "не правит")

	// Битый TOML: запись доверия не сделана, в итоге нет ложного trust_level = "".
	broken := "[projects\nx = \n"
	if err := os.WriteFile(cfg, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = runSetup(t, c, "--project", "ACME", "--trust", "--no-marker")
	if err == nil || !strings.Contains(readText(t, cfg), "[projects\nx = ") || strings.Contains(readText(t, cfg), `"trusted"`) {
		t.Fatalf("битый TOML: %v\n%s", err, readText(t, cfg))
	}
	mustContain(t, out, "не разобран")
	if strings.Contains(out, `trust_level = ""`) {
		t.Fatalf("ложный уровень доверия:\n%s", out)
	}

	// Без codex среди агентов — предупреждение, записи нет.
	t.Setenv("PATH", "/usr/bin:/bin")
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	out, _, err = runSetup(t, c, "--project", "ACME", "--agent", "generic", "--trust", "--no-marker")
	if err != nil || exists(cfg) {
		t.Fatalf("generic --trust: %v", err)
	}
	mustContain(t, out, "--trust относится к Codex")
}

// AC-9: из linked worktree пишется корень основной копии (настоящий git
// worktree), путь с точкой (.worktrees) попадает в запись как есть.
func TestSetupTrustLinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git нет")
	}
	withJSON(t, false)
	mainRepo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("SDLC_PROJECT", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	gitIn := func(dir string, args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.email=a@b.c", "-c", "user.name=n"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v %s", args, err, out)
		}
	}
	gitIn(mainRepo, "init", "-q")
	gitIn(mainRepo, "commit", "-q", "--allow-empty", "-m", "i")
	wt := filepath.Join(mainRepo, ".worktrees", "K-1", "backend")
	gitIn(mainRepo, "worktree", "add", "-q", wt, "-b", "k1")
	t.Chdir(wt)
	gitPath, _ := exec.LookPath("git")
	t.Setenv("PATH", "/usr/bin:/bin:"+filepath.Dir(gitPath))
	installCodex(t, "0.167.0")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)

	if _, _, err := runSetup(t, c, "--project", "ACME", "--trust"); err != nil {
		t.Fatal(err)
	}
	realMain, _ := filepath.EvalSymlinks(mainRepo)
	cfg := readText(t, filepath.Join(home, ".codex", "config.toml"))
	if !strings.HasSuffix(cfg, trustLines(realMain)) || strings.Contains(cfg, ".worktrees") {
		t.Fatalf("доверие не корню основной копии %s:\n%s", realMain, cfg)
	}
	realWT, _ := filepath.EvalSymlinks(wt)
	if !exists(filepath.Join(realWT, ".agents/skills/clab-work/SKILL.md")) || exists(filepath.Join(mainRepo, ".agents")) {
		t.Fatal("набор не в рабочей копии запуска")
	}
}

// AC-10: ядро в общем каталоге, конфигурация с ${SDLC_TOKEN}, ни одна
// программа не вызвана; generic и codex не удаляют файлы друг друга.
func TestSetupGeneric(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	cx := installCodex(t, "0.167.0")
	srv := &setupServer{kit: agentKitFx("v1.0.0", "1", nil)}
	c := srv.start(t)

	// codex → generic: файл агента Codex остаётся.
	if _, _, err := runSetup(t, c, "--project", "ACME", "--agent", "codex"); err != nil {
		t.Fatal(err)
	}
	calls := len(cx.calls(t))
	srv.kit = agentKitFx("v1.1.0", "2", nil)
	out, _, err := runSetup(t, c, "--project", "ACME", "--agent", "generic")
	if err != nil {
		t.Fatal(err)
	}
	if len(cx.calls(t)) != calls {
		t.Fatalf("generic позвал программу: %v", cx.calls(t))
	}
	stage := filepath.Join(root, ".codex/agents/clab-stage.toml")
	if readText(t, stage) != stageToml("1") {
		t.Fatalf("generic тронул файл Codex: %q", readText(t, stage))
	}
	if readText(t, filepath.Join(root, ".agents/skills/clab-work/SKILL.md")) != skillBody("clab-work", "2") {
		t.Fatal("ядро не обновлено")
	}
	var m kitState
	_ = json.Unmarshal([]byte(readText(t, filepath.Join(root, ".agents/clab-kit.json"))), &m)
	if _, ok := m.Files[".codex/agents/clab-stage.toml"]; !ok {
		t.Fatalf("запись Codex пропала из общего манифеста: %+v", m)
	}
	mustContain(t, out, "${SDLC_TOKEN}", c.origin+"/docs", "инструкции: clab guide", `"mcpServers"`, "clab-work")
	noToken(t, []string{out}, root, home)

	// generic → codex: после обновления Codex файлы generic на месте.
	srv.kit = agentKitFx("v1.2.0", "3", nil)
	if _, _, err := runSetup(t, c, "--project", "ACME", "--agent", "codex"); err != nil {
		t.Fatal(err)
	}
	if readText(t, stage) != stageToml("3") || !exists(filepath.Join(root, ".agents/skills/clab-guide/SKILL.md")) {
		t.Fatal("codex после generic")
	}

	// auto без агентов — generic, JSON без токена.
	t.Setenv("PATH", "/usr/bin:/bin")
	withJSON(t, true)
	out, _, err = runSetup(t, c, "--project", "ACME", "--no-marker")
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Agent  string `json:"agent"`
		Agents []struct {
			Config map[string]any `json:"config"`
			Start  string         `json:"start"`
		} `json:"agents"`
	}
	if json.Unmarshal([]byte(out), &rep) != nil || rep.Agent != "generic" || len(rep.Agents) != 1 || rep.Agents[0].Config == nil || rep.Agents[0].Start != "clab guide" {
		t.Fatalf("--json:\n%s", out)
	}
	noToken(t, []string{out})
}

// AC-11: модели уровней по настройке агентов проекта; нет версии, 404 или
// сбой — таблица привязки; setup не падает.
func TestSetupModels(t *testing.T) {
	withJSON(t, false)
	setupEnv(t)
	installClaude(t, "")
	installCodex(t, "0.167.0")
	srv := &setupServer{kit: agentKitFx("v1.0.0", "1", nil), settings: `{"project":"ACME","version":3,"state":"active","document":{"agents":{"claude":{"levels":{"low":{"model":"cl-low","effort":"low"},"medium":{"model":"cl-mid"},"high":{"model":"cl-high","effort":"high"}}},"codex":{"levels":{"low":{"model":"gpt-low"},"medium":{"model":"gpt-mid"},"high":{"model":"gpt-high"}}}}}}`}
	c := srv.start(t)

	out, _, err := runSetup(t, c, "--project", "ACME", "--no-marker")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "claude (настройка агентов v3): low cl-low (усилие low), medium cl-mid, high cl-high (усилие high)", "codex (настройка агентов v3): low gpt-low, medium gpt-mid, high gpt-high")

	for name, body := range map[string]string{
		"нет версии": `{"project":"ACME","version":null,"state":null,"document":null}`,
		"404":        "",
		"сбой":       "<html>not json</html>",
	} {
		srv.settings = body
		out, _, err := runSetup(t, c, "--project", "ACME", "--no-marker")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mustContain(t, out, "claude (таблица привязки по умолчанию): low haiku, medium sonnet, high opus", "codex (таблица привязки по умолчанию): модель Codex по умолчанию")
	}
}

// AC-12: версия ниже минимальной — предупреждение и установка; не
// разобрана — «не определена»; в --dry-run версия не запрашивается.
func TestSetupVersions(t *testing.T) {
	withJSON(t, false)
	root, _ := setupEnv(t)
	cl := installClaude(t, "")
	setClaudeVersion(t, cl, "2.1.100 (Claude Code)")
	cx := installCodex(t, "0.100.0")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)

	out, _, err := runSetup(t, c, "--project", "ACME", "--no-marker")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "claude 2.1.100 старше минимальной проверенной 2.1.296", "codex 0.100.0 старше минимальной проверенной 0.167.0")
	if !exists(filepath.Join(root, ".claude/skills/clab-work/SKILL.md")) || !exists(filepath.Join(root, ".agents/skills/clab-work/SKILL.md")) {
		t.Fatal("установка не прошла из-за версии")
	}

	setClaudeVersion(t, cl, "что-то непонятное")
	out, _, err = runSetup(t, c, "--project", "ACME", "--no-marker")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "версия claude не определена")

	_ = os.Remove(cl.log)
	_ = os.Remove(cx.log)
	if _, _, err := runSetup(t, c, "--project", "ACME", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if len(cl.calls(t)) != 0 || len(cx.calls(t)) != 0 {
		t.Fatalf("--dry-run спросил версию: %v %v", cl.calls(t), cx.calls(t))
	}
}

// AC-13: сервер без привязок — Claude из files, привязки названы
// недоступными, Codex не тронут и ошибка с советом обновить; сервер без
// /v1/agent-kit — то же для Codex.
func TestSetupServerWithoutBindings(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	cl := installClaude(t, "")
	cx := installCodex(t, "0.167.0")
	srv := &setupServer{kit: kitV1}
	c := srv.start(t)

	out, _, err := runSetup(t, c, "--project", "ACME")
	if err == nil || !strings.Contains(err.Error(), "Обновите инстанс") {
		t.Fatalf("ожидалась ошибка с советом обновить: %v", err)
	}
	mustContain(t, out, "привязки агентов недоступны", "сервер v1.0.0")
	// Codex в PATH есть: причина не «не найден в PATH», а сервер без набора.
	mustContain(t, out, "набор для codex недоступен у этого сервера")
	if strings.Contains(out, "codex не найден в PATH") {
		t.Fatalf("причина названа неверно:\n%s", out)
	}
	for _, f := range kitV1["files"].([]map[string]string) {
		if readText(t, filepath.Join(root, ".claude", f["path"])) != f["content"] {
			t.Errorf("%s не как в files", f["path"])
		}
	}
	if !cl.called(t, "mcp add") || len(cx.calls(t)) != 0 || exists(filepath.Join(root, ".agents")) || exists(filepath.Join(root, ".codex")) || exists(filepath.Join(home, ".codex")) {
		t.Fatalf("Codex тронут: %v", cx.calls(t))
	}
	if strings.Contains(out, "panic") || strings.Contains(out, "<html") {
		t.Fatalf("вывод:\n%s", out)
	}

	srv.kit = nil
	srv.version = "v0.11.0"
	_, _, err = runSetup(t, c, "--project", "ACME", "--agent", "codex")
	if err == nil || !strings.Contains(err.Error(), "v0.11.0") {
		t.Fatalf("404 и codex: %v", err)
	}
}

// AC-14: блок привязки во всех режимах; ошибка Codex его не теряет; Codex и
// только CLAUDE.md — подсказка.
func TestSetupMarkerWithAgents(t *testing.T) {
	withJSON(t, false)
	root, _ := setupEnv(t)
	installClaude(t, "")
	installCodex(t, "0.167.0")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)
	block := "cadence-lab: project=ACME"

	for _, agent := range []string{"claude", "codex", "generic", "auto"} {
		_ = os.Remove(filepath.Join(root, "AGENTS.md"))
		if _, _, err := runSetup(t, c, "--project", "ACME", "--agent", agent); err != nil {
			t.Fatal(agent, err)
		}
		if !strings.Contains(readText(t, filepath.Join(root, "AGENTS.md")), block) {
			t.Errorf("%s: блока привязки нет", agent)
		}
	}
	_ = os.Remove(filepath.Join(root, "AGENTS.md"))
	if _, _, err := runSetup(t, c, "--project", "ACME", "--dry-run"); err != nil || exists(filepath.Join(root, "AGENTS.md")) {
		t.Fatalf("--dry-run записал блок: %v", err)
	}
	if _, _, err := runSetup(t, c, "--project", "ACME", "--no-marker"); err != nil || exists(filepath.Join(root, "AGENTS.md")) {
		t.Fatalf("--no-marker записал блок: %v", err)
	}

	// Только CLAUDE.md: блок в него, Codex о нём предупреждён.
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := runSetup(t, c, "--project", "ACME", "--agent", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readText(t, filepath.Join(root, "CLAUDE.md")), block) || exists(filepath.Join(root, "AGENTS.md")) {
		t.Fatal("блок не в CLAUDE.md")
	}
	mustContain(t, out, "Codex читает только AGENTS.md")

	// Явный --marker-file и ошибка Codex: блок записан, код ненулевой.
	_ = os.Remove(filepath.Join(root, "CLAUDE.md"))
	c2 := (&setupServer{kit: kitV1}).start(t)
	if _, _, err := runSetup(t, c2, "--project", "ACME", "--agent", "codex", "--marker-file", "AGENTS.md"); err == nil {
		t.Fatal("ожидалась ошибка Codex")
	}
	if !strings.Contains(readText(t, filepath.Join(root, "AGENTS.md")), block) {
		t.Fatal("ошибка Codex потеряла блок привязки")
	}
}

// --scope project у Codex: запись в .codex/config.toml корня без токена,
// повтор безвреден, запись с другим адресом не правится.
func TestSetupCodexProjectScope(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	cx := installCodex(t, "0.167.0")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)
	cfg := filepath.Join(root, ".codex", "config.toml")

	out, errOut, err := runSetup(t, c, "--project", "ACME", "--agent", "codex", "--scope", "project")
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := toml.Unmarshal([]byte(readText(t, cfg)), &doc); err != nil {
		t.Fatal(err)
	}
	srv := doc["mcp_servers"].(map[string]any)["sdlc"].(map[string]any)
	if srv["url"] != c.origin+"/mcp/projects/ACME" || srv["bearer_token_env_var"] != "SDLC_TOKEN" {
		t.Fatalf("запись: %v", srv)
	}
	if cx.called(t, "mcp add") || exists(filepath.Join(home, ".codex")) {
		t.Fatal("project-область дошла до пользовательского конфига")
	}
	noToken(t, []string{out, errOut}, root, home)

	before := readText(t, cfg)
	out, _, err = runSetup(t, c, "--project", "ACME", "--agent", "codex", "--scope", "project")
	if err != nil || readText(t, cfg) != before {
		t.Fatalf("повтор: %v", err)
	}
	mustContain(t, out, "уже подключён")

	c2 := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)
	out, _, err = runSetup(t, c2, "--project", "ACME", "--agent", "codex", "--scope", "project")
	if err != nil || readText(t, cfg) != before {
		t.Fatalf("чужой адрес правится: %v", err)
	}
	mustContain(t, out, "запись с другим адресом")
}

// AC-15: clab mcp install по той же привязке.
func TestMCPInstallAgents(t *testing.T) {
	withJSON(t, false)
	_, home := setupEnv(t)
	cl := installClaude(t, "")
	cx := installCodex(t, "0.167.0")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)
	install := func(args ...string) (string, error) {
		out, _, err := capture(t, func() error {
			return cmdMCP(context.Background(), c, append([]string{"install", "--project", "ACME"}, args...))
		})
		return out, err
	}

	out, err := install("--agent", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(cl.calls(t)) != 0 || !cx.called(t, "mcp add sdlc-acme --url "+c.origin+"/mcp/projects/ACME --bearer-token-env-var SDLC_TOKEN") {
		t.Fatalf("codex: %v %v", cl.calls(t), cx.calls(t))
	}
	mustContain(t, out, "sdlc-acme", "SDLC_TOKEN", "clab auth token")
	noToken(t, []string{out}, home)

	out, err = install("--agent", "generic")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, "${SDLC_TOKEN}", `"mcpServers"`)
	noToken(t, []string{out})

	// Только claude и auto — вызов один и прежний.
	t.Setenv("PATH", filepath.Dir(cl.log)+":/usr/bin:/bin")
	_ = os.Remove(cl.log)
	out, err = install()
	if err != nil {
		t.Fatal(err)
	}
	want := "mcp add --transport http --scope local sdlc " + c.origin + "/mcp/projects/ACME --header Authorization: Bearer " + setupToken
	if got := cl.calls(t); len(got) != 1 || got[0] != want {
		t.Fatalf("claude: %v", got)
	}
	mustContain(t, out, "MCP-сервер sdlc подключён к проекту ACME (local).", "/mcp__sdlc__work_on_task")

	// Неизвестный агент — отказ; config и url без изменений.
	if _, err := install("--agent", "nope"); err == nil {
		t.Fatal("неизвестный агент принят")
	}
	cfgOut, _, err := capture(t, func() error {
		return cmdMCP(context.Background(), c, []string{"config", "--project", "ACME"})
	})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, cfgOut, `"sdlc"`, "Bearer "+setupToken)
	urlOut, _, _ := capture(t, func() error {
		return cmdMCP(context.Background(), c, []string{"url", "--project", "ACME"})
	})
	if strings.TrimSpace(urlOut) != c.origin+"/mcp/projects/ACME" {
		t.Fatalf("url: %q", urlOut)
	}
}

func TestAgentNames(t *testing.T) {
	for in, want := range map[string]string{"auto": "auto", "claude": "claude", "codex": "codex", "generic": "generic", "other": "generic"} {
		if got, err := normalizeAgent(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := normalizeAgent("x"); err == nil || !strings.Contains(err.Error(), "auto, claude, codex, generic") {
		t.Errorf("x: %v", err)
	}
	if agentMCPName(agentCodex, "", "", "ACME") != "sdlc-acme" || agentMCPName(agentCodex, "project", "", "ACME") != "sdlc" ||
		agentMCPName(agentClaude, "", "", "ACME") != "sdlc" || agentMCPName(agentCodex, "", "x", "ACME") != "x" {
		t.Error("имена серверов")
	}
	if agentScope(agentCodex, "local") != "user" || agentScope(agentCodex, "project") != "project" || agentScope(agentClaude, "") != "local" {
		t.Error("области")
	}
}

// Привязка агента, которого clab не знает, не ломает setup: пропускается с
// предупреждением, Claude ставится. Пути такой привязки не проверяются и не
// пишутся — что бы сервер в ней ни отдал.
func TestSetupUnknownAgentSkipped(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	cl := installClaude(t, "")
	outside := filepath.Join(filepath.Dir(root), "gemini-escape")

	kit := agentKitFx("v1.0.0", "1", nil)
	kit["bindings"] = append(kit["bindings"].([]any), map[string]any{
		"name": "gemini",
		"kit":  map[string]string{"manifest": "../evil.json", "skills_dir": ".gemini/skills", "agents_dir": "/etc"},
		"files": entries([]kitFile{
			{"../gemini-escape/x.md", "x"}, {"/etc/x", "x"}, {".claude/settings.json", "{}"},
			{".gemini/skills/x/SKILL.md", "x"},
		}),
		"detect": nil, "levels": nil,
	})
	c := (&setupServer{kit: kit}).start(t)

	out, errOut, err := runSetup(t, c, "--project", "ACME")
	if err != nil {
		t.Fatalf("неизвестный агент остановил setup: %v", err)
	}
	mustContain(t, out, `привязка агента "gemini" пропущена`)
	if !cl.called(t, "mcp add") || !exists(filepath.Join(root, ".claude/skills/clab-work/SKILL.md")) {
		t.Fatalf("Claude не настроен: %v", cl.calls(t))
	}
	for _, p := range []string{filepath.Join(root, ".gemini"), filepath.Join(root, ".claude/settings.json"), outside, filepath.Join(filepath.Dir(root), "evil.json")} {
		if exists(p) {
			t.Errorf("записано по привязке неизвестного агента: %s", p)
		}
	}
	if strings.Contains(out+errOut, "ничего не записано") {
		t.Errorf("отказ из-за неизвестного агента:\n%s%s", out, errOut)
	}
	_ = home

	// --json: предупреждение там же.
	withJSON(t, true)
	out, _, err = runSetup(t, c, "--project", "ACME", "--no-marker")
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Warnings []string `json:"warnings"`
	}
	if json.Unmarshal([]byte(out), &rep) != nil || len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "gemini") {
		t.Fatalf("--json:\n%s", out)
	}
}

// Известный агент с вредным путём — по-прежнему отказ, даже если рядом есть
// привязка неизвестного агента: защита AC-8 для claude, codex и generic.
func TestSetupKnownAgentStaysStrictBesideUnknown(t *testing.T) {
	withJSON(t, false)
	root, home := setupEnv(t)
	installClaude(t, "")
	installCodex(t, "0.167.0")

	unknown := map[string]any{
		"name": "gemini", "kit": map[string]string{"manifest": ".gemini/clab-kit.json", "skills_dir": ".gemini/skills", "agents_dir": ".gemini/agents"},
		"files": entries([]kitFile{{".gemini/skills/x/SKILL.md", "x"}}), "detect": nil, "levels": nil,
	}
	cases := map[string]func(b map[string]any){
		"claude: путь наружу":        func(b map[string]any) { b["files"] = entries([]kitFile{{"../x/SKILL.md", "x"}}) },
		"claude: settings.json":      func(b map[string]any) { b["files"] = entries([]kitFile{{".claude/settings.json", "{}"}}) },
		"claude: чужой каталог":      func(b map[string]any) { b["kit"].(map[string]string)["skills_dir"] = ".git/hooks" },
		"codex: config.toml":         func(b map[string]any) { b["files"] = entries([]kitFile{{".codex/config.toml", "x"}}) },
		"codex: манифест вне списка": func(b map[string]any) { b["kit"].(map[string]string)["manifest"] = ".codex/config.toml" },
		"generic: каталог набора": func(b map[string]any) {
			b["kit"] = map[string]string{"manifest": ".agents/clab-kit.json", "skills_dir": "docs", "agents_dir": ".codex/agents"}
		},
	}
	for name, mod := range cases {
		kit := agentKitFx("v1.0.0", "1", nil)
		bs := kit["bindings"].([]any)
		var idx int
		switch {
		case strings.HasPrefix(name, "claude"):
			idx = 0
		case strings.HasPrefix(name, "codex"):
			idx = 1
		default:
			idx = 2
			bs[2].(map[string]any)["files"] = entries([]kitFile{{".agents/skills/x/SKILL.md", "x"}})
		}
		mod(bs[idx].(map[string]any))
		kit["bindings"] = append([]any{unknown}, bs...)
		c := (&setupServer{kit: kit}).start(t)
		before := snapshot(t, root, home)
		_, _, err := runSetup(t, c, "--project", "ACME")
		if err == nil || !strings.Contains(err.Error(), "недопустимый путь") || !strings.Contains(err.Error(), "ничего не записано") {
			t.Errorf("%s: %v", name, err)
		}
		sameSnapshot(t, before, snapshot(t, root, home), name)
	}
}

// Токен не печатается нигде: ни когда агента нет в PATH, ни с --dry-run, ни
// в --json, ни в тексте ошибки. Единственное место — --header у вызова
// claude mcp add (журнал заглушки), его тест не смотрит.
func TestTokenNeverPrinted(t *testing.T) {
	root, home := setupEnv(t)
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)

	setup := func(args ...string) func() (string, string, error) {
		return func() (string, string, error) {
			return runSetup(t, c, append([]string{"--project", "ACME"}, args...)...)
		}
	}
	install := func(args ...string) func() (string, string, error) {
		return func() (string, string, error) {
			return capture(t, func() error {
				return cmdMCP(context.Background(), c, append([]string{"install", "--project", "ACME"}, args...))
			})
		}
	}
	runs := map[string]func() (string, string, error){
		"setup auto":               setup(),
		"setup claude":             setup("--agent", "claude"),
		"setup claude dry-run":     setup("--agent", "claude", "--dry-run"),
		"setup codex":              setup("--agent", "codex"),
		"setup generic":            setup("--agent", "generic"),
		"setup auto dry-run":       setup("--dry-run"),
		"mcp install auto":         install(),
		"mcp install claude":       install("--agent", "claude"),
		"mcp install codex":        install("--agent", "codex"),
		"mcp install generic":      install("--agent", "generic"),
		"mcp install claude scope": install("--agent", "claude", "--scope", "user"),
	}

	for _, asJSON := range []bool{false, true} {
		for _, found := range []bool{false, true} {
			if found {
				installClaude(t, "")
				installCodex(t, "0.167.0")
			} else {
				t.Setenv("PATH", "/usr/bin:/bin")
			}
			for name, f := range runs {
				withJSON(t, asJSON)
				out, errOut, err := f()
				texts := []string{out, errOut}
				if err != nil {
					texts = append(texts, err.Error())
				}
				for i, s := range texts {
					if strings.Contains(s, setupToken) {
						t.Errorf("%s (json=%v, агенты в PATH=%v): токен в тексте #%d:\n%s", name, asJSON, found, i, s)
					}
				}
			}
		}
	}
	noToken(t, nil, root, home)
}
