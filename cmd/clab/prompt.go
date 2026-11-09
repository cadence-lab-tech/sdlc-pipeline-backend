package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Стартовая инструкция и руководство командами clab (решение 0077 пп. 1–2,
// CLAB-416): агенту без MCP-промптов вход в этап — clab prompt work, общий
// текст о механизме — clab guide. Текст строит сервер, CLI печатает его как
// есть и правил не знает.

// agentTextMinServer — первая версия сервера с ручкой стартовой инструкции
// (GET /v1/tasks/{key}/prompts/work). Номер — по плану выпуска эпика
// CLAB-414; интегратор сверяет его с тегом выпуска.
const agentTextMinServer = "v0.28.0"

// guideKitPath — руководство в наборе сервера прежней версии (до CLAB-417):
// тело скила clab-guide в files.
const guideKitPath = "skills/clab-guide/SKILL.md"

// guideKit — то из ответа /v1/agent-kit, что нужно clab guide: нейтральная
// справка core.guide (CLAB-417, решение 0077, п. 3) и прежнее поле files.
// Тип свой, а не agentKit из setup.go: набор привязок читает clab setup.
type guideKit struct {
	Version string `json:"version"`
	Core    *struct {
		Guide struct {
			Content string `json:"content"`
		} `json:"guide"`
	} `json:"core"`
	Files []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
}

// errOldServer — сервер не отдаёт того, что нужно команде: маршрута нет,
// вместо JSON пришла страница, в наборе нет руководства. Ошибка домена (404
// с кодом not_found — «задачи нет») сюда не относится.
var errOldServer = errors.New("сервер прежней версии")

func cmdPrompt(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	switch sub {
	case "work":
		return promptWork(ctx, c, rest)
	case "":
		// «clab prompt» и «clab prompt --help» показывают форму команды.
		fs, err := flags("prompt", rest, func(*flag.FlagSet) {})
		if err != nil {
			return err
		}
		fs.Usage()
		return fmt.Errorf("не указана подкоманда: work")
	default:
		fs := newFlagSet("prompt", func(*flag.FlagSet) {})
		fs.Usage()
		return fmt.Errorf("неизвестная подкоманда %q: work", sub)
	}
}

// promptWork печатает стартовую инструкцию этапа — тот же текст, что
// промпт MCP work_on_task с теми же аргументами.
func promptWork(ctx context.Context, c *Client, args []string) error {
	task, rest := popTask(args)
	var role, usage string
	fs, err := flags("prompt", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&role, "role", "", "ключ роли, если этап адресован конкретной; без него — роль текущего этапа")
		fs.StringVar(&usage, "usage", "", "runner — стоимость сообщит запустивший агента раннер, и шаг отчёта заменён запретом")
	})
	if err != nil {
		return err
	}
	if task == "" {
		fs.Usage()
		return fmt.Errorf("не указан ключ задачи: clab prompt work <ключ> [--role <роль>] [--usage runner]")
	}

	q := url.Values{}
	if role != "" {
		q.Set("role", role)
	}
	if usage != "" {
		q.Set("usage", usage)
	}
	path := "/tasks/" + url.PathEscape(task) + "/prompts/work"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, _, err := c.getContent(ctx, path)
	if routeMissing(err) {
		return c.oldServerRefusal(ctx, "clab prompt work", "стартовую инструкцию этапа", task)
	}
	if err != nil {
		return err
	}
	var out struct {
		Name string `json:"name"`
		Text string `json:"text"`
	}
	// Ответ не JSON или без текста — так отвечает не ручка, а что-то перед
	// ней (страница интерфейса у сервера без маршрута).
	if json.Unmarshal(raw, &out) != nil || out.Text == "" {
		return c.oldServerRefusal(ctx, "clab prompt work", "стартовую инструкцию этапа", task)
	}
	return emit(json.RawMessage(raw), func() error {
		fmt.Println(out.Text)
		return nil
	})
}

// cmdGuide печатает руководство по механизму из набора агента инстанса
// (GET /v1/agent-kit, открытая ручка): нейтральную справку core.guide — тот
// же текст, что тело скила clab-guide без заметок привязки. Сервер прежней
// версии core не отдаёт — тогда тело скила clab-guide из files.
func cmdGuide(ctx context.Context, c *Client, args []string) error {
	if _, err := flags("guide", args, func(*flag.FlagSet) {}); err != nil {
		return err
	}
	raw, _, err := c.getContent(ctx, "/agent-kit")
	if routeMissing(err) {
		return c.oldServerRefusal(ctx, "clab guide", "руководство агента", "")
	}
	if err != nil {
		return err
	}
	var kit guideKit
	if json.Unmarshal(raw, &kit) != nil {
		return c.oldServerRefusal(ctx, "clab guide", "руководство агента", "")
	}
	text := ""
	if kit.Core != nil {
		text = kit.Core.Guide.Content
	}
	for _, f := range kit.Files {
		if text == "" && f.Path == guideKitPath {
			text = stripFrontmatter(f.Content)
		}
	}
	if text == "" {
		return c.oldServerRefusal(ctx, "clab guide", "руководство агента", "")
	}
	return emit(map[string]string{"version": kit.Version, "name": "clab-guide", "text": text}, func() error {
		fmt.Print(text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Println()
		}
		return nil
	})
}

// stripFrontmatter отрезает шапку скила (--- … ---): она для агента, который
// ставит скилы, а не для того, кто читает руководство.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	end := strings.Index(s[len("---\n"):], "\n---\n")
	if end < 0 {
		return s
	}
	return strings.TrimLeft(s[len("---\n")+end+len("\n---\n"):], "\n")
}

// oldServerRefusal — понятный отказ команды против сервера прежней версии:
// версия сервера, что команде нужен сервер новее, и как войти в этап без
// неё. Тела ответа в тексте нет — у старого сервера это могла быть
// страница интерфейса.
func (c *Client) oldServerRefusal(ctx context.Context, command, what, task string) error {
	if task == "" {
		task = "<ключ>"
	}
	return &oldServerError{msg: fmt.Sprintf("%s: сервер %s не отдаёт %s — команде нужен сервер %s или новее.\n"+
		"Войти в этап можно и с этим сервером: clab claim %s — инструкция роли придёт в ответе захвата",
		command, c.knownServerVersion(ctx), what, agentTextMinServer, task)}
}

// oldServerError — текст отказа целиком; errors.Is узнаёт его как
// errOldServer.
type oldServerError struct{ msg string }

func (e *oldServerError) Error() string        { return e.msg }
func (e *oldServerError) Is(target error) bool { return target == errOldServer }

// versionShape — версия сервера, которую можно напечатать: заголовок или
// поле прежнего сервера могли прийти чем угодно.
var versionShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// knownServerVersion — версия сервера из заголовка последнего ответа, иначе
// из /v1/instance; не узнали — так и говорим.
func (c *Client) knownServerVersion(ctx context.Context) string {
	v, _ := c.serverVersion.Load().(string)
	if v == "" {
		var info instanceInfo
		if c.get(ctx, "/instance", nil, &info) == nil {
			v = info.Version
		}
	}
	if !versionShape.MatchString(v) {
		return "(версия неизвестна)"
	}
	return v
}
