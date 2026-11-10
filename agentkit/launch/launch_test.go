package launch

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// CLAB-419, AC-6: подстановки в шаблоны run привязок claude и codex из
// набора; пустое значение снимает слово вместе с флагом.

func runOf(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile("../bindings/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var b struct {
		Run []string `json:"run"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b.Run
}

func full() map[string]string {
	return map[string]string{
		SlotModel: "m1", SlotEffort: "high", SlotMCPConfig: "/r/mcp.json", SlotMCPURL: "https://h/mcp/projects/P",
		SlotConfigDir: "/r/clab", SlotRunDir: "/r", SlotAllowedTools: "Bash,Read", SlotDisallowedTools: "Write",
		SlotTask: "P-1", SlotRole: "developer", SlotWorkdir: "/w", SlotHookScript: `"exit 0"`, SlotPrompt: "do {model} it",
	}
}

func without(v map[string]string, slots ...string) map[string]string {
	out := map[string]string{}
	for k, x := range v {
		out[k] = x
	}
	for _, s := range slots {
		out[s] = ""
	}
	return out
}

func has(argv []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(argv); i++ {
		if reflect.DeepEqual(argv[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestExpandClaude(t *testing.T) {
	run := runOf(t, "claude")
	argv := Expand(run, full())
	for _, seq := range [][]string{
		// Порядок команды раннера до CLAB-420: argv с одним claude не меняется.
		{"claude", "-p", "--output-format", "json", "--mcp-config", "/r/mcp.json", "--strict-mcp-config"},
		{"--model", "m1", "--effort", "high"},
		{"--allowedTools", "Bash,Read", "--disallowedTools", "Write", "do {model} it"},
	} {
		if !has(argv, seq...) {
			t.Errorf("нет %q в %q", seq, argv)
		}
	}
	argv = Expand(run, without(full(), SlotModel, SlotEffort, SlotAllowedTools, SlotDisallowedTools))
	for _, w := range []string{"--model", "--effort", "--allowedTools", "--disallowedTools", ""} {
		for _, a := range argv {
			if a == w {
				t.Errorf("пустая подстановка оставила %q: %q", w, argv)
			}
		}
	}
	if argv[len(argv)-1] != "do {model} it" || !has(argv, "--mcp-config", "/r/mcp.json", "--strict-mcp-config", "do {model} it") {
		t.Errorf("инструкция не на месте: %q", argv)
	}
}

func TestExpandCodex(t *testing.T) {
	run := runOf(t, "codex")
	argv := Expand(run, full())
	for _, seq := range [][]string{
		{"codex", "exec", "--json"},
		{"--add-dir", "/r"},
		{"-m", "m1"},
		{"-c", `model_reasoning_effort="high"`},
		{"-c", `mcp_servers.sdlc.url="https://h/mcp/projects/P"`},
		{"-c", `mcp_servers.sdlc.bearer_token_env_var="SDLC_TOKEN"`},
		{"-c", `shell_environment_policy.exclude=["SDLC_TOKEN"]`},
		{"-c", `hooks.PreToolUse=[{matcher="Bash",hooks=[{type="command",command="exit 0"}]}]`},
		{"--dangerously-bypass-hook-trust"},
		{"--ignore-user-config"},
	} {
		if !has(argv, seq...) {
			t.Errorf("нет %q в %q", seq, argv)
		}
	}
	// {run_dir}, а не {config_dir}: временные файлы — в каталоге захода.
	if has(argv, "--add-dir", "/r/clab") {
		t.Errorf("--add-dir называет каталог токена: %q", argv)
	}
	argv = Expand(run, without(full(), SlotModel, SlotEffort, SlotHookScript))
	for _, a := range argv {
		if a == "-m" || strings.Contains(a, "model_reasoning_effort") || strings.Contains(a, "hooks.PreToolUse") {
			t.Errorf("пустая подстановка оставила %q: %q", a, argv)
		}
	}
	// Флаги перед снятыми значениями ушли парой, соседние -c остались.
	if !has(argv, "-c", "sandbox_workspace_write.network_access=true") || !has(argv, "-c", `mcp_servers.sdlc.url="https://h/mcp/projects/P"`) {
		t.Errorf("снято лишнее: %q", argv)
	}
	if argv[len(argv)-1] != "do {model} it" {
		t.Errorf("инструкция не последним словом: %q", argv)
	}
}

func TestExpandPairs(t *testing.T) {
	cases := []struct {
		run  []string
		vals map[string]string
		want []string
	}{
		{[]string{"a", "--model", "{model}", "{prompt}"}, map[string]string{SlotPrompt: "p"}, []string{"a", "p"}},
		{[]string{"a", "-m", "{model}", "-x"}, nil, []string{"a", "-x"}},
		{[]string{"a", "-c", `k="{effort}"`, "-c", "z=1"}, nil, []string{"a", "-c", "z=1"}},
		{[]string{"a", "--json", "{prompt}"}, nil, []string{"a", "--json"}},
		{[]string{"a", "--model={model}", "--json"}, nil, []string{"a", "--json"}},
		{[]string{"a", "--flag=1", "{model}"}, nil, []string{"a", "--flag=1"}},
		{[]string{"a", "{task}-{role}", "{unknown}"}, map[string]string{SlotTask: "P-1", SlotRole: "dev"}, []string{"a", "P-1-dev", "{unknown}"}},
	}
	for i, c := range cases {
		if got := Expand(c.run, c.vals); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%d: %q → %q, ждали %q", i, c.run, got, c.want)
		}
	}
}

func TestQuotedIsTOMLAndJSON(t *testing.T) {
	s := "a \"b\" \\ c\n\td & <e>"
	q := Quoted(s)
	var back string
	if err := json.Unmarshal([]byte(q), &back); err != nil || back != s {
		t.Fatalf("%s → %q, %v", q, back, err)
	}
	if strings.Contains(q, "\\u003c") || strings.Contains(q, "\n") {
		t.Errorf("лишнее экранирование или перевод строки: %s", q)
	}
}

func TestHookCommand(t *testing.T) {
	content := "---\ntools: Bash\nhooks:\n  PreToolUse:\n    - matcher: Bash\n      hooks:\n        - type: command\n          command: |\n            in=$(cat)\n\n            exit 2\n---\n\nbody\n"
	got, err := HookCommand(content)
	if err != nil || got != "in=$(cat)\n\nexit 2" {
		t.Fatalf("%q, %v", got, err)
	}
	if _, err := HookCommand("---\nname: x\n---\n"); err != ErrNoHook {
		t.Fatalf("без хука: %v", err)
	}
}
