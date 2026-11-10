package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLAB-470: учёт clab exec по выводу привязки. Привязки — с произвольными
// именами и полем output, а не claude и codex: решение ветвится по
// формату, а не по имени агента.

const execPersonalToken = "clab_human_personal" // токен личного конфига фикстуры

// usageKit — набор с привязками: разбор claude-json, разбор codex-jsonl и
// без разбора.
const usageKit = `{"bindings":[
 {"name":"relay","run":["agent","-p","--output-format","json","{prompt}"],"output":"claude-json","prompt":"text","executor":"process"},
 {"name":"relay-x","run":["agent","exec","--json","-m","{model}","{prompt}"],"output":"codex-jsonl","prompt":"text","executor":"process"},
 {"name":"plain","run":["agent","{prompt}"],"output":null,"prompt":"text","executor":"process"},
 {"name":"odd","run":["agent","{prompt}"],"output":"yaml-stream","prompt":"text","executor":"process"}
]}`

const claudeResult = `{"type":"result","subtype":"success","is_error":false,"duration_ms":81234,"total_cost_usd":3.4567,` +
	`"model":"model-main","usage":{"input_tokens":21,"output_tokens":4512,"cache_read_input_tokens":281000,"cache_creation_input_tokens":13000},` +
	`"modelUsage":{"model-main":{"inputTokens":21,"outputTokens":4512,"costUSD":3.4},"model-aux":{"inputTokens":900,"outputTokens":40,"costUSD":0.05}}}` + "\n"

const codexEvents = `{"type":"thread.started","thread_id":"th-9"}
{"type":"turn.started"}
{"type":"turn.completed","usage":{"input_tokens":294000,"cached_input_tokens":250000,"cache_write_input_tokens":30000,"output_tokens":4500,"reasoning_output_tokens":900}}
`

func newUsageFixture(t *testing.T) *execFixture {
	t.Helper()
	f := newExecFixture(t)
	f.srv.kit = usageKit
	return f
}

// print — файл, который заглушка агента печатает в stdout.
func (f *execFixture) print(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(f.out, "print.out")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return "STUB_PRINT=" + p
}

func (f *execFixture) usageBodies() ([]map[string]any, []string) {
	f.srv.mu.Lock()
	defer f.srv.mu.Unlock()
	return append([]map[string]any(nil), f.srv.usages...), append([]string(nil), f.srv.usageAuths...)
}

func (f *execFixture) promptQueries() []string {
	f.srv.mu.Lock()
	defer f.srv.mu.Unlock()
	return append([]string(nil), f.srv.promptQueries...)
}

func (f *execFixture) noUsageFile(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.runDir, "usage.json")); err == nil {
		t.Error("usage.json записан, а сохранять было нечего")
	}
}

// AC-2, AC-4, AC-5, AC-9, AC-10, AC-14: строка по выводу claude-json; клиент
// запустившего строится после выхода агента, агент его данных не видит.
func TestExecUsageClaudeJSON(t *testing.T) {
	f := newUsageFixture(t)
	personal := filepath.Join(f.home, ".config", "clab")
	built := 0
	old := launcherClient
	launcherClient = func() (*Client, error) {
		built++
		if _, err := os.Stat(filepath.Join(f.out, "finished")); err != nil {
			t.Error("клиент запустившего построен до выхода агента")
		}
		return old()
	}
	t.Cleanup(func() { launcherClient = old })

	// В окружении clab — каталог личного конфига, как у настоящего запуска.
	r := f.run(t, f.env(f.print(t, claudeResult), "CLAB_CONFIG_DIR="+personal), execTask, "--dir", f.runDir, "--agent", "relay")
	if r.code != 0 || built != 1 {
		t.Fatalf("код %d, клиентов %d\n%s", r.code, built, r.stderr)
	}
	if r.stdout != claudeResult {
		t.Errorf("stdout запустившего не байт в байт вывод агента:\n%q", r.stdout)
	}
	if q := f.promptQueries(); len(q) != 1 || q[0] != "usage=runner" {
		t.Errorf("инструкция запрошена с %q, ждали usage=runner", q)
	}
	bodies, auths := f.usageBodies()
	if len(bodies) != 1 || auths[0] != "Bearer "+execPersonalToken {
		t.Fatalf("строк учёта %d, Authorization %q", len(bodies), auths)
	}
	calls := f.srv.requests()
	if want := "POST /v1/tasks/PROJ-7/runs/" + leaseRun + "/usage"; calls[len(calls)-1] != want {
		t.Errorf("последний запрос %q, ждали %q", calls[len(calls)-1], want)
	}
	b := bodies[0]
	want := map[string]any{"model": "model-main", "input_tokens": 21.0, "output_tokens": 4512.0,
		"cache_read_tokens": 281000.0, "cache_write_tokens": 13000.0, "cost_usd": 3.4567, "duration_ms": 81234.0}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("%s = %v, ждали %v", k, b[k], v)
		}
	}
	if len(b) != len(want)+1 {
		t.Errorf("лишние поля строки (сумму считает сервер): %v", b)
	}
	if d, _ := b["details"].(map[string]any); d["reported_by"] != "clab exec" || d["agent"] != "relay" {
		t.Errorf("details %v", b["details"])
	}
	if !strings.Contains(r.stderr, "учёт: о стоимости отчитается clab exec по выводу claude-json") ||
		!strings.Contains(r.stderr, "учёт: строка записана — модель model-main") {
		t.Errorf("нет строк учёта:\n%s", r.stderr)
	}
	// Граница: личных данных нет ни в окружении, ни в argv, ни в stdin
	// агента, ни в файлах каталога захода.
	env := f.childEnv(t)
	if env["CLAB_CONFIG_DIR"] != filepath.Join(f.runDir, "clab") {
		t.Errorf("CLAB_CONFIG_DIR агента %q", env["CLAB_CONFIG_DIR"])
	}
	for _, name := range []string{"argv", "env", "stdin"} {
		if body := f.file(t, name); strings.Contains(body, execPersonalToken) || strings.Contains(body, personal+"\n") {
			t.Errorf("%s агента содержит личные учётные данные", name)
		}
	}
	_ = filepath.WalkDir(f.runDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if raw, _ := os.ReadFile(p); strings.Contains(string(raw), execPersonalToken) {
				t.Errorf("%s содержит личный токен", p)
			}
		}
		return nil
	})
	if strings.Contains(r.stderr, execPersonalToken) {
		t.Errorf("вывод показал личный токен:\n%s", r.stderr)
	}
	f.noUsageFile(t)
}

// AC-3: codex-jsonl — модель переданная агенту, токены по turn.completed;
// своя команда без {model} — без учёта и без usage=runner.
func TestExecUsageCodexJSONL(t *testing.T) {
	f := newUsageFixture(t)
	r := f.run(t, f.env(f.print(t, codexEvents)), execTask, "--dir", f.runDir, "--agent", "relay-x", "--model", "gpt-q")
	if r.code != 0 {
		t.Fatalf("код %d\n%s", r.code, r.stderr)
	}
	bodies, _ := f.usageBodies()
	if len(bodies) != 1 {
		t.Fatalf("строк учёта %d\n%s", len(bodies), r.stderr)
	}
	b := bodies[0]
	for k, v := range map[string]any{"model": "gpt-q", "input_tokens": 14000.0, "cache_read_tokens": 250000.0,
		"cache_write_tokens": 30000.0, "output_tokens": 4500.0} {
		if b[k] != v {
			t.Errorf("%s = %v, ждали %v", k, b[k], v)
		}
	}
	if _, ok := b["cost_usd"]; ok {
		t.Errorf("у Codex денег в выводе нет, а cost_usd передан: %v", b)
	}
	if q := f.promptQueries(); len(q) != 1 || q[0] != "usage=runner" {
		t.Errorf("инструкция: %q", q)
	}

	f = newUsageFixture(t)
	r = f.run(t, f.env(f.print(t, codexEvents)), execTask, "--dir", f.runDir, "--agent", "relay-x", "--model", "gpt-q", "--", "agent", "exec", "--json", "{prompt}")
	if bodies, _ := f.usageBodies(); r.code != 0 || len(bodies) != 0 {
		t.Fatalf("без {model}: код %d, строк %d\n%s", r.code, len(bodies), r.stderr)
	}
	if q := f.promptQueries(); len(q) != 1 || q[0] != "" {
		t.Errorf("без {model} инструкция с %q", q)
	}
	if !strings.Contains(r.stderr, "учёт: о стоимости отчитывается агент сам по инструкции — в выводе codex-jsonl модели нет") {
		t.Errorf("нет причины:\n%s", r.stderr)
	}
}

// AC-6: агент вышел с кодом 3, вывод разобран — строка отправлена, код 3.
func TestExecUsageAgentFailed(t *testing.T) {
	f := newUsageFixture(t)
	r := f.run(t, f.env(f.print(t, claudeResult), "STUB_CODE=3"), execTask, "--dir", f.runDir, "--agent", "relay")
	if bodies, _ := f.usageBodies(); r.code != 3 || len(bodies) != 1 {
		t.Fatalf("код %d, строк %d\n%s", r.code, len(bodies), r.stderr)
	}
}

// AC-7: нечего разбирать — ни строки, ни нуля, ни usage.json.
func TestExecUsageNothingToParse(t *testing.T) {
	for _, c := range []struct {
		name, agent, out string
	}{
		{"пустой вывод", "relay", ""},
		{"мусор", "relay", "готово, всё сделал\n"},
		{"JSON без модели", "relay", `{"total_cost_usd":0.5}`},
		{"codex без turn.completed", "relay-x", `{"type":"turn.started"}` + "\n" + `{"type":"turn.failed","error":{"message":"x"}}` + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUsageFixture(t)
			r := f.run(t, f.env(f.print(t, c.out), "STUB_CODE=2"), execTask, "--dir", f.runDir, "--agent", c.agent, "--model", "m1")
			if bodies, _ := f.usageBodies(); r.code != 2 || len(bodies) != 0 {
				t.Fatalf("код %d, строк %d", r.code, len(bodies))
			}
			if !strings.Contains(r.stderr, "предупреждение: учёт не отправлен — вывод агента не разобран") {
				t.Errorf("нет предупреждения:\n%s", r.stderr)
			}
			f.noUsageFile(t)
		})
	}
}

// AC-8: отказ сервера, чужой хост, нет учётных данных — usage.json и
// готовая команда, код агента, токенов в выводе и файле нет.
func TestExecUsageNotSent(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, f *execFixture)
		reason string
		posted bool
	}{
		{"сервер отказал", func(t *testing.T, f *execFixture) {
			f.srv.usage = func() (int, string) {
				return apiFail(http.StatusForbidden, "forbidden", "о прогоне отчитывается тот, кто его забирал")
			}
		}, "сервер не принял строку: forbidden", true},
		{"чужой хост", func(t *testing.T, f *execFixture) {
			other := Config{DefaultHost: "o", Hosts: map[string]HostConfig{"o": {URL: "https://other.example", Token: execPersonalToken}}}
			if err := saveConfigAt(filepath.Join(f.home, ".config", "clab"), other); err != nil {
				t.Fatal(err)
			}
		}, "хост учётных данных запустившего https://other.example не тот, что у токена задачи", false},
		{"нет учётных данных", func(t *testing.T, f *execFixture) {
			t.Setenv("CLAB_CONFIG_DIR", t.TempDir())
		}, "учётных данных запустившего нет", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newUsageFixture(t)
			c.setup(t, f)
			r := f.run(t, f.env(f.print(t, claudeResult), "STUB_CODE=5"), execTask, "--dir", f.runDir, "--agent", "relay")
			bodies, _ := f.usageBodies()
			if r.code != 5 || (len(bodies) == 1) != c.posted {
				t.Fatalf("код %d, строк %d\n%s", r.code, len(bodies), r.stderr)
			}
			path := filepath.Join(f.runDir, "usage.json")
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("usage.json: %v %v", info, err)
			}
			raw, _ := os.ReadFile(path)
			var saved map[string]any
			if json.Unmarshal(raw, &saved) != nil || saved["model"] != "model-main" || saved["input_tokens"] != 21.0 {
				t.Errorf("usage.json: %s", raw)
			}
			for _, want := range []string{c.reason, "строка сохранена в " + path,
				"сдать вручную: clab --host " + f.srv.srv.URL + " usage PROJ-7 --run " + leaseRun + " --model model-main --input 21 --output 4512 --cache-read 281000 --cache-write 13000 --cost 3.4567 --duration-ms 81234 --details '{\"agent\":\"relay\",\"reported_by\":\"clab exec\"}'"} {
				if !strings.Contains(r.stderr, want) {
					t.Errorf("нет %q:\n%s", want, r.stderr)
				}
			}
			for _, s := range []string{r.stderr, string(raw)} {
				if strings.Contains(s, execPersonalToken) || strings.Contains(s, execToken) {
					t.Errorf("токен в выводе или файле:\n%s", s)
				}
			}
		})
	}
}

// AC-10: вывод больше предела перехвата — агент не оборван, stdout
// целиком, учёт не отправлен.
func TestExecUsageCaptureLimit(t *testing.T) {
	old := execCaptureLimit
	execCaptureLimit = 64
	t.Cleanup(func() { execCaptureLimit = old })
	f := newUsageFixture(t)
	r := f.run(t, f.env(f.print(t, claudeResult)), execTask, "--dir", f.runDir, "--agent", "relay")
	if bodies, _ := f.usageBodies(); r.code != 0 || len(bodies) != 0 || r.stdout != claudeResult {
		t.Fatalf("код %d, строк %d, stdout %d байт", r.code, len(bodies), len(r.stdout))
	}
	if !strings.Contains(r.stderr, "больше предела перехвата 64 байт") {
		t.Errorf("нет предупреждения о пределе:\n%s", r.stderr)
	}
	f.noUsageFile(t)
}

// AC-12: план и сухой прогон называют, кто отчитается; сухой прогон ничего
// не шлёт и не пишет.
func TestExecUsageDryRun(t *testing.T) {
	f := newUsageFixture(t)
	r := f.run(t, f.env(), execTask, "--dir", f.runDir, "--agent", "relay", "--dry-run")
	if bodies, _ := f.usageBodies(); r.code != 0 || f.ran() || len(bodies) != 0 || f.srv.heartbeats() != 0 {
		t.Fatalf("код %d, запущен %v, строк %d", r.code, f.ran(), len(bodies))
	}
	if !strings.Contains(r.stdout, "clab exec: учёт: о стоимости отчитается clab exec по выводу claude-json после выхода агента; инструкция без самоотчёта (usage=runner)") {
		t.Errorf("нет строки учёта:\n%s", r.stdout)
	}
	f.noUsageFile(t)
}

// AC-13, R-8: привязка без разбора, неизвестный формат, набор без привязок,
// своя команда без флага формата — агент отчитывается сам, clab exec
// ничего не шлёт, инструкция без usage=runner.
func TestExecUsageAgentReportsItself(t *testing.T) {
	cases := []struct {
		name string
		kit  string
		args []string
		line string
		warn string
	}{
		{"без разбора", usageKit, []string{"--agent", "plain"}, "у привязки plain нет разбора вывода", ""},
		{"неизвестный формат", usageKit, []string{"--agent", "odd"}, "у привязки odd нет разбора вывода", ""},
		{"набор без привязок", `{"version":"v0.27.0","files":[]}`, []string{"--", "agent", "--output-format", "json"}, "набор инстанса не отдал привязок", ""},
		{"своя команда без флага формата", usageKit, []string{"--agent", "relay", "--", "agent", "-p", "{prompt}"},
			"в своей команде нет флага формата вывода claude-json", "предупреждение: своя команда не включает формат вывода claude-json (--output-format json)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newUsageFixture(t)
			f.srv.kit = c.kit
			r := f.run(t, f.env(f.print(t, claudeResult), "STUB_CODE=4"), append([]string{execTask, "--dir", f.runDir}, c.args...)...)
			if bodies, _ := f.usageBodies(); r.code != 4 || len(bodies) != 0 || f.srv.heartbeats() != 1 {
				t.Fatalf("код %d, строк %d, продлений %d\n%s", r.code, len(bodies), f.srv.heartbeats(), r.stderr)
			}
			if q := f.promptQueries(); len(q) != 1 || q[0] != "" {
				t.Errorf("инструкция с %q", q)
			}
			if !strings.Contains(r.stderr, "учёт: о стоимости отчитывается агент сам по инструкции — "+c.line) {
				t.Errorf("нет причины %q:\n%s", c.line, r.stderr)
			}
			if c.warn != "" && !strings.Contains(r.stderr, c.warn) {
				t.Errorf("нет предупреждения %q:\n%s", c.warn, r.stderr)
			}
			if strings.Contains(r.stderr, "учёт не отправлен") {
				t.Errorf("clab exec разбирал вывод, хотя не отчитывается:\n%s", r.stderr)
			}
			f.noUsageFile(t)
		})
	}
}

// Поле output привязки: известные форматы — как есть, прочее — как null.
func TestParseBindingsOutput(t *testing.T) {
	got := parseBindings(json.RawMessage(`[{"name":"a","output":"claude-json"},{"name":"b","output":"codex-jsonl"},
		{"name":"c","output":null},{"name":"d","output":"yaml"},{"name":"e","output":{"x":1}},{"name":"f"}]`))
	want := map[string]string{"a": "claude-json", "b": "codex-jsonl", "c": "", "d": "", "e": "", "f": ""}
	if len(got) != len(want) {
		t.Fatalf("привязок %d", len(got))
	}
	for _, b := range got {
		if b.Output != want[b.Name] {
			t.Errorf("%s: output %q, ждали %q", b.Name, b.Output, want[b.Name])
		}
	}
}
