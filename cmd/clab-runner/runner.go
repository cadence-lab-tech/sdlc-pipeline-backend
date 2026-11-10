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

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit/launch"
)

type runner struct {
	cfg Config
	log *slog.Logger
	api *client
	// Привязка агента (CLAB-420): команда, форма инструкции, разбор вывода,
	// окружение; в тестах подменяется.
	agent agentkit.Binding
	// Рабочие копии задач; nil — раннер без клонов, агент в его каталоге.
	ws *workspaces
	// Окружение раннера, из которого собирается окружение агента; в тестах
	// подменяется.
	environ func() []string

	// Часы раннера: паузы считаются от них, в тестах они подменяются.
	now func() time.Time
	// Сбои подряд по прогону (CLAB-215).
	fails *failures

	mu      sync.Mutex
	running map[string]bool
	// Пауза всех слотов по лимиту подписки агента (CLAB-215): до
	// pauseUntil раннер не забирает ни одной задачи.
	paused     bool
	pauseUntil time.Time
	// Долг по учёту принципала раннера (CLAB-219): какие прогоны и когда
	// о них написано в журнал — запись не повторяется на каждом опросе.
	debt debtLog
}

func newRunner(cfg Config, log *slog.Logger) *runner {
	// Раннер, собранный в обход loadConfig, получает те же значения по
	// умолчанию: нулевой предел значил бы пропуск после первого же сбоя.
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = defaultMaxFailures
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = defaultRetryDelay
	}
	if cfg.RetryDelayMax < cfg.RetryDelay {
		cfg.RetryDelayMax = max(defaultRetryDelayMax, cfg.RetryDelay)
	}
	if cfg.LimitPause <= 0 {
		cfg.LimitPause = defaultLimitPause
	}
	r := &runner{cfg: cfg, log: log, api: newClient(cfg), environ: os.Environ, running: map[string]bool{},
		now: time.Now, fails: newFailures(cfg.MaxFailures, cfg.RetryDelay, cfg.RetryDelayMax)}
	if cfg.ReposDir != "" {
		r.ws = newWorkspaces(cfg.ReposDir, cfg.WorktreesDir, log)
	}
	if cfg.Agent != nil {
		r.agent = *cfg.Agent
	} else {
		r.agent = defaultAgent()
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
//
// На паузе по лимиту подписки раннер не спрашивает даже очередь, а прогон на
// паузе после сбоя или пропущенный после K сбоев отсеивается до claim:
// пропуск не оставляет в ленте ни stage.claimed, ни запроса захвата.
func (r *runner) dispatch(ctx context.Context, slots chan struct{}, wg *sync.WaitGroup) int {
	if r.pausedNow() {
		return 0
	}
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
		if r.fails.blocked(runKey(item), r.now()) {
			r.log.Debug("прогон после сбоя ждёт", "task", item.Task.Key, "run", runKey(item))
			continue
		}
		if r.pausedNow() {
			// Пока шёл этот опрос, агент другого слота упёрся в лимит.
			return took
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
			if errors.As(err, &apiErr) && apiErr.Code == codeUsageReportRequired {
				// Долг на принципале, а не на задаче: остальные захваты этого
				// опроса отбились бы тем же. Аренды нет — release не нужен,
				// и сбоем прогона отказ не считается.
				r.debt.note(r.log, apiErr, r.now())
				return took
			}
			if errors.As(err, &apiErr) && (apiErr.Code == "lease_held_by_other" ||
				apiErr.Code == "task_not_claimable" || apiErr.Code == "forbidden") {
				continue // кто-то оказался быстрее или задача не наша
			}
			r.log.Warn("захват не удался", "task", item.Task.Key, "error", err)
			continue
		}

		r.debt.clear()
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

// runKey — ключ счётчика сбоев: прогон из очереди. Задача, вернувшаяся в
// статус, приходит новым прогоном, и её счёт начинается с нуля. Строки
// очереди без прогона раннер считает по задаче — так защита не пропадает.
func runKey(item queueItem) string {
	if item.Run.ID != "" {
		return item.Run.ID
	}
	return "task:" + item.Task.Key
}

// pausedNow — раннер на паузе по лимиту подписки. Выход из паузы пишется в
// журнал один раз, первым опросом после её конца.
func (r *runner) pausedNow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.paused {
		return false
	}
	if r.now().Before(r.pauseUntil) {
		return true
	}
	r.paused = false
	r.log.Info("пауза по лимиту подписки снята: раннер продолжает опрос", "resume_at", r.pauseUntil.Format(time.RFC3339))
	return false
}

// pauseForLimit ставит все слоты на паузу до сброса лимита и отвечает, до
// какого момента раннер стоит. Работающих агентов пауза не трогает: если и
// они упрутся в лимит, пауза продлевается до более поздней границы, но не
// сокращается.
func (r *runner) pauseForLimit(log *slog.Logger, lim agentLimit) time.Time {
	now := r.now()
	until, known := lim.resetAt, !lim.resetAt.IsZero()
	if !known {
		until = now.Add(r.cfg.LimitPause)
	}

	r.mu.Lock()
	active := r.paused && now.Before(r.pauseUntil)
	extended := !active || until.After(r.pauseUntil)
	if extended {
		r.pauseUntil = until
	}
	r.paused = true
	resume := r.pauseUntil
	r.mu.Unlock()

	attrs := []any{"resume_at", resume.Format(time.RFC3339), "reset_known", known}
	switch {
	case !active && known:
		log.Warn("лимит подписки агента исчерпан: раннер на паузе до сброса", attrs...)
	case !active:
		log.Warn("лимит подписки агента исчерпан, время сброса неизвестно: раннер на паузе SDLC_RUNNER_LIMIT_PAUSE",
			append(attrs, "limit_pause", r.cfg.LimitPause)...)
	case extended:
		log.Warn("лимит подписки агента исчерпан ещё раз: пауза продлена", attrs...)
	default:
		log.Info("лимит подписки агента исчерпан, раннер уже на паузе", attrs...)
	}
	return resume
}

// limitNote — причина release по лимиту подписки.
func limitNote(resume time.Time, known bool) string {
	note := "лимит подписки агента исчерпан, раннер на паузе до " + resume.Format(time.RFC3339)
	if !known {
		note += " (время сброса неизвестно)"
	}
	return note
}

// fail — единственный путь release из-за поломки: агент упал или убит по
// таймауту, не подготовлена рабочая копия, не выпущен токен задачи, не
// собран каталог агента. Путь, который обошёл бы его, остался бы петлёй
// захватов. Остановка раннера сбоем прогона не считается: виноват не он.
func (r *runner) fail(ctx context.Context, log *slog.Logger, item queueItem, reason string) {
	if ctx.Err() != nil {
		r.releaseQuietly(ctx, item.Task.Key, reason+"; раннер остановлен")
		return
	}
	v := r.fails.fail(runKey(item), r.now())
	if v.skipped {
		log.Warn("прогон пропускается до перезапуска раннера", "run", runKey(item), "failures", v.n, "max_failures", v.max)
	} else {
		log.Info("повторный захват прогона после паузы", "run", runKey(item), "failures", v.n,
			"max_failures", v.max, "retry_in", v.delay)
	}
	r.releaseQuietly(ctx, item.Task.Key, reason+"; "+v.note())
	// До возврата из work прогон занят слотом, и dispatch его не возьмёт:
	// пауза начинается после release, как написано в причине.
	r.fails.holdFrom(runKey(item), r.now())
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
	if r.cfg.Billing != "" {
		// Тип оплаты виден в каждой строке прогона, в том числе в запуске
		// агента и в сухом прогоне (решение 0062).
		log = log.With("billing", r.cfg.Billing)
	}

	space, err := r.prepareWorkspace(ctx, item.Task.Key, claim)
	if err != nil {
		// Агент в чужом каталоге хуже, чем агент, который не запустился:
		// задача вернётся в очередь, а причина останется в ленте.
		log.Error("рабочая копия не подготовлена", "error", err)
		r.fail(ctx, log, item, "раннер не подготовил рабочую копию: "+err.Error())
		return
	}
	defer r.cleanupWorkspace(ctx, space)
	workdir := space.dir()

	choice := r.chooseModel(claim)
	if choice.hintDeprecated {
		log.Warn("model_hint устарел, задайте SDLC_RUNNER_MODEL_LEVELS: модель взята из model_hint роли",
			"model_level", choice.level, "model_level_source", choice.source, "model", choice.model)
	}
	template := r.template()
	if len(template) == 0 {
		// generic без своей команды: выбор агента такого не даёт, но раннер,
		// собранный в обход main, мог бы.
		log.Error("команда агента не задана: у привязки нет шаблона run", "agent", r.agent.Name)
		r.fail(ctx, log, item, "у привязки "+r.agent.Name+" нет команды запуска: задайте SDLC_RUNNER_COMMAND")
		return
	}
	// Модель, которую агент получит на самом деле: своя команда без {model}
	// её не передаёт, и назвать её в отчёте значило бы приписать прогон
	// чужой модели.
	model := choice.model
	if model != "" && !launch.Uses(template, launch.SlotModel) {
		log.Warn("модель не передана: в команде нет {model} — агент возьмёт свою модель по умолчанию", "model", model)
		model = ""
	}
	// Отчитается ли раннер, решается до запуска: от этого зависит текст
	// инструкции (usage=runner), и строка учёта за прогон одна.
	reports := runnerReports(output(r.agent), model)
	start, reason, ok := r.instruction(ctx, log, item, claim, reports)
	if !ok {
		r.fail(ctx, log, item, reason)
		return
	}
	for _, warn := range launch.PolicyWarnings(r.agent.Name, r.agent.ToolPolicy != nil, template,
		claim.allowTools(), claim.denyTools()) {
		log.Warn(warn)
	}
	if hookPlacement(r.agent) == "launch" {
		// Хук привязки крепится к запуску, а раннер его не крепит: граница —
		// окружение и токен задачи (решение 0049), как у clab exec без хука.
		log.Info("хук-страховка не крепится: изоляция окружением", "agent", r.agent.Name)
	}

	if r.cfg.DryRun {
		// Токен задачи в сухом прогоне не выпускается: он гас бы через
		// секунду вместе с отпущенной арендой, а след в ленте остался бы.
		argv := r.argv(item, claim, runSlots{mcpConfig: "<mcp.json>", runDir: "<каталог агента>",
			configDir: "<каталог агента>/clab", workdir: workdir, text: start})
		log.Info("сухой прогон: агент получил бы токен задачи и окружение по списку",
			append([]any{"argv", r.displayArgv(argv, start), "workdir", workdir, "agent", r.agent.Name,
				"instruction", r.instructionHow(template, start)}, choice.logAttrs()...)...)
		r.releaseQuietly(ctx, item.Task.Key, "сухой прогон раннера")
		return
	}

	creds, reason, ok := r.agentCredentials(ctx, log, item.Task.Key)
	if !ok {
		r.fail(ctx, log, item, reason)
		return
	}
	defer r.revokeQuietly(ctx, log, creds)

	home, err := r.agentHome(creds)
	if err != nil {
		log.Error("не собрать конфигурацию агента", "error", err)
		r.fail(ctx, log, item, "раннер не смог подготовить запуск")
		return
	}
	defer home.cleanup()

	argv := r.argv(item, claim, runSlots{mcpConfig: home.mcpConfig, runDir: home.root, configDir: home.clabDir,
		workdir: workdir, text: start})
	log.Info("запуск агента", append(choice.logAttrs(), "command", argv[0], "workdir", workdir,
		"credentials", creds.kind(), "agent", r.agent.Name, "instruction", r.instructionHow(template, start))...)

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	stop := r.keepLease(runCtx, item.Task.Key, claim)
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = r.agentEnv(creds, home)
	cmd.Stderr = os.Stderr
	if !launch.Uses(template, launch.SlotPrompt) {
		// Команда без {prompt} получает инструкцию на stdin, как у clab exec.
		cmd.Stdin = strings.NewReader(r.promptValue(item, claim, start))
	}
	out, err := cmd.Output()
	stop()

	// Под раннером о стоимости отчитывается только он (CLAB-214): агенту
	// инструкция это запрещает, поэтому отчёт идёт и за упавшего агента —
	// Claude Code печатает JSON и при ненулевом коде выхода, Codex — события
	// до сбоя.
	report := parseOutput(output(r.agent), out, model)
	usage := func() bool {
		if !reports {
			log.Info("о стоимости отчитывается агент: у привязки нет разбора вывода или модель не выбрана",
				"agent", r.agent.Name, "output", dashOf(r.agent.Output))
			return false
		}
		return r.reportUsage(ctx, log, item.Task.Key, claim.StageRunID, report)
	}
	// Лимит подписки — признак вывода Claude Code (claude-json); проверяется
	// при любом коде выхода и в счётчик сбоев не идёт: задача в нём не
	// виновата, а раннер встаёт целиком. У других привязок лимита нет:
	// ненулевой код — обычный сбой прогона.
	if output(r.agent) == outputClaudeJSON {
		if lim, hit := detectLimit(out, r.now()); hit {
			log.Warn("агент упёрся в лимит подписки", "exit_error", err)
			usage()
			resume := r.pauseForLimit(log, lim)
			r.releaseQuietly(ctx, item.Task.Key, limitNote(resume, !lim.resetAt.IsZero()))
			return
		}
	}
	if err != nil {
		log.Error("агент завершился с ошибкой", "error", err)
		// Сначала учёт, потом release: отпуск снимает аренду, и без истории
		// захвата отчёт после него отбился бы.
		usage()
		// Агент умер — аренда протухнет сама, но отпустить задачу сразу
		// честнее: следующий слот возьмёт её после паузы, не дожидаясь
		// протухания.
		r.fail(ctx, log, item, "агент завершился с ошибкой")
		return
	}
	r.fails.reset(runKey(item))
	if usage() {
		log.Info("агент отработал", "cost_usd", report["cost_usd"], "model", report["model"])
	}
}

// instruction — стартовая инструкция прогона в форме привязки (поле
// prompt): слэш-командой промпта MCP work_on_task или текстом ручки
// prompts/work. reports — usage=runner в инструкции. Отказ — причина для
// release через учёт сбоев.
func (r *runner) instruction(ctx context.Context, log *slog.Logger, item queueItem, claim claimResult, reports bool) (string, string, bool) {
	role := claim.roleKey(item.RoleKey)
	if r.agent.Prompt == promptSlash {
		if role == "" {
			// Признак режима раннера — позиционный аргумент после роли: без
			// роли его не передать, и агент отчитается о стоимости сам, рядом
			// с отчётом раннера.
			log.Warn("роль этапа неизвестна: промпт без режима раннера, стоимость прогона может посчитаться дважды")
		}
		return prompt(item.Task.Key, role), "", true
	}
	text, err := r.api.workPrompt(ctx, item.Task.Key, role, reports)
	switch {
	case err == nil:
		return text, "", true
	case noRoute(err):
		log.Error("сервер не отдаёт стартовую инструкцию текстом (нужен v0.28.0 или новее): агент не запущен", "agent", r.agent.Name)
		return "", "сервер прежней версии: нет стартовой инструкции", false
	default:
		log.Error("стартовая инструкция не получена, агент не запущен", "error", err)
		return "", "раннер не получил стартовую инструкцию: " + errCode(err), false
	}
}

// instructionHow — как инструкция дойдёт до агента, для журнала.
func (r *runner) instructionHow(template []string, start string) string {
	how := "аргументом {prompt}"
	if !launch.Uses(template, launch.SlotPrompt) {
		how = "на stdin"
	}
	form := "текстом"
	if r.agent.Prompt == promptSlash {
		form = "слэш-командой MCP"
	}
	return fmt.Sprintf("%s %s, %d байт", form, how, len(start))
}

// displayArgv — argv для журнала: текст инструкции — размером, слэш-промпт
// — как есть (его читают глазами, он короткий).
func (r *runner) displayArgv(argv []string, start string) string {
	words := make([]string, 0, len(argv))
	for _, a := range argv {
		if r.agent.Prompt != promptSlash && start != "" && a == start {
			a = fmt.Sprintf("<инструкция: %d байт>", len(start))
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// reportUsage отправляет разобранный вывод агента своим токеном раннера и
// отвечает, принят ли отчёт. Неразобранный вывод строки не пишет:
// молчание честнее выдуманного нуля, прогон останется «неизвестно».
func (r *runner) reportUsage(ctx context.Context, log *slog.Logger, task, runID string, report map[string]any) bool {
	if report == nil {
		log.Info("стоимость неизвестна: вывод агента не разобран")
		return false
	}
	// Без SDLC_RUNNER_BILLING поля нет: строка ляжет без признака и
	// посчитается деньгами (решение 0062).
	if r.cfg.Billing != "" {
		report["billing"] = r.cfg.Billing
	}
	// Какой привязкой разобран вывод — рядом с reported_by: строки разных
	// агентов сравнимы по прайсу, но различимы в учёте.
	details, _ := report["details"].(map[string]any)
	if details == nil {
		details = map[string]any{"reported_by": "clab-runner"}
	}
	details["agent"] = r.agent.Name
	report["details"] = details
	// Отчёт переживает остановку раннера так же, как release после него.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := r.api.reportUsage(ctx, task, runID, report); err != nil {
		log.Warn("отчёт о стоимости не принят", "error", err)
		return false
	}
	return true
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
//
// Отказ возвращается причиной для release: отпускает задачу work, через
// учёт сбоев.
func (r *runner) agentCredentials(ctx context.Context, log *slog.Logger, task string) (agentCreds, string, bool) {
	issued, err := r.api.issueTaskToken(ctx, task)
	switch {
	case err == nil:
		return agentCreds{token: issued.Token, tokenID: issued.Record.ID}, "", true
	case noRoute(err):
		log.Warn("сервер не выдаёт токен задачи: агент получил токен раннера; обновите сервер")
		return agentCreds{token: r.cfg.Token, actor: r.cfg.Actor}, "", true
	default:
		log.Error("токен задачи не выпущен, агент не запущен", "error", err)
		return agentCreds{}, "раннер не выпустил токен задачи: " + errCode(err), false
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

// runSlots — значения подстановок, которые даёт прогон: файлы и каталоги
// агента, рабочая копия и текст стартовой инструкции.
type runSlots struct {
	mcpConfig, runDir, configDir, workdir string
	// text — инструкция текстом для привязки с prompt=text; слэш-промпт
	// раннер собирает сам.
	text string
}

// template — слова команды: своя команда SDLC_RUNNER_COMMAND (режется по
// пробелам, кавычек нет), иначе шаблон run привязки.
func (r *runner) template() []string {
	if r.cfg.Command != "" {
		return strings.Fields(r.cfg.Command)
	}
	return r.agent.Run
}

// argv собирает команду запуска из шаблона общим кодом подстановок
// (agentkit/launch, тот же, что у clab exec). Инструменты приезжают из
// версии роли, модель — по рекомендованному уровню (chooseModel). Пустая
// подстановка снимает слово вместе с флагом при нём: «--model », «-m » и
// «-c k=""» без значения сломали бы запуск, а роль без модели — законный
// случай. Токена задачи в подстановках нет: он в окружении и mcp.json.
func (r *runner) argv(item queueItem, claim claimResult, s runSlots) []string {
	choice := r.chooseModel(claim)
	values := map[string]string{
		launch.SlotTask:            item.Task.Key,
		launch.SlotWorkdir:         s.workdir,
		launch.SlotRole:            claim.roleKey(item.RoleKey),
		launch.SlotModel:           choice.model,
		launch.SlotEffort:          choice.effort,
		launch.SlotAllowedTools:    strings.Join(claim.allowTools(), ","),
		launch.SlotDisallowedTools: strings.Join(claim.denyTools(), ","),
		launch.SlotMCPConfig:       s.mcpConfig,
		launch.SlotMCPURL:          r.cfg.mcpURL(),
		launch.SlotConfigDir:       s.configDir,
		launch.SlotRunDir:          s.runDir,
		launch.SlotPrompt:          r.promptValue(item, claim, s.text),
	}
	return launch.Expand(r.template(), values)
}

// promptValue — значение {prompt}: слэш-промпт у привязки mcp_slash, иначе
// текст инструкции.
func (r *runner) promptValue(item queueItem, claim claimResult, text string) string {
	if r.agent.Prompt == promptSlash {
		return prompt(item.Task.Key, claim.roleKey(item.RoleKey))
	}
	return text
}

// prompt — единая точка старта работы: тот же промпт, которым пользуется
// человек. В Claude Code промпт MCP-сервера доступен как слэш-команда, и
// агент забирает его с того же сервера, к которому подключён.
//
// Третий аргумент — usage=runner: стоимость сообщит раннер, и агенту промпт
// отчитываться запрещает (CLAB-214). Аргументы слэш-команды позиционные,
// поэтому признак ставится только вместе с ролью. Сервер прежней версии
// лишний аргумент пропускает и отдаёт прежний текст.
func prompt(task, role string) string {
	if role == "" {
		return "/mcp__sdlc__work_on_task " + task
	}
	return "/mcp__sdlc__work_on_task " + task + " " + role + " " + usageByRunner
}

// usageByRunner — значение аргумента usage промпта work_on_task. Та же
// константа есть в internal/mcp: раннер не тянет пакет сервера ради одной
// строки, значение держат тесты с обеих сторон.
const usageByRunner = "runner"

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
	// Учётные данные и настройки самого агента — поле env привязки
	// (claude — ANTHROPIC_*, CLAUDE_*; codex — OPENAI_*, CODEX_*), затем
	// SDLC_RUNNER_ENV администратора.
	extra := append(append([]string{}, r.agent.Env...), r.cfg.AgentEnv...)
	return agentEnv(r.environ(), extra, set)
}

func (c claimResult) roleKey(fallback string) string {
	if c.Role != nil && c.Role.Key != "" {
		return c.Role.Key
	}
	return fallback
}

func (c claimResult) modelHint() string {
	if c.Role != nil && c.Role.ModelHint != nil {
		return *c.Role.ModelHint
	}
	return ""
}

// Откуда взята модель запуска — значения поля model_from в журнале.
// Прежние table, model_hint и none не меняются: по ним настроены разборы
// журналов; agent_settings и binding — шаги 0077 п. 7 (CLAB-420).
const (
	modelFromTable    = "table"
	modelFromSettings = "agent_settings"
	modelFromBinding  = "binding"
	modelFromHint     = "model_hint"
	modelFromNone     = "none"
)

// modelChoice — модель запуска и как она выбрана. Один результат идёт в
// {model}, в журнал запуска и в сухой прогон, чтобы они не разошлись.
type modelChoice struct {
	level  string // рекомендованный уровень; none — не задан
	source string // источник уровня; absent — сервер без execution
	model  string // пусто — флаг --model снимается
	from   string
	// Усилие и номер версии настройки агентов — только у модели из
	// настройки (agent_settings): у других шагов их нет.
	effort          string
	settingsVersion string
	// Модель взята из устаревшего model_hint у сервера, который уже
	// рекомендует уровень, — повод предупредить владельца раннера.
	hintDeprecated bool
}

func (m modelChoice) logAttrs() []any {
	attrs := []any{"model_level", m.level, "model_level_source", m.source, "model", m.model, "model_from", m.from,
		"model_source", m.pickSource()}
	if m.effort != "" {
		attrs = append(attrs, "effort", m.effort)
	}
	if m.settingsVersion != "" {
		attrs = append(attrs, "agent_settings_version", m.settingsVersion)
	}
	return attrs
}

// pickSource — источник модели именами 0077 п. 7 (explicit, agent_settings,
// binding, model_hint, agent_default): те же, что печатает clab exec.
func (m modelChoice) pickSource() string {
	switch m.from {
	case modelFromTable:
		return launch.SourceExplicit
	case modelFromNone:
		return launch.SourceAgentDefault
	}
	return m.from
}

// chooseModel выбирает модель запуска (решения 0060, 0077 п. 7):
// SDLC_RUNNER_MODEL_LEVELS для рекомендованного уровня, запись версии-
// снимка настройки агентов для своей привязки, таблица уровней привязки
// (общий launch.PickModel), иначе model_hint роли, иначе ничего. Против
// сервера без execution — сразу model_hint и без предупреждения: так вёл
// себя раннер прежней версии, и обновлённый раннер у необновлённого
// сервера собирает тот же argv.
func (r *runner) chooseModel(c claimResult) modelChoice {
	choice := modelChoice{level: "none", source: "absent", from: modelFromNone}
	level := ""
	var models map[string]launch.ModelChoice
	if c.Execution != nil {
		choice.source = c.Execution.ModelLevelSource
		models = c.Execution.Models
		if level = ptrString(c.Execution.ModelLevel); level != "" {
			choice.level = level
			if model, ok := r.cfg.ModelLevels[level]; ok {
				choice.model, choice.from = model, modelFromTable
				return choice
			}
		}
	}
	switch p := launch.PickModel(level, r.agent.Name, models, levelsOf(r.agent)); p.Source {
	case launch.SourceAgentSettings:
		choice.model, choice.effort, choice.from = p.Model, p.Effort, modelFromSettings
		choice.settingsVersion = c.Execution.settingsVersion()
		return choice
	case launch.SourceBinding:
		choice.model, choice.from = p.Model, modelFromBinding
		return choice
	}
	if hint := c.modelHint(); hint != "" {
		choice.model, choice.from = hint, modelFromHint
		choice.hintDeprecated = c.Execution != nil
	}
	return choice
}

func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
