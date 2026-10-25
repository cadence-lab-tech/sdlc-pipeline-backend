package main

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"strings"
	"testing"
)

// CLAB-260: таблица уровней раннера и выбор модели запуска (решение 0060).

// AC-9: разбор SDLC_RUNNER_MODEL_LEVELS при загрузке конфига.
func TestLoadConfigModelLevels(t *testing.T) {
	t.Setenv("SDLC_API_URL", "https://c.example.com/v1")
	t.Setenv("SDLC_PROJECT", "CLAB")
	ok := map[string]map[string]string{
		"low=haiku,medium=sonnet,high=opus": {"low": "haiku", "medium": "sonnet", "high": "opus"},
		" high=opus , ":                     {"high": "opus"},
		"medium=sonnet":                     {"medium": "sonnet"},
		"":                                  nil,
		" , ,":                              nil,
	}
	for raw, want := range ok {
		t.Setenv("SDLC_RUNNER_MODEL_LEVELS", raw)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if !maps.Equal(cfg.ModelLevels, want) || (want == nil) != (cfg.ModelLevels == nil) {
			t.Fatalf("%q: таблица %v, ждали %v", raw, cfg.ModelLevels, want)
		}
	}
	bad := map[string]string{
		"hihg=opus":        "hihg=opus",
		"low":              "low",
		"low=":             "low=",
		"low=a b":          "low=a b",
		"low=a,low=b":      "low=b",
		"medium=sonnet,xl": "xl",
	}
	for raw, item := range bad {
		t.Setenv("SDLC_RUNNER_MODEL_LEVELS", raw)
		_, err := loadConfig()
		if err == nil || !strings.Contains(err.Error(), "SDLC_RUNNER_MODEL_LEVELS") ||
			!strings.Contains(err.Error(), `"`+item+`"`) {
			t.Fatalf("%q: ошибка %v, ждали имя переменной и элемент %q", raw, err, item)
		}
	}
	t.Setenv("SDLC_RUNNER_MODEL_LEVELS", "hihg=opus")
	if _, err := loadConfig(); !strings.Contains(err.Error(), "low, medium, high") {
		t.Fatalf("в ошибке нет допустимых уровней: %v", err)
	}
	if got := modelLevelsString(map[string]string{"high": "opus", "low": "haiku"}); got != "low=haiku,high=opus" {
		t.Fatalf("таблица в журнале: %s", got)
	}
}

func levelClaim(level *string, source string, hint string) claimResult {
	c := claimResult{}
	c.Role = &struct {
		Key        string  `json:"key"`
		ModelHint  *string `json:"model_hint"`
		ToolPolicy struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"tool_policy"`
	}{Key: "developer"}
	if hint != "" {
		c.Role.ModelHint = &hint
	}
	if source != "" {
		c.Execution = &claimExecution{ModelLevel: level, ModelLevelSource: source}
	}
	return c
}

// AC-10, AC-11: выбор модели и argv по таблице, model_hint и серверу без
// execution.
func TestChooseModel(t *testing.T) {
	high, low := "high", "low"
	table := map[string]string{"high": "opus"}
	cases := []struct {
		name  string
		table map[string]string
		claim claimResult
		want  modelChoice
		flag  string // "" — флага --model в argv нет
	}{
		{"таблица", table, levelClaim(&high, "stage", "sonnet"),
			modelChoice{level: "high", source: "stage", model: "opus", from: modelFromTable}, "--model opus"},
		{"уровня нет в таблице", table, levelClaim(&low, "task", "sonnet"),
			modelChoice{level: "low", source: "task", model: "sonnet", from: modelFromHint, hintDeprecated: true}, "--model sonnet"},
		{"none без model_hint", table, levelClaim(nil, "none", ""),
			modelChoice{level: "none", source: "none", from: modelFromNone}, ""},
		{"none с model_hint", table, levelClaim(nil, "none", "sonnet"),
			modelChoice{level: "none", source: "none", model: "sonnet", from: modelFromHint, hintDeprecated: true}, "--model sonnet"},
		{"без таблицы", nil, levelClaim(&high, "role", ""),
			modelChoice{level: "high", source: "role", from: modelFromNone}, ""},
		// Сервер прежней версии: model_hint без предупреждения, таблица не при чём.
		{"старый сервер", table, levelClaim(nil, "", "opus"),
			modelChoice{level: "none", source: "absent", model: "opus", from: modelFromHint}, "--model opus"},
		{"старый сервер без model_hint", table, levelClaim(nil, "", ""),
			modelChoice{level: "none", source: "absent", from: modelFromNone}, ""},
	}
	item := queueItem{RoleKey: "developer"}
	item.Task.Key = "CLAB-1"
	for _, tc := range cases {
		r := newRunner(Config{Project: "CLAB", Command: defaultCommand, ModelLevels: tc.table}, slog.Default())
		if got := r.chooseModel(tc.claim); got != tc.want {
			t.Errorf("%s: %+v, ждали %+v", tc.name, got, tc.want)
		}
		argv := strings.Join(r.argv(item, tc.claim, "/tmp/mcp.json", ""), " ")
		if tc.flag == "" && strings.Contains(argv, "--model") || tc.flag != "" && !strings.Contains(argv, tc.flag+" ") {
			t.Errorf("%s: argv %s, ждали %q", tc.name, argv, tc.flag)
		}
		// Своя команда получает в {model} то же значение.
		r.cfg.Command = "agent --use {model} {prompt}"
		custom := strings.Join(r.argv(item, tc.claim, "/tmp/mcp.json", ""), " ")
		if tc.want.model != "" && !strings.Contains(custom, "--use "+tc.want.model+" ") ||
			tc.want.model == "" && strings.Contains(custom, "--use") {
			t.Errorf("%s: своя команда %s", tc.name, custom)
		}
	}

	// Против сервера без execution argv тот же, что у раннера прежней
	// версии: с таблицей и без неё. Признак режима раннера в промпте
	// (CLAB-214) раннер ставит всегда: сервер прежней версии лишний
	// аргумент промпта пропускает.
	old := levelClaim(nil, "", "opus")
	withTable := newRunner(Config{Project: "CLAB", Command: defaultCommand, ModelLevels: table}, slog.Default())
	without := newRunner(Config{Project: "CLAB", Command: defaultCommand}, slog.Default())
	a := strings.Join(withTable.argv(item, old, "/tmp/mcp.json", ""), " ")
	b := strings.Join(without.argv(item, old, "/tmp/mcp.json", ""), " ")
	want := "claude -p --output-format json --mcp-config /tmp/mcp.json --strict-mcp-config --model opus " +
		"/mcp__sdlc__work_on_task CLAB-1 developer runner"
	if a != b || a != want {
		t.Fatalf("argv против старого сервера:\n%s\n%s\nждали\n%s", a, b, want)
	}
}

func claimWithLevel(level any, source, hint string) map[string]any {
	role := map[string]any{"key": "developer"}
	if hint != "" {
		role["model_hint"] = hint
	}
	reply := map[string]any{"stage_run_id": "run-1", "role": role}
	if source != "" {
		reply["execution"] = map[string]any{"model_level": level, "model_level_source": source}
	}
	return reply
}

// AC-12: сухой прогон и журнал запуска несут уровень, источник, модель и
// откуда она взята.
func TestDryRunAndLaunchLogModelLevel(t *testing.T) {
	fields := []string{"model_level=high", "model_level_source=stage", "model=opus", "model_from=table"}

	api := newFakeAPI(t)
	api.claim = claimWithLevel("high", "stage", "")
	command, _ := stubAgent(t, agentOK)
	var logs bytes.Buffer
	r := testRunner(t, api, command, &logs)
	r.cfg.ModelLevels = map[string]string{"high": "opus"}
	r.cfg.DryRun = true
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	dry := lineWith(logs.String(), "сухой прогон")
	for _, f := range fields {
		if !strings.Contains(dry, f) {
			t.Errorf("в записи сухого прогона нет %s: %s", f, dry)
		}
	}

	api = newFakeAPI(t)
	api.claim = claimWithLevel("high", "stage", "")
	command, _ = stubAgent(t, agentOK)
	logs.Reset()
	r = testRunner(t, api, command, &logs)
	r.cfg.ModelLevels = map[string]string{"high": "opus"}
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	launch := lineWith(logs.String(), "запуск агента")
	for _, f := range fields {
		if !strings.Contains(launch, f) {
			t.Errorf("в записи запуска нет %s: %s", f, launch)
		}
	}
	if strings.Contains(logs.String(), "model_hint устарел") {
		t.Errorf("модель из таблицы, а предупреждение есть:\n%s", logs.String())
	}

	// Уровня нет в таблице — model_hint и предупреждение с уровнем и источником.
	api = newFakeAPI(t)
	api.claim = claimWithLevel("low", "task", "sonnet")
	command, _ = stubAgent(t, agentOK)
	logs.Reset()
	r = testRunner(t, api, command+" --model {model}", &logs)
	r.cfg.ModelLevels = map[string]string{"high": "opus"}
	r.cfg.DryRun = true
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	warn := lineWith(logs.String(), "model_hint устарел")
	for _, f := range []string{"level=WARN", "SDLC_RUNNER_MODEL_LEVELS", "model_level=low", "model_level_source=task", "model=sonnet"} {
		if !strings.Contains(warn, f) {
			t.Errorf("в предупреждении нет %s: %s", f, warn)
		}
	}
	if dry := lineWith(logs.String(), "сухой прогон"); !strings.Contains(dry, "model_from=model_hint") ||
		!strings.Contains(dry, "--model sonnet") {
		t.Errorf("сухой прогон с model_hint: %s", dry)
	}

	// Сервер без execution — model_hint без предупреждения.
	api = newFakeAPI(t)
	api.claim = claimWithLevel(nil, "", "opus")
	command, _ = stubAgent(t, agentOK)
	logs.Reset()
	r = testRunner(t, api, command+" --model {model}", &logs)
	r.cfg.ModelLevels = map[string]string{"high": "haiku"}
	r.cfg.DryRun = true
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "model_hint устарел") {
		t.Errorf("сервер без execution, а предупреждение есть:\n%s", logs.String())
	}
	if dry := lineWith(logs.String(), "сухой прогон"); !strings.Contains(dry, "--model opus") ||
		!strings.Contains(dry, "model_level_source=absent") {
		t.Errorf("сухой прогон против старого сервера: %s", dry)
	}
}

func lineWith(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// Модель отчёта о стоимости из разреза по моделям выбирается одинаково при
// любом порядке обхода map: наибольшая стоимость, затем выходные токены,
// затем имя. Договорённость с CLAB-264 о stage_run_usage.model.
func TestParseUsagePicksMainModel(t *testing.T) {
	cases := map[string]string{
		`{"modelUsage": {"claude-haiku-4-5": {"costUSD": 0.1, "outputTokens": 900},
			"claude-opus-5-5": {"costUSD": 2.5, "outputTokens": 100}}}`: "claude-opus-5-5",
		`{"modelUsage": {"b-model": {"costUSD": 1, "outputTokens": 10},
			"a-model": {"costUSD": 1, "outputTokens": 20}}}`: "a-model",
		`{"modelUsage": {"b-model": {"costUSD": 1, "outputTokens": 30},
			"a-model": {"costUSD": 1, "outputTokens": 20}}}`: "b-model",
		`{"modelUsage": {"zeta": {"costUSD": 1, "outputTokens": 5},
			"alpha": {"costUSD": 1, "outputTokens": 5}, "mid": {"costUSD": 1, "outputTokens": 5}}}`: "alpha",
		`{"modelUsage": {"reported": {"costUSD": 0}, "silent": {}}}`: "reported",
		// Поле model главнее разреза.
		`{"model": "claude-sonnet-5", "modelUsage": {"claude-opus-5-5": {"costUSD": 9}}}`: "claude-sonnet-5",
	}
	for raw, want := range cases {
		for i := 0; i < 20; i++ {
			report := parseUsage([]byte(raw))
			if report == nil || report["model"] != want {
				t.Fatalf("%s: модель %v, ждали %s", raw, report, want)
			}
		}
	}
}
