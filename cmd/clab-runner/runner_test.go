package main

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// Разбор вывода агента: неполнота допустима, выдумывание — нет.
func TestParseUsage(t *testing.T) {
	report := parseUsage([]byte(`{
		"total_cost_usd": 0.42, "duration_ms": 61000, "model": "claude-sonnet-5",
		"usage": {"input_tokens": 1200, "output_tokens": 800, "cache_read_input_tokens": 40}
	}`))
	if report == nil {
		t.Fatal("вывод агента не разобран")
	}
	if report["model"] != "claude-sonnet-5" || report["cost_usd"] != 0.42 {
		t.Fatalf("отчёт: %+v", report)
	}
	if report["input_tokens"] != int64(1200) || report["cache_read_tokens"] != int64(40) {
		t.Fatalf("токены: %+v", report)
	}
	if _, ok := report["cache_write_tokens"]; ok {
		t.Fatal("несообщённое поле не должно появляться в отчёте")
	}

	// Модель берётся из разреза по моделям, если отдельного поля нет.
	report = parseUsage([]byte(`{"total_cost_usd": 1, "modelUsage": {"claude-haiku-4-5": {"costUSD": 1}}}`))
	if report == nil || report["model"] != "claude-haiku-4-5" {
		t.Fatalf("модель из modelUsage: %+v", report)
	}

	// Чужой агент отдаёт что-то своё: это «неизвестно», а не ноль.
	if parseUsage([]byte("готово")) != nil {
		t.Fatal("неразобранный вывод не должен превращаться в отчёт")
	}
	if parseUsage([]byte(`{"total_cost_usd": 0.1}`)) != nil {
		t.Fatal("отчёт без модели записывать не с чем сравнивать")
	}
}

// Сборка команды: подстановки из версии роли, пустые — вместе со своим флагом.
func TestArgvSubstitutions(t *testing.T) {
	r := newRunner(Config{
		Project: "SDLC",
		Command: defaultCommand,
	}, slog.Default())

	item := queueItem{RoleKey: "developer"}
	item.Task.Key = "SDLC-42"
	model := "claude-sonnet-5"
	claim := claimResult{}
	claim.Role = &struct {
		Key        string  `json:"key"`
		ModelHint  *string `json:"model_hint"`
		ToolPolicy struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"tool_policy"`
	}{Key: "developer", ModelHint: &model}
	claim.Role.ToolPolicy.Allow = []string{"Read", "Grep"}

	argv := strings.Join(r.argv(item, claim, "/tmp/mcp.json", ""), " ")
	if !strings.Contains(argv, "--model claude-sonnet-5") {
		t.Fatalf("модель роли не подставлена: %s", argv)
	}
	if !strings.Contains(argv, "--allowedTools Read,Grep") {
		t.Fatalf("инструменты роли не подставлены: %s", argv)
	}
	if !strings.Contains(argv, "/mcp__sdlc__work_on_task") {
		t.Fatalf("раннер обязан стартовать тем же промптом, что человек: %s", argv)
	}
	// CLAB-214, AC-1: промпт несёт роль и признак режима раннера — агент не
	// отчитывается о стоимости, это делает раннер.
	if got := r.argv(item, claim, "/tmp/mcp.json", ""); !slices.Contains(got, "/mcp__sdlc__work_on_task SDLC-42 developer runner") {
		t.Fatalf("промпт без роли или признака режима раннера: %q", got)
	}
	// Без роли признак не передать: аргументы позиционные.
	if got := prompt("SDLC-42", ""); got != "/mcp__sdlc__work_on_task SDLC-42" {
		t.Fatalf("промпт без роли: %q", got)
	}

	// Роль без подсказки модели — законный случай: флаг уходит целиком.
	claim.Role.ModelHint = nil
	argv = strings.Join(r.argv(item, claim, "/tmp/mcp.json", ""), " ")
	if strings.Contains(argv, "--model") {
		t.Fatalf("пустая модель обязана убрать флаг: %s", argv)
	}

	// Каталог копии доступен агенту, которому его надо передать флагом.
	r.cfg.Command = "agent --dir {workdir} {prompt}"
	argv = strings.Join(r.argv(item, claim, "/tmp/mcp.json", "/w/SDLC-42/backend"), " ")
	if !strings.Contains(argv, "--dir /w/SDLC-42/backend") {
		t.Fatalf("каталог копии не подставлен: %s", argv)
	}
}

func TestMCPURLFollowsAPI(t *testing.T) {
	cfg := Config{APIURL: "https://sdlc.example.com/v1", Project: "SDLC"}
	if got := cfg.mcpURL(); got != "https://sdlc.example.com/mcp/projects/SDLC" {
		t.Fatalf("адрес MCP: %s", got)
	}
}
