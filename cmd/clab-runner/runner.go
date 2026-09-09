package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type runner struct {
	cfg Config
	log *slog.Logger
	api *client

	mu      sync.Mutex
	running map[string]bool
}

func newRunner(cfg Config, log *slog.Logger) *runner {
	return &runner{cfg: cfg, log: log, api: newClient(cfg), running: map[string]bool{}}
}

// run опрашивает очередь и раздаёт свободные задачи слотам.
//
// Опрос, а не подписка: раннер живёт на чужой машине за NAT, и адреса, по
// которому его можно позвать, у бэкенда нет. Зато очередь — это состояние в
// базе, и потерять её нельзя.
func (r *runner) run(ctx context.Context) error {
	slots := make(chan struct{}, r.cfg.Slots)
	var wg sync.WaitGroup

	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	for {
		took := r.dispatch(ctx, slots, &wg)
		if r.cfg.Once && took > 0 {
			wg.Wait()
			return nil
		}

		select {
		case <-ctx.Done():
			// Запущенным агентам дают доработать: аренда всё равно за ними,
			// и обрывать их на середине этапа бессмысленно.
			r.log.Info("остановка: ждём запущенных агентов")
			wg.Wait()
			return nil
		case <-ticker.C:
		}
	}
}

// dispatch забирает столько задач, сколько свободно слотов.
func (r *runner) dispatch(ctx context.Context, slots chan struct{}, wg *sync.WaitGroup) int {
	items, err := r.fetch(ctx)
	if err != nil {
		r.log.Warn("очередь недоступна", "error", err)
		return 0
	}

	took := 0
	for _, item := range items {
		if !item.Claimable || r.busy(item.Task.Key) {
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			return took // слоты кончились, остальное подождёт следующего опроса
		}

		// Задачу забирает раннер, а не запущенный им агент: иначе два слота
		// увидели бы одну свободную задачу и подняли бы двух агентов на неё.
		// Агент ходит тем же токеном, поэтому его собственный захват — это
		// подтверждение уже своей аренды, а не спор за неё.
		claim, err := r.api.claim(ctx, item.Task.Key, "runner")
		if err != nil {
			<-slots
			var apiErr *apiError
			if errors.As(err, &apiErr) && (apiErr.Code == "lease_held_by_other" ||
				apiErr.Code == "task_not_claimable" || apiErr.Code == "forbidden") {
				continue // кто-то оказался быстрее или задача не наша
			}
			r.log.Warn("захват не удался", "task", item.Task.Key, "error", err)
			continue
		}

		r.mark(item.Task.Key, true)
		took++
		wg.Add(1)
		go func(item queueItem, claim claimResult) {
			defer wg.Done()
			defer func() { r.mark(item.Task.Key, false); <-slots }()
			r.work(ctx, item, claim)
		}(item, claim)
	}
	return took
}

// fetch собирает очередь по ролям токена. Пустой список ролей означает «всё,
// что даёт токен»: ограничение живёт в токене, а не в настройках раннера.
func (r *runner) fetch(ctx context.Context) ([]queueItem, error) {
	if len(r.cfg.Roles) == 0 {
		return r.api.queue(ctx, r.cfg.Project, "")
	}
	var out []queueItem
	for _, role := range r.cfg.Roles {
		items, err := r.api.queue(ctx, r.cfg.Project, role)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

func (r *runner) busy(task string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[task]
}

func (r *runner) mark(task string, busy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if busy {
		r.running[task] = true
	} else {
		delete(r.running, task)
	}
}

// work поднимает агента на задачу, держит аренду живой, пока он работает, и
// отчитывается о стоимости по его выводу.
func (r *runner) work(ctx context.Context, item queueItem, claim claimResult) {
	log := r.log.With("task", item.Task.Key, "role", claim.roleKey(item.RoleKey))

	config, cleanup, err := r.mcpConfig()
	if err != nil {
		log.Error("не собрать конфигурацию MCP", "error", err)
		r.releaseQuietly(ctx, item.Task.Key, "раннер не смог подготовить запуск")
		return
	}
	defer cleanup()

	argv := r.argv(item, claim, config)
	if r.cfg.DryRun {
		log.Info("сухой прогон", "argv", strings.Join(argv, " "))
		r.releaseQuietly(ctx, item.Task.Key, "сухой прогон раннера")
		return
	}
	log.Info("запуск агента", "model", claim.model(), "command", argv[0])

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	stop := r.keepLease(runCtx, item.Task.Key, claim)
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	stop()

	if err != nil {
		// Агент умер — аренда протухнет сама, но отпустить задачу сразу
		// честнее: следующий слот возьмёт её, не дожидаясь протухания.
		log.Error("агент завершился с ошибкой", "error", err)
		r.releaseQuietly(ctx, item.Task.Key, "агент завершился с ошибкой")
		return
	}

	if report := parseUsage(out); report != nil {
		if err := r.api.reportUsage(ctx, item.Task.Key, claim.StageRunID, report); err != nil {
			log.Warn("отчёт о стоимости не принят", "error", err)
		} else {
			log.Info("агент отработал", "cost_usd", report["cost_usd"], "model", report["model"])
		}
	} else {
		// Молчание честнее выдуманного нуля: прогон останется «неизвестно».
		log.Info("агент отработал, стоимость неизвестна")
	}
}

// keepLease продлевает аренду, пока агент работает: он может молчать долго,
// а протухшая аренда вернёт задачу в очередь прямо из-под него.
func (r *runner) keepLease(ctx context.Context, task string, claim claimResult) func() {
	interval := 5 * time.Minute
	if claim.Lease != nil && claim.Lease.HeartbeatIntervalSeconds > 0 {
		interval = time.Duration(claim.Lease.HeartbeatIntervalSeconds) * time.Second
	}
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.api.heartbeat(ctx, task); err != nil && ctx.Err() == nil {
					r.log.Warn("аренда не продлена", "task", task, "error", err)
				}
			}
		}
	}()
	return cancel
}

func (r *runner) releaseQuietly(ctx context.Context, task, note string) {
	if err := r.api.release(context.WithoutCancel(ctx), task, note); err != nil {
		r.log.Warn("задача не отпущена", "task", task, "error", err)
	}
}

// argv собирает команду запуска из шаблона. Модель и инструменты приезжают из
// версии роли: «аналитик думает большой моделью, ревьюер читает маленькой» —
// свойство роли, а не задачи и не раннера.
func (r *runner) argv(item queueItem, claim claimResult, mcpConfig string) []string {
	replacer := strings.NewReplacer(
		"{task}", item.Task.Key,
		"{role}", claim.roleKey(item.RoleKey),
		"{model}", claim.model(),
		"{allowed_tools}", strings.Join(claim.allowTools(), ","),
		"{disallowed_tools}", strings.Join(claim.denyTools(), ","),
		"{mcp_config}", mcpConfig,
		"{prompt}", prompt(item.Task.Key, claim.roleKey(item.RoleKey)),
	)

	fields := strings.Fields(r.cfg.Command)
	argv := make([]string, 0, len(fields))
	for _, f := range fields {
		value := replacer.Replace(f)
		// Пустая подстановка убирает флаг целиком: `--model ` без значения
		// сломал бы запуск, а роль без подсказки модели — законный случай.
		if value == "" {
			argv = trimDanglingFlag(argv)
			continue
		}
		argv = append(argv, value)
	}
	return argv
}

// trimDanglingFlag снимает флаг, значение которого оказалось пустым.
func trimDanglingFlag(argv []string) []string {
	if n := len(argv); n > 0 && strings.HasPrefix(argv[n-1], "--") {
		return argv[:n-1]
	}
	return argv
}

// prompt — единая точка старта работы: тот же промпт, которым пользуется
// человек. В Claude Code промпт MCP-сервера доступен как слэш-команда, и
// агент забирает его с того же сервера, к которому подключён.
func prompt(task, role string) string {
	if role == "" {
		return "/mcp__sdlc__work_on_task " + task
	}
	return "/mcp__sdlc__work_on_task " + task + " " + role
}

// mcpConfig пишет временную конфигурацию MCP для запускаемого агента: тот же
// проект, тот же токен. Файл живёт ровно столько, сколько работает агент.
func (r *runner) mcpConfig() (string, func(), error) {
	body := map[string]any{
		"mcpServers": map[string]any{
			"sdlc": map[string]any{
				"type": "http",
				"url":  r.cfg.mcpURL(),
			},
		},
	}
	server := body["mcpServers"].(map[string]any)["sdlc"].(map[string]any)
	if r.cfg.Token != "" {
		server["headers"] = map[string]string{"Authorization": "Bearer " + r.cfg.Token}
	} else {
		server["headers"] = map[string]string{"X-SDLC-Actor": r.cfg.Actor}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return "", func() {}, err
	}
	dir, err := os.MkdirTemp("", "clab-runner-*")
	if err != nil {
		return "", func() {}, err
	}
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", func() {}, err
	}
	return path, func() { os.RemoveAll(dir) }, nil
}

func (c claimResult) roleKey(fallback string) string {
	if c.Role != nil && c.Role.Key != "" {
		return c.Role.Key
	}
	return fallback
}

func (c claimResult) model() string {
	if c.Role != nil && c.Role.ModelHint != nil {
		return *c.Role.ModelHint
	}
	return ""
}

func (c claimResult) allowTools() []string {
	if c.Role == nil {
		return nil
	}
	return c.Role.ToolPolicy.Allow
}

func (c claimResult) denyTools() []string {
	if c.Role == nil {
		return nil
	}
	return c.Role.ToolPolicy.Deny
}

// parseUsage достаёт стоимость из вывода агента.
//
// Формат — тот, что отдаёт Claude Code с --output-format json. Чужой агент
// отдаст что-то другое, и это не ошибка: неразобранный вывод означает
// «стоимость неизвестна», а не «ноль» (решение 0026).
func parseUsage(out []byte) map[string]any {
	var result struct {
		TotalCostUSD *float64 `json:"total_cost_usd"`
		DurationMS   *int64   `json:"duration_ms"`
		Model        string   `json:"model"`
		Usage        struct {
			InputTokens              *int64 `json:"input_tokens"`
			OutputTokens             *int64 `json:"output_tokens"`
			CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		ModelUsage map[string]struct {
			InputTokens  *int64   `json:"inputTokens"`
			OutputTokens *int64   `json:"outputTokens"`
			CostUSD      *float64 `json:"costUSD"`
		} `json:"modelUsage"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil
	}

	model := result.Model
	if model == "" {
		for name := range result.ModelUsage {
			model = name
			break
		}
	}
	if model == "" {
		// Без модели отчёт не с чем сравнивать, а сравнение моделей — весь
		// смысл учёта; такой вывод честнее не записывать.
		return nil
	}

	report := map[string]any{"model": model}
	if result.TotalCostUSD != nil {
		report["cost_usd"] = *result.TotalCostUSD
	}
	if result.DurationMS != nil {
		report["duration_ms"] = *result.DurationMS
	}
	if v := result.Usage.InputTokens; v != nil {
		report["input_tokens"] = *v
	}
	if v := result.Usage.OutputTokens; v != nil {
		report["output_tokens"] = *v
	}
	if v := result.Usage.CacheReadInputTokens; v != nil {
		report["cache_read_tokens"] = *v
	}
	if v := result.Usage.CacheCreationInputTokens; v != nil {
		report["cache_write_tokens"] = *v
	}
	report["details"] = map[string]any{"reported_by": "clab-runner"}
	return report
}
