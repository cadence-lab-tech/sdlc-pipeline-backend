package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config — настройки раннера. Команда запуска вынесена в переменную
// намеренно: вместо Claude Code может стоять любой агент, понимающий MCP, и
// раннер не должен знать, какой именно.
type Config struct {
	APIURL  string
	Token   string
	Actor   string
	Project string
	Roles   []string
	Slots   int
	// Как часто спрашивать очередь. Захват остаётся pull: раннер сам находит
	// работу, ему никто её не присылает.
	Interval time.Duration
	// Сколько ждать агента, прежде чем считать его мёртвым и отпустить задачу.
	Timeout time.Duration
	// Шаблон команды. Подстановки: {task}, {role}, {model}, {allowed_tools},
	// {disallowed_tools}, {mcp_config}, {prompt}, {workdir}.
	Command string
	// Забрать одну задачу и выйти: так раннер работает в CI.
	Once bool
	// Ничего не запускать, только показывать, что было бы запущено.
	DryRun bool
	// Каталог с клонами репозиториев проекта и каталог рабочих копий задач.
	// С клонами агент работает в git worktree своей задачи; без них — в
	// каталоге раннера, как раньше, и это безопасно только при одном слоте.
	ReposDir     string
	WorktreesDir string
}

// Команда по умолчанию — Claude Code: агент получает промпт work_on_task с
// того же MCP-сервера, к которому подключён, поэтому человек и раннер
// стартуют одинаково (решение 0025).
const defaultCommand = `claude -p --output-format json --mcp-config {mcp_config} ` +
	`--model {model} --allowedTools {allowed_tools} {prompt}`

func loadConfig() (Config, error) {
	c := Config{
		APIURL:   strings.TrimRight(env("SDLC_API_URL", "http://localhost:8088/v1"), "/"),
		Token:    env("SDLC_TOKEN", ""),
		Actor:    env("SDLC_ACTOR", "agent:"+env("USER", "sdlc-runner")),
		Project:  env("SDLC_PROJECT", ""),
		Slots:    1,
		Interval: 15 * time.Second,
		Timeout:  30 * time.Minute,
		Command:  env("SDLC_RUNNER_COMMAND", defaultCommand),
		Once:     env("SDLC_RUNNER_ONCE", "") != "",
		DryRun:   env("SDLC_RUNNER_DRY_RUN", "") != "",
	}
	if c.Project == "" {
		return c, fmt.Errorf("не задан SDLC_PROJECT: раннер следит за очередью одного проекта")
	}

	for _, role := range strings.Split(env("SDLC_RUNNER_ROLES", ""), ",") {
		if role = strings.TrimSpace(role); role != "" {
			c.Roles = append(c.Roles, role)
		}
	}
	if raw := env("SDLC_RUNNER_SLOTS", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("SDLC_RUNNER_SLOTS: нужно положительное число, а не %q", raw)
		}
		c.Slots = n
	}
	if err := c.loadWorkspaces(); err != nil {
		return c, err
	}
	var err error
	if c.Interval, err = envDuration("SDLC_RUNNER_INTERVAL", c.Interval); err != nil {
		return c, err
	}
	if c.Timeout, err = envDuration("SDLC_RUNNER_TIMEOUT", c.Timeout); err != nil {
		return c, err
	}
	return c, nil
}

// loadWorkspaces читает, где лежат клоны и куда класть рабочие копии.
//
// Несколько слотов без клонов — отказ, а не предупреждение: агенты разных
// задач переключали бы ветки друг у друга в одном каталоге. Отличить
// безопасную роль от опасной раннер не может, не зашивая роли в код.
func (c *Config) loadWorkspaces() error {
	c.ReposDir = env("SDLC_RUNNER_REPOS", "")
	c.WorktreesDir = env("SDLC_RUNNER_WORKTREES", "")
	if c.ReposDir == "" {
		if c.WorktreesDir != "" {
			return fmt.Errorf("SDLC_RUNNER_WORKTREES задан без SDLC_RUNNER_REPOS: рабочие копии заводить не из чего")
		}
		if c.Slots > 1 {
			return fmt.Errorf("SDLC_RUNNER_SLOTS=%d без SDLC_RUNNER_REPOS: агенты работали бы в одном каталоге "+
				"и переключали ветки друг у друга; задайте каталог с клонами репозиториев проекта", c.Slots)
		}
		return nil
	}

	repos, err := filepath.Abs(c.ReposDir)
	if err != nil {
		return fmt.Errorf("SDLC_RUNNER_REPOS: %w", err)
	}
	if info, err := os.Stat(repos); err != nil || !info.IsDir() {
		return fmt.Errorf("SDLC_RUNNER_REPOS: %s — не каталог", repos)
	}
	c.ReposDir = repos

	if c.WorktreesDir == "" {
		// Рядом с клонами и скрыто: так же, как .worktrees в рабочей
		// директории Cadence Lab, и поиск клонов его пропускает.
		c.WorktreesDir = filepath.Join(repos, ".worktrees")
	}
	if c.WorktreesDir, err = filepath.Abs(c.WorktreesDir); err != nil {
		return fmt.Errorf("SDLC_RUNNER_WORKTREES: %w", err)
	}
	return nil
}

// mcpURL — адрес MCP-сервера проекта. Он выводится из адреса API: это один
// процесс, и держать два адреса в настройках значило бы дать им разойтись.
func (c Config) mcpURL() string {
	base := strings.TrimSuffix(c.APIURL, "/v1")
	return base + "/mcp/projects/" + c.Project
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
