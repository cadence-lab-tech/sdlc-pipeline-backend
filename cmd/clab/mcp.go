package main

// Подключение локального агента к MCP-серверу проекта тем же токеном, что у
// clab. «install» зовёт claude mcp add, «config» печатает JSON для любого
// другого агента, понимающего MCP.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func cmdMCP(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	var projectFlag, name, scope string
	_, err := flags("mcp", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&name, "name", "sdlc", "имя MCP-сервера у агента; промпт work_on_task будет /mcp__<имя>__work_on_task")
		fs.StringVar(&scope, "scope", "local", "область у claude mcp add: local (этот каталог), user (везде), project (.mcp.json в репозитории)")
	})
	if err != nil {
		return err
	}
	switch sub {
	case "install", "config", "url", "":
	default:
		return fmt.Errorf("clab mcp install|config|url")
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	mcpURL := c.origin + "/mcp/projects/" + key

	headerName, headerValue := "X-SDLC-Actor", c.actor
	if c.token != "" {
		headerName, headerValue = "Authorization", "Bearer "+c.token
	}
	server := map[string]any{
		"type":    "http",
		"url":     mcpURL,
		"headers": map[string]string{headerName: headerValue},
	}
	config := map[string]any{"mcpServers": map[string]any{name: server}}

	switch sub {
	case "url":
		fmt.Println(mcpURL)
		return nil
	case "config", "":
		return print(config)
	}

	// install
	if scope == "project" && c.token != "" {
		// .mcp.json обычно коммитят, а токен — личный.
		return fmt.Errorf("--scope project положил бы токен в .mcp.json репозитория; используйте local или user, либо clab mcp config и ${SDLC_TOKEN} в заголовке")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		raw, _ := json.MarshalIndent(config, "", "  ")
		return fmt.Errorf("claude не найден в PATH. Конфигурация для агента:\n%s", raw)
	}
	argv := []string{"mcp", "add", "--transport", "http", "--scope", scope, name, mcpURL,
		"--header", headerName + ": " + headerValue}
	cmd := exec.CommandContext(ctx, claude, argv...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude mcp add: %w", err)
	}
	return emit(map[string]any{"name": name, "url": mcpURL, "scope": scope, "project": key}, func() error {
		fmt.Printf("MCP-сервер %s подключён к проекту %s (%s).\n", name, key, scope)
		fmt.Printf("В агенте: /mcp__%s__work_on_task <ключ задачи>\n", strings.ReplaceAll(name, "-", "_"))
		return nil
	})
}
