package launch

import (
	"maps"
	"testing"
)

// CLAB-470: разбор вывода — один на раннер и clab exec. Кто отчитался,
// вызывающий называет сам; имя здесь произвольное, не наших программ.
const testReporter = "launcher-x"

func TestLauncherReports(t *testing.T) {
	for _, c := range []struct {
		format, model string
		want          bool
	}{
		{OutputClaudeJSON, "", true},
		{OutputClaudeJSON, "m", true},
		{OutputCodexJSONL, "", false},
		{OutputCodexJSONL, "m", true},
		{"", "m", false},
		{"yaml-stream", "m", false},
	} {
		if got := LauncherReports(c.format, c.model); got != c.want {
			t.Errorf("LauncherReports(%q, %q) = %v", c.format, c.model, got)
		}
	}
	if !Parses(OutputClaudeJSON) || !Parses(OutputCodexJSONL) || Parses("") || Parses("yaml-stream") {
		t.Error("Parses: известны ровно claude-json и codex-jsonl")
	}
	if ParseOutput("yaml-stream", []byte(`{"model":"m"}`), "m", testReporter) != nil || ParseOutput("", nil, "m", testReporter) != nil {
		t.Error("формат без разбора дал отчёт")
	}
}

func TestEmitsFormat(t *testing.T) {
	for _, c := range []struct {
		format string
		argv   []string
		want   bool
	}{
		{OutputClaudeJSON, []string{"tool", "-p", "--output-format", "json", "go"}, true},
		{OutputClaudeJSON, []string{"tool", "--output-format=json"}, true},
		{OutputClaudeJSON, []string{"tool", "--output-format", "stream-json"}, false},
		{OutputClaudeJSON, []string{"tool", "-p", "json"}, false},
		{OutputCodexJSONL, []string{"tool", "exec", "--json", "go"}, true},
		{OutputCodexJSONL, []string{"tool", "exec", "--experimental-json"}, true},
		{OutputCodexJSONL, []string{"tool", "exec", "go"}, false},
		{"yaml-stream", []string{"tool", "--json"}, false},
	} {
		if got := EmitsFormat(c.format, c.argv); got != c.want {
			t.Errorf("EmitsFormat(%q, %q) = %v", c.format, c.argv, got)
		}
	}
	if FormatFlagHint(OutputClaudeJSON) != "--output-format json" || FormatFlagHint(OutputCodexJSONL) != "--json" || FormatFlagHint("x") != "" {
		t.Error("FormatFlagHint")
	}
}

func TestParseClaudeJSON(t *testing.T) {
	raw := []byte(`{"type":"result","total_cost_usd":1.25,"duration_ms":4200,"model":"model-a",
		"usage":{"input_tokens":12,"output_tokens":4500,"cache_read_input_tokens":280000,"cache_creation_input_tokens":14000},
		"modelUsage":{"model-b":{"costUSD":9}}}`)
	got := ParseOutput(OutputClaudeJSON, raw, "ignored", testReporter)
	want := map[string]any{
		"model": "model-a", "cost_usd": 1.25, "duration_ms": int64(4200),
		"input_tokens": int64(12), "output_tokens": int64(4500),
		"cache_read_tokens": int64(280000), "cache_write_tokens": int64(14000),
	}
	assertReport(t, got, want)
	// Без model — основная модель по стоимости из разреза.
	got = ParseClaudeJSON([]byte(`{"modelUsage":{"cheap":{"costUSD":0.1,"outputTokens":9},"dear":{"costUSD":2,"outputTokens":1}}}`), testReporter)
	if got == nil || got["model"] != "dear" {
		t.Errorf("основная модель: %v", got)
	}
	for _, bad := range []string{"", "готово", `{"total_cost_usd":0.1}`, `[1,2]`} {
		if ParseClaudeJSON([]byte(bad), testReporter) != nil {
			t.Errorf("%q дал отчёт", bad)
		}
	}
}

// Образец — Codex 0.167.0 (github.com/openai/codex, тег rust-v0.167.0):
// событие turn.completed из codex-rs/exec/src/exec_events.rs (Usage) с
// числами теста turn_completion_* в codex-rs/exec/tests/
// event_processor_with_json_output.rs. input_tokens включает чтение и
// запись кэша (codex-rs/codex-api/src/sse/responses.rs,
// parses_cache_write_token_usage: 100 = 40 + 60).
const codex167Turn = `{"type":"thread.started","thread_id":"th-1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"готово"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":3,"cache_write_input_tokens":4,"output_tokens":29,"reasoning_output_tokens":7}}
`

func TestParseCodexJSONLCacheWrite(t *testing.T) {
	got := ParseOutput(OutputCodexJSONL, []byte(codex167Turn), "gpt-q", testReporter)
	assertReport(t, got, map[string]any{
		"model": "gpt-q", "input_tokens": int64(3), "cache_read_tokens": int64(3),
		"cache_write_tokens": int64(4), "output_tokens": int64(29),
	})
	// Ни потери, ни двойного счёта: три корзины входа — весь input_tokens.
	if sum := got["input_tokens"].(int64) + got["cache_read_tokens"].(int64) + got["cache_write_tokens"].(int64); sum != 10 {
		t.Errorf("вход по корзинам %d, у Codex 10", sum)
	}
	// Ответ Responses API из теста Codex: 100 = 40 чтения + 60 записи.
	got = ParseCodexJSONL([]byte(`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"cache_write_input_tokens":60,"output_tokens":10,"reasoning_output_tokens":5}}`), "gpt-q", testReporter)
	if got["input_tokens"] != int64(0) || got["cache_read_tokens"] != int64(40) || got["cache_write_tokens"] != int64(60) || got["output_tokens"] != int64(10) {
		t.Errorf("Responses API: %v", got)
	}
}

// Codex до поля записи в кэш (serde default в 0.167.0 — у прежних версий
// поля нет): разбор прежний, cache_write_tokens не сообщается.
func TestParseCodexJSONLWithoutCacheWrite(t *testing.T) {
	raw := []byte(`{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":600,"output_tokens":50}}
{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":0,"output_tokens":5}}
`)
	assertReport(t, ParseCodexJSONL(raw, "gpt-q", testReporter), map[string]any{
		"model": "gpt-q", "input_tokens": int64(600), "cache_read_tokens": int64(600), "output_tokens": int64(55),
	})
	if ParseCodexJSONL(raw, "", testReporter) != nil {
		t.Error("без модели — отчёт")
	}
	for _, bad := range []string{"", "готово\n", `{"type":"turn.started"}` + "\n" + `{"type":"turn.failed","error":{"message":"x"}}`} {
		if ParseCodexJSONL([]byte(bad), "gpt-q", testReporter) != nil {
			t.Errorf("%q дал отчёт", bad)
		}
	}
}

func assertReport(t *testing.T, got, want map[string]any) {
	t.Helper()
	if got == nil {
		t.Fatal("отчёта нет")
	}
	details, _ := got["details"].(map[string]any)
	if details["reported_by"] != testReporter || len(details) != 1 {
		t.Errorf("details %v", got["details"])
	}
	rest := maps.Clone(got)
	delete(rest, "details")
	if !maps.Equal(rest, want) {
		t.Errorf("отчёт %v\nждали %v", rest, want)
	}
}
