package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strings"
)

// Команды оркестрации: завести задачу, связать задачи, назначить исполнителя,
// отчитаться о стоимости прогона (решения 0024-0026). Сюда ходит раннер и CI;
// локальный агент делает то же самое MCP-тулами.

func cmdCreate(ctx context.Context, c *Client, args []string) error {
	var projectFlag, title, description, taskType, parent, priority, assignee string
	var repos, labels, blocks, properties string
	fs, err := flags("create", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&title, "title", "", "заголовок задачи")
		fs.StringVar(&description, "description", "", "описание")
		fs.StringVar(&taskType, "type", "", "тип задачи: по нему выбирается цикл")
		fs.StringVar(&parent, "parent", "", "ключ родительской задачи")
		fs.StringVar(&priority, "priority", "", "low, normal, high, urgent")
		fs.StringVar(&assignee, "assignee", "", "хэндл исполнителя")
		fs.StringVar(&repos, "repos", "", "область задачи через запятую")
		fs.StringVar(&labels, "labels", "", "метки через запятую")
		fs.StringVar(&blocks, "blocks", "", "ключи задач, которые новая блокирует")
		fs.StringVar(&properties, "properties", "", "свойства задачи объектом JSON")
	})
	if err != nil {
		return err
	}
	key, err := project(fs, projectFlag)
	if err != nil {
		return err
	}
	if title == "" {
		return fmt.Errorf("не указан --title")
	}

	body := map[string]any{"title": title}
	if description != "" {
		body["description"] = description
	}
	if taskType != "" {
		body["type"] = taskType
	}
	if parent != "" {
		body["parent_key"] = parent
	}
	if priority != "" {
		body["priority"] = priority
	}
	if assignee != "" {
		body["assignee"] = assignee
	}
	if list := splitList(repos); len(list) > 0 {
		body["repositories"] = list
	}
	if list := splitList(labels); len(list) > 0 {
		body["labels"] = list
	}
	if list := splitList(blocks); len(list) > 0 {
		body["blocks"] = list
	}
	if properties != "" {
		var props json.RawMessage
		if err := json.Unmarshal([]byte(properties), &props); err != nil {
			return fmt.Errorf("--properties не JSON: %w", err)
		}
		body["properties"] = props
	}

	var out any
	if err := c.do(ctx, "POST", "/projects/"+key+"/tasks", body, &out); err != nil {
		return err
	}
	return print(out)
}

func cmdRelate(ctx context.Context, c *Client, args []string) error {
	var to, rel, note string
	var remove bool
	task, rest := popTask(args)
	fs, err := flags("relate", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&to, "to", "", "ключ задачи на другом конце связи")
		fs.StringVar(&rel, "rel", "blocks", "blocks, relates_to, duplicates, shares_surface, split_from")
		fs.StringVar(&note, "note", "", "пояснение к связи")
		fs.BoolVar(&remove, "remove", false, "снять связь")
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

	var out any
	if err := c.do(ctx, "POST", "/tasks/"+task+"/links", map[string]any{
		"task": to, "rel": rel, "note": note, "remove": remove,
	}, &out); err != nil {
		return err
	}
	return print(out)
}

func cmdAssign(ctx context.Context, c *Client, args []string) error {
	var assignee string
	var clear bool
	task, rest := popTask(args)
	fs, err := flags("assign", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&assignee, "to", "", "хэндл принципала")
		fs.BoolVar(&clear, "clear", false, "снять назначение")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if assignee == "" && !clear {
		return fmt.Errorf("нужен --to или --clear")
	}
	if clear {
		assignee = ""
	}

	var out any
	if err := c.do(ctx, "PUT", "/tasks/"+task+"/assignee",
		map[string]any{"assignee": assignee}, &out); err != nil {
		return err
	}
	return print(out)
}

// cmdUsage — отчёт о стоимости прогона. Сюда ходит раннер после того, как
// запущенный им агент закончил: сервер стоимость не измеряет и без отчёта
// прогон остаётся «неизвестно», а не нулём (решение 0026).
func cmdUsage(ctx context.Context, c *Client, args []string) error {
	var model, run, details string
	var in, out64, cacheRead, cacheWrite, duration int64
	var cost float64
	var show bool
	task, rest := popTask(args)
	fs, err := flags("usage", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&model, "model", "", "модель как она называется у провайдера")
		fs.StringVar(&run, "run", "", "идентификатор прогона; пусто — тот, который вы держали")
		fs.Int64Var(&in, "input", -1, "входные токены")
		fs.Int64Var(&out64, "output", -1, "выходные токены")
		fs.Int64Var(&cacheRead, "cache-read", -1, "токены, прочитанные из кэша")
		fs.Int64Var(&cacheWrite, "cache-write", -1, "токены, записанные в кэш")
		fs.Float64Var(&cost, "cost", -1, "стоимость в долларах")
		fs.Int64Var(&duration, "duration-ms", -1, "длительность работы агента")
		fs.StringVar(&details, "details", "", "произвольные подробности объектом JSON")
		fs.BoolVar(&show, "show", false, "показать стоимость задачи, ничего не записывая")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}

	if show {
		var summary any
		if err := c.get(ctx, "/tasks/"+task+"/usage", url.Values{}, &summary); err != nil {
			return err
		}
		return print(summary)
	}
	if model == "" {
		return fmt.Errorf("не указана --model")
	}

	body := map[string]any{"model": model}
	// Отрицательное значение флага означает «не сообщено»: ноль — это ноль.
	setInt(body, "input_tokens", in)
	setInt(body, "output_tokens", out64)
	setInt(body, "cache_read_tokens", cacheRead)
	setInt(body, "cache_write_tokens", cacheWrite)
	setInt(body, "duration_ms", duration)
	if cost >= 0 {
		body["cost_usd"] = cost
	}
	if details != "" {
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(details), &raw); err != nil {
			return fmt.Errorf("--details не JSON: %w", err)
		}
		body["details"] = raw
	}

	path := "/tasks/" + task + "/usage"
	if run != "" {
		path = "/tasks/" + task + "/runs/" + run + "/usage"
	}
	var recorded any
	if err := c.do(ctx, "POST", path, body, &recorded); err != nil {
		return err
	}
	return print(recorded)
}

func setInt(body map[string]any, key string, v int64) {
	if v >= 0 {
		body[key] = v
	}
}

func splitList(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
