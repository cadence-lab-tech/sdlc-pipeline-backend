package main

// Команды и проекты команды (решения 0027, 0033). Те же операции, что в
// интерфейсе: организацию заводит вошедший человек (на выделенном инстансе,
// SDLC_TENANCY=single, — только администратор), лид ведёт состав и заводит
// проекты, администратор инстанса выдаёт право на проекты и тариф там, где
// тарифы есть.
//
// Удаления команды здесь нет намеренно (CLAB-13): оно уносит проекты команды
// со всем содержимым и необратимо, а подтверждается диалогом в интерфейсе, а
// не флагом в скрипте. Ручка DELETE /v1/teams/{key} при этом остаётся
// публичной частью API. `clab team remove` — про вывод человека из состава.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// errNoPlans — ответ `clab team plan` на выделенном инстансе: тот же текст,
// что сервер отдаёт на перевод тарифа.
var errNoPlans = errors.New("тарифов на этом инстансе нет")

func cmdTeam(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	switch sub {
	case "list", "":
		if _, err := flags("team", rest, func(*flag.FlagSet) {}); err != nil {
			return err
		}
		var out any
		if err := c.get(ctx, "/teams", nil, &out); err != nil {
			return err
		}
		return print(out)

	case "show":
		key, tail := popTask(rest)
		if _, err := flags("team", tail, func(*flag.FlagSet) {}); err != nil {
			return err
		}
		if key == "" {
			return fmt.Errorf("clab team show <ключ команды>")
		}
		var out any
		if err := c.get(ctx, "/teams/"+url.PathEscape(key), nil, &out); err != nil {
			return err
		}
		return print(out)

	case "create":
		key, tail := popTask(rest)
		var name, description string
		var canCreate bool
		fs, err := flags("team", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "название команды")
			fs.StringVar(&description, "description", "", "описание")
			fs.BoolVar(&canCreate, "can-create-projects", false, "выдать право заводить проекты явно (администратор); иначе — по режиму инстанса")
		})
		if err != nil {
			return err
		}
		if key == "" || name == "" {
			fs.Usage()
			return fmt.Errorf("нужны ключ команды и --name")
		}
		body := map[string]any{"key": key, "name": name, "can_create_projects": canCreate}
		if description != "" {
			body["description"] = description
		}
		var out json.RawMessage
		if err := c.do(ctx, "POST", "/teams", body, &out); err != nil {
			return err
		}
		return emit(out, render(renderTeamCreated, out))

	case "convert":
		// Личная команда становится организацией (решение 0033): проекты
		// остаются, участников теперь можно звать.
		key, tail := popTask(rest)
		var newKey, name string
		fs, err := flags("team", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&newKey, "key", "", "новый ключ организации; пусто — прежний")
			fs.StringVar(&name, "name", "", "новое название; пусто — прежнее")
		})
		if err != nil {
			return err
		}
		if key == "" {
			fs.Usage()
			return fmt.Errorf("clab team convert <личная команда> [--key acme --name \"Acme\"]")
		}
		var out any
		if err := c.do(ctx, "POST", "/teams/"+url.PathEscape(key)+"/convert",
			map[string]any{"key": newKey, "name": name}, &out); err != nil {
			return err
		}
		return print(out)

	case "github":
		// Установки GitHub App команды (решение 0034). Подключаются из
		// интерфейса: право на установку доказывает повторная авторизация в
		// браузере, и у CLI такого пути нет.
		key, tail := popTask(rest)
		if _, err := flags("team", tail, func(*flag.FlagSet) {}); err != nil {
			return err
		}
		if key == "" {
			return fmt.Errorf("clab team github <ключ команды>")
		}
		var out json.RawMessage
		if err := c.get(ctx, "/teams/"+url.PathEscape(key)+"/github/installations", nil, &out); err != nil {
			return err
		}
		return emit(out, render(renderTeamGitHub, out))

	case "plan":
		// Тариф команды: без --set показывает действующий, с --set переводит
		// (администратор инстанса). На выделенном инстансе тарифов нет: это
		// видно по ответу — ключа plan в команде нет, перевод отвечает 404 с
		// тем же текстом, — без отдельного вопроса серверу о профиле.
		key, tail := popTask(rest)
		var set string
		fs, err := flags("team", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&set, "set", "", "код тарифа: free, business")
		})
		if err != nil {
			return err
		}
		if key == "" {
			fs.Usage()
			return fmt.Errorf("clab team plan <команда> [--set business]")
		}
		if set == "" {
			var out any
			if err := c.get(ctx, "/teams/"+url.PathEscape(key), nil, &out); err != nil {
				return err
			}
			if detail, ok := out.(map[string]any); ok {
				plan, ok := detail["plan"]
				if !ok {
					return errNoPlans
				}
				return print(plan)
			}
			return print(out)
		}
		var out any
		if err := c.do(ctx, "PUT", "/teams/"+url.PathEscape(key)+"/plan",
			map[string]any{"plan": set}, &out); err != nil {
			// 404 бывает и про команду, и про тариф: текст сервера различает,
			// «тарифов на этом инстансе нет» приходит от него как есть.
			return err
		}
		return print(out)

	case "update":
		key, tail := popTask(rest)
		var name, description string
		var allow, deny bool
		fs, err := flags("team", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "новое название")
			fs.StringVar(&description, "description", "", "новое описание")
			fs.BoolVar(&allow, "can-create-projects", false, "выдать право заводить проекты")
			fs.BoolVar(&deny, "no-create-projects", false, "снять право заводить проекты")
		})
		if err != nil {
			return err
		}
		if key == "" {
			fs.Usage()
			return fmt.Errorf("нужен ключ команды")
		}
		body := map[string]any{}
		if name != "" {
			body["name"] = name
		}
		if description != "" {
			body["description"] = description
		}
		// Право меняется только явным флагом: отсутствие флага — «не трогать»,
		// а не «снять».
		switch {
		case allow && deny:
			return fmt.Errorf("--can-create-projects и --no-create-projects вместе не имеют смысла")
		case allow:
			body["can_create_projects"] = true
		case deny:
			body["can_create_projects"] = false
		}
		var out any
		if err := c.do(ctx, "PATCH", "/teams/"+url.PathEscape(key), body, &out); err != nil {
			return err
		}
		return print(out)

	case "add":
		key, tail := popTask(rest)
		principal, tail := popTask(tail)
		var role string
		fs, err := flags("team", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&role, "role", "member", "роль в команде: member или lead")
		})
		if err != nil {
			return err
		}
		if key == "" || principal == "" {
			fs.Usage()
			return fmt.Errorf("clab team add <команда> <user:alice> [--role lead]")
		}
		var out any
		if err := c.do(ctx, "PUT", "/teams/"+url.PathEscape(key)+"/members",
			map[string]any{"principal": principal, "role": role}, &out); err != nil {
			return err
		}
		return print(out)

	case "remove":
		key, tail := popTask(rest)
		principal, tail := popTask(tail)
		if _, err := flags("team", tail, func(*flag.FlagSet) {}); err != nil {
			return err
		}
		if key == "" || principal == "" {
			return fmt.Errorf("clab team remove <команда> <user:alice>")
		}
		if err := c.do(ctx, "DELETE",
			"/teams/"+url.PathEscape(key)+"/members/"+url.PathEscape(principal), nil, nil); err != nil {
			return err
		}
		return print(map[string]any{"removed": principal})

	case "groups":
		key, tail := popTask(rest)
		var set string
		var clear bool
		fs, err := flags("team", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&set, "set", "", "группы провайдера через запятую: состав команды синхронизируется при входе")
			fs.BoolVar(&clear, "clear", false, "убрать сопоставление: состав снова ведётся руками")
		})
		if err != nil {
			return err
		}
		if key == "" || (set == "" && !clear) {
			fs.Usage()
			return fmt.Errorf("clab team groups <команда> --set sdlc-platform,sdlc-core")
		}
		groups := []string{}
		if !clear {
			groups = splitCSV(set)
		}
		var out any
		if err := c.do(ctx, "PUT", "/teams/"+url.PathEscape(key)+"/idp-groups",
			map[string]any{"groups": groups}, &out); err != nil {
			return err
		}
		return print(out)

	case "budget":
		key, tail := popTask(rest)
		var period string
		var limit float64
		fs, err := flags("team", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&period, "period", "", "окно: day, week или month")
			fs.Float64Var(&limit, "limit", 0, "предел в долларах; 0 снимает предел")
		})
		if err != nil {
			return err
		}
		if key == "" {
			fs.Usage()
			return fmt.Errorf("clab team budget <команда> [--period month --limit 500]")
		}
		if period == "" {
			var out any
			if err := c.get(ctx, "/teams/"+url.PathEscape(key)+"/budgets", nil, &out); err != nil {
				return err
			}
			return print(out)
		}
		var out any
		if err := c.do(ctx, "PUT", "/teams/"+url.PathEscape(key)+"/budgets",
			map[string]any{"period": period, "limit_usd": limit}, &out); err != nil {
			return err
		}
		return print(out)

	case "grant":
		key, tail := popTask(rest)
		var projectFlag, level, roles string
		fs, err := flags("team", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
			fs.StringVar(&level, "level", "", "уровень: viewer, agent, maintainer, admin")
			fs.StringVar(&roles, "roles", "", "роли специалистов через запятую для участников команды; пусто — любые")
		})
		if err != nil {
			return err
		}
		if key == "" || level == "" {
			fs.Usage()
			return fmt.Errorf("clab team grant <команда> --project SDLC --level maintainer")
		}
		projectKey, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		var out any
		if err := c.do(ctx, "PUT", "/projects/"+url.PathEscape(projectKey)+"/teams",
			map[string]any{"team": key, "level": level, "role_keys": splitCSV(roles)}, &out); err != nil {
			return err
		}
		return print(out)

	case "revoke":
		key, tail := popTask(rest)
		var projectFlag string
		fs, err := flags("team", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		})
		if err != nil {
			return err
		}
		if key == "" {
			fs.Usage()
			return fmt.Errorf("clab team revoke <команда> --project SDLC")
		}
		projectKey, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		if err := c.do(ctx, "DELETE",
			"/projects/"+url.PathEscape(projectKey)+"/teams/"+url.PathEscape(key), nil, nil); err != nil {
			return err
		}
		return print(map[string]any{"revoked": key, "project": projectKey})

	default:
		return fmt.Errorf("неизвестная подкоманда %q: list, show, create, convert, plan, github, update, add, remove, groups, budget, grant, revoke", sub)
	}
}

// cmdProject заводит и перечисляет проекты: до фазы 6 проект появлялся только
// сидом или из интерфейса администратора, теперь его заводит лид команды.
func cmdProject(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	switch sub {
	case "list", "":
		if _, err := flags("project", rest, func(*flag.FlagSet) {}); err != nil {
			return err
		}
		var out json.RawMessage
		if err := c.get(ctx, "/projects", nil, &out); err != nil {
			return err
		}
		return emit(out, render(renderProjects, out))

	case "create":
		key, tail := popTask(rest)
		var name, description, team string
		fs, err := flags("project", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "название проекта")
			fs.StringVar(&description, "description", "", "описание")
			fs.StringVar(&team, "team", "", "команда-владелец")
		})
		if err != nil {
			return err
		}
		if key == "" || name == "" {
			fs.Usage()
			return fmt.Errorf("clab project create <КЛЮЧ> --name \"Название\" [--team platform]")
		}
		body := map[string]any{"key": key, "name": name, "team": team}
		if description != "" {
			body["description"] = description
		}
		var out any
		if err := c.do(ctx, "POST", "/projects", body, &out); err != nil {
			return err
		}
		return print(out)

	case "github":
		var projectFlag, credentialRef string
		var installation int64
		var check bool
		_, err := flags("project", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
			fs.Int64Var(&installation, "installation", 0, "идентификатор установки приложения в организации")
			fs.StringVar(&credentialRef, "credential-ref", "", "имя секрета с токеном: запасной путь без приложения")
			fs.BoolVar(&check, "check", false, "проверить доступ к репозиториям проекта")
		})
		if err != nil {
			return err
		}
		projectKey, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		if installation == 0 && credentialRef == "" {
			check = true
		}
		if !check {
			body := map[string]any{}
			if installation != 0 {
				body["installation_id"] = installation
			}
			if credentialRef != "" {
				body["credential_ref"] = credentialRef
			}
			var out any
			if err := c.do(ctx, "PUT", "/projects/"+url.PathEscape(projectKey)+"/github", body, &out); err != nil {
				return err
			}
			return print(out)
		}
		var out json.RawMessage
		if err := c.get(ctx, "/projects/"+url.PathEscape(projectKey)+"/github/check", nil, &out); err != nil {
			return err
		}
		return emit(out, render(renderGitHubCheck, out))

	case "teams":
		var projectFlag string
		_, err := flags("project", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		})
		if err != nil {
			return err
		}
		projectKey, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		var out any
		if err := c.get(ctx, "/projects/"+url.PathEscape(projectKey)+"/teams", nil, &out); err != nil {
			return err
		}
		return print(out)

	case "delete":
		return projectDelete(ctx, c, rest)

	case "settings":
		return projectSettings(ctx, c, rest)

	case "set":
		return projectSet(ctx, c, rest)

	default:
		return fmt.Errorf("неизвестная подкоманда %q: list, create, delete, teams, github, settings, set", sub)
	}
}

// projectDelete удаляет проект. Подтверждение ключом здесь, а не в ручке:
// диалог — забота края, как и у отвязки репозитория. Проект, в отличие от
// команд чтения, из git remote не угадывается: удалять то, что подобралось
// само по текущему каталогу, нельзя — ключ называется явно.
func projectDelete(ctx context.Context, c *Client, args []string) error {
	key, rest := popTask(args)
	var projectFlag, confirm string
	fs, err := flags("project", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&confirm, "confirm", "", "ключ проекта ещё раз: подтверждение без терминала")
	})
	if err != nil {
		return err
	}
	if key == "" {
		key = projectFlag
	}
	if key == "" {
		fs.Usage()
		return fmt.Errorf("clab project delete <КЛЮЧ> [--confirm <КЛЮЧ>]: не указан проект")
	}
	if err := confirmProjectKey(key, confirm); err != nil {
		return err
	}
	var out json.RawMessage
	if err := c.do(ctx, "DELETE", "/projects/"+url.PathEscape(key), nil, &out); err != nil {
		return err
	}
	return emit(out, render(renderProjectDeleted, out))
}

// confirmProjectKey сверяет подтверждение: из флага или из stdin. Терминал
// специально не определяется — это ioctl ради одной ветки; важно не то, откуда
// пришёл ключ, а то, что его назвали ещё раз. Пустой stdin — отказ, поэтому
// удалить проект строкой в скрипте, не дописав --confirm, не выйдет.
func confirmProjectKey(key, confirm string) error {
	if confirm != "" {
		if confirm != key {
			return fmt.Errorf("подтверждение %q не совпало с ключом проекта %q", confirm, key)
		}
		return nil
	}
	fmt.Fprintf(os.Stderr, "Проект %s удаляется вместе с задачами, артефактами и индексом. Отменить нельзя.\n", key)
	fmt.Fprintf(os.Stderr, "Введите ключ проекта для подтверждения: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("подтверждение не прочитано: повторите ключ флагом --confirm %s", key)
	}
	if strings.TrimSpace(line) != key {
		return fmt.Errorf("введено не то: проект %s не удалён", key)
	}
	return nil
}
