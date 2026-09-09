package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Команды фазы 4: токены и участники, перенос правил, каталог репозиториев и
// документов, метрики. Как и остальной CLI — тонкая обёртка над REST.

// ── кто я и токены ──────────────────────────────────────────────────────────

func cmdMe(ctx context.Context, c *Client, args []string) error {
	if _, err := flags("me", args, func(*flag.FlagSet) {}); err != nil {
		return err
	}
	var out json.RawMessage
	if err := c.get(ctx, "/auth/me", nil, &out); err != nil {
		return err
	}
	return emit(out, render(renderMe, out))
}

func cmdToken(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	switch sub {
	case "create":
		var name, forPrincipal, projectFlag, roles, level string
		var days int
		fs, err := flags("token", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "имя токена: по нему его потом отзывают")
			fs.StringVar(&forPrincipal, "for", "", "кому: agent:bot или user:alice; пусто — себе")
			fs.StringVar(&projectFlag, "project", "", "ограничить токен проектом")
			fs.StringVar(&roles, "roles", "", "роли специалистов, которые агент сможет забирать, через запятую")
			fs.StringVar(&level, "level", "", "уровень участия в проекте: viewer, agent, maintainer, admin")
			fs.IntVar(&days, "days", 0, "срок действия в днях; 0 — бессрочно")
		})
		if err != nil {
			return err
		}
		if name == "" {
			fs.Usage()
			return fmt.Errorf("не указано --name")
		}
		body := map[string]any{"name": name, "principal": forPrincipal, "member_role": level}
		if projectFlag == "" {
			projectFlag = os.Getenv("SDLC_PROJECT")
		}
		if projectFlag != "" {
			body["project"] = projectFlag
		}
		if roles != "" {
			body["role_keys"] = splitCSV(roles)
		}
		if days > 0 {
			body["expires_in_days"] = days
		}
		var out any
		if err := c.post(ctx, "/auth/tokens", body, &out); err != nil {
			return err
		}
		return print(out)
	case "list", "":
		var projectFlag string
		if _, err := flags("token", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "токены проекта (для его администратора)")
		}); err != nil {
			return err
		}
		q := url.Values{}
		if projectFlag != "" {
			q.Set("project", projectFlag)
		}
		var out json.RawMessage
		if err := c.get(ctx, "/auth/tokens", q, &out); err != nil {
			return err
		}
		return emit(out, render(renderTokens, out))
	case "revoke":
		id, _ := popTask(rest)
		if id == "" {
			return fmt.Errorf("clab token revoke <id>")
		}
		if err := c.do(ctx, "DELETE", "/auth/tokens/"+id, nil, nil); err != nil {
			return err
		}
		return print(map[string]any{"revoked": id})
	default:
		return fmt.Errorf("clab token create|list|revoke")
	}
}

func cmdMembers(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	var projectFlag, roles string
	parse := func(name string, positional int) ([]string, error) {
		fs := flag.NewFlagSet("members", flag.ContinueOnError)
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&roles, "roles", "", "роли специалистов через запятую, которые участник может забирать; пусто — любые")
		// Позиционные аргументы идут до флагов: "members set user:alice admin --project X".
		pos := []string{}
		for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && len(pos) < positional {
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if len(pos) < positional {
			return nil, fmt.Errorf("clab members %s: не хватает аргументов", name)
		}
		return pos, nil
	}

	switch sub {
	case "set":
		pos, err := parse("set", 2)
		if err != nil {
			return err
		}
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		var out any
		if err := c.do(ctx, "PUT", "/projects/"+key+"/members",
			map[string]any{"principal": pos[0], "role": pos[1], "role_keys": splitCSV(roles)}, &out); err != nil {
			return err
		}
		return print(out)
	case "remove":
		pos, err := parse("remove", 1)
		if err != nil {
			return err
		}
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		if err := c.do(ctx, "DELETE", "/projects/"+key+"/members/"+url.PathEscape(pos[0]), nil, nil); err != nil {
			return err
		}
		return print(map[string]any{"removed": pos[0]})
	case "list", "":
		if _, err := parse("list", 0); err != nil {
			return err
		}
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		var out json.RawMessage
		if err := c.get(ctx, "/projects/"+key+"/members", nil, &out); err != nil {
			return err
		}
		return emit(out, render(renderMembers, out))
	default:
		return fmt.Errorf("clab members list|set|remove")
	}
}

// ── перенос правил ──────────────────────────────────────────────────────────

func cmdExport(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	_, err := flags("export", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
	})
	if err != nil {
		return err
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	var out any
	if err := c.get(ctx, "/projects/"+key+"/export", nil, &out); err != nil {
		return err
	}
	return print(out)
}

// cmdImport применяет связку к проекту, а с --to — к нескольким сразу: связка
// и есть способ раздать цикл и роли командам (решение 0022). Источником может
// быть файл или проект-шаблон (--from) — тогда экспорт делается на месте, без
// файла посередине.
//
// Отчёт остаётся по проектам: `unchanged` против `new_version` показывает, где
// команда ушла от шаблона. Сливать её правки с обновлением шаблона система не
// берётся — это решение человека, и правила у него пока нет (§13).
func cmdImport(ctx context.Context, c *Client, args []string) error {
	var projectFlag, file, from, to string
	var dryRun bool
	_, err := flags("import", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&file, "file", "", "связка JSON; - читает stdin")
		fs.StringVar(&from, "from", "", "взять связку из проекта-шаблона вместо файла")
		fs.StringVar(&to, "to", "", "применить к нескольким проектам через запятую")
		fs.BoolVar(&dryRun, "dry-run", false, "только отчёт, без изменений")
	})
	if err != nil {
		return err
	}

	targets := splitCSV(to)
	if len(targets) == 0 {
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		targets = []string{key}
	}

	var bundle json.RawMessage
	switch {
	case from != "":
		if err := c.get(ctx, "/projects/"+url.PathEscape(from)+"/export", nil, &bundle); err != nil {
			return err
		}
	default:
		body, err := readContent(file, "")
		if err != nil {
			return err
		}
		if err := json.Unmarshal(body, &bundle); err != nil {
			return fmt.Errorf("связка не разбирается как JSON: %w", err)
		}
	}

	reports := make([]map[string]any, 0, len(targets))
	for _, key := range targets {
		path := "/projects/" + url.PathEscape(key) + "/import"
		if dryRun {
			path += "?dry_run=true"
		}
		var out map[string]any
		if err := c.post(ctx, path, bundle, &out); err != nil {
			// Один проект не должен обрывать раздачу шаблона по остальным:
			// у команды могло не хватить прав или разъехаться цикл.
			reports = append(reports, map[string]any{"project": key, "error": err.Error()})
			continue
		}
		out["project"] = key
		reports = append(reports, out)
	}
	if len(reports) == 1 {
		return print(reports[0])
	}
	return print(map[string]any{"imports": reports})
}

func cmdImportGuide(_ context.Context, _ *Client, _ []string) error {
	fmt.Println(importGuide)
	return nil
}

// ── репозитории ─────────────────────────────────────────────────────────────

func cmdRepo(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	if sub != "add" {
		return fmt.Errorf("clab repo add owner/name [--hint backend] [--branch main]")
	}
	repo, rest := popTask(rest)
	var projectFlag, hint, branch, watch string
	_, err := flags("repo", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&hint, "hint", "", "за что отвечает: backend, frontend, docs, infra")
		fs.StringVar(&branch, "branch", "", "основная ветка")
		fs.StringVar(&watch, "watch", "", "ветки под индексацией через запятую")
	})
	if err != nil {
		return err
	}
	if repo == "" {
		return fmt.Errorf("не указан репозиторий: owner/name или URL")
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	body := map[string]any{"repository": repo, "role_hint": hint, "default_branch": branch}
	if watch != "" {
		body["watch_branches"] = splitCSV(watch)
	}
	var out any
	if err := c.post(ctx, "/projects/"+key+"/repositories", body, &out); err != nil {
		return err
	}
	return print(out)
}

// ── роли ────────────────────────────────────────────────────────────────────

func cmdRole(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	switch sub {
	case "show":
		roleKey, rest := popTask(rest)
		var projectFlag string
		_, err := flags("role", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		})
		if err != nil {
			return err
		}
		if roleKey == "" {
			return fmt.Errorf("clab role show <key>")
		}
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		var out any
		if err := c.get(ctx, "/projects/"+key+"/roles/"+roleKey, nil, &out); err != nil {
			return err
		}
		return print(out)
	case "put":
		var projectFlag, roleKey, name, summary, file, contract, inputs, changelog string
		_, err := flags("role", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
			fs.StringVar(&roleKey, "key", "", "ключ роли")
			fs.StringVar(&name, "name", "", "название")
			fs.StringVar(&summary, "summary", "", "в двух словах")
			fs.StringVar(&file, "file", "", "инструкция (markdown); - читает stdin")
			fs.StringVar(&contract, "contract", "", "файл с output_contract (JSON-список)")
			fs.StringVar(&inputs, "inputs", "", "input_kinds через запятую")
			fs.StringVar(&changelog, "changelog", "", "что изменилось")
		})
		if err != nil {
			return err
		}
		if roleKey == "" {
			return fmt.Errorf("не указан --key")
		}
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		instructions, err := readContent(file, "")
		if err != nil {
			return err
		}
		role := map[string]any{
			"key": roleKey, "name": name, "summary": summary,
			"instructions": string(instructions), "changelog": changelog,
		}
		if inputs != "" {
			role["input_kinds"] = splitCSV(inputs)
		}
		if contract != "" {
			raw, err := os.ReadFile(contract)
			if err != nil {
				return err
			}
			var parsed json.RawMessage
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return fmt.Errorf("--contract не разбирается как JSON: %w", err)
			}
			role["output_contract"] = parsed
		}
		var out any
		if err := c.post(ctx, "/projects/"+key+"/import",
			map[string]any{"version": 1, "roles": []any{role}}, &out); err != nil {
			return err
		}
		return print(out)
	default:
		return fmt.Errorf("clab role show|put")
	}
}

// ── документы ───────────────────────────────────────────────────────────────

func cmdDoc(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	switch sub {
	case "list", "":
		var projectFlag string
		_, err := flags("doc", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		})
		if err != nil {
			return err
		}
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		var out any
		if err := c.get(ctx, "/projects/"+key+"/documents", nil, &out); err != nil {
			return err
		}
		return print(out)
	case "get":
		id, rest := popTask(rest)
		var projectFlag string
		_, err := flags("doc", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		})
		if err != nil {
			return err
		}
		if id == "" {
			return fmt.Errorf("clab doc get <id>")
		}
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		raw, err := c.getRaw(ctx, "/projects/"+key+"/documents/"+id+"/content")
		if err != nil {
			return err
		}
		os.Stdout.Write(raw)
		return nil
	case "put":
		var projectFlag, kind, title, file, link, repo, repoPath string
		_, err := flags("doc", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
			fs.StringVar(&kind, "kind", "", "guideline | architecture | adr | api_registry | glossary | runbook")
			fs.StringVar(&title, "title", "", "название")
			fs.StringVar(&file, "file", "", "содержимое; - читает stdin")
			fs.StringVar(&link, "url", "", "документ по ссылке")
			fs.StringVar(&repo, "repo", "", "репозиторий, в котором живёт файл")
			fs.StringVar(&repoPath, "path", "", "путь к файлу в репозитории")
		})
		if err != nil {
			return err
		}
		if kind == "" || title == "" {
			return fmt.Errorf("нужны --kind и --title")
		}
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		body := map[string]any{"kind": kind, "title": title, "url": link,
			"repository": repo, "repo_path": repoPath}
		if file != "" {
			content, err := readContent(file, "")
			if err != nil {
				return err
			}
			body["content"] = string(content)
			body["format"] = guessFormat(file)
		}
		var out any
		if err := c.post(ctx, "/projects/"+key+"/documents", body, &out); err != nil {
			return err
		}
		return print(out)
	case "delete":
		id, rest := popTask(rest)
		var projectFlag string
		_, err := flags("doc", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		})
		if err != nil {
			return err
		}
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		if err := c.do(ctx, "DELETE", "/projects/"+key+"/documents/"+id, nil, nil); err != nil {
			return err
		}
		return print(map[string]any{"deleted": id})
	default:
		return fmt.Errorf("clab doc list|get|put|delete")
	}
}

// ── версии цикла ────────────────────────────────────────────────────────────

// cmdWorkflow нужен импорту: цикл из связки приходит черновиком, и
// активировать его должен человек, увидев результат проверки.
func cmdWorkflow(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	switch sub {
	case "list", "":
		var projectFlag string
		_, err := flags("workflow", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		})
		if err != nil {
			return err
		}
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		var out any
		if err := c.get(ctx, "/projects/"+key+"/workflows", nil, &out); err != nil {
			return err
		}
		return print(out)
	case "validate", "activate":
		id, _ := popTask(rest)
		if id == "" {
			return fmt.Errorf("clab workflow %s <id>", sub)
		}
		var out any
		if sub == "validate" {
			if err := c.get(ctx, "/workflows/"+id+"/validation", nil, &out); err != nil {
				return err
			}
		} else if err := c.post(ctx, "/workflows/"+id+"/activate", map[string]any{}, &out); err != nil {
			return err
		}
		return print(out)
	default:
		return fmt.Errorf("clab workflow list|validate|activate")
	}
}

// ── метрики ─────────────────────────────────────────────────────────────────

func cmdMetrics(ctx context.Context, c *Client, args []string) error {
	var projectFlag, period string
	_, err := flags("metrics", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&period, "period", "", "окно назад: 30d, 12h")
	})
	if err != nil {
		return err
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	q := url.Values{}
	if period != "" {
		q.Set("period", period)
	}
	var out any
	if err := c.get(ctx, "/projects/"+key+"/metrics", q, &out); err != nil {
		return err
	}
	return print(out)
}

func splitCSV(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// importGuide дублирует текст MCP-тула sdlc_import_guide: CLI не ходит в MCP,
// а агенту без MCP инструкция нужна та же.
const importGuide = `Перенос правил команды в проект SDLC

1. Найди в репозитории: CLAUDE.md, AGENTS.md, .cursorrules, .cursor/rules/*,
   .github/copilot-instructions.md, CONTRIBUTING.md, docs/ARCHITECTURE.md,
   docs/adr/*, глоссарии, реестры API, описания ролей и чек-листы ревью.
2. Описание работы специалиста -> роль (roles[].instructions + output_contract).
   Соглашения, архитектура, ADR, глоссарий -> документы (documents[]).
   Репозитории проекта -> repositories[] с role_hint. Явные этапы с условиями
   перехода -> workflow (придёт черновиком).
3. Собери bundle.json:
   {"version":1,
    "repositories":[{"repository":"acme/api","role_hint":"backend"}],
    "roles":[{"key":"reviewer","name":"Ревьюер","instructions":"...",
              "output_contract":[{"kind":"review.verdict","format":"json","required":true}]}],
    "documents":[{"kind":"guideline","title":"Соглашения","content":"..."}]}
4. clab import --file bundle.json --dry-run  -> покажи отчёт человеку.
5. clab import --file bundle.json            -> примени.
Ничего не сочиняй: скажи, чего в репозитории не нашлось.`
