package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// CLAB-483, AC-11: clab mcp url|config|install --instance — инстансовый адрес
// <origin>/mcp, проект не определяется, имя сервера по умолчанию cadence.
func TestMCPInstance(t *testing.T) {
	withJSON(t, false)
	setupEnv(t)
	t.Setenv("SDLC_PROJECT", "")
	c := (&setupServer{kit: agentKitFx("v1.0.0", "1", nil)}).start(t)
	run := func(args ...string) (string, error) {
		out, _, err := capture(t, func() error { return cmdMCP(context.Background(), c, args) })
		return out, err
	}

	// Каталог без git remote и без --project: команды работают.
	out, err := run("url", "--instance")
	if err != nil || strings.TrimSpace(out) != c.origin+"/mcp" {
		t.Fatalf("url: %q %v", out, err)
	}
	out, err = run("config", "--instance")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(out), &cfg) != nil || cfg.MCPServers["cadence"].URL != c.origin+"/mcp" ||
		cfg.MCPServers["cadence"].Headers["Authorization"] != "Bearer "+setupToken {
		t.Fatalf("config: %s", out)
	}

	out, err = run("install", "--instance", "--agent", "generic")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, out, c.origin+"/mcp", `"cadence"`, "${SDLC_TOKEN}", "Конфигурация MCP инстанса")

	cl := installClaude(t, "")
	out, err = run("install", "--instance", "--agent", "claude")
	if err != nil {
		t.Fatal(err)
	}
	want := "mcp add --transport http --scope local cadence " + c.origin + "/mcp --header Authorization: Bearer " + setupToken
	if got := cl.calls(t); len(got) != 1 || got[0] != want {
		t.Fatalf("claude: %v", got)
	}
	mustContain(t, out, "MCP-сервер cadence подключён к инстансу", "/mcp__cadence__work_on_task")

	// --json: признак инстансового адреса вместо ключа проекта.
	withJSON(t, true)
	out, err = run("install", "--instance", "--agent", "claude")
	withJSON(t, false)
	var rep map[string]any
	if err != nil || json.Unmarshal([]byte(out), &rep) != nil || rep["instance"] != true || rep["name"] != "cadence" ||
		rep["url"] != c.origin+"/mcp" {
		t.Fatalf("--json: %s %v", out, err)
	}
	if _, has := rep["project"]; has {
		t.Fatalf("--json с ключом проекта: %v", rep)
	}

	// --instance с --project — ошибка использования (код 2), ничего не вызвано.
	_ = os.Remove(cl.log)
	for _, sub := range []string{"url", "config", "install"} {
		out, err = run(sub, "--instance", "--project", "ACME")
		var u usageError
		if !errors.As(err, &u) || out != "" {
			t.Fatalf("%s --instance --project: %q %v", sub, out, err)
		}
	}
	if got := cl.calls(t); len(got) != 0 {
		t.Fatalf("claude позван: %v", got)
	}

	// Прежние формы не меняются: адрес проекта и имя sdlc.
	out, err = run("url", "--project", "ACME")
	if err != nil || strings.TrimSpace(out) != c.origin+"/mcp/projects/ACME" {
		t.Fatalf("url проекта: %q %v", out, err)
	}
}
