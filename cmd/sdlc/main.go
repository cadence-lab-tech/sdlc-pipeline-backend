// Команда sdlc — CLI для CI и не-MCP клиентов.
//
// Те же операции, что и у MCP-тулов, только транспорт другой: локальный агент
// ходит по MCP, скрипт сборки — сюда. Логики цикла здесь нет и быть не должно.
//
//	SDLC_API_URL   адрес API           (по умолчанию http://localhost:8088/v1)
//	SDLC_TOKEN     токен доступа        (режим токенов; без него — заголовок актора)
//	SDLC_ACTOR     кто действует        (режим заголовка; по умолчанию agent:$USER)
//	SDLC_PROJECT   проект по умолчанию для команд, которым нужен проект
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
)

type command struct {
	name    string
	summary string
	usage   string
	run     func(ctx context.Context, c *Client, args []string) error
}

func commands() []command {
	return []command{
		{"whoami", "кто я и к какому проекту подключён", "sdlc whoami", cmdWhoami},
		{"queue", "что сейчас можно забрать", "sdlc queue [--role reviewer] [--status analysis]", cmdQueue},
		{"tasks", "задачи проекта", "sdlc tasks [--status analysis] [--type epic] [--parent SDLC-4]", cmdTasks},
		{"show", "состояние задачи глазами агента", "sdlc show SDLC-42", cmdShow},
		{"create", "завести задачу или эпик", "sdlc create --title \"Раннер\" [--type epic] [--parent SDLC-4] [--repos backend] [--blocks SDLC-2] [--assignee agent:bot]", cmdCreate},
		{"relate", "связать задачи порядком или контекстом", "sdlc relate SDLC-5 --to SDLC-6 [--rel blocks] [--remove]", cmdRelate},
		{"assign", "назначить задачу исполнителю", "sdlc assign SDLC-5 --to agent:runner [--clear]", cmdAssign},
		{"usage", "отчитаться о стоимости прогона или показать её", "sdlc usage SDLC-5 --model claude-sonnet-5 --input 1200 --output 800 --cost 0.42 | sdlc usage SDLC-4 --show", cmdUsage},
		{"claim", "забрать задачу и получить контекст этапа", "sdlc claim SDLC-42 [--session ref]", cmdClaim},
		{"heartbeat", "продлить аренду", "sdlc heartbeat SDLC-42", cmdHeartbeat},
		{"release", "отпустить задачу, не завершив этап", "sdlc release SDLC-42 [--note ...]", cmdRelease},
		{"submit", "сдать артефакт", "sdlc submit SDLC-42 --kind analysis.brief --title ... --file brief.md", cmdSubmit},
		{"link", "привязать PR, ветку или коммит", "sdlc link SDLC-42 --repo acme/api --ref 128 [--url ...] [--state open]", cmdLink},
		{"scope", "расширить область задачи", "sdlc scope SDLC-42 --add acme/web --reason ...", cmdScope},
		{"gates", "что мешает переходу", "sdlc gates SDLC-42 [--transition analysis_done]", cmdGates},
		{"transition", "запросить переход", "sdlc transition SDLC-42 --to analysis_done [--summary ...] [--artifact kind=file]", cmdTransition},
		{"decide", "закрыть гейт решением", "sdlc decide SDLC-42 --transition analysis_done --gate human_approval [--status passed]", cmdDecide},
		{"search", "поиск по коду, артефактам и документам проекта", "sdlc search \"ротация токенов\" [--scope code] [--repo acme/api] [--top 10]", cmdSearch},
		{"index", "состояние индекса репозиториев", "sdlc index [--project SDLC]", cmdIndex},
		{"drift", "открытые расхождения задачи", "sdlc drift SDLC-42", cmdDrift},
		{"resolve-drift", "закрыть сигнал дрейфа", "sdlc resolve-drift <signal-id> --resolution resolved [--note ...]", cmdResolveDrift},
		{"metrics", "метрики цикла: где застревают задачи, какие гейты валятся", "sdlc metrics [--period 30d]", cmdMetrics},

		{"me", "кто я по токену, где состою", "sdlc me", cmdMe},
		{"token", "выдать, перечислить или отозвать токен", "sdlc token create --name ci [--for agent:bot] [--project SDLC] [--roles reviewer] [--days 90] | sdlc token list [--project SDLC] | sdlc token revoke <id>", cmdToken},
		{"members", "участники проекта и их уровни", "sdlc members [list] | sdlc members set user:alice maintainer | sdlc members remove user:alice", cmdMembers},
		{"team", "команды: состав и права на проекты", "sdlc team list | sdlc team show platform | sdlc team create platform --name Платформа [--can-create-projects] | sdlc team add platform user:alice --role lead | sdlc team grant platform --project SDLC --level maintainer", cmdTeam},
		{"project", "проекты: список, создание, команды проекта", "sdlc project list | sdlc project create ACME --name \"Проект\" [--team platform] | sdlc project teams", cmdProject},

		{"export", "выгрузить проект связкой: репозитории, роли, документы, цикл", "sdlc export [--project SDLC] > bundle.json", cmdExport},
		{"import", "применить связку к проекту", "sdlc import --file bundle.json [--dry-run]", cmdImport},
		{"import-guide", "инструкция агенту по переносу правил команды", "sdlc import-guide", cmdImportGuide},
		{"repo", "привязать репозиторий к проекту", "sdlc repo add acme/api [--hint backend] [--branch main]", cmdRepo},
		{"role", "показать роль или завести версию инструкции", "sdlc role show analyst | sdlc role put --key analyst --name Аналитик --file instructions.md [--contract contract.json]", cmdRole},
		{"doc", "проектные документы", "sdlc doc list | sdlc doc put --kind guideline --title Соглашения --file CLAUDE.md | sdlc doc get <id>", cmdDoc},
		{"workflow", "версии цикла: список, проверка, активация черновика", "sdlc workflow list | sdlc workflow validate <id> | sdlc workflow activate <id>", cmdWorkflow},
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		return
	}

	name := os.Args[1]
	for _, cmd := range commands() {
		if cmd.name != name {
			continue
		}
		if err := cmd.run(ctx, NewClient(), os.Args[2:]); err != nil {
			exit(err)
		}
		return
	}

	fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n", name)
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Fprint(os.Stderr, "sdlc — работа с циклом разработки из CI и скриптов\n\n")
	for _, cmd := range commands() {
		fmt.Fprintf(os.Stderr, "  %-14s %s\n", cmd.name, cmd.summary)
	}
	fmt.Fprintln(os.Stderr, "\nsdlc <команда> --help — аргументы команды")
	fmt.Fprintln(os.Stderr, "\nАдрес API — SDLC_API_URL, токен — SDLC_TOKEN (без него действующее")
	fmt.Fprintln(os.Stderr, "лицо берётся из SDLC_ACTOR), проект по умолчанию — SDLC_PROJECT.")
}

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
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func print(v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(body))
	return nil
}

// popTask отделяет ключ задачи от флагов.
//
// Пакет flag прекращает разбор на первом позиционном аргументе, а ключ задачи
// читается первым: "sdlc claim SDLC-42 --session x" должно работать, потому
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

func project(fs *flag.FlagSet, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if key := os.Getenv("SDLC_PROJECT"); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("не указан проект: --project или SDLC_PROJECT")
}

func requireTask(fs *flag.FlagSet, task string) error {
	if task == "" {
		fs.Usage()
		return fmt.Errorf("не указан ключ задачи")
	}
	return nil
}

// ── команды ─────────────────────────────────────────────────────────────────

func cmdWhoami(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	fs, err := flags("whoami", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
	})
	if err != nil {
		return err
	}
	key, err := project(fs, projectFlag)
	if err != nil {
		return err
	}

	var out any
	if err := c.get(ctx, "/projects/"+key, nil, &out); err != nil {
		return err
	}
	return print(map[string]any{"actor": c.actor, "api": c.base, "project": out})
}

func cmdQueue(ctx context.Context, c *Client, args []string) error {
	var projectFlag, role, status string
	fs, err := flags("queue", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&role, "role", "", "оставить работу только для этой роли")
		fs.StringVar(&status, "status", "", "оставить только этот статус")
	})
	if err != nil {
		return err
	}
	key, err := project(fs, projectFlag)
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

	var out any
	if err := c.get(ctx, "/projects/"+key+"/queue", q, &out); err != nil {
		return err
	}
	return print(out)
}

func cmdTasks(ctx context.Context, c *Client, args []string) error {
	var projectFlag, status, taskType, parent string
	fs, err := flags("tasks", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&status, "status", "", "фильтр по статусу")
		fs.StringVar(&taskType, "type", "", "фильтр по типу задачи, например epic")
		fs.StringVar(&parent, "parent", "", "только дочерние задачи этой")
	})
	if err != nil {
		return err
	}
	key, err := project(fs, projectFlag)
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

	var out any
	if err := c.get(ctx, "/projects/"+key+"/tasks", q, &out); err != nil {
		return err
	}
	return print(out)
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

	var out any
	if err := c.get(ctx, "/tasks/"+task+"/state", nil, &out); err != nil {
		return err
	}
	return print(out)
}

func cmdClaim(ctx context.Context, c *Client, args []string) error {
	var session string
	task, rest := popTask(args)
	fs, err := flags("claim", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&session, "session", "", "идентификатор сессии агента")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if session == "" {
		host, _ := os.Hostname()
		session = host + "/sdlc-cli"
	}

	var out any
	if err := c.post(ctx, "/tasks/"+task+"/claim",
		map[string]any{"session_ref": session}, &out); err != nil {
		return err
	}
	return print(out)
}

func cmdHeartbeat(ctx context.Context, c *Client, args []string) error {
	task, rest := popTask(args)
	fs, err := flags("heartbeat", rest, func(*flag.FlagSet) {})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}

	var out any
	if err := c.post(ctx, "/tasks/"+task+"/heartbeat", map[string]any{}, &out); err != nil {
		return err
	}
	return print(out)
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

	var out any
	if err := c.post(ctx, "/tasks/"+task+"/artifacts", map[string]any{
		"kind": kind, "title": title, "format": format, "content": string(body),
	}, &out); err != nil {
		return err
	}
	return print(out)
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
	var add, reason string
	task, rest := popTask(args)
	fs, err := flags("scope", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&add, "add", "", "репозитории через запятую")
		fs.StringVar(&reason, "reason", "", "почему изменение не помещается в текущую область")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if add == "" {
		return fmt.Errorf("не указан --add")
	}

	repos := []string{}
	for _, r := range strings.Split(add, ",") {
		if r = strings.TrimSpace(r); r != "" {
			repos = append(repos, r)
		}
	}

	var out any
	if err := c.do(ctx, "PATCH", "/tasks/"+task+"/scope", map[string]any{
		"add_repositories": repos, "note": reason,
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

	var out struct {
		Transitions []struct {
			Key     string `json:"key"`
			Allowed bool   `json:"allowed"`
			Gates   []any  `json:"gates"`
		} `json:"transitions"`
	}
	if err := c.get(ctx, "/tasks/"+task+"/transitions", nil, &out); err != nil {
		return err
	}
	if transition == "" {
		return print(out.Transitions)
	}
	for _, t := range out.Transitions {
		if t.Key == transition {
			return print(t)
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

	var out any
	if err := c.post(ctx, "/tasks/"+task+"/transitions/"+to,
		map[string]any{"comment": comment}, &out); err != nil {
		return err
	}
	return print(out)
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

	var out any
	if err := c.post(ctx,
		"/tasks/"+task+"/transitions/"+transition+"/gates/"+gate+"/decision",
		body, &out); err != nil {
		return err
	}
	return print(out)
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

	var out any
	if err := c.get(ctx, "/tasks/"+task+"/drift", q, &out); err != nil {
		return err
	}
	return print(out)
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
	fs, err := flags("search", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&scope, "scope", "", "code, artifacts, documents через запятую")
		fs.StringVar(&repos, "repo", "", "owner/name через запятую: сузить поиск по коду")
		fs.IntVar(&top, "top", 0, "сколько кусков вернуть")
	})
	if err != nil {
		return err
	}
	if query == "" {
		return errors.New("не указан запрос: sdlc search \"как устроена ротация токенов\"")
	}
	key, err := project(fs, projectFlag)
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

	var out any
	if err := c.get(ctx, "/projects/"+key+"/search", q, &out); err != nil {
		return err
	}
	return print(out)
}

func cmdIndex(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	fs, err := flags("index", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
	})
	if err != nil {
		return err
	}
	key, err := project(fs, projectFlag)
	if err != nil {
		return err
	}

	var out any
	if err := c.get(ctx, "/projects/"+key+"/index", nil, &out); err != nil {
		return err
	}
	return print(out)
}
