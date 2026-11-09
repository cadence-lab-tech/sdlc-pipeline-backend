package main

// clab exec — исполнитель этапа процессом (CLAB-419, решение 0077 пп. 7, 8,
// 10–12): оркестратор любого агента запускает исполнителя не саб-агентом, а
// дочерним процессом с границей токена задачи (решение 0049).
//
//	clab exec K --dir <каталог захода> [--agent codex] [--model M] [--effort E]
//	          [--note-file note.md] [--dry-run] [-- <команда агента>]
//
// Граница — окружение и каталог токена, а не дисциплина промпта: клиент
// строится только из <каталог>/clab/config.json, личный конфиг человека,
// SDLC_API_URL и SDLC_TOKEN оркестратора не читаются вовсе. Процесс агента
// получает CLAB_CONFIG_DIR=<каталог>/clab и SDLC_PROJECT, а SDLC_TOKEN —
// только когда MCP sdlc подключается ссылкой на переменную (0077 п. 10):
// токен не попадает ни в argv, ни в файлы, ни в вывод.
//
// Аренду продлевает обёртка heartbeat (тот же heartbeater, что у clab
// heartbeat --), код выхода — код агента; отказ самой команды до запуска —
// отдельный код execRefused и строка «clab exec: …», чтобы оркестратор
// отличал «агент упал» от «агент не запускался».

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit/launch"
)

// execRefused — код выхода отказа clab exec до запуска агента. 125 — как у
// docker run и env: «не запустили», не путается с кодами агента и с 126/127
// оболочки.
const execRefused = 125

// Привязки, которые clab exec узнаёт по имени программы команды (0077 п. 8):
// остальное — generic. Имена свои, а не agentClaude и соседи из
// setup_agents.go параллельной CLAB-418: одно имя в пакете дважды не
// собирается; кто сливается вторым, сводит их в одни константы.
const (
	bindingClaude  = "claude"
	bindingCodex   = "codex"
	bindingGeneric = "generic"
)

// knownAgentEnv — признаки агентов, которые clab знает сам (builtinBindings):
// дочерний агент ставит свои, унаследованные дали бы «агент не узнан».
var knownAgentEnv = []string{"AI_AGENT", "CLAUDECODE", "CODEX_THREAD_ID"}

// execRefusal — отказ до запуска: текст без токена, код execRefused.
type execRefusal struct{ msg string }

func (e *execRefusal) Error() string { return e.msg }

func refuse(format string, a ...any) error {
	return &execRefusal{msg: fmt.Sprintf(format, a...)}
}

// execIO — всё, что clab exec берёт у процесса: потоки, окружение, каталог.
// Тесты подменяют его целиком.
type execIO struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	environ        []string
	workdir        string
	sigs           <-chan os.Signal
}

func cmdExec(ctx context.Context, _ *Client, args []string) error {
	wd, _ := os.Getwd()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	code := runExec(ctx, args, execIO{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		environ: os.Environ(), workdir: wd, sigs: sigs})
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// runExec — код выхода clab exec. Ошибки до запуска печатаются строкой
// «clab exec: …» и дают execRefused.
func runExec(ctx context.Context, args []string, sys execIO) int {
	code, err := execute(ctx, args, sys)
	if err == nil {
		return code
	}
	msg := err.Error()
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "unauthenticated" {
		msg += " — токен задачи погас или отозван: заберите этап заново и выпустите новый (clab token task <ключ> --config-dir <каталог>/clab)"
	}
	fmt.Fprintf(sys.stderr, "clab exec: %s\n", msg)
	return execRefused
}

type execFlags struct {
	dir, agent, model, effort, noteFile string
	dryRun                              bool
}

func execute(ctx context.Context, args []string, sys execIO) (int, error) {
	task, rest := popTask(args)
	flagArgs, command, wrapped := splitCommand(rest)
	var f execFlags
	fs, err := flags("exec", flagArgs, func(fs *flag.FlagSet) {
		fs.SetOutput(sys.stderr)
		fs.StringVar(&f.dir, "dir", "", "каталог захода: родитель clab/ с конфигом токена задачи (clab token task <ключ> --config-dir <каталог>/clab); обязателен")
		fs.StringVar(&f.agent, "agent", "", "привязка агента (claude, codex, generic); без него — по имени программы команды, без команды — по окружению")
		fs.StringVar(&f.model, "model", "", "модель агента; перекрывает настройку агентов проекта и таблицу привязки")
		fs.StringVar(&f.effort, "effort", "", "усилие агента; перекрывает настройку агентов проекта")
		fs.StringVar(&f.noteFile, "note-file", "", "заметка оркестратора, дописывается к стартовой инструкции: рабочие копии, каталог временных файлов, формат отчёта")
		fs.BoolVar(&f.dryRun, "dry-run", false, "ничего не запускать и аренду не продлевать: показать агента, модель, окружение и команду")
	})
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 0, refuse("%v", err)
	}
	switch {
	case task == "":
		return 0, refuse("не указан ключ задачи: clab exec <ключ> --dir <каталог захода> [-- <команда агента>]")
	case fs.NArg() > 0:
		return 0, refuse("лишние аргументы %q: команду агента передают после --", fs.Args())
	case f.dir == "":
		return 0, refuse("не указан --dir — каталог захода с токеном задачи: clab token task %s --config-dir <каталог>/clab, затем clab exec %s --dir <каталог>", task, task)
	case wrapped && len(command) == 0:
		return 0, refuse("после -- нет команды агента: clab exec %s --dir <каталог> -- <команда> [аргументы]", task)
	}
	project, ok := projectOfTask(task)
	if !ok {
		return 0, refuse("ключ задачи %q не вида <ПРОЕКТ>-<номер>", task)
	}

	// Всё локальное — до первого запроса: каталог без токена задачи не
	// стоит ни одного обращения к серверу.
	td, err := openTaskDir(f.dir, task, sys.environ)
	if err != nil {
		return 0, err
	}
	var note string
	if f.noteFile != "" {
		raw, err := os.ReadFile(f.noteFile)
		if err != nil {
			return 0, refuse("заметка --note-file: %v", err)
		}
		note = string(raw)
	}

	c := td.client()
	st, err := readExecState(ctx, c, task)
	if err != nil {
		return 0, err
	}
	prompt, err := workPrompt(ctx, c, task)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(note) != "" {
		prompt = strings.TrimRight(prompt, "\n") + "\n\n" + note
	}
	bindings := fetchBindings(ctx, c)

	plan, err := planLaunch(planInput{
		task: task, project: project, flags: f, command: command, wrapped: wrapped,
		bindings: bindings, state: st, prompt: prompt, dir: td, origin: c.origin,
		environ: sys.environ, workdir: sys.workdir,
	})
	if err != nil {
		return 0, err
	}

	out := sys.stderr
	if f.dryRun {
		out = sys.stdout
	}
	plan.print(out, td.token)
	if f.dryRun {
		fmt.Fprintln(out, "clab exec: сухой прогон — агент не запущен, аренда не продлевалась, файлы не записаны")
		return 0, nil
	}

	if plan.mcpConfig != "" {
		if err := writeMCPConfig(plan.mcpConfig, plan.mcpURL); err != nil {
			return 0, refuse("конфигурация MCP %s: %v", plan.mcpConfig, err)
		}
	}
	argv := append([]string{}, plan.argv...)
	if argv[0], err = lookPathIn(argv[0], plan.env); err != nil {
		return 0, err
	}
	h := &heartbeater{c: c, task: task, out: sys.stderr, warn: sys.stderr, env: plan.env, name: "clab exec"}
	var stdin io.Reader
	if plan.promptOnStdin {
		stdin = strings.NewReader(prompt)
	}
	code, err := h.wrap(ctx, argv, stdin, sys.stdout, sys.stderr, sys.sigs)
	if err != nil {
		return 0, err
	}
	return code, nil
}

// lookPathIn ищет программу по PATH окружения агента, а не clab: так
// команда находится там же, где её нашёл бы сам агент.
func lookPathIn(name string, env []string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) || runtime.GOOS == "windows" {
		return name, nil
	}
	pathVar := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			pathVar = v
		}
	}
	for _, dir := range filepath.SplitList(pathVar) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", refuse("программа %s не найдена в PATH агента", name)
}

// projectShape — ключ проекта (migrations: ^[A-Z][A-Z0-9_]{1,15}$); ключ
// задачи — ключ проекта и номер через дефис.
var projectShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,15}$`)

func projectOfTask(task string) (string, bool) {
	i := strings.LastIndex(task, "-")
	if i <= 0 {
		return "", false
	}
	if _, err := strconv.Atoi(task[i+1:]); err != nil || !projectShape.MatchString(task[:i]) {
		return "", false
	}
	return task[:i], true
}

// taskDir — каталог захода и конфиг токена задачи в нём.
type taskDir struct {
	runDir, clabDir string
	host, origin    string
	token           string
}

func (d taskDir) client() *Client {
	return &Client{base: d.origin + "/v1", origin: d.origin, host: d.host, token: d.token,
		http: &http.Client{Timeout: 60 * time.Second}}
}

// openTaskDir проверяет каталог захода и читает из него токен задачи. Отказ
// называет путь и как выпустить токен; самого токена в тексте нет.
func openTaskDir(raw, task string, environ []string) (taskDir, error) {
	hint := fmt.Sprintf("выпустите токен: clab token task %s --config-dir <каталог>/clab", task)
	runDir, err := filepath.Abs(raw)
	if err != nil {
		return taskDir{}, refuse("--dir %s: %v", raw, err)
	}
	clabDir := filepath.Join(runDir, "clab")
	cfgPath := filepath.Join(clabDir, "config.json")
	if own := ownConfigDir(environ); own != "" && (sameDir(own, runDir) || sameDir(own, clabDir)) {
		return taskDir{}, refuse("--dir %s — каталог вашего собственного конфига clab, а не каталог захода с токеном задачи; заведите отдельный каталог и %s",
			runDir, strings.Replace(hint, "<каталог>", runDir, 1))
	}
	info, err := os.Stat(runDir)
	if err != nil || !info.IsDir() {
		return taskDir{}, refuse("каталога захода %s нет; заведите его и %s", runDir, strings.Replace(hint, "<каталог>", runDir, 1))
	}
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		return taskDir{}, refuse("в каталоге захода нет конфига токена задачи %s; %s", cfgPath, strings.Replace(hint, "<каталог>", runDir, 1))
	}
	var cfg Config
	if json.Unmarshal(body, &cfg) != nil {
		return taskDir{}, refuse("конфиг %s не разбирается; %s", cfgPath, strings.Replace(hint, "<каталог>", runDir, 1))
	}
	hc, found := cfg.Hosts[cfg.DefaultHost]
	switch {
	case !found || hc.Token == "" || hc.URL == "":
		return taskDir{}, refuse("в конфиге %s нет токена задачи; %s", cfgPath, strings.Replace(hint, "<каталог>", runDir, 1))
	case hc.Handle != "task:"+task:
		who := hc.Handle
		if who == "" {
			who = "без отметки о задаче"
		}
		return taskDir{}, refuse("в конфиге %s токен не задачи %s (%s); %s", cfgPath, task, who, strings.Replace(hint, "<каталог>", runDir, 1))
	}
	_, origin, err := normalizeHost(hc.URL)
	if err != nil {
		return taskDir{}, refuse("адрес хоста в конфиге %s: %v", cfgPath, err)
	}
	return taskDir{runDir: runDir, clabDir: clabDir, host: cfg.DefaultHost, origin: origin, token: hc.Token}, nil
}

// ownConfigDir — каталог конфига человека, запустившего clab exec: тот же
// порядок, что у configDir, по окружению оркестратора.
func ownConfigDir(environ []string) string {
	get := func(name string) string {
		for _, kv := range environ {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				return v
			}
		}
		return ""
	}
	if dir := get("CLAB_CONFIG_DIR"); dir != "" {
		return dir
	}
	if xdg := get("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "clab")
	}
	home := get("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "clab")
}

func sameDir(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	if ea, err := filepath.EvalSymlinks(aa); err == nil {
		aa = ea
	}
	if eb, err := filepath.EvalSymlinks(bb); err == nil {
		bb = eb
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

// execState — то из состояния задачи, что нужно запуску: роль этапа, её
// tool_policy и execution. Читается GET …/state токеном задачи, а не
// захватом: повторный захват погасил бы токены прошлого прогона.
type execState struct {
	role       string
	allow      []string
	deny       []string
	policySet  bool
	execution  *executionView
	hasExecKey bool
}

func readExecState(ctx context.Context, c *Client, task string) (execState, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/tasks/"+url.PathEscape(task)+"/state", nil, &raw); err != nil {
		return execState{}, fmt.Errorf("состояние задачи %s: %w", task, err)
	}
	var s struct {
		Role *struct {
			Key        string          `json:"key"`
			ToolPolicy json.RawMessage `json:"tool_policy"`
		} `json:"role"`
		Execution *executionView `json:"execution"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return execState{}, refuse("состояние задачи %s не разбирается: %v", task, err)
	}
	out := execState{execution: s.Execution, hasExecKey: s.Execution != nil}
	if s.Role != nil {
		out.role = s.Role.Key
		var p struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		}
		if json.Unmarshal(s.Role.ToolPolicy, &p) == nil {
			out.allow, out.deny = p.Allow, p.Deny
			out.policySet = len(p.Allow)+len(p.Deny) > 0
		}
	}
	return out, nil
}

// workPrompt — стартовая инструкция этапа: ответ GET …/prompts/work как
// есть (тот же текст печатает clab prompt work). Сервер без ручки — отказ с
// версией сервера.
func workPrompt(ctx context.Context, c *Client, task string) (string, error) {
	raw, _, err := c.getContent(ctx, "/tasks/"+url.PathEscape(task)+"/prompts/work")
	if routeMissing(err) {
		return "", refuse("%v", c.oldServerRefusal(ctx, "clab exec", "стартовую инструкцию этапа", task))
	}
	if err != nil {
		return "", fmt.Errorf("стартовая инструкция %s: %w", task, err)
	}
	var out struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Text == "" {
		return "", refuse("%v", c.oldServerRefusal(ctx, "clab exec", "стартовую инструкцию этапа", task))
	}
	return out.Text, nil
}

type planInput struct {
	task, project string
	flags         execFlags
	command       []string
	wrapped       bool
	bindings      []agentBinding
	state         execState
	prompt        string
	dir           taskDir
	origin        string
	environ       []string
	workdir       string
}

// launchPlan — собранный запуск: что печатается перед стартом и в сухом
// прогоне, и что запускается. Токена в полях, кроме env, нет.
type launchPlan struct {
	agent, agentFrom string
	noBindings       bool
	model            modelPick
	effort           string
	effortFrom       string
	settingsVersion  *int
	level            string
	argv             []string
	env              []string
	setVars          []string
	removed          []string
	tokenInEnv       bool
	mcpConfig        string
	mcpURL           string
	promptOnStdin    bool
	prompt           string
	promptBytes      int
	hookScript       string
	hookLine         string
	warnings         []string
}

// sourceExplicit — модель из --model (0077 п. 7, шаг 1).
const sourceExplicit = "explicit"

func planLaunch(in planInput) (*launchPlan, error) {
	p := &launchPlan{noBindings: in.bindings == nil}
	agent, from, err := chooseAgent(in.flags.agent, in.command, in.bindings)
	if err != nil {
		return nil, err
	}
	p.agent, p.agentFrom = agent, from
	binding := findBinding(in.bindings, agent)

	template := in.command
	if !in.wrapped {
		if binding == nil || len(binding.Run) == 0 {
			return nil, refuse("команда агента не задана: у привязки %s нет шаблона запуска — clab exec %s --dir <каталог> -- <команда>", agent, in.task)
		}
		template = binding.Run
	}

	// Модель: --model, запись версии-снимка, таблица привязки, по умолчанию.
	var models map[string]modelChoice
	if e := in.state.execution; e != nil {
		p.level = ptr(e.ModelLevel)
		models = e.Models
		var n int
		if len(e.AgentSettingsVersion) > 0 && string(e.AgentSettingsVersion) != "null" && json.Unmarshal(e.AgentSettingsVersion, &n) == nil {
			p.settingsVersion = &n
		}
	}
	if in.flags.model != "" {
		p.model = modelPick{Agent: agent, Model: in.flags.model, Source: sourceExplicit}
	} else {
		p.model = pickModel(p.level, agent, models, binding)
	}
	switch {
	case in.flags.effort != "":
		p.effort, p.effortFrom = in.flags.effort, sourceExplicit
	case p.model.Source == sourceAgentSettings && p.model.Effort != "":
		p.effort, p.effortFrom = p.model.Effort, sourceAgentSettings
	}

	usesMCP := launch.Uses(template, launch.SlotMCPConfig) || launch.Uses(template, launch.SlotMCPURL)
	p.mcpURL = in.origin + "/mcp/projects/" + in.project
	if launch.Uses(template, launch.SlotMCPConfig) {
		p.mcpConfig = filepath.Join(in.dir.runDir, "mcp.json")
	}
	p.promptOnStdin = !launch.Uses(template, launch.SlotPrompt)
	p.prompt, p.promptBytes = in.prompt, len(in.prompt)

	hookScript := ""
	switch h := hookOfBinding(binding); {
	case h != nil && h.Placement == "launch" && launch.Uses(template, launch.SlotHookScript) && h.Command != "":
		hookScript = h.Command
		p.hookScript = h.Command
		p.hookLine = fmt.Sprintf("хук-страховка крепится к запуску: %s на %s, сценарий агента %s (%d байт)", dash(h.Event), dash(h.Matcher), dash(h.Agent), len(h.Command))
	case h != nil && h.Placement == "launch" && launch.Uses(template, launch.SlotHookScript):
		p.hookLine = "хук-страховка не крепится: сервер не отдал сценарий хука привязки — изоляция окружением"
		p.warnings = append(p.warnings, "у привязки "+agent+" хук при запуске, а сценария в наборе инстанса нет: команда идёт без хука")
	case h != nil && h.Placement == "launch":
		p.hookLine = "хук-страховка не крепится: в команде нет " + launch.SlotHookScript + " — изоляция окружением"
		p.warnings = append(p.warnings, "привязка "+agent+" крепит хук к запуску, а своя команда не несёт "+launch.SlotHookScript+": хук не крепится")
	default:
		p.hookLine = "хук-страховка не крепится: изоляция окружением"
	}

	values := map[string]string{
		launch.SlotModel:           p.model.Model,
		launch.SlotEffort:          p.effort,
		launch.SlotMCPConfig:       p.mcpConfig,
		launch.SlotMCPURL:          p.mcpURL,
		launch.SlotConfigDir:       in.dir.clabDir,
		launch.SlotRunDir:          in.dir.runDir,
		launch.SlotAllowedTools:    strings.Join(in.state.allow, ","),
		launch.SlotDisallowedTools: strings.Join(in.state.deny, ","),
		launch.SlotTask:            in.task,
		launch.SlotRole:            in.state.role,
		launch.SlotWorkdir:         in.workdir,
		launch.SlotPrompt:          in.prompt,
	}
	if hookScript != "" {
		values[launch.SlotHookScript] = launch.Quoted(hookScript)
	}
	if !usesMCP {
		values[launch.SlotMCPURL], values[launch.SlotMCPConfig] = "", ""
	}
	p.argv = launch.Expand(template, values)
	if len(p.argv) == 0 {
		return nil, refuse("команда агента после подстановок пуста")
	}

	if p.model.Model != "" && !launch.Uses(template, launch.SlotModel) {
		p.warnings = append(p.warnings, "модель не передана: в команде нет "+launch.SlotModel+" — агент возьмёт свою модель по умолчанию")
	}
	if in.state.policySet {
		translated := binding != nil && binding.ToolPolicy &&
			(launch.Uses(template, launch.SlotAllowedTools) || launch.Uses(template, launch.SlotDisallowedTools))
		if !translated {
			p.warnings = append(p.warnings, "tool_policy роли не переведён для привязки "+agent+": агент запущен без ограничений по тулам")
		}
	}

	p.env, p.setVars, p.removed = execEnv(in.environ, in.bindings, in.dir, in.project, usesMCP)
	p.tokenInEnv = usesMCP
	return p, nil
}

func hookOfBinding(b *agentBinding) *bindingHook {
	if b == nil {
		return nil
	}
	return b.Hook
}

// chooseAgent — привязка запуска (0077 п. 8): --agent, имя программы
// команды, признак окружения запустившего (ровно один), иначе отказ.
func chooseAgent(flagAgent string, command []string, bindings []agentBinding) (agent, from string, err error) {
	names := make([]string, 0, len(bindings))
	for _, b := range bindings {
		names = append(names, b.Name)
	}
	if bindings == nil {
		names = []string{bindingClaude, bindingCodex, bindingGeneric}
	}
	if flagAgent != "" {
		if !slices.Contains(names, flagAgent) {
			return "", "", refuse("неизвестная привязка --agent %q: есть %s", flagAgent, strings.Join(names, ", "))
		}
		return flagAgent, "флаг --agent", nil
	}
	if len(command) > 0 {
		prog := strings.ToLower(strings.TrimSuffix(filepath.Base(command[0]), ".exe"))
		for _, name := range []string{bindingClaude, bindingCodex} {
			if prog == name && slices.Contains(names, name) {
				return name, "имя программы " + prog, nil
			}
		}
		return bindingGeneric, "программа " + prog + " — не claude и не codex", nil
	}
	found := detectAgent(agentEnv, bindings)
	if len(found) == 1 {
		return found[0], "признак окружения запустившего", nil
	}
	what := "ни одного признака агента"
	if len(found) > 1 {
		what = "признаки нескольких агентов: " + strings.Join(found, ", ")
	}
	return "", "", refuse("агент не узнан (%s): задайте --agent (%s) или команду после --", what, strings.Join(names, ", "))
}

// execEnv — окружение агента (R3): наследство оркестратора без SDLC_*,
// CLAB_* и признаков агентов; CLAB_CONFIG_DIR и SDLC_PROJECT задачи;
// SDLC_TOKEN — только для MCP по ссылке на переменную. set — имена
// заданных, removed — имена вычищенных (для вывода, без значений).
func execEnv(environ []string, bindings []agentBinding, dir taskDir, project string, withToken bool) (env, set, removed []string) {
	markers := append([]string{}, knownAgentEnv...)
	for _, b := range bindings {
		for _, c := range b.detect {
			markers = append(markers, c.env)
		}
	}
	strip := func(name string) bool {
		up := name
		if runtime.GOOS == "windows" {
			up = strings.ToUpper(name)
		}
		if strings.HasPrefix(up, "SDLC_") || strings.HasPrefix(up, "CLAB_") {
			return true
		}
		for _, m := range markers {
			if name == m || (runtime.GOOS == "windows" && strings.EqualFold(name, m)) {
				return true
			}
		}
		return false
	}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strip(name) {
			if !slices.Contains(removed, name) {
				removed = append(removed, name)
			}
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "CLAB_CONFIG_DIR="+dir.clabDir, "SDLC_PROJECT="+project)
	set = []string{"CLAB_CONFIG_DIR=" + dir.clabDir, "SDLC_PROJECT=" + project}
	if withToken {
		env = append(env, "SDLC_TOKEN="+dir.token)
		set = append(set, "SDLC_TOKEN=<токен задачи>")
	}
	sort.Strings(removed)
	return env, set, removed
}

// writeMCPConfig — один сервер sdlc со ссылкой на переменную, а не с
// токеном: файл в каталоге захода, права только владельца.
func writeMCPConfig(path, mcpURL string) error {
	cfg := mcpConfig("sdlc", mcpURL, "Authorization", "Bearer ${SDLC_TOKEN}")
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// print — строки запуска: агент, модель, хук, MCP, окружение, инструкция,
// команда, предупреждения. Токен заменяется словами и на всякий случай
// вычищается из каждой строки.
func (p *launchPlan) print(w io.Writer, token string) {
	line := func(format string, a ...any) {
		s := fmt.Sprintf(format, a...)
		if token != "" {
			s = strings.ReplaceAll(s, token, "<токен задачи>")
		}
		fmt.Fprintln(w, "clab exec: "+s)
	}
	agent := "агент " + p.agent + " (" + p.agentFrom + ")"
	if p.noBindings {
		agent += "; привязок нет — набор инстанса их не отдаёт"
	}
	line("%s", agent)
	line("%s", p.modelLine())
	line("%s", p.hookLine)
	switch {
	case p.mcpConfig != "":
		line("MCP: только sdlc на токене задачи — %s (права 0600, ${SDLC_TOKEN} вместо токена), %s", p.mcpConfig, p.mcpURL)
	case p.tokenInEnv:
		line("MCP: только sdlc на токене задачи — %s, токен из переменной SDLC_TOKEN", p.mcpURL)
	default:
		line("MCP: нет — команда не подключает сервер, SDLC_TOKEN не передаётся")
	}
	line("окружение: %s; убраны: %s", strings.Join(p.setVars, ", "), joinOr(p.removed, "нечего"))
	how := "аргументом " + launch.SlotPrompt
	if p.promptOnStdin {
		how = "на stdin"
	}
	line("инструкция: %s, %d байт (clab prompt work и заметка --note-file, если задана)", how, p.promptBytes)
	line("команда: %s", p.displayArgv())
	for _, warn := range p.warnings {
		line("предупреждение: %s", warn)
	}
}

func (p *launchPlan) modelLine() string {
	m := p.model
	effort := ""
	if p.effort != "" {
		effort = ", усилие " + p.effort
		if p.effortFrom != m.Source {
			effort += " (флаг --effort)"
		}
	}
	version := "настройки агентов нет"
	if p.settingsVersion != nil {
		version = "настройка агентов v" + strconv.Itoa(*p.settingsVersion)
	}
	level := p.level
	if level == "" {
		level = "не задан"
	}
	switch m.Source {
	case sourceExplicit:
		return fmt.Sprintf("модель %s%s (%s: флаг --model; уровень %s)", m.Model, effort, sourceExplicit, level)
	case sourceAgentSettings:
		return fmt.Sprintf("модель %s%s (%s: агент %s, %s, уровень %s)", m.Model, effort, sourceAgentSettings, m.Agent, version, level)
	case sourceBinding:
		return fmt.Sprintf("модель %s%s (%s: таблица уровней привязки %s, уровень %s; %s)", m.Model, effort, sourceBinding, m.Agent, level, version)
	}
	return fmt.Sprintf("модель агента по умолчанию%s (%s: флаг модели не передаётся; уровень %s, %s)", effort, sourceAgentDefault, level, version)
}

// displayArgv — команда для вывода: инструкция и сценарий хука — размером,
// слова с пробелами и кавычками — в кавычках.
func (p *launchPlan) displayArgv() string {
	words := make([]string, 0, len(p.argv))
	quotedScript := ""
	if p.hookScript != "" {
		quotedScript = launch.Quoted(p.hookScript)
	}
	for _, a := range p.argv {
		if !p.promptOnStdin && a == p.prompt {
			words = append(words, fmt.Sprintf("<инструкция: %d байт>", p.promptBytes))
			continue
		}
		if quotedScript != "" && strings.Contains(a, quotedScript) {
			a = strings.ReplaceAll(a, quotedScript, fmt.Sprintf(`"<сценарий хука: %d байт>"`, len(p.hookScript)))
		}
		if a == "" || strings.ContainsAny(a, " \t\n\"'$`\\;&|<>(){}*?[]#~") {
			a = strconv.Quote(a)
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}
