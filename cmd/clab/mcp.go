package main

// Подключение локального агента к MCP-серверу проекта тем же токеном, что у
// clab. «install» зовёт claude mcp add, «config» печатает JSON для любого
// другого агента, понимающего MCP. С --instance — инстансовый адрес /mcp
// (CLAB-483): один сервер на все проекты участия, проект не определяется.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func cmdMCP(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	var projectFlag, name, scope, agent string
	var instance bool
	_, err := flags("mcp", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.BoolVar(&instance, "instance", false, "инстансовый адрес <хост>/mcp: один сервер на все проекты, где вы участник; проект не определяется, с --project несовместим")
		fs.StringVar(&name, "name", "", "имя MCP-сервера у агента (по умолчанию sdlc; у codex sdlc-<ключ проекта>; с --instance — "+instanceMCPName+"); промпт work_on_task будет /mcp__<имя>__work_on_task")
		fs.StringVar(&scope, "scope", "", "у claude mcp add: local (этот каталог, по умолчанию), user (везде), project (.mcp.json в репозитории); у codex: user (по умолчанию, ~/.codex/config.toml) или project (.codex/config.toml корня проекта)")
		fs.StringVar(&agent, "agent", "auto", "auto — каждый найденный в PATH агент (claude, codex); claude, codex — только он; generic — напечатать конфигурацию MCP для любого другого агента")
	})
	if err != nil {
		return err
	}
	switch sub {
	case "install", "config", "url", "":
	default:
		return fmt.Errorf("clab mcp install|config|url")
	}
	switch scope {
	case "", "local", "user", "project":
	default:
		return fmt.Errorf("--scope: local, user или project")
	}
	if sub == "install" {
		if agent, err = normalizeAgent(agent); err != nil {
			return err
		}
	}
	if instance && projectFlag != "" {
		return usageError{"--instance и --project несовместимы: инстансовый адрес не привязан к проекту"}
	}
	var key string
	if !instance {
		if key, err = c.project(ctx, projectFlag); err != nil {
			return err
		}
	} else if err := c.requireHost(); err != nil {
		return err
	}
	mcpURL, headerName, headerValue := c.mcpEndpoint(key)
	cfgName := name
	if cfgName == "" {
		cfgName = "sdlc"
		if instance {
			cfgName = instanceMCPName
		}
	}
	config := mcpConfig(cfgName, mcpURL, headerName, headerValue)

	switch sub {
	case "url":
		fmt.Println(mcpURL)
		return nil
	case "config", "":
		return print(config)
	}
	if instance && name == "" {
		name = instanceMCPName
	}
	return installMCP(ctx, c, agent, key, name, scope, mcpURL, headerName, headerValue)
}

// mcpInstalled — итог подключения одного агента командой mcp install.
type mcpInstalled struct {
	Agent    string         `json:"agent"`
	Name     string         `json:"name"`
	Scope    string         `json:"scope,omitempty"`
	MCP      string         `json:"mcp"`
	Previous string         `json:"previous_url,omitempty"`
	Config   map[string]any `json:"config,omitempty"`
}

// installMCP подключает MCP-сервер проекта к выбранным агентам. Подключение
// Codex — та же функция, что у setup; у Claude прежний claude mcp add (без
// проверки существующего сервера), чтобы вызовы и вывод одного claude
// остались прежними.
func installMCP(ctx context.Context, c *Client, agent, key, nameFlag, scopeFlag, mcpURL, headerName, headerValue string) error {
	selected := selectAgents(agent)
	if agent == "auto" && selected[0].name == agentGeneric {
		// Не нашлось ни одного агента: для команды подключения это ошибка,
		// как до выбора агента по привязкам.
		raw, _ := json.MarshalIndent(genericConfig(c, agentMCPName(agentClaude, scopeFlag, nameFlag, key), mcpURL, headerName, headerValue), "", "  ")
		return fmt.Errorf("claude не найден в PATH. Конфигурация для агента:\n%s", raw)
	}
	if scopeFlag == "project" && c.token != "" && hasAgent(selected, agentClaude) {
		return errProjectScopeToken
	}
	var done []mcpInstalled
	var errs []error
	for _, sel := range selected {
		res := mcpInstalled{
			Agent: sel.name, Scope: agentScope(sel.name, scopeFlag),
			Name: agentMCPName(sel.name, scopeFlag, nameFlag, key),
		}
		var err error
		switch sel.name {
		case agentClaude:
			err = installClaudeMCP(ctx, c, sel.path, &res, mcpURL, headerName, headerValue)
		case agentCodex:
			switch {
			case res.Scope == "project":
				var root string
				if root, err = projectRoot(ctx); err == nil {
					res.MCP, res.Previous, err = codexProjectMCP(root, res.Name, mcpURL, false)
				}
			case sel.path == "":
				err = fmt.Errorf("codex не найден в PATH: MCP не подключён. Подключить вручную:\n  %s", codexAddCommand(res.Name, mcpURL))
			default:
				res.MCP, res.Previous, err = connectCodexMCP(ctx, sel.path, res.Name, mcpURL, c.token)
			}
		case agentGeneric:
			res.MCP = mcpSkipped
			res.Config = genericConfig(c, res.Name, mcpURL, headerName, headerValue)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		done = append(done, res)
	}
	if len(done) > 0 {
		// Инстансовый адрес (пустой ключ) — признак instance вместо проекта.
		target := map[string]any{"project": key}
		if key == "" {
			target = map[string]any{"instance": true}
		}
		out := map[string]any{"url": mcpURL, "agents": done}
		if len(done) == 1 && done[0].Agent == agentClaude {
			out = map[string]any{"name": done[0].Name, "url": mcpURL, "scope": done[0].Scope}
		}
		for k, v := range target {
			out[k] = v
		}
		if err := emit(out, func() error {
			for _, r := range done {
				printMCPInstalled(r, key)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

// installClaudeMCP — прежний claude mcp add: вывод агента идёт на терминал.
func installClaudeMCP(ctx context.Context, c *Client, claude string, res *mcpInstalled, mcpURL, headerName, headerValue string) error {
	if claude == "" {
		raw, _ := json.MarshalIndent(genericConfig(c, res.Name, mcpURL, headerName, headerValue), "", "  ")
		return fmt.Errorf("claude не найден в PATH. Конфигурация для агента:\n%s", raw)
	}
	argv := []string{"mcp", "add", "--transport", "http", "--scope", res.Scope, res.Name, mcpURL,
		"--header", headerName + ": " + headerValue}
	cmd := exec.CommandContext(ctx, claude, argv...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude mcp add: %w", err)
	}
	res.MCP = mcpAdded
	return nil
}

func printMCPInstalled(r mcpInstalled, key string) {
	target := "к проекту " + key
	if key == "" {
		target = "к инстансу (все проекты, где вы участник)"
	}
	switch r.Agent {
	case agentClaude:
		fmt.Printf("MCP-сервер %s подключён %s (%s).\n", r.Name, target, r.Scope)
		fmt.Printf("В агенте: /mcp__%s__work_on_task <ключ задачи>\n", strings.ReplaceAll(r.Name, "-", "_"))
	case agentCodex:
		state := map[string]string{mcpAdded: "подключён", mcpUnchanged: "уже подключён", mcpReplaced: "заменён (был " + r.Previous + ")",
			mcpManual: "не изменён: запись с другим адресом (" + r.Previous + ") — поправьте вручную"}[r.MCP]
		fmt.Printf("MCP-сервер %s %s %s в Codex (%s).\n", r.Name, state, target, r.Scope)
		fmt.Printf("Токен Codex читает из переменной %s: export %s=\"$(clab auth token)\". В Codex: $clab-work <ключ задачи> (скилы — clab setup).\n", codexTokenEnv, codexTokenEnv)
	default:
		raw, _ := json.MarshalIndent(r.Config, "", "  ")
		what := "проекта " + key
		if key == "" {
			what = "инстанса"
		}
		fmt.Printf("Конфигурация MCP %s для вашего агента (токен — в переменной окружения %s):\n%s\n", what, codexTokenEnv, raw)
		fmt.Println("инструкции: clab guide")
	}
}

// errProjectScopeToken — отказ --scope project при входе по токену: .mcp.json
// обычно коммитят, а токен — личный. Один текст у clab mcp install и clab
// setup.
var errProjectScopeToken = errors.New("--scope project положил бы токен в .mcp.json репозитория; используйте local или user, либо clab mcp config и ${SDLC_TOKEN} в заголовке")

// instanceMCPName — имя инстансового сервера у агента по умолчанию: не sdlc,
// чтобы подключение инстансового адреса не заменило проектное.
const instanceMCPName = "cadence"

// mcpEndpoint — адрес MCP-сервера проекта и заголовок, которым агент
// предъявляет себя: токен clab, а без него — актор заголовка. Пустой ключ —
// инстансовый адрес /mcp.
func (c *Client) mcpEndpoint(key string) (mcpURL, headerName, headerValue string) {
	mcpURL = c.origin + "/mcp/projects/" + key
	if key == "" {
		mcpURL = c.origin + "/mcp"
	}
	headerName, headerValue = "X-SDLC-Actor", c.actor
	if c.token != "" {
		headerName, headerValue = "Authorization", "Bearer "+c.token
	}
	return mcpURL, headerName, headerValue
}

// mcpConfig — конфигурация MCP-сервера в формате mcpServers, понятном
// большинству агентов.
func mcpConfig(name, mcpURL, headerName, headerValue string) map[string]any {
	server := map[string]any{
		"type":    "http",
		"url":     mcpURL,
		"headers": map[string]string{headerName: headerValue},
	}
	return map[string]any{"mcpServers": map[string]any{name: server}}
}
