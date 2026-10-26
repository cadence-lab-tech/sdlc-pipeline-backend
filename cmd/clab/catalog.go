package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
	case "task":
		return tokenTask(ctx, c, rest)
	default:
		return fmt.Errorf("clab token create|list|revoke|task")
	}
}

// tokenTask выпускает токен задачи держателю живой аренды (решение 0049):
// его отдают исполнителю этапа через SDLC_TOKEN. Без --json печатается
// только сам токен — одной строкой, чтобы его можно было взять в переменную:
// TOKEN=$(clab token task CLAB-12).
//
// С --config-dir (CLAB-222) токен не печатается вовсе: он ложится конфигом
// clab в каталог — хост вызвавшего и токен задачи, — и исполнитель ходит
// с CLAB_CONFIG_DIR=<каталог>. Так токен не попадает ни в вывод, ни в
// промпт саб-агента: оркестратору /clab-run достаточно передать путь.
func tokenTask(ctx context.Context, c *Client, args []string) error {
	key, rest := popTask(args)
	var configDirFlag string
	if _, err := flags("token task", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&configDirFlag, "config-dir", "", "записать токен конфигом clab в этот каталог (для CLAB_CONFIG_DIR исполнителя), а не печатать")
	}); err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("clab token task <ключ задачи> [--config-dir <каталог>]")
	}
	var dir string
	if configDirFlag != "" {
		var err error
		if dir, err = taskConfigDir(configDirFlag); err != nil {
			return err
		}
	}
	var out json.RawMessage
	if err := c.post(ctx, "/tasks/"+url.PathEscape(key)+"/task-token", nil, &out); err != nil {
		return err
	}
	var issued struct {
		Token  string `json:"token"`
		Record struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"record"`
		TaskKey        string `json:"task_key"`
		Role           string `json:"role"`
		LeaseExpiresAt string `json:"lease_expires_at"`
	}
	if err := decode(out, &issued); err != nil {
		return err
	}
	if dir == "" {
		return emit(out, func() error {
			fmt.Println(issued.Token)
			return nil
		})
	}

	cfg := Config{DefaultHost: c.host, Hosts: map[string]HostConfig{
		c.host: {URL: c.origin, Token: issued.Token, TokenID: issued.Record.ID, Handle: "task:" + issued.TaskKey},
	}}
	if err := saveConfigAt(dir, cfg); err != nil {
		return err
	}
	// MkdirAll не меняет права существующего каталога, а в нём — токен.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("каталог конфига: %w", err)
	}
	written := map[string]any{"token_id": issued.Record.ID, "task_key": issued.TaskKey, "role": issued.Role,
		"lease_expires_at": issued.LeaseExpiresAt, "host": c.host, "config_dir": dir,
		"config_path": filepath.Join(dir, "config.json")}
	return emit(written, func() error {
		kv("Токен задачи:", issued.Record.ID+" ("+issued.TaskKey+", роль "+dash(issued.Role)+")",
			"Конфиг:", filepath.Join(dir, "config.json"),
			"Аренда до:", dash(issued.LeaseExpiresAt),
			"Отзыв:", "clab token revoke "+issued.Record.ID)
		fmt.Printf("\nКоманды исполнителя: env -u SDLC_TOKEN -u SDLC_API_URL CLAB_CONFIG_DIR=%s clab …\n", dir)
		return nil
	})
}

// taskConfigDir — абсолютный путь каталога для конфига токена задачи. Свой
// каталог конфига человека не годится: запись затёрла бы его вход.
func taskConfigDir(raw string) (string, error) {
	dir, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("--config-dir: %w", err)
	}
	own, err := filepath.Abs(configDir())
	if err == nil && filepath.Clean(own) == filepath.Clean(dir) {
		return "", fmt.Errorf("--config-dir %s — каталог вашего собственного конфига clab: токен задачи затёр бы вход; укажите отдельный каталог", dir)
	}
	return dir, nil
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
	switch sub {
	case "add":
		return repoAdd(ctx, c, rest)
	case "remove", "rm":
		return repoRemove(ctx, c, rest)
	case "":
		// "clab repo" и "clab repo --help" обязаны показать синтаксис обеих
		// подкоманд: имена без формы вызова не говорят, как отвязать.
		fs, err := flags("repo", rest, func(*flag.FlagSet) {})
		if err != nil {
			return err
		}
		fs.Usage()
		return fmt.Errorf("не указана подкоманда: add, remove")
	default:
		return fmt.Errorf("неизвестная подкоманда %q: add, remove", sub)
	}
}

func repoAdd(ctx context.Context, c *Client, args []string) error {
	repo, rest := popTask(args)
	var projectFlag, hint, branch, watch string
	var installation int64
	_, err := flags("repo", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&hint, "hint", "", "за что отвечает: backend, frontend, docs, infra")
		fs.StringVar(&branch, "branch", "", "основная ветка")
		fs.StringVar(&watch, "watch", "", "ветки под индексацией через запятую")
		fs.Int64Var(&installation, "installation", 0, "установка GitHub App команды-владельца; без неё подбирается та, которой репозиторий доступен")
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
	if installation != 0 {
		body["installation_id"] = installation
	}
	var out any
	if err := c.post(ctx, "/projects/"+key+"/repositories", body, &out); err != nil {
		return err
	}
	return print(out)
}

// repoRemove отвязывает репозиторий. Подтверждения здесь нет намеренно:
// команда названа целиком, а диалог — забота интерфейса.
func repoRemove(ctx context.Context, c *Client, args []string) error {
	repo, rest := popTask(args)
	var projectFlag string
	_, err := flags("repo", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
	})
	if err != nil {
		return err
	}
	if repo == "" {
		return fmt.Errorf("clab repo remove owner/name: не указан репозиторий")
	}
	owner, name, ok := strings.Cut(strings.TrimSuffix(repo, "/"), "/")
	if !ok || owner == "" || name == "" {
		return fmt.Errorf("репозиторий задаётся как owner/name, а не %q", repo)
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	var out json.RawMessage
	path := "/projects/" + key + "/repositories/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	if err := c.do(ctx, "DELETE", path, nil, &out); err != nil {
		return err
	}
	return emit(out, render(renderRepoRemoved, out))
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
		var projectFlag, roleKey, name, summary, file, contract, inputs, changelog, modelLevel string
		_, err := flags("role", rest, func(fs *flag.FlagSet) {
			fs.StringVar(&projectFlag, "project", "", "ключ проекта")
			fs.StringVar(&roleKey, "key", "", "ключ роли")
			fs.StringVar(&name, "name", "", "название")
			fs.StringVar(&summary, "summary", "", "в двух словах")
			fs.StringVar(&file, "file", "", "инструкция (markdown); - читает stdin")
			fs.StringVar(&contract, "contract", "", "файл с output_contract (JSON-список)")
			fs.StringVar(&inputs, "inputs", "", "input_kinds через запятую")
			fs.StringVar(&changelog, "changelog", "", "что изменилось")
			fs.StringVar(&modelLevel, "model-level", "", roleLevelUsage)
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
		// Версия собирается целиком: без флага уровня у неё нет, как нет
		// контракта без --contract. Пустое значение — то же «нет».
		if level := strings.TrimSpace(modelLevel); level != "" {
			role["model_level"] = level
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
		var out json.RawMessage
		if err := c.get(ctx, "/projects/"+key+"/workflows", nil, &out); err != nil {
			return err
		}
		return emit(out, render(renderWorkflows, out))
	case "validate", "activate":
		id, _ := popTask(rest)
		if id == "" {
			return fmt.Errorf("clab workflow %s <id>", sub)
		}
		var out json.RawMessage
		if sub == "validate" {
			if err := c.get(ctx, "/workflows/"+id+"/validation", nil, &out); err != nil {
				return err
			}
			return print(out)
		}
		if err := c.post(ctx, "/workflows/"+id+"/activate", map[string]any{}, &out); err != nil {
			return err
		}
		return emit(out, render(renderActivation, out))
	default:
		return fmt.Errorf("clab workflow list|validate|activate")
	}
}

// ── метрики ─────────────────────────────────────────────────────────────────

func cmdMetrics(ctx context.Context, c *Client, args []string) error {
	var projectFlag, period string
	var closedCost bool
	_, err := flags("metrics", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&period, "period", "", "окно назад: 30d, 12h")
		fs.BoolVar(&closedCost, "closed-cost", false, "таблица стоимости закрытых задач по типам вместо JSON")
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
	// Без --closed-cost вывод прежний — JSON, на нём держатся скрипты; таблица
	// только по флагу, а с --json отдаётся тело ответа и при флаге.
	if !closedCost {
		return print(out)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return emit(out, render(renderClosedCost, raw))
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
