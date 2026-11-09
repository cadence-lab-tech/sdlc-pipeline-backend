package agentkit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pelletier/go-toml/v2"
)

// CLAB-417 (решение 0077, п. 3): ядро набора и привязки claude, codex,
// generic. Тесты ниже — по критериям приёмки AC-1…AC-12 задачи.

var coreSkills = []string{"clab-backlog", "clab-guide", "clab-rules", "clab-run", "clab-status", "clab-work"}

func loadKit(t *testing.T) Kit {
	t.Helper()
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func binding(t *testing.T, k Kit, name string) Binding {
	t.Helper()
	for _, b := range k.Bindings {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("в наборе нет привязки %s", name)
	return Binding{}
}

func filesByPath(files []File) map[string]File {
	m := map[string]File{}
	for _, f := range files {
		m[f.Path] = f
	}
	return m
}

func sumOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// coreFiles — все тексты ядра: скилы, агент и справка.
func coreFiles(k Kit) []File {
	out := append(append([]File{}, k.Core.Skills...), k.Core.Agents...)
	return append(out, k.Core.Guide)
}

// AC-1: раскладка ядра и привязок, прежних каталогов нет, справка — один
// источник для core.guide и тела скила clab-guide.
func TestKitLayout(t *testing.T) {
	for _, old := range []string{"skills", "agents"} {
		if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("каталог agentkit/%s остался: %v", old, err)
		}
	}
	for _, name := range coreSkills {
		if _, err := fs.Stat(source, "core/skills/"+name+"/SKILL.md"); err != nil {
			t.Errorf("нет core/skills/%s/SKILL.md", name)
		}
	}
	for _, p := range []string{"core/agents/clab-stage.md", "core/guide.md", "bindings/claude.json", "bindings/codex.json", "bindings/generic.json"} {
		if _, err := fs.Stat(source, p); err != nil {
			t.Errorf("нет %s", p)
		}
	}
	k := loadKit(t)
	guide, _ := fs.ReadFile(source, "core/guide.md")
	if k.Core.Guide.Content != string(guide) || k.Core.Guide.Path != "guide.md" || k.Core.Guide.SHA256 != sumOf(string(guide)) {
		t.Error("core.guide не равен core/guide.md")
	}
	raw, _ := fs.ReadFile(source, "core/skills/clab-guide/SKILL.md")
	if Body(string(raw)) != "" {
		t.Error("core/skills/clab-guide/SKILL.md несёт тело — справка живёт только в core/guide.md")
	}
	for _, files := range [][]File{k.Files, k.Core.Skills, binding(t, k, "claude").Files, binding(t, k, "codex").Files} {
		for _, f := range files {
			if strings.HasSuffix(f.Path, "clab-guide/SKILL.md") && !strings.HasPrefix(Body(f.Content), string(guide)) {
				t.Errorf("%s: тело не начинается со справки core/guide.md", f.Path)
			}
		}
	}
	var names []string
	for _, b := range k.Bindings {
		names = append(names, b.Name)
	}
	if strings.Join(names, ",") != "claude,codex,generic" {
		t.Errorf("привязки %v — ждали claude, codex, generic по имени", names)
	}
}

// Запреты ядра (AC-2): провайдер, его модели, тулы, пути и поля. Слова —
// без учёта регистра и по границам слова; остальное — подстрокой.
var (
	neutralWords = regexp.MustCompile(`(?i)\b(claude|anthropic|haiku|sonnet|opus|codex)\b`)
	neutralIDs   = regexp.MustCompile(`(?i)\b(claude-[a-z]+-[0-9]|gpt-[0-9a-z])`)
	neutralBans  = []string{
		"mcp__", ".claude", ".codex", ".agents/", "AskUserQuestion", "run_in_background",
		"PreToolUse", "TodoWrite", "$ARGUMENTS", "argument-hint", "`Agent`", "Agent tool", "SDLC_PROJECT=",
	}
	// Исключения — явным списком, не шаблоном: имена файлов строки
	// привязки и документов репозитория.
	neutralAllowed = []string{"CLAUDE.md", "AGENTS.md"}
)

// neutralViolations — запрещённое в тексте ядра; SDLC_PROJECT= считается
// отдельно (одно место в clab-run).
func neutralViolations(text string) []string {
	for _, ok := range neutralAllowed {
		text = strings.ReplaceAll(text, ok, "")
	}
	var out []string
	out = append(out, neutralWords.FindAllString(text, -1)...)
	out = append(out, neutralIDs.FindAllString(text, -1)...)
	for _, s := range neutralBans {
		if s != "SDLC_PROJECT=" && strings.Contains(text, s) {
			out = append(out, s)
		}
	}
	return out
}

// AC-2: ядро нейтрально. Единственное SDLC_PROJECT= — префикс исполнителя
// в clab-run: под каталогом токена задачи строка привязки не читается, и
// проект исполнителю задаёт окружение.
func TestCoreIsNeutral(t *testing.T) {
	k := loadKit(t)
	project := map[string]int{"skills/clab-run/SKILL.md": 1}
	for _, f := range coreFiles(k) {
		if v := neutralViolations(f.Content); len(v) > 0 {
			t.Errorf("ядро %s: %q", f.Path, v)
		}
		if n := strings.Count(f.Content, "SDLC_PROJECT="); n != project[f.Path] {
			t.Errorf("ядро %s: SDLC_PROJECT= %d раз, ждали %d", f.Path, n, project[f.Path])
		}
		if f.Path == "guide.md" {
			continue
		}
		for key := range Frontmatter(f.Content) {
			if key != "name" && key != "description" {
				t.Errorf("ядро %s: ключ фронтматтера %q", f.Path, key)
			}
		}
	}
	// Проверка ловит каждое запрещённое слово, вернувшееся в ядро.
	samples := []string{"Claude", "anthropic", "HAIKU", "sonnet", "opus", "Codex", "claude-opus-5-5", "gpt-5"}
	samples = append(samples, neutralBans...)
	for _, s := range samples {
		if s == "SDLC_PROJECT=" {
			continue
		}
		if len(neutralViolations("Run it with "+s+" here.")) == 0 {
			t.Errorf("проверка ядра пропускает %q", s)
		}
	}
	if v := neutralViolations("See `CLAUDE.md` and `AGENTS.md`; an agent of the stage."); len(v) > 0 {
		t.Errorf("ложное срабатывание: %q", v)
	}
}

// Фронтматтер файлов files на main 023d018 — sha256 блока между «---»:
// ключи и значения не меняются ни на символ (AC-3). Исключение —
// description у clab-run и clab-stage (CLAB-419, R13): «sub-agent» и
// «/clab-run» стали нейтральными, ключи и остальные значения те же.
var legacyFrontmatter = map[string]string{
	"agents/clab-stage.md":         "06fc8d08edb777c529b7ba6ac52df55c4f4f1d13c0314bbd7049e2418db12746",
	"skills/clab-backlog/SKILL.md": "2513a7c331953b4f537bef7a2d6711db26aad7452e43d3a7146dcddc6f06a62f",
	"skills/clab-guide/SKILL.md":   "0c025381435fee7ec88755b7b1570985ad41e6c427e07020b023ff41ab3f6a32",
	"skills/clab-rules/SKILL.md":   "0574ecc95ac9828f9e0a5e5cfa69a3e6d13e4e211499c63b6988157fae50e3fe",
	"skills/clab-run/SKILL.md":     "93e6c5c2e4030847cb5c5b01998e6a378d9eac5ea06a87b72cede3574138f2a1",
	"skills/clab-status/SKILL.md":  "3b6b4a49de0bd8117ed164ad0e6892821f314a47aa075d1154feef4c2afdbc6a",
	"skills/clab-work/SKILL.md":    "0d15d03c346aba480f2e14be48e71e0d1d9010be6fd44407a011cf03b38838fd",
}

// AC-3: files — привязка claude без префикса .claude/: те же пути, тексты и
// суммы; фронтматтер — как на main.
func TestClaudeFilesAreLegacy(t *testing.T) {
	k := loadKit(t)
	b := binding(t, k, "claude")
	if len(b.Files) != len(k.Files) {
		t.Fatalf("у привязки claude %d файлов, в files %d", len(b.Files), len(k.Files))
	}
	for i, f := range b.Files {
		legacy := k.Files[i]
		if ".claude/"+legacy.Path != f.Path || legacy.Content != f.Content || legacy.SHA256 != f.SHA256 || f.SHA256 != sumOf(f.Content) {
			t.Errorf("files[%d] %s не равен %s привязки claude", i, legacy.Path, f.Path)
		}
	}
	for _, f := range k.Files {
		d, _ := parseDoc(f.Content)
		if want, ok := legacyFrontmatter[f.Path]; !ok || sumOf(d.front) != want {
			t.Errorf("%s: фронтматтер не тот, что на main:\n%s", f.Path, d.front)
		}
	}
}

// AC-4: всё провайдерное, вынесенное из ядра, вернулось в набор Claude;
// таблица уровней в заметках clab-run совпадает с полем levels привязки.
func TestClaudeNotesCarryProviderText(t *testing.T) {
	k := loadKit(t)
	flat := map[string]string{}
	for _, f := range k.Files {
		flat[f.Path] = strings.Join(strings.Fields(f.Content), " ")
	}
	want := map[string][]string{
		"skills/clab-run/SKILL.md": {
			"`model` parameter of the `Agent` tool", "run_in_background", "AskUserQuestion",
			".claude/agents/clab-stage.md", "claude-opus-5-5", "type `clab-stage`", "`inherit`",
			"`PreToolUse` hook", "trust dialog", "`general-purpose`", "$ARGUMENTS",
		},
		"skills/clab-work/SKILL.md":    {"claude-opus-5-5", "$ARGUMENTS", "run_in_background", "`.claude/skills`"},
		"skills/clab-guide/SKILL.md":   {"/mcp__sdlc__work_on_task", "`.claude/skills`", "`.claude/agents`", "`clab-plugin`", "claude mcp add"},
		"skills/clab-backlog/SKILL.md": {"$ARGUMENTS"},
		"skills/clab-rules/SKILL.md":   {"$ARGUMENTS", "`.claude/skills`"},
		"skills/clab-status/SKILL.md":  {"$ARGUMENTS"},
		"agents/clab-stage.md":         {"`PreToolUse` hook", "`.claude/skills`"},
	}
	for p, needles := range want {
		for _, s := range needles {
			if !strings.Contains(flat[p], s) {
				t.Errorf("%s (claude): нет %q", p, s)
			}
		}
	}
	for _, f := range k.Files {
		if strings.HasPrefix(f.Path, "skills/") && path.Base(path.Dir(f.Path)) != "clab-guide" && Frontmatter(f.Content)["argument-hint"] == "" {
			t.Errorf("%s: нет argument-hint", f.Path)
		}
	}
	levels := binding(t, k, "claude").Levels
	if levels == nil {
		t.Fatal("у привязки claude нет levels")
	}
	var run string
	for _, f := range k.Files {
		if f.Path == "skills/clab-run/SKILL.md" {
			run = f.Content
		}
	}
	got := map[string]string{}
	for _, m := range regexp.MustCompile("(?m)^\\| `([a-z]+)` \\| `([a-z]+)` \\|$").FindAllStringSubmatch(run, -1) {
		got[m[1]] = m[2]
	}
	if len(got) != 3 || got["low"] != levels.Low || got["medium"] != levels.Medium || got["high"] != levels.High {
		t.Errorf("таблица clab-run %v не совпадает с levels привязки %+v", got, *levels)
	}
}

// AC-5: набор Codex — скилы ядра в .agents/skills, агент TOML.
func TestCodexBinding(t *testing.T) {
	k := loadKit(t)
	b := binding(t, k, "codex")
	if b.Kit == nil || b.Kit.Manifest != ".agents/clab-kit.json" || b.Kit.SkillsDir != ".agents/skills" || b.Kit.AgentsDir != ".codex/agents" {
		t.Fatalf("kit codex: %+v", b.Kit)
	}
	files := filesByPath(b.Files)
	if len(files) != len(coreSkills)+1 {
		t.Errorf("у codex %d файлов", len(files))
	}
	core := filesByPath(k.Core.Skills)
	for _, name := range coreSkills {
		f, ok := files[".agents/skills/"+name+"/SKILL.md"]
		if !ok {
			t.Errorf("нет .agents/skills/%s/SKILL.md", name)
			continue
		}
		d, _ := parseDoc(f.Content)
		coreDoc, _ := parseDoc(core["skills/"+name+"/SKILL.md"].Content)
		meta := Frontmatter(f.Content)
		if d.front != coreDoc.front || len(meta) != 2 || meta["name"] != name || meta["description"] == "" {
			t.Errorf("%s: фронтматтер не name и description ядра:\n%s", f.Path, d.front)
		}
		// Заметки Codex (CLAB-419) — только у clab-run, дописаны в конец
		// тела ядра; остальные скилы — ядро байт в байт.
		coreContent := core["skills/"+name+"/SKILL.md"].Content
		if name == "clab-run" {
			if !strings.HasPrefix(f.Content, coreContent) || !strings.Contains(f.Content[len(coreContent):], "## Notes for Codex") {
				t.Errorf("%s: не ядро с заметками Codex в конце", f.Path)
			}
		} else if f.Content != coreContent {
			t.Errorf("%s: не ядро", f.Path)
		}
		for _, s := range []string{"argument-hint", "$ARGUMENTS", "\nmodel:", "\neffort:", "/mcp__"} {
			if strings.Contains(f.Content, s) {
				t.Errorf("%s: %q", f.Path, s)
			}
		}
	}
	for _, f := range b.Files {
		if f.SHA256 != sumOf(f.Content) {
			t.Errorf("%s: sha256 не сходится", f.Path)
		}
		// Нейтральна часть ядра; заметки привязки называют своего агента.
		content := f.Content
		if c, ok := core[strings.TrimPrefix(f.Path, ".agents/")]; ok && strings.HasPrefix(content, c.Content) {
			content = c.Content
		}
		if v := neutralViolations(content); len(v) > 0 {
			t.Errorf("%s: %q", f.Path, v)
		}
	}
	agent, ok := files[".codex/agents/clab-stage.toml"]
	if !ok {
		t.Fatal("нет .codex/agents/clab-stage.toml")
	}
	var parsed map[string]any
	if err := toml.Unmarshal([]byte(agent.Content), &parsed); err != nil {
		t.Fatalf("агент Codex не TOML: %v", err)
	}
	if parsed["name"] != "clab-stage" || parsed["description"] == "" || parsed["developer_instructions"] != Body(k.Core.Agents[0].Content) {
		t.Errorf("агент Codex: name %v, description %v, developer_instructions — не тело ядра", parsed["name"], parsed["description"])
	}
	if b.Executor != "process" || b.Output == nil || *b.Output != "codex-jsonl" || b.Levels != nil || b.ToolPolicy != nil {
		t.Errorf("поля codex: executor %q, output %v, levels %v, tool_policy %v", b.Executor, b.Output, b.Levels, b.ToolPolicy)
	}
	if len(b.Detect) != 1 || b.Detect[0].Env != "CODEX_THREAD_ID" || !b.Detect[0].NonEmpty {
		t.Errorf("detect codex: %+v", b.Detect)
	}
	if !strings.Contains(string(b.MCP), `"method":"codex-config-toml"`) || !strings.Contains(string(b.MCP), `"bearer_token_env_var":"SDLC_TOKEN"`) ||
		!strings.Contains(string(b.Hook), `"placement":"launch"`) || len(b.Run) < 3 || b.Run[0] != "codex" || b.Run[1] != "exec" || b.Run[2] != "--json" {
		t.Errorf("mcp %s, hook %s, run %v", b.MCP, b.Hook, b.Run)
	}
}

// AC-5: тело агента с обратной косой чертой и тройными кавычками собирается
// в TOML, который разбирается обратно в то же тело.
func TestTomlAgentEscapes(t *testing.T) {
	for _, body := range []string{
		"plain\ntext\n",
		"a \\ backslash and '''triple''' and \"\"\" quotes\n",
		"ends with a quote'",
		"tab\tand \x01 control\n",
	} {
		a := coreEntry{name: "x", doc: doc{front: "name: x\ndescription: \"d \\\\ \"\"\n", body: "\n" + body}}
		out, err := tomlAgent(a, "# extra\nsandbox_mode = \"read-only\"\n")
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		var parsed map[string]any
		if err := toml.Unmarshal([]byte(out), &parsed); err != nil || parsed["developer_instructions"] != body || parsed["sandbox_mode"] != "read-only" {
			t.Errorf("%q: разбор %v, %v", body, err, parsed)
		}
	}
}

// rawBindings — файлы привязок как записаны: явный null отличается от
// отсутствия поля только здесь.
func rawBindings(t *testing.T) map[string]map[string]json.RawMessage {
	t.Helper()
	out := map[string]map[string]json.RawMessage{}
	for _, name := range []string{"claude", "codex", "generic"} {
		raw, err := fs.ReadFile(source, "bindings/"+name+".json")
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out[name] = m
	}
	return out
}

// AC-6: все девять полей в каждой привязке, пустое — явный null; generic
// без набора.
func TestBindingFieldsExplicit(t *testing.T) {
	raw := rawBindings(t)
	for name, m := range raw {
		if len(m) != len(bindingFields)+1 {
			t.Errorf("%s: %d полей", name, len(m))
		}
		for _, f := range bindingFields {
			if _, ok := m[f]; !ok {
				t.Errorf("%s: нет поля %s", name, f)
			}
		}
	}
	for _, f := range []string{"kit", "mcp", "run", "output", "detect", "levels", "tool_policy", "hook"} {
		if string(raw["generic"][f]) != "null" {
			t.Errorf("generic.%s = %s, ждали null", f, raw["generic"][f])
		}
	}
	if string(raw["generic"]["executor"]) != `"process"` {
		t.Errorf("generic.executor = %s", raw["generic"]["executor"])
	}
	// В ответе — те же явные null и пустой список файлов.
	k := loadKit(t)
	body, err := json.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Bindings []map[string]json.RawMessage `json:"bindings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	for _, b := range resp.Bindings {
		var name string
		_ = json.Unmarshal(b["name"], &name)
		for _, f := range append([]string{"files"}, bindingFields...) {
			v, ok := b[f]
			if !ok {
				t.Errorf("ответ, %s: нет поля %s", name, f)
			}
			if want, isNull := raw[name][f]; isNull && string(want) == "null" && string(v) != "null" {
				t.Errorf("ответ, %s.%s = %s, ждали null", name, f, v)
			}
		}
		if name == "generic" && string(b["files"]) != "[]" {
			t.Errorf("ответ, generic.files = %s", b["files"])
		}
	}
}

// AC-12: формы полей, на которые опираются соседние части эпика, — по
// сырому JSON файлов.
func TestBindingShapes(t *testing.T) {
	raw := rawBindings(t)
	compact := func(m json.RawMessage) string {
		var v any
		_ = json.Unmarshal(m, &v)
		out, _ := json.Marshal(v)
		return string(out)
	}
	exact := map[string]map[string]string{
		"claude": {
			"detect":      `[{"env":"AI_AGENT","prefix":"claude-code_"},{"env":"CLAUDECODE","equals":"1"}]`,
			"levels":      `{"high":"opus","low":"haiku","medium":"sonnet"}`,
			"kit":         `{"agents_dir":".claude/agents","manifest":".claude/clab-kit.json","skills_dir":".claude/skills"}`,
			"executor":    `"subagent"`,
			"output":      `"claude-json"`,
			"tool_policy": `{"allow":"--allowedTools","deny":"--disallowedTools"}`,
		},
		"codex": {
			"detect":      `[{"env":"CODEX_THREAD_ID","non_empty":true}]`,
			"levels":      `null`,
			"kit":         `{"agents_dir":".codex/agents","manifest":".agents/clab-kit.json","skills_dir":".agents/skills"}`,
			"executor":    `"process"`,
			"output":      `"codex-jsonl"`,
			"tool_policy": `null`,
		},
	}
	for name, fields := range exact {
		for f, want := range fields {
			if got := compact(raw[name][f]); got != want {
				t.Errorf("%s.%s = %s, ждали %s", name, f, got, want)
			}
		}
	}
	slots := regexp.MustCompile(`\{[a-z_]+\}`)
	for name, m := range raw {
		var run []string
		if string(m["run"]) == "null" {
			continue
		}
		if err := json.Unmarshal(m["run"], &run); err != nil || len(run) == 0 {
			t.Errorf("%s.run: %s", name, m["run"])
		}
		joined := strings.Join(run, " ")
		for _, s := range slots.FindAllString(joined, -1) {
			if !contains(knownRunSlots, s) {
				t.Errorf("%s.run: подстановка %s", name, s)
			}
		}
		for _, s := range []string{"{model}", "{effort}", "{prompt}"} {
			if !strings.Contains(joined, s) {
				t.Errorf("%s.run: нет %s", name, s)
			}
		}
	}
	for name, m := range raw {
		for f, key := range map[string]string{"mcp": "method", "hook": "placement"} {
			if string(m[f]) == "null" {
				continue
			}
			var obj map[string]any
			if err := json.Unmarshal(m[f], &obj); err != nil || obj[key] == nil {
				t.Errorf("%s.%s: нужен null или объект с %s: %s", name, f, key, m[f])
			}
		}
	}
	if !strings.Contains(compact(raw["claude"]["mcp"]), `"method":"claude-mcp-add"`) ||
		!strings.Contains(compact(raw["codex"]["mcp"]), `"method":"codex-config-toml"`) ||
		!strings.Contains(compact(raw["claude"]["hook"]), `"placement":"agent-frontmatter"`) ||
		!strings.Contains(compact(raw["codex"]["hook"]), `"placement":"launch"`) {
		t.Error("mcp.method или hook.placement не по 0077")
	}
	var claudeRun []string
	_ = json.Unmarshal(raw["claude"]["run"], &claudeRun)
	if strings.Join(claudeRun[:4], " ") != "claude -p --output-format json" {
		t.Errorf("claude.run: %v", claudeRun)
	}
}

// kitFS — копия встроенного набора на fstest.MapFS: тесты отказов и
// поддельной привязки меняют её, а не настоящий набор.
func kitFS(t *testing.T) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	err := fs.WalkDir(source, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(source, p)
		m[p] = &fstest.MapFile{Data: raw}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const fakeBinding = `{
  "name": "fake",
  "kit": {"manifest": ".fake/clab-kit.json", "skills_dir": ".fake/skills", "agents_dir": ".fake/agents"},
  "mcp": {"method": "codex-config-toml"},
  "run": ["fake", "--model", "{model}", "{prompt}"],
  "executor": "process",
  "hook": null,
  "output": "codex-jsonl",
  "detect": [{"env": "FAKE_AGENT", "equals": "1"}],
  "levels": {"low": "f-1", "medium": "f-2", "high": "f-3"},
  "tool_policy": null
}`

// AC-8: новая привязка на известных форматах — только файлом набора: сборка
// отдаёт её с файлами и верными суммами, Go-код о ней не знает.
func TestFakeBindingByFileOnly(t *testing.T) {
	m := kitFS(t)
	m["bindings/fake.json"] = &fstest.MapFile{Data: []byte(fakeBinding)}
	m["bindings/fake/skills/clab-work.md"] = &fstest.MapFile{Data: []byte("---\nhint: x\n---\n\n## Notes for the fake agent\n\n- Nothing special.\n")}
	k, err := build(m)
	if err != nil {
		t.Fatal(err)
	}
	b := binding(t, k, "fake")
	files := filesByPath(b.Files)
	work, ok := files[".fake/skills/clab-work/SKILL.md"]
	if !ok || len(files) != len(coreSkills)+1 || files[".fake/agents/clab-stage.md"].Content == "" {
		t.Fatalf("файлы поддельной привязки: %v", b.Files)
	}
	if Frontmatter(work.Content)["hint"] != "x" || !strings.HasSuffix(work.Content, "- Nothing special.\n") {
		t.Errorf("фрагмент не лёг в файл:\n%s", work.Content)
	}
	for _, f := range b.Files {
		if f.SHA256 != sumOf(f.Content) {
			t.Errorf("%s: sha256", f.Path)
		}
	}
	if b.Levels == nil || b.Levels.High != "f-3" || b.Detect[0].Env != "FAKE_AGENT" {
		t.Errorf("поля: %+v", b)
	}
	// Код сборки имени поддельной привязки не знает.
	src, _ := os.ReadFile("kit.go")
	if strings.Contains(string(src), "fake") {
		t.Error("kit.go называет поддельную привязку")
	}
}

// AC-7: сборка отказывает с именем привязки и поля.
func TestBuildRejectsBadBinding(t *testing.T) {
	edit := func(field, value string) string {
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(fakeBinding), &m)
		if value == "" {
			delete(m, field)
		} else {
			m[field] = json.RawMessage(value)
		}
		out, _ := json.Marshal(m)
		return string(out)
	}
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"нет поля", map[string]string{"bindings/fake.json": edit("levels", "")}, []string{"fake", `"levels"`}},
		{"поле не из девяти", map[string]string{"bindings/fake.json": edit("model", `"x"`)}, []string{"fake", `"model"`}},
		{"executor", map[string]string{"bindings/fake.json": edit("executor", `"thread"`)}, []string{"fake", "executor"}},
		{"output", map[string]string{"bindings/fake.json": edit("output", `"xml"`)}, []string{"fake", "output"}},
		{"mcp", map[string]string{"bindings/fake.json": edit("mcp", `{"method":"telepathy"}`)}, []string{"fake", "mcp"}},
		{"hook", map[string]string{"bindings/fake.json": edit("hook", `{"placement":"elsewhere"}`)}, []string{"fake", "hook"}},
		{"detect", map[string]string{"bindings/fake.json": edit("detect", `[{"env":"X"}]`)}, []string{"fake", "detect"}},
		{"levels", map[string]string{"bindings/fake.json": edit("levels", `{"low":"a","high":"b"}`)}, []string{"fake", "levels"}},
		{"подстановка run", map[string]string{"bindings/fake.json": edit("run", `["fake","{secret}"]`)}, []string{"fake", "run"}},
		{"kit вне проекта", map[string]string{"bindings/fake.json": edit("kit", `{"manifest":"../x","skills_dir":"s","agents_dir":"a"}`)}, []string{"fake", "kit"}},
		{"имя файла", map[string]string{"bindings/fake.json": edit("name", `"other"`)}, []string{"fake", "name"}},
		{"фрагмент чужого скила", map[string]string{"bindings/fake.json": fakeBinding, "bindings/fake/skills/clab-nope.md": "x"}, []string{"bindings/fake/skills/clab-nope.md"}},
		{"хук без фрагмента", map[string]string{"bindings/fake.json": edit("hook", `{"placement":"agent-frontmatter","agent":"clab-stage","event":"PreToolUse"}`)}, []string{"fake", "hook", "bindings/fake/agents/clab-stage.md"}},
		{"кириллица во фрагменте", map[string]string{"bindings/fake.json": fakeBinding, "bindings/fake/skills/clab-work.md": "\nЗаметка\n"}, []string{"bindings/fake/skills/clab-work.md", "кириллица"}},
		{"кириллица в файле", map[string]string{"bindings/fake.json": edit("levels", `{"low":"а","medium":"b","high":"c"}`)}, []string{"bindings/fake.json", "кириллица"}},
		{"фрагменты без kit", map[string]string{"bindings/fake.json": edit("kit", "null"), "bindings/fake/skills/clab-work.md": "x"}, []string{"fake", "kit"}},
		{"каталог без файла", map[string]string{"bindings/ghost/skills/clab-work.md": "x"}, []string{"bindings/ghost"}},
	}
	for _, c := range cases {
		m := kitFS(t)
		for p, data := range c.files {
			m[p] = &fstest.MapFile{Data: []byte(data)}
		}
		_, err := build(m)
		if err == nil {
			t.Errorf("%s: сборка прошла", c.name)
			continue
		}
		for _, s := range c.want {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("%s: в ошибке %q нет %q", c.name, err, s)
			}
		}
	}
	// Ядро: лишний ключ фронтматтера и тело у скила справки.
	m := kitFS(t)
	m["core/skills/clab-work/SKILL.md"] = &fstest.MapFile{Data: []byte("---\nname: clab-work\ndescription: d\nargument-hint: x\n---\n\nbody\n")}
	if _, err := build(m); err == nil || !strings.Contains(err.Error(), "argument-hint") {
		t.Errorf("лишний ключ ядра: %v", err)
	}
	m = kitFS(t)
	m["core/skills/clab-guide/SKILL.md"] = &fstest.MapFile{Data: []byte("---\nname: clab-guide\ndescription: d\n---\n\nsecond copy\n")}
	if _, err := build(m); err == nil || !strings.Contains(err.Error(), "guide.md") {
		t.Errorf("второй источник справки: %v", err)
	}
}
