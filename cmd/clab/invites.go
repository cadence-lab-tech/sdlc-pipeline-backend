package main

// Приглашения ссылкой (решение 0035): выдать, посмотреть и отозвать из
// терминала, как остальное управление составом. Правил CLI не знает — права,
// квоту, срок, уровни и роли проверяет сервер; здесь только форма команды.
// Приём приглашения — в браузере, по ссылке.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
)

func cmdInvite(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	switch sub {
	case "create":
		return inviteCreate(ctx, c, rest)

	case "list", "":
		var team, project string
		fs, err := flags("invite", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&team, "team", "", "ключ команды")
			fs.StringVar(&project, "project", "", "ключ проекта; без --team и --project — проект из SDLC_PROJECT или git remote")
		})
		if err != nil {
			return err
		}
		if team != "" && project != "" {
			fs.Usage()
			return errInviteBothTargets
		}
		path, err := inviteTarget(ctx, c, team, project)
		if err != nil {
			return err
		}
		var out json.RawMessage
		if err := c.get(ctx, path, nil, &out); err != nil {
			return err
		}
		return emit(out, render(renderInvites, out))

	case "revoke":
		// Идентификатор не проверяется здесь: не-UUID отбивает сервер, и его
		// текст ошибки — часть контракта.
		id, tail := popTask(rest)
		fs, err := flags("invite", tail, func(*flag.FlagSet) {})
		if err != nil {
			return err
		}
		if id == "" {
			fs.Usage()
			return fmt.Errorf("clab invite revoke <id>: идентификатор — из clab invite list")
		}
		var out json.RawMessage
		if err := c.do(ctx, "DELETE", "/invites/"+url.PathEscape(id), nil, &out); err != nil {
			return err
		}
		return emit(out, render(renderInviteRevoked, out))

	default:
		return fmt.Errorf("неизвестная подкоманда %q: create, list, revoke", sub)
	}
}

var errInviteBothTargets = fmt.Errorf("--team и --project вместе не имеют смысла: приглашение ведёт в одно место")

// inviteTarget — путь коллекции приглашений цели: команда по флагу, иначе
// проект, как у clab members (флаг, SDLC_PROJECT, git remote).
func inviteTarget(ctx context.Context, c *Client, team, project string) (string, error) {
	if team != "" && project != "" {
		return "", errInviteBothTargets
	}
	if team != "" {
		return "/teams/" + url.PathEscape(team) + "/invites", nil
	}
	key, err := c.project(ctx, project)
	if err != nil {
		return "", err
	}
	return "/projects/" + url.PathEscape(key) + "/invites", nil
}

func inviteCreate(ctx context.Context, c *Client, args []string) error {
	var team, project, role, level, roles string
	var days int
	fs, err := flags("invite", args, func(fs *flag.FlagSet) {
		fs.StringVar(&team, "team", "", "пригласить в команду (организацию)")
		fs.StringVar(&project, "project", "", "пригласить в проект; без --team и --project — проект из SDLC_PROJECT или git remote")
		fs.StringVar(&role, "role", "member", "роль в команде: member или lead")
		fs.StringVar(&level, "level", "", "уровень в проекте: viewer, agent, maintainer, admin")
		fs.StringVar(&roles, "roles", "", "роли специалистов в проекте через запятую; пусто — любые")
		fs.IntVar(&days, "days", 0, "срок ссылки в днях; без флага — по умолчанию сервера")
	})
	if err != nil {
		return err
	}
	// Задан ли флаг, а не его значение: «--days 0» уходит на сервер и
	// получает его ошибку, а отсутствие флага — срок по умолчанию.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if err := inviteForm(team, project, set); err != nil {
		fs.Usage()
		return err
	}
	body := inviteBody(team != "", role, level, roles, days, set)

	path, err := inviteTarget(ctx, c, team, project)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.post(ctx, path, body, &raw); err != nil {
		return err
	}
	var issued struct {
		Invite inviteRow `json:"invite"`
		Token  string    `json:"token"`
	}
	if err := decode(raw, &issued); err != nil {
		return err
	}
	link := inviteLink(c.origin, issued.Token)

	if jsonOut {
		// Ответ сервера плюс готовая ссылка: адрес интерфейса знает clab, а
		// не сервер, и скрипту незачем собирать его заново.
		var out map[string]json.RawMessage
		if err := decode(raw, &out); err != nil {
			return err
		}
		out["url"], _ = json.Marshal(link)
		return print(out)
	}
	inv := issued.Invite
	pairs := []string{"ID:", inv.ID, "Куда:", inv.target()}
	if inv.isProject() {
		pairs = append(pairs, "Уровень:", dash(ptr(inv.Level)), "Роли:", inv.roles())
	} else {
		pairs = append(pairs, "Роль:", dash(ptr(inv.TeamRole)))
	}
	pairs = append(pairs, "Истекает:", when(inv.ExpiresAt))
	kv(pairs...)
	fmt.Println("Ссылка: " + link)
	fmt.Fprintln(os.Stderr, "Ссылка показывается один раз: скопируйте её сейчас, повторно её не покажут. Отозвать — clab invite revoke "+inv.ID)
	return nil
}

// inviteForm проверяет сочетание флагов до запроса: цель одна, у команды —
// роль, у проекта — уровень и роли специалистов.
func inviteForm(team, project string, set map[string]bool) error {
	switch {
	case team != "" && project != "":
		return errInviteBothTargets
	case team != "" && (set["level"] || set["roles"]):
		return fmt.Errorf("--level и --roles — для приглашения в проект; в команду — --role member|lead")
	case team == "" && set["role"]:
		return fmt.Errorf("--role — для приглашения в команду (--team); в проект — --level и --roles")
	case team == "" && !set["level"]:
		return fmt.Errorf("приглашению в проект нужен --level: viewer, agent, maintainer или admin")
	}
	return nil
}

// inviteBody — тело выдачи. days и role_keys уходят, только если флаг задан:
// сервер отличает «не задано» от нуля и от пустого списка.
func inviteBody(toTeam bool, role, level, roles string, days int, set map[string]bool) map[string]any {
	body := map[string]any{}
	if toTeam {
		body["role"] = role
	} else {
		body["level"] = level
		if keys := splitCSV(roles); len(keys) > 0 {
			body["role_keys"] = keys
		}
	}
	if set["days"] {
		body["days"] = days
	}
	return body
}

// inviteLink — ссылка на страницу приглашения в интерфейсе того хоста, с
// которым работает clab.
func inviteLink(origin, token string) string {
	return strings.TrimRight(origin, "/") + "/invite/" + token
}
