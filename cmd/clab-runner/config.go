package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	// Что агенту передать из окружения сверх базового списка: имена и
	// префиксы вида AWS_* из SDLC_RUNNER_ENV. Имена SDLC_* и CLAB_* сюда не
	// попадают — они в AgentEnvRefused, о них раннер предупреждает при старте.
	AgentEnv        []string
	AgentEnvRefused []string
	// Таблица «уровень → модель» из SDLC_RUNNER_MODEL_LEVELS (решение 0060):
	// сервер рекомендует уровень, а какая модель ему отвечает под подписку
	// владельца раннера — знает только раннер. nil — таблицы нет.
	ModelLevels map[string]string
}

// modelLevels — уровни из решения 0060; других ключей таблица не знает.
var modelLevels = []string{"low", "medium", "high"}

// Команда по умолчанию — Claude Code: агент получает промпт work_on_task с
// того же MCP-сервера, к которому подключён, поэтому человек и раннер
// стартуют одинаково (решение 0025). --strict-mcp-config оставляет агенту
// только сервер из mcp.json раннера: серверы из настроек Claude Code
// сотрудника — с его личным токеном (clab mcp install) — не подключаются
// (CLAB-213). За свою SDLC_RUNNER_COMMAND отвечает администратор.
const defaultCommand = `claude -p --output-format json --mcp-config {mcp_config} --strict-mcp-config ` +
	`--model {model} --allowedTools {allowed_tools} {prompt}`

func loadConfig() (Config, error) {
	c := Config{
		APIURL:   strings.TrimRight(env("SDLC_API_URL", ""), "/"),
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
	// Адреса по умолчанию нет, как и у clab (CLAB-147): подставленный адрес
	// dev-стека у клиента self-hosted оказался бы стеком разработчика, и
	// раннер молча забирал бы задачи чужой базы. Dev-стек — только явно.
	if c.APIURL == "" {
		return c, fmt.Errorf("не задан SDLC_API_URL: адрес API инстанса, например https://cadence.example.com/v1")
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
	c.AgentEnv, c.AgentEnvRefused = parseAgentEnv(env("SDLC_RUNNER_ENV", ""))
	levels, err := parseModelLevels(env("SDLC_RUNNER_MODEL_LEVELS", ""))
	if err != nil {
		return c, err
	}
	c.ModelLevels = levels
	if err := c.loadWorkspaces(); err != nil {
		return c, err
	}
	if c.Interval, err = envDuration("SDLC_RUNNER_INTERVAL", c.Interval); err != nil {
		return c, err
	}
	if c.Timeout, err = envDuration("SDLC_RUNNER_TIMEOUT", c.Timeout); err != nil {
		return c, err
	}
	return c, nil
}

// parseModelLevels разбирает SDLC_RUNNER_MODEL_LEVELS вида
// low=haiku,medium=sonnet,high=opus. Любую запись можно опустить; пустая
// переменная — таблицы нет. Опечатка — отказ старта, а не молчаливый запуск
// без модели: уровень без записи раннер обошёл бы, и никто бы не заметил.
func parseModelLevels(raw string) (map[string]string, error) {
	var table map[string]string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		level, model, ok := strings.Cut(item, "=")
		level, model = strings.TrimSpace(level), strings.TrimSpace(model)
		switch {
		case !ok:
			return nil, fmt.Errorf("SDLC_RUNNER_MODEL_LEVELS: %q не по форме <уровень>=<модель>, "+
				"например low=haiku,medium=sonnet,high=opus", item)
		case !slices.Contains(modelLevels, level):
			return nil, fmt.Errorf("SDLC_RUNNER_MODEL_LEVELS: %q — уровень %q не поддерживается, допустимы %s",
				item, level, strings.Join(modelLevels, ", "))
		case model == "":
			return nil, fmt.Errorf("SDLC_RUNNER_MODEL_LEVELS: %q — не указана модель", item)
		case strings.ContainsFunc(model, unicode.IsSpace):
			return nil, fmt.Errorf("SDLC_RUNNER_MODEL_LEVELS: %q — пробел внутри имени модели", item)
		}
		if _, dup := table[level]; dup {
			return nil, fmt.Errorf("SDLC_RUNNER_MODEL_LEVELS: %q — уровень %s указан дважды", item, level)
		}
		if table == nil {
			table = map[string]string{}
		}
		table[level] = model
	}
	return table, nil
}

// modelLevelsString — таблица для журнала в порядке уровней.
func modelLevelsString(table map[string]string) string {
	var parts []string
	for _, level := range modelLevels {
		if model, ok := table[level]; ok {
			parts = append(parts, level+"="+model)
		}
	}
	return strings.Join(parts, ",")
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
