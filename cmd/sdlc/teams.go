package main

// Команды и проекты команды (решение 0027). Те же операции, что в интерфейсе:
// администратор заводит команду и выдаёт ей право на проекты, лид ведёт
// состав и заводит проекты.

import (
	"context"
	"flag"
	"fmt"
	"net/url"
)

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
			return fmt.Errorf("sdlc team show <ключ команды>")
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
			fs.BoolVar(&canCreate, "can-create-projects", false, "разрешить команде заводить проекты")
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
		var out any
		if err := c.do(ctx, "POST", "/teams", body, &out); err != nil {
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

	case "archive":
		key, tail := popTask(rest)
		if _, err := flags("team", tail, func(*flag.FlagSet) {}); err != nil {
			return err
		}
		if key == "" {
			return fmt.Errorf("sdlc team archive <ключ команды>")
		}
		if err := c.do(ctx, "DELETE", "/teams/"+url.PathEscape(key), nil, nil); err != nil {
			return err
		}
		return print(map[string]any{"archived": key})

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
			return fmt.Errorf("sdlc team add <команда> <user:alice> [--role lead]")
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
			return fmt.Errorf("sdlc team remove <команда> <user:alice>")
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
			return fmt.Errorf("sdlc team groups <команда> --set sdlc-platform,sdlc-core")
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
			return fmt.Errorf("sdlc team budget <команда> [--period month --limit 500]")
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
		var projectFlag, level string
		fs, err := flags("team", tail, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
			fs.StringVar(&level, "level", "", "уровень: viewer, agent, maintainer, admin")
		})
		if err != nil {
			return err
		}
		if key == "" || level == "" {
			fs.Usage()
			return fmt.Errorf("sdlc team grant <команда> --project SDLC --level maintainer")
		}
		projectKey, err := project(fs, projectFlag)
		if err != nil {
			return err
		}
		var out any
		if err := c.do(ctx, "PUT", "/projects/"+url.PathEscape(projectKey)+"/teams",
			map[string]any{"team": key, "level": level}, &out); err != nil {
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
			return fmt.Errorf("sdlc team revoke <команда> --project SDLC")
		}
		projectKey, err := project(fs, projectFlag)
		if err != nil {
			return err
		}
		if err := c.do(ctx, "DELETE",
			"/projects/"+url.PathEscape(projectKey)+"/teams/"+url.PathEscape(key), nil, nil); err != nil {
			return err
		}
		return print(map[string]any{"revoked": key, "project": projectKey})

	default:
		return fmt.Errorf("неизвестная подкоманда %q: list, show, create, update, archive, add, remove, groups, budget, grant, revoke", sub)
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
		var out any
		if err := c.get(ctx, "/projects", nil, &out); err != nil {
			return err
		}
		return print(out)

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
			return fmt.Errorf("sdlc project create <КЛЮЧ> --name \"Название\" [--team platform]")
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
		fs, err := flags("project", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
			fs.Int64Var(&installation, "installation", 0, "идентификатор установки приложения в организации")
			fs.StringVar(&credentialRef, "credential-ref", "", "имя секрета с токеном: запасной путь без приложения")
			fs.BoolVar(&check, "check", false, "проверить доступ к репозиториям проекта")
		})
		if err != nil {
			return err
		}
		projectKey, err := project(fs, projectFlag)
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
		var out any
		if err := c.get(ctx, "/projects/"+url.PathEscape(projectKey)+"/github/check", nil, &out); err != nil {
			return err
		}
		return print(out)

	case "teams":
		var projectFlag string
		fs, err := flags("project", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		})
		if err != nil {
			return err
		}
		projectKey, err := project(fs, projectFlag)
		if err != nil {
			return err
		}
		var out any
		if err := c.get(ctx, "/projects/"+url.PathEscape(projectKey)+"/teams", nil, &out); err != nil {
			return err
		}
		return print(out)

	default:
		return fmt.Errorf("неизвестная подкоманда %q: list, create, teams, github", sub)
	}
}
