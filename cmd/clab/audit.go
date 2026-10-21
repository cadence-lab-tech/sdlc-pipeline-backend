package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// cmdAudit — журнал действий (CLAB-210): задачи, проекта или инстанса.
// Правил здесь нет: кому что можно читать, решает сервер.
func cmdAudit(ctx context.Context, c *Client, args []string) error {
	taskKey, rest := popTask(args)
	var projectFlag, task, principal, token, channel, action, outcome, since, until, cursor string
	var instance bool
	var limit int
	_, err := flags("audit", bareProject(rest), func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "журнал проекта (без значения — текущего)")
		fs.BoolVar(&instance, "instance", false, "записи без проекта: команды, принципалы, токены (администратор системы)")
		fs.StringVar(&task, "task", "", "в журнале проекта — только эта задача")
		fs.StringVar(&principal, "principal", "", "кто: alice или user:alice")
		fs.StringVar(&token, "token", "", "каким токеном: id или префикс")
		fs.StringVar(&channel, "channel", "", "mcp или rest")
		fs.StringVar(&action, "action", "", "тул или ручка: sdlc_submit_artifact, \"POST /v1/tasks/:taskKey/artifacts\"")
		fs.StringVar(&outcome, "outcome", "", "ok или код ошибки: forbidden, unauthenticated…")
		fs.StringVar(&since, "since", "", "с даты или времени: 2026-10-21, 2026-10-21T12:00:00Z")
		fs.StringVar(&until, "until", "", "до даты или времени")
		fs.IntVar(&limit, "limit", 0, "сколько записей (по умолчанию 50, не больше 200)")
		fs.StringVar(&cursor, "cursor", "", "следующая страница: курсор из прошлого ответа")
	})
	if err != nil {
		return err
	}

	q := url.Values{}
	for k, v := range map[string]string{
		"principal": principal, "token": token, "channel": channel, "action": action,
		"outcome": outcome, "since": since, "until": until, "cursor": cursor,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}

	var path string
	scope := "task"
	switch {
	case instance:
		if taskKey != "" {
			return fmt.Errorf("--instance не сочетается с ключом задачи")
		}
		path, scope = "/audit", "instance"
	case taskKey != "":
		path = "/tasks/" + url.PathEscape(taskKey) + "/audit"
	default:
		key, err := c.project(ctx, projectFlag)
		if err != nil {
			return err
		}
		if task != "" {
			q.Set("task", task)
		}
		path, scope = "/projects/"+url.PathEscape(key)+"/audit", "project"
	}

	var out json.RawMessage
	if err := c.get(ctx, path, q, &out); err != nil {
		return err
	}
	return emit(out, func() error { return renderAudit(out, scope) })
}

// bareProject разрешает писать --project без значения: «журнал текущего
// проекта». Пакет flag требует значение, поэтому голый флаг (последний или
// перед другим флагом) превращается в --project=.
func bareProject(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if (a == "--project" || a == "-project") && (i == len(args)-1 || strings.HasPrefix(args[i+1], "-")) {
			out = append(out, a+"=")
			continue
		}
		out = append(out, a)
	}
	return out
}

type auditEntryView struct {
	At         time.Time `json:"at"`
	ProjectKey *string   `json:"project_key"`
	TaskKey    *string   `json:"task_key"`
	Principal  *struct {
		Handle string `json:"handle"`
		Kind   string `json:"kind"`
	} `json:"principal"`
	Token *struct {
		Name   *string `json:"name"`
		Prefix *string `json:"prefix"`
		Kind   *string `json:"kind"`
	} `json:"token"`
	Channel string `json:"channel"`
	Action  string `json:"action"`
	Outcome struct {
		OK     bool    `json:"ok"`
		Code   *string `json:"code"`
		Status *int    `json:"status"`
	} `json:"outcome"`
}

func renderAudit(raw json.RawMessage, scope string) error {
	var page struct {
		Entries []auditEntryView `json:"entries"`
		Next    *string          `json:"next"`
	}
	if err := decode(raw, &page); err != nil {
		return err
	}
	headers := []string{"ВРЕМЯ", "КТО", "ТОКЕН", "КАНАЛ", "ДЕЙСТВИЕ", "ИСХОД"}
	if scope == "project" {
		headers = []string{"ВРЕМЯ", "ЗАДАЧА", "КТО", "ТОКЕН", "КАНАЛ", "ДЕЙСТВИЕ", "ИСХОД"}
	}
	t := newTable(headers...)
	for _, e := range page.Entries {
		at := e.At
		cells := []string{when(&at)}
		if scope == "project" {
			cells = append(cells, dash(ptr(e.TaskKey)))
		}
		cells = append(cells, auditWho(e), auditToken(e), e.Channel, trunc(e.Action, 60), auditOutcome(e))
		t.add(cells...)
	}
	t.print()
	if page.Next != nil {
		fmt.Printf("\nдальше: --cursor %s\n", *page.Next)
	}
	return nil
}

func auditWho(e auditEntryView) string {
	if e.Principal == nil {
		return "—"
	}
	return e.Principal.Kind + ":" + e.Principal.Handle
}

// auditToken — имя токена и его вид: личный (api), сессия интерфейса
// (session) или вид, которого CLI ещё не знает, — как есть.
func auditToken(e auditEntryView) string {
	if e.Token == nil {
		return "—"
	}
	name := ptr(e.Token.Name)
	if name == "" {
		name = ptr(e.Token.Prefix)
	}
	if k := ptr(e.Token.Kind); k != "" {
		return dash(name) + " · " + k
	}
	return dash(name)
}

func auditOutcome(e auditEntryView) string {
	out := "ok"
	if !e.Outcome.OK {
		out = ptr(e.Outcome.Code)
	}
	if e.Outcome.Status != nil {
		out += " " + strconv.Itoa(*e.Outcome.Status)
	}
	return out
}
