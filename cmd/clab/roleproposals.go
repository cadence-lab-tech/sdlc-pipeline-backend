package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Предложения версий ролей (CLAB-381, решение 0072 п. 5): участник уровня
// agent предлагает версию своей роли, сопровождающий утверждает или
// отклоняет. Право считает сервер; clab только собирает версию из тех же
// источников, что role put, и печатает ответ общим print.

// proposeHint — подсказка к отказу 403 на clab role put: роль put идёт
// через /import, и сервер не знает, что это была правка роли.
const proposeHint = "версию роли участник уровня agent предлагает, а утверждает сопровождающий: " +
	"clab role propose <роль> --dir <каталог> | --file <файл> --changelog \"…\""

// withProposeHint дописывает подсказку к отказу 403, если сервер её ещё не
// дал (сервер прежней версии или /import).
func withProposeHint(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "forbidden" || strings.Contains(apiErr.Message, "role propose") {
		return err
	}
	return &APIError{Code: apiErr.Code, Message: apiErr.Message + "; " + proposeHint,
		Details: apiErr.Details, Status: apiErr.Status}
}

func roleProposalsPath(project, role string) string {
	return "/projects/" + url.PathEscape(project) + "/roles/" + url.PathEscape(role) + "/proposals"
}

// rolePropose — clab role propose <роль> --dir D | --file F --changelog "…".
// Тело — форма версии роли REST: документы содержимым, закреплённый за
// библиотекой документ каталога — ссылкой {key} с содержимым (сервер
// находит версию инструкции по sha).
func rolePropose(ctx context.Context, c *Client, args []string) error {
	roleKey, rest := popTask(args)
	var projectFlag, file, dir, contract, inputs, changelog, modelLevel string
	_, err := flags("role", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&file, "file", "", "инструкция одним файлом (markdown) — документ main.md; - читает stdin")
		fs.StringVar(&dir, "dir", "", "каталог роли: role.json и <имя>.md, как у role put --dir")
		fs.StringVar(&contract, "contract", "", "файл с output_contract (JSON-список)")
		fs.StringVar(&inputs, "inputs", "", "input_kinds через запятую")
		fs.StringVar(&changelog, "changelog", "", "что и зачем меняется — обязательно")
		fs.StringVar(&modelLevel, "model-level", "", roleLevelUsage)
	})
	if err != nil {
		return err
	}
	if dir != "" && file != "" {
		return fmt.Errorf("--dir и --file вместе не задаются: каталог роли или один файл main.md")
	}
	if dir == "" && file == "" {
		return fmt.Errorf("clab role propose <роль> --dir <каталог> | --file <файл> --changelog \"…\"")
	}
	body := map[string]any{}
	if dir != "" {
		role, err := readRoleDir(dir)
		if err != nil {
			return err
		}
		if roleKey == "" {
			roleKey = roleFieldString(role, "key")
		}
		if changelog == "" {
			changelog = roleFieldString(role, "changelog")
		}
		// Поля версии из role.json как есть; вывеска роли (key, name,
		// summary) версией не меняется и на сервер не уходит.
		for k, v := range role {
			switch k {
			case "key", "name", "summary", "changelog", "documents":
				continue
			}
			body[k] = v
		}
		docs := role["documents"].([]map[string]any)
		out := make([]map[string]any, len(docs))
		for i, d := range docs {
			doc := map[string]any{"name": d["name"], "content": d["content"]}
			if k, ok := d["kind"]; ok {
				doc["kind"] = k
			}
			if inst, ok := d["instruction"].(string); ok && inst != "" {
				doc["instruction"] = map[string]any{"key": inst}
			}
			out[i] = doc
		}
		body["documents"] = out
	} else {
		instructions, err := readContent(file, "")
		if err != nil {
			return err
		}
		body["instructions"] = string(instructions)
	}
	if roleKey == "" {
		return fmt.Errorf("не указана роль: clab role propose <роль> (или key в role.json)")
	}
	if strings.TrimSpace(changelog) == "" {
		return fmt.Errorf("--changelog обязателен: сопровождающий решает по нему, что и зачем меняется")
	}
	body["changelog"] = changelog
	if inputs != "" {
		body["input_kinds"] = splitCSV(inputs)
	}
	if level := strings.TrimSpace(modelLevel); level != "" {
		body["model_level"] = level
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
		body["output_contract"] = parsed
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	var out any
	if err := c.post(ctx, roleProposalsPath(key, roleKey), body, &out); err != nil {
		return err
	}
	return print(out)
}

// roleProposals — clab role proposals <роль> [--state pending] [<id>]:
// список роли или одно предложение с содержимым документов.
func roleProposals(ctx context.Context, c *Client, args []string) error {
	var projectFlag, state string
	_, positional, err := flagsInterspersed("role", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&state, "state", "", "pending, approved, rejected, withdrawn; пусто — все")
	})
	if err != nil {
		return err
	}
	if len(positional) == 0 || len(positional) > 2 {
		return fmt.Errorf("clab role proposals <роль> [--state pending] [<id>]")
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	var out any
	if len(positional) == 2 {
		if err := c.get(ctx, roleProposalsPath(key, positional[0])+"/"+url.PathEscape(positional[1]), nil, &out); err != nil {
			return err
		}
		return print(out)
	}
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	if err := c.get(ctx, roleProposalsPath(key, positional[0]), q, &out); err != nil {
		return err
	}
	return print(out)
}

// roleDecide — clab role approve|reject|withdraw <id> [--role <роль>]
// [--note "…"]. Без --role роль находится перебором ролей проекта: путь
// REST — по роли, а человек знает предложение по id из списка.
func roleDecide(ctx context.Context, c *Client, action string, args []string) error {
	var projectFlag, roleKey, note string
	_, positional, err := flagsInterspersed("role", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&roleKey, "role", "", "ключ роли предложения; без него ищется по ролям проекта")
		fs.StringVar(&note, "note", "", "заметка автору; у reject обязательна")
	})
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("clab role %s <id> [--role <роль>] [--note \"…\"]", action)
	}
	id := positional[0]
	if action == "reject" && strings.TrimSpace(note) == "" {
		return fmt.Errorf("--note обязателен: напишите автору, что поправить")
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	if roleKey == "" {
		if roleKey, err = proposalRole(ctx, c, key, id); err != nil {
			return err
		}
	}
	var body any
	if note != "" {
		body = map[string]any{"note": note}
	}
	var out any
	if err := c.post(ctx, roleProposalsPath(key, roleKey)+"/"+url.PathEscape(id)+"/"+action, body, &out); err != nil {
		return err
	}
	return print(out)
}

// proposalRole ищет роль предложения id среди ролей проекта, архивные
// тоже: предложение могли подать до архивации.
func proposalRole(ctx context.Context, c *Client, project, id string) (string, error) {
	var roles struct {
		Roles []struct {
			Key string `json:"key"`
		} `json:"roles"`
	}
	if err := c.get(ctx, "/projects/"+url.PathEscape(project)+"/roles",
		url.Values{"include_archived": {"true"}}, &roles); err != nil {
		return "", err
	}
	for _, r := range roles.Roles {
		var out any
		err := c.get(ctx, roleProposalsPath(project, r.Key)+"/"+url.PathEscape(id), nil, &out)
		if err == nil {
			return r.Key, nil
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "not_found" {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("предложение %s не найдено ни у одной роли проекта %s", id, project)
}
