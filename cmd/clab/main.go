// Команда clab — CLI для людей, CI и не-MCP клиентов.
//
// Те же операции, что и у MCP-тулов, только транспорт другой: локальный агент
// ходит по MCP, человек и скрипт сборки — сюда. Логики цикла здесь нет и быть
// не должно.
//
// Куда ходить и кем, clab узнаёт из конфига ~/.config/clab/config.json,
// который заполняет «clab auth login» (решение 0032). Окружение перекрывает
// конфиг — так clab работает в CI:
//
//	SDLC_API_URL   адрес API           (иначе хост из конфига; без него хост не настроен)
//	SDLC_TOKEN     токен доступа        (иначе токен хоста из конфига; без него — заголовок актора)
//	SDLC_ACTOR     кто действует        (режим заголовка; по умолчанию agent:$USER)
//	SDLC_PROJECT   проект по умолчанию  (иначе проект по git remote текущего каталога)
//
// Общие флаги: --host <адрес> выбирает хост из конфига, --json печатает ответ
// сервера как есть вместо таблицы.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type command struct {
	name    string
	summary string
	usage   string
	run     func(ctx context.Context, c *Client, args []string) error
}

func commands() []command {
	return []command{
		{"auth", "вход через браузер, состояние, выход", "clab auth login [--host https://sdlc.example.com] [--days 365] [--no-browser] | clab auth status | clab auth logout | clab auth token", cmdAuth},
		{"mcp", "подключить локального агента к MCP проекта", "clab mcp install [--project SDLC] [--name sdlc] [--scope local|user] | clab mcp config | clab mcp url", cmdMCP},
		{"setup", "связать локальный проект с проектом: MCP и набор агента (скилы) с инстанса", "clab setup [--project SDLC] [--scope local|user] [--agent claude|other] [--name sdlc] [--dry-run] [--force] — в корне локального клона; повторный запуск обновляет набор до версии инстанса", cmdSetup},
		{"whoami", "кто я, на каком инстансе и что могу в проекте", "clab whoami [--project SDLC]", cmdWhoami},
		{"queue", "что сейчас можно забрать", "clab queue [--role reviewer] [--status analysis]", cmdQueue},
		{"tasks", "задачи проекта", "clab tasks [--status analysis] [--type epic] [--parent SDLC-4] [--external PROJ-12 | --external jira:PROJ-12]", cmdTasks},
		{"show", "состояние задачи глазами агента", "clab show SDLC-42", cmdShow},
		{"artifact", "содержимое артефакта: входы этапа без MCP", "clab artifact <id> [--json] — id из prior_artifacts ответа clab claim или clab show", cmdArtifact},
		{"create", "завести задачу или эпик", "clab create --title \"Раннер\" [--type epic] [--parent SDLC-4] [--repos backend] [--blocks SDLC-2] [--assignee agent:bot] [--external-ref jira:PROJ-12=https://jira.example.com/browse/PROJ-12] [--model-level medium] [--model-level-for testing=low ...]", cmdCreate},
		{"edit", "поправить постановку задачи", "clab edit SDLC-42 [--title ...] [--description ... | --description - | --description-file d.md] [--priority high] [--labels bug,phase8] [--external-ref jira:PROJ-12=https://… | --external-ref \"\"] [--model-level low|medium|high | --model-level \"\"] [--model-level-for development=high ... | --model-level-for development=] (пустое значение снимает уровень)", cmdEdit},
		{"relate", "связать задачи порядком или контекстом", "clab relate SDLC-5 --to SDLC-6 [--rel blocks] [--remove]", cmdRelate},
		{"assign", "назначить задачу исполнителю", "clab assign SDLC-5 --to agent:runner [--clear]", cmdAssign},
		{"usage", "отчитаться о стоимости прогона или показать её", "clab usage SDLC-5 --model claude-sonnet-5 --input 1200 --output 800 --cost 0.42 | clab usage SDLC-4 --show", cmdUsage},
		{"claim", "забрать задачу и получить контекст этапа: инструкцию роли, что сдать, входы", "clab claim SDLC-42 [--session ref] [--instructions-file role.md]", cmdClaim},
		{"heartbeat", "продлить аренду: один раз, по интервалу или пока работает долгая команда", "clab heartbeat SDLC-42 | clab heartbeat SDLC-42 --every 5m [--for 2h] | clab heartbeat SDLC-42 [--every 5m] -- <команда> [аргументы] (код выхода — код команды; без --every интервал из ответа сервера)", cmdHeartbeat},
		{"release", "отпустить задачу, не завершив этап", "clab release SDLC-42 [--note ...]", cmdRelease},
		{"submit", "сдать артефакт", "clab submit SDLC-42 --kind analysis.brief --title ... --file brief.md", cmdSubmit},
		{"link", "привязать PR, ветку или коммит", "clab link SDLC-42 --repo acme/api --ref 128 [--url ...] [--state open]", cmdLink},
		{"scope", "расширить или сузить область задачи", "clab scope SDLC-42 [--add acme/web] [--remove acme/api] --reason ...", cmdScope},
		{"gates", "что мешает переходу", "clab gates SDLC-42 [--transition analysis_done]", cmdGates},
		{"transition", "запросить переход", "clab transition SDLC-42 --to analysis_done [--summary ...] [--artifact kind=file]", cmdTransition},
		{"decide", "закрыть гейт решением", "clab decide SDLC-42 --transition analysis_done --gate human_approval [--status passed]", cmdDecide},
		{"search", "поиск по коду, артефактам и документам проекта", "clab search \"ротация токенов\" [--scope code] [--repo acme/api] [--top 10]", cmdSearch},
		{"index", "состояние индекса репозиториев", "clab index [--project SDLC]", cmdIndex},
		{"drift", "открытые расхождения задачи", "clab drift SDLC-42", cmdDrift},
		{"resolve-drift", "закрыть сигнал дрейфа", "clab resolve-drift <signal-id> --resolution resolved [--note ...]", cmdResolveDrift},
		{"metrics", "метрики цикла: где застревают задачи, какие гейты валятся", "clab metrics [--period 30d]", cmdMetrics},
		{"audit", "журнал действий: кто, каким токеном, каким тулом или ручкой, с каким исходом", "clab audit SDLC-42 | clab audit --project [SDLC] [--task SDLC-42] | clab audit --instance — фильтры: [--principal user:alice] [--token <id|префикс>] [--channel mcp|rest] [--action sdlc_submit_artifact] [--outcome forbidden] [--since 2026-10-01] [--until 2026-10-21] [--limit 50] [--cursor <курсор>]", cmdAudit},

		{"me", "кто я по токену, где состою", "clab me", cmdMe},
		{"token", "выдать, перечислить или отозвать токен; токен задачи исполнителю этапа", "clab token create --name ci [--for agent:bot] [--project SDLC] [--roles reviewer] [--days 90] | clab token list [--project SDLC] | clab token revoke <id> | clab token task <ключ> [--config-dir <каталог>] (на вашу живую аренду, гаснет вместе с ней; с --config-dir токен не печатается, а пишется конфигом clab в каталог — для CLAB_CONFIG_DIR исполнителя)", cmdToken},
		{"members", "участники проекта и их уровни", "clab members [list] | clab members set user:alice agent [--roles developer,reviewer] | clab members remove user:alice", cmdMembers},
		{"team", "команды: организации, состав, тариф (на общем инстансе), группы провайдера, права на проекты", "clab team list | clab team show acme | clab team create acme --name Acme | clab team convert <личная> --key acme | clab team plan acme [--set business] | clab team github acme | clab team add acme user:alice --role lead | clab team groups acme --set sdlc-platform | clab team grant acme --project SDLC --level maintainer [--roles reviewer]", cmdTeam},
		{"invite", "приглашения ссылкой в команду или проект: выдать, список, отозвать", "clab invite create --team acme [--role lead] [--days 7] | clab invite create --project SDLC --level agent [--roles developer] [--days 7] | clab invite list --team acme | clab invite list --project SDLC | clab invite revoke <id>", cmdInvite},
		{"project", "проекты: список, создание, удаление, команды и доступ к GitHub", "clab project list | clab project create ACME --name \"Проект\" [--team platform] | clab project delete ACME [--confirm ACME] | clab project teams | clab project github --installation 42 | clab project github --check", cmdProject},

		{"export", "выгрузить проект связкой: репозитории, роли, документы, цикл", "clab export [--project SDLC] > bundle.json", cmdExport},
		{"import", "применить связку к проекту или раздать её нескольким", "clab import --file bundle.json [--dry-run] | clab import --from SDLC --to ACME,WEB", cmdImport},
		{"import-guide", "инструкция агенту по переносу правил команды", "clab import-guide", cmdImportGuide},
		{"repo", "привязать репозиторий к проекту или отвязать", "clab repo add acme/api [--hint backend] [--branch main] [--installation 42]\n  clab repo remove acme/api", cmdRepo},
		{"role", "показать роль или завести версию инструкции", "clab role show analyst | clab role put --key analyst --name Аналитик --file instructions.md [--contract contract.json] [--model-level medium] (версия собирается целиком: без --model-level у неё уровня нет)", cmdRole},
		{"doc", "проектные документы", "clab doc list | clab doc put --kind guideline --title Соглашения --file CLAUDE.md | clab doc get <id>", cmdDoc},
		{"workflow", "версии цикла: список, проверка, активация черновика", "clab workflow list | clab workflow validate <id> | clab workflow activate <id>", cmdWorkflow},
		{"version", "версия clab и сервера", "clab version", cmdVersion},

		{"server", "self-hosted installation on this machine: install, upgrade, backup, restore, status, starter", "clab server install|upgrade|backup|restore|status|starter [--dir DIR] — clab server --help", cmdServer},
	}
}

// localCommand — команда работает с этой машиной, а не с API: клиент не
// строится, и вход не нужен (clab server управляет установкой, на которой
// API может ещё не быть или уже не работать).
func localCommand(name string) bool { return name == "server" }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, args := splitGlobals(os.Args[1:])
	jsonOut = g.json
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
		return
	}
	if args[0] == "--version" {
		args[0] = "version"
	}

	name := args[0]
	for _, cmd := range commands() {
		if cmd.name != name {
			continue
		}
		var c *Client
		if !localCommand(name) {
			var err error
			if c, err = NewClient(g); err != nil {
				exit(err)
			}
		}
		if err := cmd.run(ctx, c, args[1:]); err != nil {
			exit(err)
		}
		return
	}

	fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n", name)
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Fprint(os.Stderr, "clab — Cadence Lab: работа с циклом разработки из CI и скриптов\n\n")
	for _, cmd := range commands() {
		fmt.Fprintf(os.Stderr, "  %-14s %s\n", cmd.name, cmd.summary)
	}
	fmt.Fprintln(os.Stderr, "\nclab <команда> --help — аргументы команды")
	fmt.Fprintln(os.Stderr, "\nОбщие флаги: --host <адрес> — хост из конфига, --json — ответ сервера как есть.")
	fmt.Fprintln(os.Stderr, "Начало работы: clab auth login --host https://sdlc.example.com; конфиг — ~/.config/clab.")
	fmt.Fprintln(os.Stderr, "Окружение перекрывает конфиг: SDLC_API_URL, SDLC_TOKEN (без него действующее")
	fmt.Fprintln(os.Stderr, "лицо берётся из SDLC_ACTOR), SDLC_PROJECT — иначе проект по git remote.")
}

// version подставляется при сборке: -ldflags "-X main.version=…".
// Сверка с сервером и clab version — в version.go.
var version = "dev"

// exit печатает ошибку домена так, чтобы её можно было разобрать скриптом:
// код ошибки уходит в stderr первым словом, подробности — JSON'ом.
func exit(err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		fmt.Fprintf(os.Stderr, "%s: %s\n", apiErr.Code, apiErr.Message)
		if len(apiErr.Details) > 0 {
			var pretty any
			if json.Unmarshal(apiErr.Details, &pretty) == nil {
				body, _ := json.MarshalIndent(pretty, "", "  ")
				fmt.Fprintln(os.Stderr, string(body))
			}
		}
		if apiErr.Code == "unauthenticated" {
			fmt.Fprintln(os.Stderr, "Войти: clab auth login --host <адрес>; входы — clab auth status.")
		}
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// popTask отделяет ключ задачи от флагов.
//
// Пакет flag прекращает разбор на первом позиционном аргументе, а ключ задачи
// читается первым: "clab claim SDLC-42 --session x" должно работать, потому
// что именно так эту команду и пишут.
func popTask(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// flags собирает набор флагов команды и печатает её строку использования,
// если попросили --help.
func flags(cmd string, args []string, setup func(*flag.FlagSet)) (*flag.FlagSet, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	for _, c := range commands() {
		if c.name == cmd {
			fs.Usage = func() {
				fmt.Fprintf(os.Stderr, "%s\n\n%s\n\n", c.summary, c.usage)
				fs.PrintDefaults()
			}
		}
	}
	setup(fs)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return fs, nil
}

func requireTask(fs *flag.FlagSet, task string) error {
	if task == "" {
		fs.Usage()
		return fmt.Errorf("не указан ключ задачи")
	}
	return nil
}

// ── команды ─────────────────────────────────────────────────────────────────

// cmdWhoami отвечает всегда, когда есть вход: кто я, на каком инстансе и как
// предъявлен. Проект — дополнительная секция: задан — уровень доступа и роли,
// которые можно забирать; не задан — строка с подсказкой и код выхода 0.
// Роли и уровень считает сервер (поле access в GET /projects/:key), CLI их
// только раскладывает. Сервер старше этих полей или без /instance —
// соответствующих строк просто нет.
func cmdWhoami(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	_, err := flags("whoami", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
	})
	if err != nil {
		return err
	}
	key, err := c.optionalProject(ctx, projectFlag)
	if err != nil {
		return err
	}

	var me struct {
		Principal struct {
			Kind   string `json:"kind"`
			Handle string `json:"handle"`
		} `json:"principal"`
		Authenticated bool `json:"authenticated"`
		// Токен задачи (решение 0049): к какой задаче привязан SDLC_TOKEN.
		Token *struct {
			TokenKind string   `json:"token_kind"`
			TaskKey   string   `json:"task_key"`
			RoleKeys  []string `json:"role_keys"`
		} `json:"token"`
	}
	if err := c.get(ctx, "/auth/me", nil, &me); err != nil {
		return err
	}
	who := me.Principal.Kind + ":" + me.Principal.Handle
	var taskToken map[string]any
	if me.Token != nil && me.Token.TokenKind == "task" {
		taskToken = map[string]any{"task_key": me.Token.TaskKey, "role_keys": me.Token.RoleKeys}
	}

	// /instance появилась позже /auth/me: её отсутствие — не повод молчать
	// про всё остальное.
	var instance *instanceInfo
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var info instanceInfo
	if err := c.get(qctx, "/instance", nil, &info); err == nil {
		instance = &info
	}
	cancel()

	var project *whoamiProject
	if key != "" {
		if project, err = c.whoamiProject(ctx, key); err != nil {
			return err
		}
	}

	out := map[string]any{"principal": who, "authenticated": me.Authenticated, "api": c.base,
		"instance": instance, "project": project}
	if taskToken != nil {
		out["task_token"] = taskToken
	}
	return emit(out, func() error {
		how := "по токену"
		if !me.Authenticated {
			how = "по заголовку"
		}
		pairs := []string{"Кто:", who + " (" + how + ")", "API:", c.base}
		if taskToken != nil {
			pairs = append(pairs, "Токен:", "задачи "+me.Token.TaskKey+", роль "+joinOr(me.Token.RoleKeys, "—"))
		}
		if instance != nil {
			line := dash(instance.Version)
			if instance.Tenancy != "" {
				line += ", профиль " + instance.Tenancy
			}
			pairs = append(pairs, "Инстанс:", line)
		}
		if project == nil {
			pairs = append(pairs, "Проект:", "не задан — --project, SDLC_PROJECT или каталог привязанного репозитория")
			kv(pairs...)
			return nil
		}
		pairs = append(pairs, "Проект:", project.Key+" — "+project.Name)
		if a := project.Access; a != nil {
			roles := "любые"
			if a.AllowedRoles != nil {
				roles = joinOr(*a.AllowedRoles, "ни одной")
			}
			pairs = append(pairs, "Уровень:", dash(a.Level), "Можно забирать:", roles)
		}
		if project.Roles != nil {
			pairs = append(pairs, "Роли проекта:", joinOr(project.Roles, "нет"))
		}
		kv(pairs...)
		return nil
	})
}

// whoamiProject — секция проекта в clab whoami. Access указателем: у сервера
// до CLAB-146 поля нет, и это не «ни одной роли». AllowedRoles указателем:
// null — любые, [] — ни одной.
type whoamiProject struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Access *struct {
		Level        string    `json:"level"`
		AllowedRoles *[]string `json:"allowed_roles"`
	} `json:"access"`
	Roles []string `json:"roles"`
}

func (c *Client) whoamiProject(ctx context.Context, key string) (*whoamiProject, error) {
	var view struct {
		Project struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"project"`
		Access json.RawMessage `json:"access"`
	}
	if err := c.get(ctx, "/projects/"+url.PathEscape(key), nil, &view); err != nil {
		return nil, err
	}
	p := &whoamiProject{Key: view.Project.Key, Name: view.Project.Name}
	if len(view.Access) > 0 && string(view.Access) != "null" {
		if err := decode(view.Access, &p.Access); err != nil {
			return nil, err
		}
	}
	var roles struct {
		Roles []struct {
			Key string `json:"key"`
		} `json:"roles"`
	}
	if err := c.get(ctx, "/projects/"+url.PathEscape(key)+"/roles", nil, &roles); err != nil {
		return nil, err
	}
	p.Roles = []string{}
	for _, r := range roles.Roles {
		p.Roles = append(p.Roles, r.Key)
	}
	return p, nil
}

func cmdQueue(ctx context.Context, c *Client, args []string) error {
	var projectFlag, role, status string
	_, err := flags("queue", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&role, "role", "", "оставить работу только для этой роли")
		fs.StringVar(&status, "status", "", "оставить только этот статус")
	})
	if err != nil {
		return err
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}

	q := url.Values{}
	if role != "" {
		q.Set("role", role)
	}
	if status != "" {
		q.Set("status", status)
	}

	var out json.RawMessage
	if err := c.get(ctx, "/projects/"+key+"/queue", q, &out); err != nil {
		return err
	}
	return emit(out, render(renderQueue, out))
}

func cmdTasks(ctx context.Context, c *Client, args []string) error {
	var projectFlag, status, taskType, parent, external string
	_, err := flags("tasks", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&status, "status", "", "фильтр по статусу")
		fs.StringVar(&taskType, "type", "", "фильтр по типу задачи, например epic")
		fs.StringVar(&parent, "parent", "", "только дочерние задачи этой")
		fs.StringVar(&external, "external", "", "только задачи со ссылкой на этот тикет: PROJ-12 или jira:PROJ-12")
	})
	if err != nil {
		return err
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}

	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if taskType != "" {
		q.Set("type", taskType)
	}
	if parent != "" {
		q.Set("parent", parent)
	}
	if external != "" {
		q.Set("external", external)
	}

	var out json.RawMessage
	if err := c.get(ctx, "/projects/"+key+"/tasks", q, &out); err != nil {
		return err
	}
	return emit(out, render(renderTasks, out))
}

func cmdShow(ctx context.Context, c *Client, args []string) error {
	task, rest := popTask(args)
	fs, err := flags("show", rest, func(*flag.FlagSet) {})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}

	var out json.RawMessage
	if err := c.get(ctx, "/tasks/"+task+"/state", nil, &out); err != nil {
		return err
	}
	return emit(out, render(renderState, out))
}

func cmdClaim(ctx context.Context, c *Client, args []string) error {
	var session, instructionsFile string
	task, rest := popTask(args)
	fs, err := flags("claim", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&session, "session", "", "идентификатор сессии агента")
		fs.StringVar(&instructionsFile, "instructions-file", "", "записать инструкцию роли в этот файл вместо вывода")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if session == "" {
		host, _ := os.Hostname()
		session = host + "/clab-cli"
	}

	var out json.RawMessage
	if err := c.post(ctx, "/tasks/"+task+"/claim",
		map[string]any{"session_ref": session}, &out); err != nil {
		return err
	}
	// Аренда уже взята: что бы ни случилось с файлом, сводка с прогоном
	// печатается первой, чтобы агент мог отпустить задачу.
	if jsonOut {
		if err := print(out); err != nil {
			return err
		}
		if instructionsFile == "" {
			return nil
		}
		var s stateView
		if err := decode(out, &s); err != nil {
			return err
		}
		_, err := writeInstructions(s, instructionsFile)
		return err
	}
	return renderClaim(out, instructionsFile)
}

func cmdRelease(ctx context.Context, c *Client, args []string) error {
	var note string
	task, rest := popTask(args)
	fs, err := flags("release", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&note, "note", "", "почему отпускаем")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}

	var out any
	if err := c.post(ctx, "/tasks/"+task+"/release",
		map[string]any{"note": note}, &out); err != nil {
		return err
	}
	return print(out)
}

func cmdSubmit(ctx context.Context, c *Client, args []string) error {
	var kind, title, format, file, content string
	task, rest := popTask(args)
	fs, err := flags("submit", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&kind, "kind", "", "kind из output_contract роли")
		fs.StringVar(&title, "title", "", "заголовок артефакта")
		fs.StringVar(&format, "format", "", "markdown | json | yaml | diff | text | html")
		fs.StringVar(&file, "file", "", "файл с содержимым; - читает stdin")
		fs.StringVar(&content, "content", "", "содержимое строкой")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if kind == "" {
		return fmt.Errorf("не указан --kind")
	}

	body, err := readContent(file, content)
	if err != nil {
		return err
	}
	if title == "" {
		title = kind
	}
	if format == "" {
		format = guessFormat(file)
	}

	var out json.RawMessage
	if err := c.post(ctx, "/tasks/"+task+"/artifacts", map[string]any{
		"kind": kind, "title": title, "format": format, "content": string(body),
	}, &out); err != nil {
		return err
	}
	return emit(out, render(renderArtifact, out))
}

func cmdLink(ctx context.Context, c *Client, args []string) error {
	var repo, ref, refType, link, state string
	task, rest := popTask(args)
	fs, err := flags("link", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&repo, "repo", "", "owner/name, имя или role_hint репозитория")
		fs.StringVar(&ref, "ref", "", "номер PR, имя ветки или коммит")
		fs.StringVar(&refType, "type", "pull_request", "pull_request | branch | commit")
		fs.StringVar(&link, "url", "", "ссылка на PR")
		fs.StringVar(&state, "state", "", "open | merged | closed")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if repo == "" || ref == "" {
		return fmt.Errorf("нужны --repo и --ref")
	}

	var out any
	if err := c.post(ctx, "/tasks/"+task+"/repo-refs", map[string]any{
		"repository": repo, "ref": ref, "ref_type": refType,
		"url": link, "state": state,
	}, &out); err != nil {
		return err
	}
	return print(out)
}

func cmdScope(ctx context.Context, c *Client, args []string) error {
	var add, remove, reason string
	task, rest := popTask(args)
	fs, err := flags("scope", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&add, "add", "", "репозитории через запятую: добавить в область")
		fs.StringVar(&remove, "remove", "", "репозитории через запятую: убрать из области (нужен уровень maintainer)")
		fs.StringVar(&reason, "reason", "", "почему область меняется")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if add == "" && remove == "" {
		return fmt.Errorf("не указан ни --add, ни --remove")
	}

	var out any
	if err := c.do(ctx, "PATCH", "/tasks/"+task+"/scope", map[string]any{
		"add_repositories": splitList(add), "remove_repositories": splitList(remove),
		"note": reason,
	}, &out); err != nil {
		return err
	}
	return print(out)
}

func cmdGates(ctx context.Context, c *Client, args []string) error {
	var transition string
	task, rest := popTask(args)
	fs, err := flags("gates", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&transition, "transition", "", "конкретный переход")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}

	var raw json.RawMessage
	if err := c.get(ctx, "/tasks/"+task+"/transitions", nil, &raw); err != nil {
		return err
	}
	var out struct {
		Transitions []json.RawMessage `json:"transitions"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	var items []transitionPreview
	if err := decode(raw, &struct {
		Transitions *[]transitionPreview `json:"transitions"`
	}{&items}); err != nil {
		return err
	}
	if transition == "" {
		return emit(out.Transitions, func() error { return renderTransitions(items) })
	}
	for i, t := range items {
		if t.Key == transition {
			return emit(out.Transitions[i], func() error { return renderTransitions(items[i : i+1]) })
		}
	}
	return fmt.Errorf("перехода %q нет из текущего статуса", transition)
}

func cmdTransition(ctx context.Context, c *Client, args []string) error {
	var to, summary, reason string
	var artifacts artifactList
	task, rest := popTask(args)
	fs, err := flags("transition", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&to, "to", "", "ключ перехода")
		fs.StringVar(&summary, "summary", "", "чем закончился этап")
		fs.StringVar(&reason, "reason", "", "причина отката")
		fs.Var(&artifacts, "artifact", "kind=файл; можно повторять")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if to == "" {
		return fmt.Errorf("не указан --to")
	}

	// Артефакты, которых требует гейт перехода, сдаются до самого перехода:
	// движок прогоняет гейты по тому, что уже лежит в задаче.
	for _, a := range artifacts {
		kind, path, ok := strings.Cut(a, "=")
		if !ok {
			return fmt.Errorf("артефакт задаётся как kind=файл, а не %q", a)
		}
		body, err := readContent(path, "")
		if err != nil {
			return err
		}
		if err := c.post(ctx, "/tasks/"+task+"/artifacts", map[string]any{
			"kind": kind, "title": kind, "format": guessFormat(path),
			"content": string(body),
		}, nil); err != nil {
			return err
		}
	}

	comment := summary
	if reason != "" {
		comment = reason
	}

	var out json.RawMessage
	if err := c.post(ctx, "/tasks/"+task+"/transitions/"+to,
		map[string]any{"comment": comment}, &out); err != nil {
		return blockedTransition(task, to, err)
	}
	return emit(out, render(renderTransitionResult, out))
}

// blockedTransition показывает гейты заблокированного перехода тем же
// списком, что clab gates, а не JSON деталей ошибки. С --json детали
// остаются как есть: скрипты читают их из stderr.
func blockedTransition(task, to string, err error) error {
	var apiErr *APIError
	if jsonOut || !errors.As(err, &apiErr) || apiErr.Code != "gates_not_satisfied" {
		return err
	}
	var details struct {
		Gates []gateOutcome `json:"gates"`
	}
	if json.Unmarshal(apiErr.Details, &details) != nil || len(details.Gates) == 0 {
		return err
	}
	fmt.Printf("%s остаётся на месте: переход %s заблокирован гейтами\n", task, to)
	printGates(details.Gates)
	return &APIError{Code: apiErr.Code, Message: apiErr.Message, Status: apiErr.Status}
}

// cmdDecide закрывает гейт решением. Нужен CI: чек-лист и внешнюю проверку
// закрывает не агент, а тот, кто её выполнил.
func cmdDecide(ctx context.Context, c *Client, args []string) error {
	var transition, gate, status, details string
	task, rest := popTask(args)
	fs, err := flags("decide", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&transition, "transition", "", "переход, на котором висит гейт")
		fs.StringVar(&gate, "gate", "", "ключ гейта")
		fs.StringVar(&status, "status", "passed", "passed | failed | waived")
		fs.StringVar(&details, "details", "", "подробности решения в JSON")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if transition == "" || gate == "" {
		return fmt.Errorf("нужны --transition и --gate")
	}

	body := map[string]any{"status": status}
	if details != "" {
		var parsed json.RawMessage
		if err := json.Unmarshal([]byte(details), &parsed); err != nil {
			return fmt.Errorf("--details не разбирается как JSON: %w", err)
		}
		body["details"] = parsed
	}

	var out json.RawMessage
	if err := c.post(ctx,
		"/tasks/"+task+"/transitions/"+transition+"/gates/"+gate+"/decision",
		body, &out); err != nil {
		return err
	}
	return emit(out, render(renderDecision, out))
}

func cmdDrift(ctx context.Context, c *Client, args []string) error {
	var state string
	task, rest := popTask(args)
	fs, err := flags("drift", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&state, "state", "", "состояния сигналов через запятую")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}

	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}

	var out json.RawMessage
	if err := c.get(ctx, "/tasks/"+task+"/drift", q, &out); err != nil {
		return err
	}
	return emit(out, render(renderDrift, out))
}

func cmdResolveDrift(ctx context.Context, c *Client, args []string) error {
	var resolution, note string
	signal, rest := popTask(args)
	fs, err := flags("resolve-drift", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&resolution, "resolution", "resolved", "resolved | dismissed")
		fs.StringVar(&note, "note", "", "разбор в двух словах")
	})
	if err != nil {
		return err
	}
	if signal == "" {
		fs.Usage()
		return fmt.Errorf("не указан идентификатор сигнала")
	}

	var out any
	if err := c.post(ctx, "/drift-signals/"+signal+"/resolve", map[string]any{
		"resolution": resolution, "note": note,
	}, &out); err != nil {
		return err
	}
	return print(out)
}

// artifactList — повторяющийся флаг --artifact kind=файл.
type artifactList []string

func (a *artifactList) String() string { return strings.Join(*a, ",") }

func (a *artifactList) Set(v string) error {
	*a = append(*a, v)
	return nil
}

func readContent(file, inline string) ([]byte, error) {
	if inline != "" {
		return []byte(inline), nil
	}
	switch file {
	case "":
		return nil, fmt.Errorf("нужен --file или --content")
	case "-":
		return io.ReadAll(os.Stdin)
	default:
		return os.ReadFile(file)
	}
}

// guessFormat выводит формат из расширения файла: агент почти всегда сдаёт
// markdown, а вердикты — json, и указывать это руками каждый раз лишнее.
func guessFormat(file string) string {
	switch {
	case strings.HasSuffix(file, ".json"):
		return "json"
	case strings.HasSuffix(file, ".yaml"), strings.HasSuffix(file, ".yml"):
		return "yaml"
	case strings.HasSuffix(file, ".diff"), strings.HasSuffix(file, ".patch"):
		return "diff"
	case strings.HasSuffix(file, ".txt"):
		return "text"
	default:
		return "markdown"
	}
}

// cmdSearch — тот же поиск, что у агента через sdlc_search_context.
func cmdSearch(ctx context.Context, c *Client, args []string) error {
	var projectFlag, scope, repos string
	var top int
	// Запрос читается первым позиционным аргументом по той же причине, что и
	// ключ задачи: именно так эту команду пишут.
	query, rest := popTask(args)
	_, err := flags("search", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&scope, "scope", "", "code, artifacts, documents через запятую")
		fs.StringVar(&repos, "repo", "", "owner/name через запятую: сузить поиск по коду")
		fs.IntVar(&top, "top", 0, "сколько кусков вернуть")
	})
	if err != nil {
		return err
	}
	if query == "" {
		return errors.New("не указан запрос: clab search \"как устроена ротация токенов\"")
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}

	q := url.Values{}
	q.Set("q", query)
	if scope != "" {
		q.Set("scope", scope)
	}
	if repos != "" {
		q.Set("repositories", repos)
	}
	if top > 0 {
		q.Set("top_k", strconv.Itoa(top))
	}

	var out json.RawMessage
	if err := c.get(ctx, "/projects/"+key+"/search", q, &out); err != nil {
		return err
	}
	return emit(out, render(renderSearch, out))
}

func cmdIndex(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	_, err := flags("index", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
	})
	if err != nil {
		return err
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}

	var out json.RawMessage
	if err := c.get(ctx, "/projects/"+key+"/index", nil, &out); err != nil {
		return err
	}
	return emit(out, render(renderIndex, out))
}
