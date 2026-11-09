package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// CLAB-427: модель уровня для своего агента в clab claim и clab show
// (решение 0077 пп. 7–8) и пояснение к clab usage.

// setAgentEnv подменяет окружение, по которому clab узнаёт агента.
func setAgentEnv(t *testing.T, env map[string]string) {
	t.Helper()
	prev := agentEnv
	agentEnv = func(k string) string { return env[k] }
	t.Cleanup(func() { agentEnv = prev })
}

var (
	envClaude    = map[string]string{"AI_AGENT": "claude-code_2.1.314"}
	envClaudeOne = map[string]string{"CLAUDECODE": "1"}
	envCodex     = map[string]string{"CODEX_THREAD_ID": "019a-thread"}
	envBoth      = map[string]string{"AI_AGENT": "claude-code_2.1.314", "CODEX_THREAD_ID": "019a-thread"}
)

const settingsModels = `{"claude":{"model":"claude-opus-5-5","effort":"high"},"codex":{"model":"gpt-6.1-sol","effort":"high"}}`

// execReply — ответ захвата с execution; version — "3", "null" или "" (ключа
// нет, сервер прежней версии); models — JSON или "".
func execReply(t *testing.T, level, version, models string) string {
	t.Helper()
	return claimReply(t, func(m map[string]any) {
		exec := map[string]any{"model_level": nil, "model_level_source": "none"}
		if level != "" {
			exec["model_level"], exec["model_level_source"] = level, "stage"
		}
		if version != "" {
			exec["agent_settings_version"] = json.RawMessage(version)
		}
		if models != "" {
			exec["models"] = json.RawMessage(models)
		}
		m["execution"] = exec
	})
}

type kitStub struct {
	calls int
	// kit — тело /v1/agent-kit; status — код; hang — ответ после паузы.
	kit    string
	status int
	hang   time.Duration
	ctype  string
}

// claimClient — клиент со стабом: захват отдаёт reply, /v1/agent-kit — kit.
func claimClient(t *testing.T, reply string, kit *kitStub) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agent-kit":
			kit.calls++
			if kit.hang > 0 {
				time.Sleep(kit.hang)
			}
			if kit.ctype == "" {
				kit.ctype = "application/json"
			}
			w.Header().Set("Content-Type", kit.ctype)
			if kit.status != 0 {
				w.WriteHeader(kit.status)
			}
			_, _ = w.Write([]byte(kit.kit))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(reply))
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: srv.URL, token: "clab_x", http: srv.Client()}
}

func claimOut(t *testing.T, c *Client) string {
	t.Helper()
	withJSON(t, false)
	out, _, err := capture(t, func() error {
		return cmdClaim(context.Background(), c, []string{"CLAB-146"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func showOut(t *testing.T, c *Client) string {
	t.Helper()
	withJSON(t, false)
	out, _, err := capture(t, func() error {
		return cmdShow(context.Background(), c, []string{"CLAB-146"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func modelBlock(out string) string {
	var lines []string
	collect := false
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "Модель уровня:") {
			collect = true
		} else if collect && !strings.HasPrefix(l, "  ") {
			collect = false
		}
		if collect {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

const bindingsKit = `{"version":"v0.28.0","files":[],"bindings":[` +
	`{"name":"claude","detect":[{"env":"AI_AGENT","prefix":"claude-code_"},{"env":"CLAUDECODE","equals":"1"}],"levels":{"low":"haiku","medium":"sonnet","high":"opus"}},` +
	`{"name":"codex","detect":[{"env":"CODEX_THREAD_ID","non_empty":true}],"levels":null},` +
	`{"name":"generic","detect":null,"levels":null}]}`

// AC-11: агент узнан по AI_AGENT, CLAUDECODE и CODEX_THREAD_ID; строка
// рекомендации и «Уровни задачи» на месте; набор не запрашивается.
func TestModelLineAgentRecognized(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{envClaude, "Модель уровня: claude-opus-5-5, усилие high (agent_settings: агент claude, настройка агентов v3)"},
		{envClaudeOne, "Модель уровня: claude-opus-5-5, усилие high (agent_settings: агент claude, настройка агентов v3)"},
		{envCodex, "Модель уровня: gpt-6.1-sol, усилие high (agent_settings: агент codex, настройка агентов v3)"},
	}
	for _, tc := range cases {
		setAgentEnv(t, tc.env)
		kit := &kitStub{kit: bindingsKit}
		c := claimClient(t, execReply(t, "high", "3", settingsModels), kit)
		for name, out := range map[string]string{"claim": claimOut(t, c), "show": showOut(t, c)} {
			lines := strings.Split(out, "\n")
			idx := -1
			for i, l := range lines {
				if l == "Рекомендованный уровень: high (этап задачи)" {
					idx = i
				}
			}
			if idx < 0 || lines[idx+1] != tc.want {
				t.Errorf("%s %v: строка модели не сразу под рекомендацией:\n%s", name, tc.env, out)
			}
			if !strings.HasPrefix(lines[idx+2], "Уровни задачи:") {
				t.Errorf("%s: «Уровни задачи» сдвинулась: %q", name, lines[idx+2])
			}
		}
		if kit.calls != 0 {
			t.Errorf("%v: набор запрошен %d раз при записи агента в версии", tc.env, kit.calls)
		}
	}

	// без effort — без «, усилие …»
	setAgentEnv(t, envClaude)
	c := claimClient(t, execReply(t, "high", "3", `{"claude":{"model":"claude-sonnet-5"}}`), &kitStub{})
	if got := modelBlock(claimOut(t, c)); got != "Модель уровня: claude-sonnet-5 (agent_settings: агент claude, настройка агентов v3)" {
		t.Errorf("без усилия: %q", got)
	}
}

// AC-12: без признаков и при двух признаках — агент не узнан, модели всех.
func TestModelLineAgentUnrecognized(t *testing.T) {
	for _, env := range []map[string]string{{}, envBoth} {
		setAgentEnv(t, env)
		c := claimClient(t, execReply(t, "high", "3", settingsModels), &kitStub{status: 404, kit: "nope"})
		got := modelBlock(claimOut(t, c))
		want := "Модель уровня: агент не узнан\n" +
			"  claude  claude-opus-5-5  усилие high\n" +
			"  codex   gpt-6.1-sol      усилие high\n" +
			"  источник: agent_settings, настройка агентов v3"
		if got != want {
			t.Errorf("%v:\n%s\nждали:\n%s", env, got, want)
		}
	}
	setAgentEnv(t, envClaude)
	c := claimClient(t, execReply(t, "high", "3", `{}`), &kitStub{status: 404})
	// агент узнан, но записей нет и таблицы привязки нет — agent_default
	if got := modelBlock(claimOut(t, c)); !strings.Contains(got, "agent_default: агент claude") {
		t.Errorf("узнанный агент без записи: %q", got)
	}
	setAgentEnv(t, map[string]string{})
	c = claimClient(t, execReply(t, "high", "3", `{}`), &kitStub{status: 404})
	if got := modelBlock(claimOut(t, c)); got != "Модель уровня: агент не узнан; в настройке агентов v3 для уровня high записей нет" {
		t.Errorf("пустые models: %q", got)
	}
}

// AC-13: версии нет — таблица привязки; нет привязок, таблицы, агента —
// вывод как до задачи; версия есть, записи нет — agent_default; сервер
// прежней версии и model_level null — строк нет.
func TestModelLineBindingAndFallbacks(t *testing.T) {
	// binding: версии нет, привязка claude с таблицей
	setAgentEnv(t, envClaude)
	kit := &kitStub{kit: bindingsKit}
	c := claimClient(t, execReply(t, "high", "null", ""), kit)
	if got := modelBlock(claimOut(t, c)); got != "Модель уровня: opus (binding: агент claude, таблица привязки по умолчанию)" {
		t.Errorf("binding: %q", got)
	}
	if kit.calls != 1 {
		t.Errorf("набор запрошен %d раз", kit.calls)
	}

	// версия есть, записи для claude нет, таблица привязки есть — binding
	c = claimClient(t, execReply(t, "high", "3", `{"codex":{"model":"gpt-6.1-sol"}}`), &kitStub{kit: bindingsKit})
	if got := modelBlock(claimOut(t, c)); !strings.HasPrefix(got, "Модель уровня: opus (binding:") {
		t.Errorf("версия без записи, но с таблицей: %q", got)
	}

	// вывод байт в байт как без execution-строк: версии нет и таблицы/привязок/агента нет
	plain := claimOut(t, claimClient(t, claimReply(t, func(m map[string]any) {
		m["execution"] = map[string]any{"model_level": "high", "model_level_source": "stage"}
	}), &kitStub{}))
	silent := []struct {
		name string
		env  map[string]string
		kit  *kitStub
	}{
		{"привязок нет", envClaude, &kitStub{kit: `{"version":"v0.28.0","files":[]}`}},
		{"у агента нет таблицы", envCodex, &kitStub{kit: bindingsKit}},
		{"агент не узнан", map[string]string{}, &kitStub{kit: bindingsKit}},
		{"набор недоступен", envClaude, &kitStub{status: 500, kit: "boom"}},
	}
	for _, tc := range silent {
		setAgentEnv(t, tc.env)
		got := claimOut(t, claimClient(t, execReply(t, "high", "null", ""), tc.kit))
		if got != plain {
			t.Errorf("%s: вывод изменился:\n%s", tc.name, got)
		}
	}

	// версия есть, записи нет, таблицы нет — agent_default
	setAgentEnv(t, envCodex)
	c = claimClient(t, execReply(t, "high", "3", `{"claude":{"model":"claude-opus-5-5"}}`), &kitStub{kit: bindingsKit})
	want := "Модель уровня: не задана, модель агента по умолчанию (agent_default: агент codex, в настройке агентов v3 нет записи для уровня high)"
	if got := modelBlock(claimOut(t, c)); got != want {
		t.Errorf("agent_default: %q", got)
	}

	// сервер прежней версии: ключа agent_settings_version нет
	setAgentEnv(t, envClaude)
	kit = &kitStub{kit: bindingsKit}
	got := claimOut(t, claimClient(t, execReply(t, "high", "", ""), kit))
	if got != plain || kit.calls != 0 {
		t.Errorf("прежний сервер: вывод изменился или запрошен набор (%d):\n%s", kit.calls, got)
	}

	// model_level null — строки нет ни в одном случае
	for _, version := range []string{"3", "null", ""} {
		got := claimOut(t, claimClient(t, execReply(t, "", version, settingsModels), &kitStub{kit: bindingsKit}))
		if strings.Contains(got, "Модель уровня") {
			t.Errorf("version=%q: строка модели без уровня:\n%s", version, got)
		}
	}
}

// AC-14: сбой набора не ломает захват; --json байт в байт.
func TestModelLineKitFailures(t *testing.T) {
	setAgentEnv(t, envClaude)
	prev := agentKitTimeout
	agentKitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { agentKitTimeout = prev })

	plain := claimOut(t, claimClient(t, claimReply(t, func(m map[string]any) {
		m["execution"] = map[string]any{"model_level": "high", "model_level_source": "stage"}
	}), &kitStub{}))
	failures := map[string]*kitStub{
		"500":          {status: 500, kit: "boom"},
		"зависло":      {hang: 400 * time.Millisecond, kit: bindingsKit},
		"HTML":         {ctype: "text/html", kit: "<html>SPA</html>"},
		"без привязок": {kit: `{"version":"v0.28.0","files":[]}`},
		"форма иная":   {kit: `{"bindings":"строка"}`},
	}
	for name, kit := range failures {
		got := claimOut(t, claimClient(t, execReply(t, "high", "null", ""), kit))
		if got != plain {
			t.Errorf("%s: захват изменился:\n%s", name, got)
		}
		if strings.Contains(got, "binding") {
			t.Errorf("%s: строка binding без набора", name)
		}
	}

	// --json: ответ сервера как есть, набор не запрашивается
	reply := execReply(t, "high", "3", settingsModels)
	kit := &kitStub{kit: bindingsKit}
	c := claimClient(t, reply, kit)
	withJSON(t, true)
	for name, run := range map[string]func() error{
		"claim": func() error { return cmdClaim(context.Background(), c, []string{"CLAB-146"}) },
		"show":  func() error { return cmdShow(context.Background(), c, []string{"CLAB-146"}) },
	} {
		out, _, err := capture(t, run)
		if err != nil {
			t.Fatal(err)
		}
		var got, want any
		_ = json.Unmarshal([]byte(out), &got)
		_ = json.Unmarshal([]byte(reply), &want)
		if !equalJSON(got, want) || strings.Contains(out, "Модель уровня") {
			t.Errorf("%s --json изменился:\n%s", name, out)
		}
	}
	if kit.calls != 0 {
		t.Errorf("--json запросил набор %d раз", kit.calls)
	}
}

// Признаки агента берутся из detect привязок набора; непонятная форма — как
// без привязок (встроенные признаки).
func TestDetectAgentFromBindings(t *testing.T) {
	custom := parseBindings(json.RawMessage(`[
		{"name":"claude","detect":[{"env":"MY_CLAUDE","non_empty":true}]},
		{"name":"codex","detect":[{"env":"CODEX_THREAD_ID","non_empty":true}]},
		{"name":"weird","detect":[{"env":"X","regex":"."}]}]`))
	get := func(env map[string]string) func(string) string { return func(k string) string { return env[k] } }
	if got := detectAgent(get(map[string]string{"MY_CLAUDE": "1"}), custom); len(got) != 1 || got[0] != "claude" {
		t.Errorf("признак набора: %v", got)
	}
	// у набора признак AI_AGENT не объявлен — встроенный не подмешивается
	if got := detectAgent(get(envClaude), custom); len(got) != 0 {
		t.Errorf("встроенный признак подмешан к набору: %v", got)
	}
	// набор без понятных признаков — встроенные
	if got := detectAgent(get(envClaudeOne), parseBindings(json.RawMessage(`{"claude":{"detect":"что-то"}}`))); len(got) != 1 || got[0] != "claude" {
		t.Errorf("встроенные при непонятной форме: %v", got)
	}
	if got := detectAgent(get(envBoth), nil); len(got) != 2 {
		t.Errorf("оба признака должны дать неоднозначность: %v", got)
	}
	if got := detectAgent(get(map[string]string{"AI_AGENT": "cursor_1"}), nil); len(got) != 0 {
		t.Errorf("чужой AI_AGENT: %v", got)
	}
	// массив и объект по имени разбираются одинаково
	byName := parseBindings(json.RawMessage(`{"codex":{"levels":{"high":"x"}}}`))
	if len(byName) != 1 || byName[0].Name != "codex" || byName[0].Levels["high"] != "x" {
		t.Errorf("объект по имени: %+v", byName)
	}
}

// pickModel — порядок 0077 п. 7 без печати; её берут clab exec и раннер.
func TestPickModelOrder(t *testing.T) {
	binding := &agentBinding{Name: "claude", Levels: map[string]string{"high": "opus"}}
	snap := map[string]modelChoice{"claude": {Model: "claude-opus-5-5", Effort: "high"}}
	if p := pickModel("high", "claude", snap, binding); p.Source != sourceAgentSettings || p.Model != "claude-opus-5-5" || p.Effort != "high" {
		t.Errorf("версия первой: %+v", p)
	}
	if p := pickModel("high", "claude", nil, binding); p.Source != sourceBinding || p.Model != "opus" {
		t.Errorf("затем привязка: %+v", p)
	}
	if p := pickModel("low", "claude", nil, binding); p.Source != sourceAgentDefault || p.Model != "" {
		t.Errorf("затем модель по умолчанию: %+v", p)
	}
	if p := pickModel("high", "codex", snap, nil); p.Source != sourceAgentDefault {
		t.Errorf("чужой агент: %+v", p)
	}
}

// AC-15: пояснение к clab usage в stderr, stdout без изменений.
func TestUsageCostSourceLine(t *testing.T) {
	cases := []struct {
		name, reply, want string
	}{
		{"pricing", `{"id":"u1","cost_usd":18,"cost_source":"pricing","pricing_version":4,"reported_cost_usd":20}`,
			"сумма 18 USD по прайсу версии 4, агент назвал 20 USD"},
		{"pricing без названной", `{"id":"u1","cost_usd":18,"cost_source":"pricing","pricing_version":4,"reported_cost_usd":null}`,
			"сумма 18 USD по прайсу версии 4"},
		{"reported", `{"id":"u1","cost_usd":0.42,"cost_source":"reported","pricing_version":null,"reported_cost_usd":0.42}`,
			"сумма 0.42 USD как сообщена"},
		{"суммы нет", `{"id":"u1","cost_usd":null,"cost_source":null}`, "суммы нет"},
		{"вложенный recorded", `{"recorded":{"id":"u1","cost_usd":3,"cost_source":"reported"}}`, "сумма 3 USD как сообщена"},
		{"сервер прежний", `{"id":"u1","cost_usd":1.5}`, ""},
	}
	for _, tc := range cases {
		withJSON(t, false)
		for _, args := range [][]string{
			{"CLAB-7", "--model", "m", "--input", "10", "--output", "5", "--cost", "20"},
			{"CLAB-7", "--model", "m", "--input", "10", "--output", "5"},
		} {
			c, rec := newInviteClient(t, http.StatusCreated, tc.reply)
			stdout, stderr, err := capture(t, func() error { return cmdUsage(context.Background(), c, args) })
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if strings.TrimSpace(stderr) != tc.want {
				t.Errorf("%s: stderr %q, ждали %q", tc.name, stderr, tc.want)
			}
			var got, want any
			if json.Unmarshal([]byte(stdout), &got) != nil {
				t.Fatalf("%s: stdout не JSON: %q", tc.name, stdout)
			}
			_ = json.Unmarshal([]byte(tc.reply), &want)
			if !equalJSON(got, want) {
				t.Errorf("%s: stdout изменился: %s", tc.name, stdout)
			}
			_, hasCost := rec.body["cost_usd"]
			if hasCost != (len(args) == 9) {
				t.Errorf("%s: тело запроса %v", tc.name, rec.body)
			}
		}
	}
}
