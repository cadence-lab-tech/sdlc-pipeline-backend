package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
	// Рабочие копии задач; nil — раннер без клонов, агент в его каталоге.
	ws *workspaces
	// Окружение раннера, из которого собирается окружение агента; в тестах
	// подменяется.
	environ func() []string

	mu      sync.Mutex
	running map[string]bool
}

func newRunner(cfg Config, log *slog.Logger) *runner {
	r := &runner{cfg: cfg, log: log, api: newClient(cfg), environ: os.Environ, running: map[string]bool{}}
	if cfg.ReposDir != "" {
		r.ws = newWorkspaces(cfg.ReposDir, cfg.WorktreesDir, log)
	}
	return r
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
		// Агент ходит токеном задачи на эту аренду — тот же принципал, что у
		// раннера, — поэтому его собственный захват — это подтверждение уже
		// своей аренды, а не спор за неё.
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
//
// Агент ходит не токеном раннера, а токеном задачи на захваченный прогон
// (решение 0049): раннер выпускает его после захвата, кладёт в mcp.json и
// конфиг clab агента и отзывает, когда агент закончил. Аренду, отпуск и
// отчёт о стоимости раннер по-прежнему ведёт своим токеном: токен задачи
// гаснет на переходе, а отчёт идёт после него.
func (r *runner) work(ctx context.Context, item queueItem, claim claimResult) {
	log := r.log.With("task", item.Task.Key, "role", claim.roleKey(item.RoleKey))

	space, err := r.prepareWorkspace(ctx, item.Task.Key, claim)
	if err != nil {
		// Агент в чужом каталоге хуже, чем агент, который не запустился:
		// задача вернётся в очередь, а причина останется в ленте.
		log.Error("рабочая копия не подготовлена", "error", err)
		r.releaseQuietly(ctx, item.Task.Key, "раннер не подготовил рабочую копию: "+err.Error())
		return
	}
	defer r.cleanupWorkspace(ctx, space)
	workdir := space.dir()

	if r.cfg.DryRun {
		// Токен задачи в сухом прогоне не выпускается: он гас бы через
		// секунду вместе с отпущенной арендой, а след в ленте остался бы.
		argv := r.argv(item, claim, "<mcp.json>", workdir)
		log.Info("сухой прогон: агент получил бы токен задачи и окружение по списку",
			"argv", strings.Join(argv, " "), "workdir", workdir)
		r.releaseQuietly(ctx, item.Task.Key, "сухой прогон раннера")
		return
	}

	creds, ok := r.agentCredentials(ctx, log, item.Task.Key)
	if !ok {
		return
	}
	defer r.revokeQuietly(ctx, log, creds)

	home, err := r.agentHome(creds)
	if err != nil {
		log.Error("не собрать конфигурацию агента", "error", err)
		r.releaseQuietly(ctx, item.Task.Key, "раннер не смог подготовить запуск")
		return
	}
	defer home.cleanup()

	argv := r.argv(item, claim, home.mcpConfig, workdir)
	log.Info("запуск агента", "model", claim.model(), "command", argv[0], "workdir", workdir,
		"credentials", creds.kind())

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	stop := r.keepLease(runCtx, item.Task.Key, claim)
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = r.agentEnv(creds, home)
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

// agentCreds — чем агент представляется серверу: токеном задачи, а у
// сервера без ручки выпуска — токеном раннера или его актором заголовка.
// Значения не печатаются: у типа свой LogValue.
type agentCreds struct {
	token   string
	actor   string
	tokenID string // запись токена задачи; пусто — отзывать нечего
}

func (c agentCreds) kind() string {
	switch {
	case c.tokenID != "":
		return "task"
	case c.token != "":
		return "runner"
	default:
		return "actor"
	}
}

func (c agentCreds) LogValue() slog.Value { return slog.StringValue(c.kind()) }

// agentCredentials выпускает токен задачи. Откат на учётные данные раннера —
// только у сервера, который ручки не знает: любой другой отказ значит, что
// с арендой что-то не так, и агент не запускается вовсе.
func (r *runner) agentCredentials(ctx context.Context, log *slog.Logger, task string) (agentCreds, bool) {
	issued, err := r.api.issueTaskToken(ctx, task)
	switch {
	case err == nil:
		return agentCreds{token: issued.Token, tokenID: issued.Record.ID}, true
	case noRoute(err):
		log.Warn("сервер не выдаёт токен задачи: агент получил токен раннера; обновите сервер")
		return agentCreds{token: r.cfg.Token, actor: r.cfg.Actor}, true
	default:
		log.Error("токен задачи не выпущен, агент не запущен", "error", err)
		r.releaseQuietly(ctx, task, "раннер не выпустил токен задачи: "+errCode(err))
		return agentCreds{}, false
	}
}

// revokeQuietly отзывает токен задачи, когда агент закончил, чем бы ни
// кончилось. После перехода или release он уже мёртв, и 401 или 404 на
// отзыв — штатный исход, а не повод шуметь.
func (r *runner) revokeQuietly(ctx context.Context, log *slog.Logger, creds agentCreds) {
	if creds.tokenID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	err := r.api.revokeToken(ctx, creds.tokenID)
	var apiErr *apiError
	switch {
	case err == nil:
		log.Debug("токен задачи отозван", "token_id", creds.tokenID)
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusUnauthorized):
		log.Debug("токен задачи уже недействителен", "token_id", creds.tokenID)
	default:
		log.Warn("токен задачи не отозван", "token_id", creds.tokenID, "error", err)
	}
}

// prepareWorkspace заводит рабочие копии по области задачи из ответа
// захвата. Без клонов раннер работает по-старому: копий нет.
func (r *runner) prepareWorkspace(ctx context.Context, task string, claim claimResult) (*workspace, error) {
	if r.ws == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return r.ws.prepare(ctx, task, claim.Context.Repositories)
}

// cleanupWorkspace убирает копии и после остановки раннера: агент уже
// отработал, а копия без уборки занимала бы место и каталог задачи.
func (r *runner) cleanupWorkspace(ctx context.Context, space *workspace) {
	if r.ws == nil || space == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	r.ws.cleanup(ctx, space)
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
func (r *runner) argv(item queueItem, claim claimResult, mcpConfig, workdir string) []string {
	replacer := strings.NewReplacer(
		"{task}", item.Task.Key,
		"{workdir}", workdir,
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

// agentDir — временный каталог агента: mcp.json и конфиг clab с его
// учётными данными. Живёт ровно столько, сколько работает агент.
type agentDir struct {
	root      string
	mcpConfig string // путь к mcp.json
	clabDir   string // CLAB_CONFIG_DIR агента
}

func (d agentDir) cleanup() { os.RemoveAll(d.root) }

// agentHome пишет временный каталог агента. mcp.json — сервер проекта с
// учётными данными агента; рядом каталог для CLAB_CONFIG_DIR с config.json в
// формате clab (cmd/clab/config.go), чтобы clab в агенте по умолчанию шёл
// токеном задачи, а не конфигом сотрудника из ~/.config/clab.
func (r *runner) agentHome(creds agentCreds) (agentDir, error) {
	server := map[string]any{"type": "http", "url": r.cfg.mcpURL()}
	if creds.token != "" {
		server["headers"] = map[string]string{"Authorization": "Bearer " + creds.token}
	} else {
		server["headers"] = map[string]string{"X-SDLC-Actor": creds.actor}
	}
	mcpRaw, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"sdlc": server}})
	if err != nil {
		return agentDir{}, err
	}
	host, origin, err := apiOrigin(r.cfg.APIURL)
	if err != nil {
		return agentDir{}, err
	}
	hostConfig := map[string]any{"url": origin}
	if creds.token != "" {
		hostConfig["token"] = creds.token
	}
	clabRaw, err := json.MarshalIndent(map[string]any{
		"default_host": host,
		"hosts":        map[string]any{host: hostConfig},
	}, "", "  ")
	if err != nil {
		return agentDir{}, err
	}

	root, err := os.MkdirTemp("", "clab-runner-*") // 0700
	if err != nil {
		return agentDir{}, err
	}
	d := agentDir{
		root:      root,
		mcpConfig: filepath.Join(root, "mcp.json"),
		clabDir:   filepath.Join(root, "clab"),
	}
	if err := os.WriteFile(d.mcpConfig, mcpRaw, 0o600); err != nil {
		d.cleanup()
		return agentDir{}, err
	}
	if err := os.Mkdir(d.clabDir, 0o700); err != nil {
		d.cleanup()
		return agentDir{}, err
	}
	if err := os.WriteFile(filepath.Join(d.clabDir, "config.json"), append(clabRaw, '\n'), 0o600); err != nil {
		d.cleanup()
		return agentDir{}, err
	}
	return d, nil
}

// apiOrigin — хост и адрес инстанса из SDLC_API_URL без хвоста /v1: так их
// хранит конфиг clab (normalizeHost в cmd/clab; пакеты main разные, импорт
// невозможен).
func apiOrigin(apiURL string) (host, origin string, err error) {
	u, err := url.Parse(strings.TrimSpace(apiURL))
	if err != nil || u.Host == "" || u.Scheme == "" {
		return "", "", fmt.Errorf("SDLC_API_URL %q не разбирается как адрес", apiURL)
	}
	path := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	return u.Host, u.Scheme + "://" + u.Host + path, nil
}

// agentEnv — окружение агента: базовый список и SDLC_RUNNER_ENV из окружения
// раннера, а SDLC_* и CLAB_CONFIG_DIR — заданные раннером. У сервера без
// ручки токена задачи в режиме заголовка агенту уходит актор раннера, как
// раньше уходил через наследование.
func (r *runner) agentEnv(creds agentCreds, home agentDir) []string {
	set := map[string]string{
		"SDLC_API_URL":    r.cfg.APIURL,
		"SDLC_PROJECT":    r.cfg.Project,
		"SDLC_TOKEN":      creds.token,
		"CLAB_CONFIG_DIR": home.clabDir,
	}
	if creds.token == "" {
		set["SDLC_ACTOR"] = creds.actor
	}
	return agentEnv(r.environ(), r.cfg.AgentEnv, set)
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
