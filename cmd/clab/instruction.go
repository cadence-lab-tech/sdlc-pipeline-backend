package main

// clab instruction — библиотека инструкций проекта (решение 0069): список,
// инструкция с версией и «где используется», заведение версии и «обновить
// в ролях». Таблица по умолчанию, --json — ответ сервера как есть.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// libraryMinVersion — первая версия сервера с библиотекой: её называет
// отказ сервера прежней версии.
const libraryMinVersion = "0.21.0"

// repeated — повторяемый строковый флаг (--role a --role b).
type repeated []string

func (r *repeated) String() string { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*r = append(*r, p)
		}
	}
	return nil
}

func cmdInstruction(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	var err error
	switch sub {
	case "list", "ls":
		err = instructionList(ctx, c, rest)
	case "show":
		err = instructionShow(ctx, c, rest)
	case "put":
		err = instructionPut(ctx, c, rest)
	case "apply":
		err = instructionApply(ctx, c, rest)
	default:
		return fmt.Errorf("clab instruction list | show <ключ> [--version N] | put --key <ключ> --file <файл> | apply <ключ> --role <роль>")
	}
	return libraryError(err)
}

// libraryError переводит 404 маршрута (сервер без библиотеки отвечает
// gin-овским «404 page not found» без конверта) в понятный отказ. 404 с
// кодом not_found — это «нет такой инструкции», его не трогаем.
func libraryError(err error) error {
	var api *APIError
	if errors.As(err, &api) && api.Status == http.StatusNotFound && api.Code == "" {
		return fmt.Errorf("сервер не знает библиотеку инструкций (нужна версия ≥ %s): обновите сервис", libraryMinVersion)
	}
	return err
}

type instructionRow struct {
	Key           string  `json:"key"`
	Name          string  `json:"name"`
	Summary       *string `json:"summary"`
	LatestVersion int     `json:"latest_version"`
	PinnedRoles   int     `json:"pinned_roles"`
	LaggingRoles  int     `json:"lagging_roles"`
}

func instructionList(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	if _, err := flags("instruction list", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
	}); err != nil {
		return err
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.get(ctx, "/projects/"+url.PathEscape(key)+"/instructions", nil, &raw); err != nil {
		return err
	}
	return emit(raw, render(renderInstructions, raw))
}

func renderInstructions(raw json.RawMessage) error {
	var out struct {
		Instructions []instructionRow `json:"instructions"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	t := newTable("KEY", "НАЗВАНИЕ", "ВЕРСИЯ", "ЗАКРЕПЛЕНА", "ОТСТАЮТ")
	for _, i := range out.Instructions {
		t.add(i.Key, trunc(i.Name, 40), fmt.Sprintf("v%d", i.LatestVersion),
			strconv.Itoa(i.PinnedRoles), strconv.Itoa(i.LaggingRoles))
	}
	t.print()
	return nil
}

type instructionVersionView struct {
	Version   int     `json:"version"`
	Kind      string  `json:"kind"`
	SHA256    string  `json:"sha256"`
	SizeBytes int64   `json:"size_bytes"`
	Changelog *string `json:"changelog"`
	Content   *string `json:"content"`
}

type instructionDetailView struct {
	Instruction instructionRow           `json:"instruction"`
	Versions    []instructionVersionView `json:"versions"`
	Version     *instructionVersionView  `json:"version"`
	Usage       []struct {
		RoleKey     string `json:"role_key"`
		RoleVersion int    `json:"role_version"`
		Pinned      *int   `json:"pinned_version"`
	} `json:"usage"`
}

func instructionShow(ctx context.Context, c *Client, args []string) error {
	instKey, rest := popTask(args)
	var projectFlag string
	var version int
	if _, err := flags("instruction show", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.IntVar(&version, "version", 0, "номер версии; по умолчанию последняя")
	}); err != nil {
		return err
	}
	if instKey == "" {
		return fmt.Errorf("clab instruction show <ключ> [--version N]")
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	var q url.Values
	if version > 0 {
		q = url.Values{"version": {strconv.Itoa(version)}}
	}
	var raw json.RawMessage
	if err := c.get(ctx, "/projects/"+url.PathEscape(key)+"/instructions/"+url.PathEscape(instKey), q, &raw); err != nil {
		return err
	}
	return emit(raw, render(renderInstructionDetail, raw))
}

func renderInstructionDetail(raw json.RawMessage) error {
	var d instructionDetailView
	if err := decode(raw, &d); err != nil {
		return err
	}
	i := d.Instruction
	fmt.Printf("%s  %s\n", i.Key, i.Name)
	versions := make([]string, len(d.Versions))
	for n, v := range d.Versions {
		versions[n] = strconv.Itoa(v.Version)
		if v.Version == i.LatestVersion {
			versions[n] += " (последняя)"
		}
	}
	kv("Описание:", dash(ptr(i.Summary)), "Версии:", strings.Join(versions, ", "))
	fmt.Println("Где используется:")
	if len(d.Usage) == 0 {
		fmt.Printf("  ни у одной роли нет документа %s.md\n", i.Key)
	} else {
		t := newTable("  РОЛЬ", "ВЕРСИЯ РОЛИ", "ЗАКРЕПЛЕНО")
		for _, u := range d.Usage {
			pin := "не закреплён"
			if u.Pinned != nil {
				pin = fmt.Sprintf("%s v%d", i.Key, *u.Pinned)
				if *u.Pinned < i.LatestVersion {
					pin += fmt.Sprintf(" (есть v%d)", i.LatestVersion)
				}
			}
			t.add("  "+u.RoleKey, fmt.Sprintf("v%d", u.RoleVersion), pin)
		}
		t.print()
	}
	if v := d.Version; v != nil {
		sha := v.SHA256
		if len(sha) > 7 {
			sha = sha[:7]
		}
		fmt.Printf("--- %s v%d (%s, %d байт, sha256 %s) ---\n", i.Key, v.Version, dash(v.Kind), v.SizeBytes, sha)
		text := ptr(v.Content)
		fmt.Print(text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Println()
		}
	}
	return nil
}

type instructionWriteView struct {
	Instruction instructionRow         `json:"instruction"`
	Version     instructionVersionView `json:"version"`
	Created     bool                   `json:"created"`
}

// instructionPut заводит инструкцию или её новую версию: ключ есть —
// версия, нет — инструкция с версией 1.
func instructionPut(ctx context.Context, c *Client, args []string) error {
	var projectFlag, instKey, file, name, summary, kind, changelog string
	if _, err := flags("instruction put", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&instKey, "key", "", "ключ инструкции: документ в ролях — <ключ>.md")
		fs.StringVar(&file, "file", "", "текст версии (markdown); - читает stdin")
		fs.StringVar(&name, "name", "", "название; у новой инструкции по умолчанию ключ")
		fs.StringVar(&summary, "summary", "", "описание")
		fs.StringVar(&kind, "kind", "", "тип: instructions, checklist, reference…; по умолчанию тип последней версии")
		fs.StringVar(&changelog, "changelog", "", "что изменилось")
	}); err != nil {
		return err
	}
	if instKey == "" {
		return fmt.Errorf("не указан --key")
	}
	if file == "" {
		return fmt.Errorf("не указан --file")
	}
	content, err := readContent(file, "")
	if err != nil {
		return err
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	base := "/projects/" + url.PathEscape(key) + "/instructions"
	body := map[string]any{"kind": kind, "content": string(content)}
	if changelog != "" {
		body["changelog"] = changelog
	}
	if summary != "" {
		body["summary"] = summary
	}

	var probe json.RawMessage
	err = c.get(ctx, base+"/"+url.PathEscape(instKey), nil, &probe)
	var api *APIError
	var raw json.RawMessage
	switch {
	case errors.As(err, &api) && api.Code == "not_found":
		body["key"] = instKey
		body["name"] = name
		if name == "" {
			body["name"] = instKey
		}
		if err := c.post(ctx, base, body, &raw); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if name != "" || summary != "" {
			patch := map[string]any{}
			if name != "" {
				patch["name"] = name
			}
			if summary != "" {
				patch["summary"] = summary
			}
			if err := c.do(ctx, http.MethodPatch, base+"/"+url.PathEscape(instKey), patch, nil); err != nil {
				return err
			}
		}
		if err := c.post(ctx, base+"/"+url.PathEscape(instKey)+"/versions", body, &raw); err != nil {
			return err
		}
	}
	return emit(raw, render(renderInstructionWrite, raw))
}

func renderInstructionWrite(raw json.RawMessage) error {
	var w instructionWriteView
	if err := decode(raw, &w); err != nil {
		return err
	}
	sha := w.Version.SHA256
	if len(sha) > 7 {
		sha = sha[:7]
	}
	if w.Created {
		fmt.Printf("инструкция %s заведена, версия %d (%s, %d байт, sha256 %s)\n",
			w.Instruction.Key, w.Version.Version, w.Version.Kind, w.Version.SizeBytes, sha)
		return nil
	}
	fmt.Printf("инструкция %s: версия %d создана (%s, %d байт, sha256 %s)\n",
		w.Instruction.Key, w.Version.Version, w.Version.Kind, w.Version.SizeBytes, sha)
	return nil
}

func instructionApply(ctx context.Context, c *Client, args []string) error {
	instKey, rest := popTask(args)
	var projectFlag, changelog string
	var version int
	var roles repeated
	if _, err := flags("instruction apply", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.Var(&roles, "role", "роль; флаг можно повторять или перечислить через запятую")
		fs.IntVar(&version, "version", 0, "версия инструкции; по умолчанию последняя")
		fs.StringVar(&changelog, "changelog", "", "допишется к описанию каждой новой версии роли")
	}); err != nil {
		return err
	}
	if instKey == "" {
		return fmt.Errorf("clab instruction apply <ключ> --role <роль> [--version N]")
	}
	if len(roles) == 0 {
		return fmt.Errorf("не указана роль: --role <ключ>, флаг можно повторять")
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	list := make([]map[string]any, len(roles))
	for i, r := range roles {
		list[i] = map[string]any{"key": r}
	}
	body := map[string]any{"roles": list, "version": version, "changelog": changelog}
	var raw json.RawMessage
	if err := c.post(ctx, "/projects/"+url.PathEscape(key)+"/instructions/"+url.PathEscape(instKey)+"/apply", body, &raw); err != nil {
		return err
	}
	return emit(raw, render(renderApply, raw))
}

func renderApply(raw json.RawMessage) error {
	var out struct {
		Instruction string `json:"instruction"`
		Version     int    `json:"version"`
		Roles       []struct {
			Role        string `json:"role"`
			Outcome     string `json:"outcome"`
			RoleVersion int    `json:"role_version"`
			From        *int   `json:"from_version"`
			To          int    `json:"to_version"`
		} `json:"roles"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	fmt.Printf("%s v%d:\n", out.Instruction, out.Version)
	t := newTable("  РОЛЬ", "РЕЗУЛЬТАТ", "ВЕРСИЯ РОЛИ", "ИНСТРУКЦИЯ")
	for _, r := range out.Roles {
		outcome, inst := "создана", ""
		from := "обычный документ"
		if r.From != nil {
			from = fmt.Sprintf("%s v%d", out.Instruction, *r.From)
		}
		inst = fmt.Sprintf("%s → v%d", from, r.To)
		if r.Outcome == "unchanged" {
			outcome = "без изменений"
			inst = fmt.Sprintf("%s v%d", out.Instruction, r.To)
		}
		t.add("  "+r.Role, outcome, fmt.Sprintf("v%d", r.RoleVersion), inst)
	}
	t.print()
	return nil
}
