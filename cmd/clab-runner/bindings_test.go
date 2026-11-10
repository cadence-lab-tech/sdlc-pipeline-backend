package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit/launch"
)

// Раннер на привязках агентов (CLAB-420). Сервер — fakeAPI, агенты —
// скрипты-заглушки claude и codex в PATH временного каталога.

func builtin(t *testing.T) []agentkit.Binding {
	t.Helper()
	b, err := builtinBindings()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bindingNamed(t *testing.T, name string) agentkit.Binding {
	t.Helper()
	b := findBinding(builtin(t), name)
	if b == nil {
		t.Fatalf("нет привязки %s", name)
	}
	return *b
}

// binDir — каталог PATH с исполняемыми заглушками; lookIn ищет только в нём.
func binDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func lookIn(dir string) func(string) (string, error) {
	return func(name string) (string, error) {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode()&0o111 != 0 {
			return p, nil
		}
		return "", errors.New("не найден")
	}
}

// kitHandler — GET /v1/agent-kit: набор как его отдаёт сервер, с правкой
// привязок (сервер прежней версии, лишняя привязка).
func kitHandler(t *testing.T, edit func([]map[string]any) []map[string]any) http.HandlerFunc {
	t.Helper()
	k, err := agentkit.Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(k.Bindings)
	var bindings []map[string]any
	_ = json.Unmarshal(raw, &bindings)
	if edit != nil {
		bindings = edit(bindings)
	}
	body, _ := json.Marshal(map[string]any{"version": "dev", "bindings": bindings})
	return func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }
}

// AC-2: привязка по имени программы SDLC_RUNNER_COMMAND.
func TestSelectAgentByCommand(t *testing.T) {
	bindings := builtin(t)
	never := func(string) (string, error) {
		t.Fatal("своя команда не смотрит PATH")
		return "", nil
	}
	for command, want := range map[string]string{
		"claude -p --output-format json {prompt}": "claude",
		"/opt/bin/codex exec --json {prompt}":     "codex",
		"codex.exe exec {prompt}":                 "codex",
		"aider --message {prompt}":                "generic",
		"env X=1 claude -p {prompt}":              "generic",
		"node agent.js {prompt}":                  "generic",
		"generic {prompt}":                        "generic",
	} {
		b, from, err := selectAgent(command, bindings, never)
		if err != nil || b.Name != want || from != agentFromCommand {
			t.Errorf("%q → %s (%s, %v), ждали %s", command, b.Name, from, err, want)
		}
	}
	// Набора нет вовсе — generic без привязки, инструкция текстом.
	if b, _, _ := selectAgent("aider", nil, never); b.Name != "generic" || b.Prompt != promptText || b.Output != nil {
		t.Errorf("generic без набора: %+v", b)
	}
}

// AC-3: привязка по PATH.
func TestSelectAgentByPath(t *testing.T) {
	bindings := append(builtin(t), agentkit.Binding{Name: "gemini", Run: []string{"gemini", "{prompt}"}, Prompt: promptText})
	cases := []struct {
		name    string
		path    []string
		want    string
		wantErr []string
	}{
		{"claude и codex", []string{"claude", "codex"}, "claude", nil},
		{"только claude", []string{"claude"}, "claude", nil},
		{"только codex", []string{"codex"}, "codex", nil},
		{"два без claude", []string{"codex", "gemini"}, "", []string{"codex, gemini", "SDLC_RUNNER_COMMAND"}},
		{"ни одного", nil, "", []string{"нет ни одного агента", "claude, codex, gemini", "SDLC_RUNNER_COMMAND"}},
	}
	for _, c := range cases {
		b, from, err := selectAgent("", bindings, lookIn(binDir(t, c.path...)))
		if c.wantErr != nil {
			if err == nil {
				t.Errorf("%s: выбран %s, ждали отказ", c.name, b.Name)
				continue
			}
			for _, s := range c.wantErr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("%s: в отказе %q нет %q", c.name, err, s)
				}
			}
			continue
		}
		if err != nil || b.Name != c.want || from != agentFromPath {
			t.Errorf("%s: %s (%s, %v), ждали %s", c.name, b.Name, from, err, c.want)
		}
	}
}

// AC-2, AC-3, AC-11: журнал старта называет агента, источник выбора и
// источник набора; сервер без набора — встроенный, с предупреждением.
func TestPrepareAgentLogs(t *testing.T) {
	cases := []struct {
		name    string
		kit     http.HandlerFunc
		command string
		want    []string
	}{
		{"инстанс, PATH", kitHandler(t, nil), "", []string{"agent=claude", "agent_from=PATH", "bindings=инстанс", "output=claude-json", "prompt=mcp_slash"}},
		{"нет набора", nil, "", []string{"набор агента инстанса недоступен", "agent=claude", "bindings=встроенные"}},
		{"своя команда", kitHandler(t, nil), "codex exec {prompt}", []string{"agent=codex", "agent_from=SDLC_RUNNER_COMMAND", "output=codex-jsonl"}},
	}
	for _, c := range cases {
		api := newFakeAPI(t)
		if c.kit != nil {
			api.routes = map[string]http.HandlerFunc{"/v1/agent-kit": c.kit}
		}
		var logs bytes.Buffer
		cfg := Config{APIURL: api.srv.URL + "/v1", Project: "CLAB", Command: c.command}
		log := slog.New(slog.NewTextHandler(&logs, nil))
		if err := prepareAgent(context.Background(), &cfg, log, lookIn(binDir(t, "claude", "codex"))); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, s := range c.want {
			if !strings.Contains(logs.String(), s) {
				t.Errorf("%s: в журнале нет %q:\n%s", c.name, s, logs.String())
			}
		}
		for _, call := range api.seen() {
			if call.Path != "/v1/agent-kit" || call.Auth != "" {
				t.Errorf("%s: на старте лишний запрос или токен: %+v", c.name, call)
			}
		}
	}
}

// AC-11: сервер прежней версии отдаёт привязки без prompt и env — их
// дополняет встроенная привязка того же имени.
func TestInstanceBindingsFillOldFields(t *testing.T) {
	api := newFakeAPI(t)
	api.routes = map[string]http.HandlerFunc{"/v1/agent-kit": kitHandler(t, func(bs []map[string]any) []map[string]any {
		for _, b := range bs {
			delete(b, "prompt")
			delete(b, "env")
		}
		return append(bs, map[string]any{"name": "fresh", "run": []string{"fresh", "{prompt}"}})
	})}
	got, err := instanceBindings(context.Background(), api.srv.URL+"/v1", builtin(t))
	if err != nil {
		t.Fatal(err)
	}
	claude, fresh := findBinding(got, "claude"), findBinding(got, "fresh")
	if claude == nil || claude.Prompt != promptSlash || !slices.Equal(claude.Env, []string{"ANTHROPIC_*", "CLAUDE_*"}) || len(claude.Files) != 0 {
		t.Errorf("claude сервера прежней версии: %+v", claude)
	}
	if fresh == nil || fresh.Prompt != promptText || fresh.Env != nil {
		t.Errorf("незнакомая привязка без prompt: %+v", fresh)
	}
}

// AC-3: отказ выбора — код выхода 2 до очереди. main в подпроцессе.
func TestRunnerExitsTwoWithoutAgent(t *testing.T) {
	if os.Getenv("CLAB_RUNNER_TEST_MAIN") == "1" {
		main()
		return
	}
	for name, tc := range map[string]struct {
		path []string
		kit  http.HandlerFunc
		want string
	}{
		"ни одного": {nil, nil, "нет ни одного агента"},
		"два без claude": {[]string{"codex", "gemini"}, kitHandler(t, func(bs []map[string]any) []map[string]any {
			return append(bs, map[string]any{"name": "gemini", "run": []string{"gemini", "{prompt}"}, "prompt": "text"})
		}), "codex, gemini"},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			if tc.kit != nil {
				api.routes = map[string]http.HandlerFunc{"/v1/agent-kit": tc.kit}
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunnerExitsTwoWithoutAgent$")
			cmd.Env = []string{"CLAB_RUNNER_TEST_MAIN=1", "PATH=" + binDir(t, tc.path...), "HOME=" + t.TempDir(),
				"SDLC_API_URL=" + api.srv.URL + "/v1", "SDLC_PROJECT=CLAB", "SDLC_TOKEN=" + runnerToken}
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("код выхода: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.want) || !strings.Contains(string(out), "SDLC_RUNNER_COMMAND") {
				t.Errorf("сообщение отказа:\n%s", out)
			}
			for _, c := range api.seen() {
				if c.Path != "/v1/agent-kit" {
					t.Errorf("при отказе раннер ходил в %s", c.Path)
				}
			}
		})
	}
}

// AC-1: один claude в PATH, без SDLC_RUNNER_COMMAND, настройки агентов и
// уровня — argv сухого прогона посимвольно тот же, что у main 937bbca:
// команда defaultCommand раннера до CLAB-420 с теми же подстановками.
func TestDryRunMatchesMain(t *testing.T) {
	const ref = "claude -p --output-format json --mcp-config <mcp.json> --strict-mcp-config"
	noLevel := map[string]any{"model_level": nil, "model_level_source": "none"}
	cases := []struct {
		name  string
		claim map[string]any
		want  string
	}{
		{"роль без модели и тулов", map[string]any{"stage_run_id": "run-1", "role": map[string]any{"key": "developer"}, "execution": noLevel},
			ref + " /mcp__sdlc__work_on_task CLAB-1 developer runner"},
		{"model_hint и allow", map[string]any{"stage_run_id": "run-1", "execution": noLevel, "role": map[string]any{"key": "developer",
			"model_hint": "sonnet", "tool_policy": map[string]any{"allow": []string{"Read", "Grep"}}}},
			ref + " --model sonnet --allowedTools Read,Grep /mcp__sdlc__work_on_task CLAB-1 developer runner"},
		{"сервер без execution", map[string]any{"stage_run_id": "run-1", "role": map[string]any{"key": "developer", "model_hint": "opus"}},
			ref + " --model opus /mcp__sdlc__work_on_task CLAB-1 developer runner"},
	}
	for _, kit := range []string{"инстанс", "встроенные"} {
		for _, c := range cases {
			api := newFakeAPI(t)
			api.claim = c.claim
			if kit == "инстанс" {
				api.routes = map[string]http.HandlerFunc{"/v1/agent-kit": kitHandler(t, nil)}
			}
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			cfg := Config{APIURL: api.srv.URL + "/v1", Token: runnerToken, Project: "CLAB", Slots: 1,
				Interval: 1 << 40, Timeout: 1 << 40, Once: true, DryRun: true}
			if err := prepareAgent(context.Background(), &cfg, log, lookIn(binDir(t, "claude"))); err != nil {
				t.Fatal(err)
			}
			r := newRunner(cfg, log)
			r.environ = func() []string { return parentEnv }
			if err := r.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), "argv="+strconv.Quote(c.want)+" ") {
				t.Errorf("%s, %s: argv сухого прогона не как у main:\n%s\nждали %s", kit, c.name, lineWith(logs.String(), "сухой прогон"), c.want)
			}
			if len(api.find(http.MethodGet, "/v1/tasks/CLAB-1/prompts/work")) != 0 {
				t.Errorf("%s: claude стартует слэш-промптом, текст инструкции не нужен", c.name)
			}
		}
	}
}

// codexStub — скрипт codex в PATH раннера: сохраняет argv, окружение,
// stdin и ps, печатает JSONL и выходит с кодом code.
func codexStub(t *testing.T, jsonl string, code int) (out string) {
	t.Helper()
	dir := t.TempDir()
	out = filepath.Join(dir, "out")
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{out, bin} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	body := "#!/bin/sh\nOUT=" + out + "\n" +
		": > \"$OUT/argv\"\nfor a; do printf '%s\\0' \"$a\" >> \"$OUT/argv\"; done\n" +
		"env > \"$OUT/env\"\ncat > \"$OUT/stdin\"\n" +
		"ps -o args= -p $$ > \"$OUT/ps\" 2>/dev/null\n" +
		"cat <<'JSONL'\n" + jsonl + "\nJSONL\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return out
}

const codexTurns = `{"type":"thread.started","thread_id":"th-1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"ok"}}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":50}}
{"type":"turn.completed","usage":{"input_tokens":2000,"cached_input_tokens":1500,"output_tokens":70}}
не JSON — строка лога
{"type":"turn.completed","usage":{"input_tokens":300,"cached_input_tokens":0,"output_tokens":5}}`

const workText = "Возьми задачу CLAB-1 ролью developer. Слоты {model} в тексте — не подстановки."

// promptsRoute — ручка инструкции: запоминает запросы.
func promptsRoute(seen *[]url.Values) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		*seen = append(*seen, req.URL.Query())
		json.NewEncoder(w).Encode(map[string]any{"name": "work_on_task", "text": workText})
	}
}

func codexRunner(t *testing.T, api *fakeAPI, logs *bytes.Buffer) *runner {
	t.Helper()
	r := testRunner(t, api, "", logs)
	r.agent = bindingNamed(t, "codex")
	r.environ = func() []string {
		return append(append([]string{}, parentEnv...), "OPENAI_API_KEY=sk-openai", "CODEX_HOME=/home/dev/.codex")
	}
	return r
}

func argvOf(t *testing.T, out string) []string {
	t.Helper()
	return strings.Split(strings.TrimSuffix(readOut(t, out, "argv"), "\x00"), "\x00")
}

func hasSeq(argv []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

// AC-4, AC-7, AC-10: Codex без модели — argv по run привязки без пар
// модели и усилия, инструкция текстом без usage=runner, токена нет ни в
// argv, ни в ps; окружение — OPENAI_*, CODEX_*, без ANTHROPIC_*; раннер о
// стоимости не отчитывается.
func TestCodexArgvWithoutModel(t *testing.T) {
	api := newFakeAPI(t)
	var queries []url.Values
	api.routes = map[string]http.HandlerFunc{"/v1/tasks/CLAB-1/prompts/work": promptsRoute(&queries)}
	out := codexStub(t, codexTurns, 0)
	var logs bytes.Buffer
	r := codexRunner(t, api, &logs)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	argv := argvOf(t, out)
	for _, seq := range [][]string{
		{"exec", "--json", "--sandbox", "workspace-write"},
		{"-c", "sandbox_workspace_write.network_access=true"},
		{"-c", `mcp_servers.sdlc.url="` + api.srv.URL + `/mcp/projects/CLAB"`},
		{"-c", `mcp_servers.sdlc.bearer_token_env_var="SDLC_TOKEN"`},
	} {
		if !hasSeq(argv, seq...) {
			t.Errorf("нет %q в %q", seq, argv)
		}
	}
	if i := slices.Index(argv, "--add-dir"); i < 0 || !strings.Contains(argv[i+1], "clab-runner-") {
		t.Errorf("--add-dir не каталог агента: %q", argv)
	}
	if argv[len(argv)-1] != workText {
		t.Errorf("инструкция не последним словом: %q", argv[len(argv)-1])
	}
	for i, a := range argv {
		if a == "-m" || strings.Contains(a, "model_reasoning_effort") || strings.Contains(a, "hooks.PreToolUse") ||
			strings.Contains(a, "/mcp__") || strings.Contains(a, taskTokenV) {
			t.Errorf("лишнее слово %q в %q", a, argv)
		}
		if a == "-c" && (i+1 >= len(argv) || !strings.Contains(argv[i+1], "=")) {
			t.Errorf("висящий -c в %q", argv)
		}
	}
	if ps := readOut(t, out, "ps"); strings.Contains(ps, taskTokenV) {
		t.Errorf("токен задачи в ps: %s", ps)
	}
	env := envOf(t, out)
	if env["SDLC_TOKEN"] != taskTokenV || env["OPENAI_API_KEY"] != "sk-openai" || env["CODEX_HOME"] != "/home/dev/.codex" {
		t.Errorf("окружение codex: %v", env)
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Error("ключ Anthropic ушёл агенту codex")
	}
	if len(queries) != 1 || queries[0].Get("role") != "developer" || queries[0].Has("usage") {
		t.Errorf("запрос инструкции: %v", queries)
	}
	if n := len(api.find(http.MethodPost, "/v1/tasks/CLAB-1/runs/run-1/usage")); n != 0 {
		t.Errorf("модели нет — раннер не отчитывается, а отчётов %d", n)
	}
	if !strings.Contains(logs.String(), "о стоимости отчитывается агент") ||
		!strings.Contains(logs.String(), "хук-страховка не крепится: изоляция окружением") {
		t.Errorf("журнал:\n%s", logs.String())
	}
}

// AC-5, AC-7: модель и усилие из настройки агентов проекта — пары -m и
// -c в argv, usage=runner в инструкции, отчёт — сумма ходов, модель —
// переданная, без cost_usd, details.agent = codex; журнал запуска —
// источник и версия настройки.
func TestCodexReportsUsageFromOutput(t *testing.T) {
	api := newFakeAPI(t)
	var queries []url.Values
	api.routes = map[string]http.HandlerFunc{"/v1/tasks/CLAB-1/prompts/work": promptsRoute(&queries)}
	api.claim = map[string]any{"stage_run_id": "run-1", "role": map[string]any{"key": "developer"},
		"execution": map[string]any{"model_level": "high", "model_level_source": "stage", "agent_settings_version": 4,
			"models": map[string]any{"codex": map[string]any{"model": "gpt-x", "effort": "high"}, "claude": map[string]any{"model": "opus-x"}}}}
	out := codexStub(t, codexTurns, 0)
	var logs bytes.Buffer
	r := codexRunner(t, api, &logs)
	r.cfg.Billing = "subscription"
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	argv := argvOf(t, out)
	if !hasSeq(argv, "-m", "gpt-x") || !hasSeq(argv, "-c", `model_reasoning_effort="high"`) {
		t.Errorf("модель и усилие: %q", argv)
	}
	if len(queries) != 1 || queries[0].Get("usage") != "runner" {
		t.Errorf("инструкция без usage=runner: %v", queries)
	}
	usage := api.find(http.MethodPost, "/v1/tasks/CLAB-1/runs/run-1/usage")
	if len(usage) != 1 {
		t.Fatalf("отчётов о стоимости %d, ждали один", len(usage))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(usage[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"model": "gpt-x", "input_tokens": float64(400 + 500 + 300), "cache_read_tokens": float64(2100),
		"output_tokens": float64(125), "billing": "subscription",
		"details": map[string]any{"reported_by": "clab-runner", "agent": "codex"}}
	b1, _ := json.Marshal(body)
	b2, _ := json.Marshal(want)
	if string(b1) != string(b2) {
		t.Errorf("тело отчёта:\n%s\nждали\n%s", b1, b2)
	}
	if usage[0].Auth != "Bearer "+runnerToken {
		t.Error("отчёт не токеном раннера")
	}
	launchLine := lineWith(logs.String(), "запуск агента")
	for _, f := range []string{"model=gpt-x", "model_from=agent_settings", "model_source=agent_settings", "effort=high",
		"agent_settings_version=4", "agent=codex"} {
		if !strings.Contains(launchLine, f) {
			t.Errorf("в записи запуска нет %s: %s", f, launchLine)
		}
	}
}

// AC-11, AC-13: сервер без prompts/work — codex не стартует, release с
// причиной и сбоем; лимит в выводе codex паузы не ставит — это сбой.
func TestCodexOldServerAndNoLimit(t *testing.T) {
	api := newFakeAPI(t)
	codexStub(t, codexTurns, 0)
	var logs bytes.Buffer
	r := codexRunner(t, api, &logs)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rel := api.find(http.MethodPost, "/v1/tasks/CLAB-1/release")
	if len(rel) != 1 || !strings.Contains(rel[0].Body, "сервер прежней версии: нет стартовой инструкции") || !strings.Contains(rel[0].Body, "сбой 1 из 3") {
		t.Fatalf("release: %+v", rel)
	}
	if len(api.find(http.MethodPost, "/v1/tasks/CLAB-1/task-token")) != 0 {
		t.Error("без инструкции выпущен токен задачи")
	}

	api = newFakeAPI(t)
	var queries []url.Values
	api.routes = map[string]http.HandlerFunc{"/v1/tasks/CLAB-1/prompts/work": promptsRoute(&queries)}
	codexStub(t, `{"type":"error","message":"You've hit your session limit · resets 3pm"}`, 1)
	logs.Reset()
	r = codexRunner(t, api, &logs)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.pausedNow() || strings.Contains(logs.String(), "лимит подписки") {
		t.Errorf("лимит у codex поставил паузу:\n%s", logs.String())
	}
	rel = api.find(http.MethodPost, "/v1/tasks/CLAB-1/release")
	if len(rel) != 1 || !strings.Contains(rel[0].Body, "агент завершился с ошибкой") || !strings.Contains(rel[0].Body, "сбой 1 из 3") {
		t.Fatalf("release: %+v", rel)
	}
}

// AC-8: своя команда неизвестного агента — generic: инструкция текстом
// без usage=runner, на stdin, если {prompt} нет; раннер не отчитывается.
func TestGenericSelfReports(t *testing.T) {
	for name, withPrompt := range map[string]bool{"аргументом": true, "на stdin": false} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			var queries []url.Values
			api.routes = map[string]http.HandlerFunc{"/v1/tasks/CLAB-1/prompts/work": promptsRoute(&queries)}
			out := codexStub(t, `{"total_cost_usd": 0.5, "model": "claude-sonnet-5"}`, 0)
			command := "codex-like {prompt}"
			if !withPrompt {
				command = "codex-like --quiet"
			}
			var logs bytes.Buffer
			r := testRunner(t, api, strings.Replace(command, "codex-like", filepath.Join(filepath.Dir(out), "bin", "codex"), 1), &logs)
			r.agent = genericBinding(builtin(t))
			if err := r.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(queries) != 1 || queries[0].Has("usage") {
				t.Errorf("запрос инструкции: %v", queries)
			}
			argv := argvOf(t, out)
			stdin := readOut(t, out, "stdin")
			if withPrompt && (argv[len(argv)-1] != workText || stdin != "") || !withPrompt && (stdin != workText || slices.Contains(argv, workText)) {
				t.Errorf("инструкция: argv %q, stdin %q", argv, stdin)
			}
			if n := len(api.find(http.MethodPost, "/v1/tasks/CLAB-1/runs/run-1/usage")); n != 0 {
				t.Errorf("generic: раннер отчитался %d раз", n)
			}
			if _, ok := envOf(t, out)["ANTHROPIC_API_KEY"]; ok {
				t.Error("ключ Anthropic ушёл агенту generic")
			}
		})
	}
}

// AC-6: отчёт по выводу Claude Code — как до задачи, details.agent = claude.
func TestClaudeUsageNamesAgent(t *testing.T) {
	api := newFakeAPI(t)
	command, _ := stubAgent(t, agentOK)
	var logs bytes.Buffer
	r := testRunner(t, api, command, &logs)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	usage := api.find(http.MethodPost, "/v1/tasks/CLAB-1/runs/run-1/usage")
	if len(usage) != 1 {
		t.Fatalf("отчётов %d", len(usage))
	}
	var body struct {
		Model   string            `json:"model"`
		CostUSD float64           `json:"cost_usd"`
		Details map[string]string `json:"details"`
	}
	_ = json.Unmarshal([]byte(usage[0].Body), &body)
	if body.Model != "claude-sonnet-5" || body.CostUSD != 0.5 || len(body.Details) != 2 ||
		body.Details["reported_by"] != "clab-runner" || body.Details["agent"] != "claude" {
		t.Errorf("тело отчёта: %s", usage[0].Body)
	}
}

// AC-9 и решение человека после проверки CLAB-419: deny уходит в
// --disallowedTools шаблона claude; своя команда без {disallowed_tools}
// и привязка без перевода — предупреждение в журнале, не молчание.
func TestRunnerToolPolicy(t *testing.T) {
	policy := map[string]any{"allow": []string{"Read", "Grep"}, "deny": []string{"Bash"}}
	claimWith := func(p map[string]any) map[string]any {
		role := map[string]any{"key": "developer"}
		if p != nil {
			role["tool_policy"] = p
		}
		return map[string]any{"stage_run_id": "run-1", "role": role}
	}
	cases := []struct {
		name, agent, command string
		policy               map[string]any
		argv                 string
		warn                 string
	}{
		{"claude шаблон", "claude", "", policy, "--allowedTools Read,Grep --disallowedTools Bash /mcp__", ""},
		{"claude своя команда без deny", "claude", "claude -p --allowedTools {allowed_tools} {prompt}", policy, "--allowedTools Read,Grep /mcp__",
			"tool_policy роли: deny не передан привязке claude — в команде нет {disallowed_tools} (deny=Bash)"},
		{"codex", "codex", "", policy, "codex exec", "tool_policy роли не переведён для привязки codex: allow=Read,Grep, deny=Bash"},
		{"пустая политика", "codex", "", nil, "codex exec", ""},
	}
	for _, c := range cases {
		api := newFakeAPI(t)
		var queries []url.Values
		api.routes = map[string]http.HandlerFunc{"/v1/tasks/CLAB-1/prompts/work": promptsRoute(&queries)}
		api.claim = claimWith(c.policy)
		var logs bytes.Buffer
		r := testRunner(t, api, c.command, &logs)
		r.agent = bindingNamed(t, c.agent)
		r.cfg.DryRun = true
		if err := r.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		dry := lineWith(logs.String(), "сухой прогон")
		if !strings.Contains(dry, c.argv) {
			t.Errorf("%s: argv %s, ждали %q", c.name, dry, c.argv)
		}
		if c.agent == "codex" && !strings.Contains(dry, "<инструкция: ") {
			t.Errorf("%s: текст инструкции в журнале не заменён размером: %s", c.name, dry)
		}
		warn := lineWith(logs.String(), "tool_policy")
		if c.warn == "" && warn != "" || c.warn != "" && (!strings.Contains(warn, c.warn) || !strings.Contains(warn, "level=WARN")) {
			t.Errorf("%s: предупреждение %q, ждали %q", c.name, warn, c.warn)
		}
		if strings.Contains(c.argv, "Bash") && strings.Contains(dry, "--disallowedTools") != strings.Contains(c.argv, "--disallowedTools") {
			t.Errorf("%s: %s", c.name, dry)
		}
	}
}

// AC-5: порядок выбора модели, один случай на шаг.
func TestChooseModelOrder(t *testing.T) {
	high := "high"
	execution := func(models map[string]launch.ModelChoice, version string) *claimExecution {
		return &claimExecution{ModelLevel: &high, ModelLevelSource: "stage", Models: models, AgentSettingsVersion: json.RawMessage(version)}
	}
	settings := map[string]launch.ModelChoice{"claude": {Model: "claude-opus-x", Effort: "max"}, "codex": {Model: "gpt-x", Effort: "high"}}
	claim := func(e *claimExecution, hint string) claimResult {
		c := levelClaim(nil, "", hint)
		c.Execution = e
		return c
	}
	cases := []struct {
		name   string
		agent  string
		table  map[string]string
		claim  claimResult
		model  string
		effort string
		from   string
		source string
		ver    string
	}{
		{"explicit сильнее настройки", "claude", map[string]string{"high": "my-opus"}, claim(execution(settings, "4"), ""), "my-opus", "", modelFromTable, launch.SourceExplicit, ""},
		{"настройка сильнее таблицы привязки", "claude", nil, claim(execution(settings, "4"), "sonnet"), "claude-opus-x", "max", modelFromSettings, launch.SourceAgentSettings, "4"},
		{"запись своего агента", "codex", nil, claim(execution(settings, "4"), ""), "gpt-x", "high", modelFromSettings, launch.SourceAgentSettings, "4"},
		{"таблица привязки", "claude", nil, claim(execution(map[string]launch.ModelChoice{"codex": {Model: "gpt-x"}}, "4"), "sonnet"), "opus", "", modelFromBinding, launch.SourceBinding, ""},
		{"model_hint", "codex", nil, claim(execution(nil, "null"), "sonnet"), "sonnet", "", modelFromHint, "model_hint", ""},
		{"ничего", "codex", nil, claim(execution(nil, "null"), ""), "", "", modelFromNone, launch.SourceAgentDefault, ""},
		{"сервер без models", "claude", nil, claim(&claimExecution{ModelLevel: &high, ModelLevelSource: "stage"}, ""), "opus", "", modelFromBinding, launch.SourceBinding, ""},
	}
	for _, c := range cases {
		r := newRunner(Config{Project: "CLAB", ModelLevels: c.table}, slog.Default())
		r.agent = bindingNamed(t, c.agent)
		got := r.chooseModel(c.claim)
		if got.model != c.model || got.effort != c.effort || got.from != c.from || got.pickSource() != c.source || got.settingsVersion != c.ver {
			t.Errorf("%s: %+v (%s)", c.name, got, got.pickSource())
		}
		if c.name == "model_hint" && !got.hintDeprecated {
			t.Errorf("%s: без предупреждения об устаревшем model_hint", c.name)
		}
	}
}

// AC-10: окружение агента по привязке.
func TestAgentEnvByBinding(t *testing.T) {
	parent := append(append([]string{}, parentEnv...), "CLAUDE_CODE_USE_BEDROCK=1", "OPENAI_API_KEY=sk-openai",
		"CODEX_HOME=/c", "AWS_REGION=eu", "CLAB_X=1")
	set := map[string]string{"SDLC_TOKEN": taskTokenV, "SDLC_API_URL": "https://c/v1", "SDLC_PROJECT": "CLAB", "CLAB_CONFIG_DIR": "/tmp/a/clab"}
	cases := []struct {
		agent    string
		extra    []string
		pass, no []string
	}{
		{"claude", nil, []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK"}, []string{"OPENAI_API_KEY", "CODEX_HOME", "AWS_REGION"}},
		{"codex", nil, []string{"OPENAI_API_KEY", "CODEX_HOME"}, []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK"}},
		{"generic", nil, nil, []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEX_HOME", "AWS_REGION"}},
		{"generic", []string{"AWS_*"}, []string{"AWS_REGION"}, []string{"ANTHROPIC_API_KEY"}},
		{"codex", []string{"AWS_*"}, []string{"AWS_REGION", "OPENAI_API_KEY"}, []string{"ANTHROPIC_API_KEY"}},
	}
	for _, c := range cases {
		b := genericBinding(builtin(t))
		if c.agent != "generic" {
			b = bindingNamed(t, c.agent)
		}
		got := map[string]string{}
		for _, kv := range agentEnv(parent, append(append([]string{}, b.Env...), c.extra...), set) {
			k, v, _ := strings.Cut(kv, "=")
			got[k] = v
		}
		for _, k := range c.pass {
			if _, ok := got[k]; !ok {
				t.Errorf("%s %v: нет %s", c.agent, c.extra, k)
			}
		}
		for _, k := range append(c.no, "SDLC_ACTOR", "SDLC_RUNNER_ROLES", "CLAB_X") {
			if _, ok := got[k]; ok {
				t.Errorf("%s %v: прошло %s", c.agent, c.extra, k)
			}
		}
		if got["SDLC_TOKEN"] != taskTokenV || got["CLAB_CONFIG_DIR"] != "/tmp/a/clab" || got["SDLC_PROJECT"] != "CLAB" {
			t.Errorf("%s: заданное раннером: %v", c.agent, got)
		}
	}
}

// AC-7: разбор codex-jsonl.
func TestParseCodexUsage(t *testing.T) {
	got := parseCodexUsage([]byte(codexTurns), "gpt-x")
	if got["input_tokens"] != int64(1200) || got["cache_read_tokens"] != int64(2100) || got["output_tokens"] != int64(125) ||
		got["model"] != "gpt-x" || got["cost_usd"] != nil {
		t.Errorf("сумма ходов: %v", got)
	}
	if parseCodexUsage([]byte(codexTurns), "") != nil {
		t.Error("без модели отчёт не с чем сравнивать")
	}
	if parseCodexUsage([]byte("готово\n{\"type\":\"turn.started\"}"), "gpt-x") != nil {
		t.Error("без turn.completed — стоимость неизвестна, а не ноль")
	}
	if parseOutput("", []byte(codexTurns), "gpt-x") != nil || runnerReports("", "gpt-x") ||
		!runnerReports(outputClaudeJSON, "") || runnerReports(outputCodexJSONL, "") || !runnerReports(outputCodexJSONL, "m") {
		t.Error("решение об отчёте раннера")
	}
}

// Своя команда без {model}: модель агенту не уходит, и раннер не приписывает
// прогон выбранной модели — инструкция без usage=runner, отчёта нет.
func TestCodexCommandWithoutModelSlot(t *testing.T) {
	api := newFakeAPI(t)
	var queries []url.Values
	api.routes = map[string]http.HandlerFunc{"/v1/tasks/CLAB-1/prompts/work": promptsRoute(&queries)}
	api.claim = map[string]any{"stage_run_id": "run-1", "role": map[string]any{"key": "developer"},
		"execution": map[string]any{"model_level": "high", "model_level_source": "stage", "agent_settings_version": 4,
			"models": map[string]any{"codex": map[string]any{"model": "gpt-x"}}}}
	codexStub(t, codexTurns, 0)
	var logs bytes.Buffer
	r := codexRunner(t, api, &logs)
	r.cfg.Command = "codex exec --json {prompt}"
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || queries[0].Has("usage") {
		t.Errorf("запрос инструкции: %v", queries)
	}
	if n := len(api.find(http.MethodPost, "/v1/tasks/CLAB-1/runs/run-1/usage")); n != 0 {
		t.Errorf("отчётов %d, модель агенту не передана", n)
	}
	if !strings.Contains(logs.String(), "модель не передана: в команде нет {model}") {
		t.Errorf("журнал:\n%s", logs.String())
	}
}
