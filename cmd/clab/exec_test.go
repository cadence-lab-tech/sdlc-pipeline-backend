package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit/launch"
)

// CLAB-419: clab exec — исполнитель этапа процессом. Подставной сервер
// отвечает на состояние, инструкцию, набор и продления; агент — заглушка
// на sh, которая пишет свои argv, окружение и stdin в каталог STUB_OUT.

const (
	execTask  = "PROJ-7"
	execToken = "clab_task_s3cr3t-token-value"
	execText  = "Возьми задачу PROJ-7 в работу.\n\n1. clab claim PROJ-7 {model}\n"
)

type execServer struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	hbs    int
	state  string
	kit    string // "" — ручки нет
	prompt string // "" — ручки нет
	// heartbeat — ответ на n-е продление (с нуля).
	heartbeat func(n int) (int, string)
	srv       *httptest.Server
}

func newExecServer(t *testing.T) *execServer {
	t.Helper()
	k, err := agentkit.Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	kit, _ := json.Marshal(k)
	s := &execServer{t: t, kit: string(kit), prompt: execText,
		state:     stateJSON(`{"model_level":"high","model_level_source":"stage","agent_settings_version":3,"models":{"codex":{"model":"gpt-x","effort":"high"}}}`, `null`),
		heartbeat: func(int) (int, string) { return leaseOK(time.Hour, 300) },
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func stateJSON(execution, toolPolicy string) string {
	return `{"stage_run_id":"` + leaseRun + `","task":{"key":"PROJ-7","status":"development"},"role":{"key":"developer","tool_policy":` +
		toolPolicy + `},"execution":` + execution + `}`
}

func (s *execServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	if got := r.Header.Get("Authorization"); got != "Bearer "+execToken {
		s.t.Errorf("%s %s: заголовок %q — ждали токен задачи", r.Method, r.URL.Path, got)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/PROJ-7/state":
		_, _ = w.Write([]byte(s.state))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/PROJ-7/prompts/work" && s.prompt != "":
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "work_on_task", "text": s.prompt})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/agent-kit" && s.kit != "":
		_, _ = w.Write([]byte(s.kit))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/instance":
		_, _ = w.Write([]byte(`{"version":"v0.27.0"}`))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/PROJ-7/heartbeat":
		s.mu.Lock()
		n := s.hbs
		s.hbs++
		s.mu.Unlock()
		code, body := s.heartbeat(n)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	default:
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}
}

func (s *execServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *execServer) heartbeats() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hbs
}

// stubAgent — заглушка-агент: claude, codex и agent в каталоге PATH.
const stubAgent = `#!/bin/sh
out="$STUB_OUT"
: > "$out/argv"
for a; do printf '%s\0' "$a" >> "$out/argv"; done
env > "$out/env"
cat > "$out/stdin"
if command -v ps >/dev/null 2>&1; then ps -o args= -p $$ > "$out/ps" 2>/dev/null; fi
touch "$out/ran"
if [ -n "$STUB_SLEEP" ]; then sleep "$STUB_SLEEP"; fi
if [ -n "$STUB_KILL" ]; then kill -TERM $$; fi
exit "${STUB_CODE:-0}"
`

type execFixture struct {
	srv     *execServer
	runDir  string
	out     string
	bin     string
	home    string
	environ []string
}

func newExecFixture(t *testing.T) *execFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("заглушка агента — sh")
	}
	f := &execFixture{srv: newExecServer(t)}
	root := t.TempDir()
	f.runDir = filepath.Join(root, "PROJ-7-developer-1")
	f.out = filepath.Join(root, "out")
	f.bin = filepath.Join(root, "bin")
	f.home = filepath.Join(root, "home")
	for _, d := range []string{f.out, f.bin, filepath.Join(f.home, ".config", "clab")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"claude", "codex", "agent"} {
		if err := os.WriteFile(filepath.Join(f.bin, name), []byte(stubAgent), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Личный конфиг человека — с другим токеном: исполнитель его не видит.
	human := Config{DefaultHost: "h", Hosts: map[string]HostConfig{"h": {URL: f.srv.srv.URL, Token: "clab_human_personal", Handle: "user:alice"}}}
	if err := saveConfigAt(filepath.Join(f.home, ".config", "clab"), human); err != nil {
		t.Fatal(err)
	}
	f.writeTaskConfig(t, "task:"+execTask, execToken)
	f.environ = []string{
		"PATH=" + f.bin + ":/usr/bin:/bin", "HOME=" + f.home, "OPENAI_API_KEY=provider-key",
		"STUB_OUT=" + f.out,
		"SDLC_API_URL=https://prod.example/v1", "SDLC_TOKEN=clab_orchestrator_token", "SDLC_ACTOR=user:alice",
		"SDLC_PROJECT=OTHER", "SDLC_FOO=1", "CLAB_CONFIG_DIR_X=1",
		"AI_AGENT=claude-code_2", "CLAUDECODE=1", "CODEX_THREAD_ID=th-1",
	}
	old := agentEnv
	agentEnv = func(string) string { return "" }
	t.Cleanup(func() { agentEnv = old })
	return f
}

func (f *execFixture) writeTaskConfig(t *testing.T, handle, token string) {
	t.Helper()
	cfg := Config{DefaultHost: "127.0.0.1", Hosts: map[string]HostConfig{"127.0.0.1": {URL: f.srv.srv.URL, Token: token, TokenID: "tok-1", Handle: handle}}}
	if err := saveConfigAt(filepath.Join(f.runDir, "clab"), cfg); err != nil {
		t.Fatal(err)
	}
}

func (f *execFixture) env(extra ...string) []string {
	return append(append([]string{}, f.environ...), extra...)
}

type execRun struct {
	code           int
	stdout, stderr string
}

func (f *execFixture) run(t *testing.T, environ []string, args ...string) execRun {
	t.Helper()
	// Буфер под замком и без ReadFrom: копия потока агента в bytes.Buffer
	// держит его растянутым, пока ждёт чтения, и строка clab exec из другой
	// горутины пропала бы.
	var stdout, stderr lockedBuffer
	code := runExec(context.Background(), args, execIO{stdin: strings.NewReader("orchestrator stdin"), stdout: &stdout, stderr: &stderr,
		environ: environ, workdir: f.out, sigs: make(chan os.Signal)})
	r := execRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
	for _, s := range []string{r.stdout, r.stderr} {
		if strings.Contains(s, execToken) {
			t.Errorf("вывод clab exec содержит токен задачи:\n%s", s)
		}
	}
	return r
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (f *execFixture) ran() bool {
	_, err := os.Stat(filepath.Join(f.out, "ran"))
	return err == nil
}

func (f *execFixture) argv(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.out, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

func (f *execFixture) childEnv(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.out, "env"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[k] = v
		}
	}
	return m
}

func (f *execFixture) file(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.out, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func hasSeq(argv []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

// AC-1: каталог без токена задачи — отказ до запуска и без запросов.
func TestExecRefusesWithoutTaskToken(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *execFixture) []string
	}{
		{"каталога нет", func(f *execFixture) []string {
			return []string{execTask, "--dir", filepath.Join(f.out, "missing"), "--", "agent"}
		}},
		{"нет конфига", func(f *execFixture) []string {
			_ = os.Remove(filepath.Join(f.runDir, "clab", "config.json"))
			return []string{execTask, "--dir", f.runDir, "--", "agent"}
		}},
		{"нет токена", func(f *execFixture) []string {
			f.writeTaskConfig(t, "task:"+execTask, "")
			return []string{execTask, "--dir", f.runDir, "--", "agent"}
		}},
		{"токен другой задачи", func(f *execFixture) []string {
			f.writeTaskConfig(t, "task:PROJ-8", execToken)
			return []string{execTask, "--dir", f.runDir, "--", "agent"}
		}},
		{"токен человека", func(f *execFixture) []string {
			f.writeTaskConfig(t, "user:alice", execToken)
			return []string{execTask, "--dir", f.runDir, "--", "agent"}
		}},
		{"каталог конфига человека", func(f *execFixture) []string {
			return []string{execTask, "--dir", filepath.Join(f.home, ".config"), "--", "agent"}
		}},
		{"сам каталог конфига человека", func(f *execFixture) []string {
			return []string{execTask, "--dir", filepath.Join(f.home, ".config", "clab"), "--", "agent"}
		}},
		{"без --dir", func(*execFixture) []string { return []string{execTask, "--", "agent"} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newExecFixture(t)
			r := f.run(t, f.env(), c.setup(f)...)
			if r.code != execRefused || f.ran() || len(f.srv.requests()) != 0 {
				t.Fatalf("код %d, агент запущен %v, запросы %v\n%s", r.code, f.ran(), f.srv.requests(), r.stderr)
			}
			if !strings.HasPrefix(r.stderr, "clab exec: ") || !strings.Contains(r.stderr, "clab token task "+execTask+" --config-dir ") {
				t.Errorf("отказ без пути или совета:\n%s", r.stderr)
			}
			if strings.Contains(r.stderr, "clab_human_personal") {
				t.Errorf("отказ показал токен человека:\n%s", r.stderr)
			}
		})
	}
}

// AC-2, AC-3, AC-6: окружение generic без MCP, инструкция на stdin с
// заметкой, токена нет ни в argv, ни в выводе; запроса захвата нет.
func TestExecGenericEnvironmentAndStdin(t *testing.T) {
	f := newExecFixture(t)
	note := filepath.Join(f.out, "note.md")
	if err := os.WriteFile(note, []byte("Рабочая копия: /w\nОтчёт: report.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := f.run(t, f.env("STUB_SLEEP=0.3"), execTask, "--dir", f.runDir, "--note-file", note, "--agent", "generic", "--",
		"claude", "-p", "--strict-mcp-config", "--mcp-config", filepath.Join(f.out, "empty.json"))
	if r.code != 0 || !f.ran() {
		t.Fatalf("код %d, запущен %v\n%s", r.code, f.ran(), r.stderr)
	}
	env := f.childEnv(t)
	if env["CLAB_CONFIG_DIR"] != filepath.Join(f.runDir, "clab") || env["SDLC_PROJECT"] != "PROJ" {
		t.Errorf("CLAB_CONFIG_DIR %q, SDLC_PROJECT %q", env["CLAB_CONFIG_DIR"], env["SDLC_PROJECT"])
	}
	for _, gone := range []string{"SDLC_API_URL", "SDLC_TOKEN", "SDLC_ACTOR", "SDLC_FOO", "CLAB_CONFIG_DIR_X", "AI_AGENT", "CLAUDECODE", "CODEX_THREAD_ID"} {
		if _, ok := env[gone]; ok {
			t.Errorf("в окружении агента осталось %s", gone)
		}
	}
	for k, v := range map[string]string{"HOME": f.home, "OPENAI_API_KEY": "provider-key"} {
		if env[k] != v {
			t.Errorf("%s = %q, ждали %q", k, env[k], v)
		}
	}
	if got, want := f.file(t, "stdin"), strings.TrimRight(execText, "\n")+"\n\nРабочая копия: /w\nОтчёт: report.md\n"; got != want {
		t.Errorf("stdin агента:\n%q\nждали:\n%q", got, want)
	}
	argv := f.argv(t)
	if !slices.Equal(argv, []string{"-p", "--strict-mcp-config", "--mcp-config", filepath.Join(f.out, "empty.json")}) {
		t.Errorf("argv агента %q", argv)
	}
	for _, name := range []string{"argv", "env", "stdin"} {
		if strings.Contains(f.file(t, name), execToken) {
			t.Errorf("%s агента содержит токен задачи", name)
		}
	}
	if ps, err := os.ReadFile(filepath.Join(f.out, "ps")); err == nil && (len(ps) == 0 || strings.Contains(string(ps), execToken)) {
		t.Errorf("ps -o args: %q", ps)
	}
	if _, err := os.Stat(filepath.Join(f.runDir, "mcp.json")); err == nil {
		t.Error("без MCP в команде записан mcp.json")
	}
	for _, c := range f.srv.requests() {
		if strings.HasSuffix(c, "/claim") {
			t.Errorf("запрос захвата: %s", c)
		}
	}
	for _, want := range []string{"агент generic (флаг --agent)", "MCP: нет", "хук-страховка не крепится: изоляция окружением", "на stdin"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("в выводе нет %q:\n%s", want, r.stderr)
		}
	}
}

// AC-8: привязка claude без команды — шаблон run, один сервер sdlc в
// mcp.json со ссылкой на переменную, SDLC_TOKEN — только агенту.
func TestExecClaudeTemplateMCP(t *testing.T) {
	f := newExecFixture(t)
	f.srv.state = stateJSON(`{"model_level":"high","model_level_source":"stage","agent_settings_version":null,"models":{}}`, `{"allow":["Bash","Read"],"deny":[]}`)
	r := f.run(t, f.env(), execTask, "--dir", f.runDir, "--agent", "claude")
	if r.code != 0 {
		t.Fatalf("код %d\n%s", r.code, r.stderr)
	}
	argv := f.argv(t)
	mcp := filepath.Join(f.runDir, "mcp.json")
	for _, seq := range [][]string{{"--strict-mcp-config", "--mcp-config", mcp}, {"--model", "opus"}, {"--allowedTools", "Bash,Read"}} {
		if !hasSeq(argv, seq...) {
			t.Errorf("нет %q в %q", seq, argv)
		}
	}
	if slices.Contains(argv, "--effort") {
		t.Errorf("усилия нет, а флаг остался: %q", argv)
	}
	if argv[len(argv)-1] != execText {
		t.Errorf("инструкция не последним аргументом: %q", argv[len(argv)-1])
	}
	info, err := os.Stat(mcp)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mcp.json: %v %v", info, err)
	}
	raw, _ := os.ReadFile(mcp)
	var cfg struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || len(cfg.MCPServers) != 1 {
		t.Fatalf("mcp.json %s: %v", raw, err)
	}
	sdlc := cfg.MCPServers["sdlc"]
	if sdlc.URL != f.srv.srv.URL+"/mcp/projects/PROJ" || sdlc.Headers["Authorization"] != "Bearer ${SDLC_TOKEN}" || strings.Contains(string(raw), execToken) {
		t.Errorf("mcp.json: %s", raw)
	}
	if env := f.childEnv(t); env["SDLC_TOKEN"] != execToken {
		t.Errorf("SDLC_TOKEN агента не токен задачи")
	}
	for _, want := range []string{"модель opus (binding", "SDLC_TOKEN=<токен задачи>", "<инструкция: "} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("в выводе нет %q:\n%s", want, r.stderr)
		}
	}
	if strings.Contains(r.stderr, "tool_policy") {
		t.Errorf("claude переводит tool_policy, а вывод предупреждает:\n%s", r.stderr)
	}
}

// AC-9: привязка codex без команды — argv по codex.json, хук при запуске,
// каталог захода в --add-dir, модель и усилие из настройки агентов.
func TestExecCodexTemplate(t *testing.T) {
	f := newExecFixture(t)
	f.srv.state = stateJSON(`{"model_level":"high","model_level_source":"stage","agent_settings_version":3,"models":{"codex":{"model":"gpt-x","effort":"high"}}}`, `{"allow":["Bash"],"deny":["Write"]}`)
	r := f.run(t, f.env(), execTask, "--dir", f.runDir, "--agent", "codex")
	if r.code != 0 {
		t.Fatalf("код %d\n%s", r.code, r.stderr)
	}
	argv := f.argv(t)
	k, _ := agentkit.Load("dev")
	var hook struct {
		Command string `json:"command"`
	}
	for _, b := range k.Bindings {
		if b.Name == "codex" {
			_ = json.Unmarshal(b.Hook, &hook)
		}
	}
	quoted := launch.Quoted(hook.Command)
	for _, seq := range [][]string{
		{"exec", "--json"}, {"--sandbox", "workspace-write"}, {"--add-dir", f.runDir}, {"--ignore-user-config"},
		{"-m", "gpt-x"}, {"-c", `model_reasoning_effort="high"`},
		{"-c", `mcp_servers.sdlc.url="` + f.srv.srv.URL + `/mcp/projects/PROJ"`},
		{"-c", `mcp_servers.sdlc.bearer_token_env_var="SDLC_TOKEN"`},
		{"-c", `shell_environment_policy.exclude=["SDLC_TOKEN"]`},
		{"--dangerously-bypass-hook-trust"},
	} {
		if !hasSeq(argv, seq...) {
			t.Errorf("нет %q в %q", seq, argv)
		}
	}
	found := false
	for _, a := range argv {
		if strings.HasPrefix(a, "hooks.PreToolUse=") && strings.Contains(a, "command="+quoted) && strings.Contains(a, `matcher="Bash"`) {
			found = true
		}
	}
	if !found || hook.Command == "" {
		t.Errorf("хук не прикреплён к запуску: %q", argv)
	}
	if env := f.childEnv(t); env["SDLC_TOKEN"] != execToken || env["CODEX_THREAD_ID"] != "" {
		t.Errorf("окружение codex: SDLC_TOKEN задан %v, CODEX_THREAD_ID %q", env["SDLC_TOKEN"] != "", env["CODEX_THREAD_ID"])
	}
	for _, want := range []string{
		"модель gpt-x, усилие high (agent_settings: агент codex, настройка агентов v3, уровень high)",
		"хук-страховка крепится к запуску: PreToolUse на Bash",
		"tool_policy роли не переведён для привязки codex",
		"<сценарий хука: ",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("в выводе нет %q:\n%s", want, r.stderr)
		}
	}
}

// AC-7: модель — explicit, agent_settings, binding, agent_default.
func TestExecModelSources(t *testing.T) {
	cases := []struct {
		name      string
		execution string
		args      []string
		wantLine  string
		wantArgv  []string
		noArgv    string
	}{
		{"explicit", `{"model_level":"high","model_level_source":"stage","agent_settings_version":3,"models":{"codex":{"model":"gpt-x","effort":"high"}}}`,
			[]string{"--agent", "codex", "--model", "gpt-y", "--effort", "low"}, "модель gpt-y, усилие low (explicit: флаг --model; уровень high)", []string{"-m", "gpt-y", "-c", `model_reasoning_effort="low"`}, ""},
		{"agent_settings", `{"model_level":"medium","model_level_source":"task","agent_settings_version":4,"models":{"claude":{"model":"sonnet-x","effort":"max"}}}`,
			[]string{"--agent", "claude"}, "модель sonnet-x, усилие max (agent_settings: агент claude, настройка агентов v4, уровень medium)", []string{"--model", "sonnet-x", "--effort", "max"}, ""},
		{"binding", `{"model_level":"low","model_level_source":"role","agent_settings_version":4,"models":{}}`,
			[]string{"--agent", "claude"}, "модель haiku (binding: таблица уровней привязки claude, уровень low; настройка агентов v4)", []string{"--model", "haiku"}, "--effort"},
		{"agent_default", `{"model_level":"high","model_level_source":"stage","agent_settings_version":null,"models":{}}`,
			[]string{"--agent", "codex"}, "модель агента по умолчанию (agent_default: флаг модели не передаётся; уровень high, настройки агентов нет)", nil, "-m"},
		{"без execution", `null`, []string{"--agent", "codex"}, "модель агента по умолчанию (agent_default", nil, "-m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newExecFixture(t)
			f.srv.state = stateJSON(c.execution, "null")
			r := f.run(t, f.env(), append([]string{execTask, "--dir", f.runDir}, c.args...)...)
			if r.code != 0 {
				t.Fatalf("код %d\n%s", r.code, r.stderr)
			}
			if !strings.Contains(r.stderr, c.wantLine) {
				t.Errorf("нет строки %q:\n%s", c.wantLine, r.stderr)
			}
			argv := f.argv(t)
			if c.wantArgv != nil && !hasSeq(argv, c.wantArgv...) {
				t.Errorf("нет %q в %q", c.wantArgv, argv)
			}
			if c.noArgv != "" && slices.Contains(argv, c.noArgv) {
				t.Errorf("флаг %s передан: %q", c.noArgv, argv)
			}
		})
	}
	// Модель есть, а в своей команде {model} нет — предупреждение.
	f := newExecFixture(t)
	r := f.run(t, f.env(), execTask, "--dir", f.runDir, "--model", "m1", "--", "agent", "go")
	if r.code != 0 || !strings.Contains(r.stderr, "модель не передана") {
		t.Errorf("код %d, нет предупреждения:\n%s", r.code, r.stderr)
	}
}

// AC-5: выбор агента.
func TestExecChooseAgent(t *testing.T) {
	k, _ := agentkit.Load("dev")
	raw, _ := json.Marshal(k.Bindings)
	bindings := parseBindings(raw)
	for _, c := range []struct {
		flag    string
		command []string
		want    string
	}{
		{"codex", []string{"claude"}, "codex"},
		{"", []string{"/usr/local/bin/claude", "-p"}, "claude"},
		{"", []string{"codex", "exec"}, "codex"},
		{"", []string{"aider"}, "generic"},
		{"generic", []string{"claude", "-p"}, "generic"},
	} {
		if got, _, err := chooseAgent(c.flag, c.command, bindings); err != nil || got != c.want {
			t.Errorf("--agent %q %q: %q %v, ждали %q", c.flag, c.command, got, err, c.want)
		}
	}
	old := agentEnv
	t.Cleanup(func() { agentEnv = old })
	env := map[string]string{"CODEX_THREAD_ID": "x"}
	agentEnv = func(k string) string { return env[k] }
	if got, from, err := chooseAgent("", nil, bindings); err != nil || got != "codex" || !strings.Contains(from, "окружения") {
		t.Errorf("по признаку: %q %q %v", got, from, err)
	}
	env["CLAUDECODE"] = "1"
	if _, _, err := chooseAgent("", nil, bindings); err == nil || !strings.Contains(err.Error(), "claude, codex") {
		t.Errorf("два признака: %v", err)
	}
	if _, _, err := chooseAgent("gemini", nil, bindings); err == nil || !strings.Contains(err.Error(), "claude, codex, generic") {
		t.Errorf("неизвестная привязка: %v", err)
	}

	f := newExecFixture(t)
	r := f.run(t, f.env(), execTask, "--dir", f.runDir, "--agent", "generic")
	if r.code != execRefused || !strings.Contains(r.stderr, "команда агента не задана") || f.ran() {
		t.Errorf("generic без команды: код %d\n%s", r.code, r.stderr)
	}
	r = f.run(t, f.env(), execTask, "--dir", f.runDir)
	if r.code != execRefused || !strings.Contains(r.stderr, "агент не узнан") {
		t.Errorf("без команды и признака: код %d\n%s", r.code, r.stderr)
	}
}

// AC-4: аренда и код выхода.
func TestExecLeaseAndExitCodes(t *testing.T) {
	t.Run("отказ первого продления", func(t *testing.T) {
		f := newExecFixture(t)
		f.srv.heartbeat = func(int) (int, string) {
			return apiFail(http.StatusConflict, "lease_held_by_other", "аренда у другого")
		}
		r := f.run(t, f.env(), execTask, "--dir", f.runDir, "--", "agent")
		if r.code != execRefused || f.ran() || !strings.Contains(r.stderr, "lease_held_by_other") {
			t.Fatalf("код %d, запущен %v\n%s", r.code, f.ran(), r.stderr)
		}
	})
	t.Run("код агента", func(t *testing.T) {
		f := newExecFixture(t)
		r := f.run(t, f.env("STUB_CODE=3"), execTask, "--dir", f.runDir, "--", "agent")
		if r.code != 3 || f.srv.heartbeats() != 1 {
			t.Fatalf("код %d, продлений %d", r.code, f.srv.heartbeats())
		}
	})
	t.Run("убит сигналом", func(t *testing.T) {
		f := newExecFixture(t)
		r := f.run(t, f.env("STUB_KILL=1"), execTask, "--dir", f.runDir, "--", "agent")
		if r.code != 128+15 {
			t.Fatalf("код %d, ждали 143\n%s", r.code, r.stderr)
		}
	})
	t.Run("продления и потеря аренды", func(t *testing.T) {
		f := newExecFixture(t)
		f.srv.heartbeat = func(n int) (int, string) {
			if n < 2 {
				return leaseOK(time.Hour, 1)
			}
			return apiFail(http.StatusConflict, "lease_expired", "аренда истекла")
		}
		r := f.run(t, f.env("STUB_SLEEP=3.5", "STUB_CODE=4"), execTask, "--dir", f.runDir, "--", "agent")
		if r.code != 4 || f.srv.heartbeats() != 3 || !strings.Contains(r.stderr, "аренда прогона") || !strings.Contains(r.stderr, "потеряна, команда продолжает работу") {
			t.Fatalf("код %d, продлений %d\n%s", r.code, f.srv.heartbeats(), r.stderr)
		}
	})
}

// AC-10: сухой прогон ничего не запускает, не продлевает и не пишет.
func TestExecDryRun(t *testing.T) {
	f := newExecFixture(t)
	r := f.run(t, f.env(), execTask, "--dir", f.runDir, "--agent", "claude", "--dry-run")
	if r.code != 0 || f.ran() || f.srv.heartbeats() != 0 {
		t.Fatalf("код %d, запущен %v, продлений %d", r.code, f.ran(), f.srv.heartbeats())
	}
	if _, err := os.Stat(filepath.Join(f.runDir, "mcp.json")); err == nil {
		t.Error("сухой прогон записал mcp.json")
	}
	for _, want := range []string{
		"агент claude (флаг --agent)", "модель opus (binding", "SDLC_TOKEN=<токен задачи>",
		"убраны: AI_AGENT, CLAB_CONFIG_DIR_X, CLAUDECODE, CODEX_THREAD_ID, SDLC_ACTOR, SDLC_API_URL, SDLC_FOO, SDLC_PROJECT, SDLC_TOKEN",
		fmt.Sprintf("<инструкция: %d байт>", len(execText)), "хук-страховка не крепится", "сухой прогон",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("в выводе нет %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "clab_orchestrator_token") || strings.Contains(r.stdout, execText) {
		t.Errorf("сухой прогон показал значение:\n%s", r.stdout)
	}
	if execRefused == 0 || execRefused == 1 || execRefused == 2 || execRefused == 126 || execRefused == 127 {
		t.Errorf("код отказа %d совпадает с кодом агента или оболочки", execRefused)
	}
}

// AC-12: набор без привязок и сервер без инструкции.
func TestExecOldServer(t *testing.T) {
	f := newExecFixture(t)
	f.srv.kit = `{"version":"v0.27.0","files":[]}`
	r := f.run(t, f.env(), execTask, "--dir", f.runDir, "--", "claude", "-p")
	if r.code != 0 || !strings.Contains(r.stderr, "привязок нет") || !strings.Contains(r.stderr, "агент claude (имя программы claude)") ||
		!strings.Contains(r.stderr, "agent_default") {
		t.Fatalf("код %d\n%s", r.code, r.stderr)
	}
	f = newExecFixture(t)
	f.srv.prompt = ""
	r = f.run(t, f.env(), execTask, "--dir", f.runDir, "--", "agent")
	if r.code != execRefused || f.ran() || !strings.Contains(r.stderr, agentTextMinServer) || strings.Contains(r.stderr, "404 page") {
		t.Fatalf("код %d\n%s", r.code, r.stderr)
	}
}

// AC-15: вложенный clab агента берёт токен задачи из CLAB_CONFIG_DIR, а не
// личный конфиг человека из HOME.
func TestExecNestedClabSeesTaskToken(t *testing.T) {
	f := newExecFixture(t)
	r := f.run(t, f.env(), execTask, "--dir", f.runDir, "--", "agent")
	if r.code != 0 {
		t.Fatalf("код %d\n%s", r.code, r.stderr)
	}
	env := f.childEnv(t)
	t.Setenv("CLAB_CONFIG_DIR", env["CLAB_CONFIG_DIR"])
	t.Setenv("HOME", env["HOME"])
	t.Setenv("SDLC_TOKEN", "")
	t.Setenv("SDLC_API_URL", "")
	c, err := NewClient(globals{})
	if err != nil {
		t.Fatal(err)
	}
	if c.token != execToken {
		t.Fatalf("вложенный clab взял не токен задачи")
	}
}
