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

// usagePolicyNote — кого держит политика учёта (CLAB-219): её читают и
// администратор, который включает, и сотрудник, который гадает, встанет ли
// его раннер.
const usagePolicyNote = "Политика действует только на токены агентов (clab token create --for agent:…): " +
	"человек под своей сессией и раннер сотрудника на личном токене под неё не попадают."

// projectSettings показывает настройки проекта: пока это политика учёта.
func projectSettings(ctx context.Context, c *Client, args []string) error {
	key, rest := popTask(args)
	var projectFlag string
	if _, err := flags("project", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
	}); err != nil {
		return err
	}
	if key == "" {
		key = projectFlag
	}
	projectKey, err := c.project(ctx, key)
	if err != nil {
		return err
	}
	var out json.RawMessage
	if err := c.get(ctx, "/projects/"+url.PathEscape(projectKey)+"/usage-policy", nil, &out); err != nil {
		return err
	}
	return emit(out, render(renderUsagePolicy, out))
}

// usagePolicySetUsage — форма clab project set.
const usagePolicySetUsage = "clab project set <КЛЮЧ> usage-policy required|off | " +
	"clab project set <КЛЮЧ> usage-grace <минуты>|default"

// projectSet меняет настройку проекта. Ключ проекта называется явно, как у
// удаления: менять политику проекта, подобранного по текущему каталогу, —
// лёгкий способ промахнуться.
func projectSet(ctx context.Context, c *Client, args []string) error {
	body, err := usagePolicyBody(args)
	if err != nil {
		return err
	}
	var out json.RawMessage
	if err := c.do(ctx, "PATCH", "/projects/"+url.PathEscape(args[0])+"/usage-policy", body, &out); err != nil {
		return err
	}
	return emit(out, render(renderUsagePolicy, out))
}

// usagePolicyBody — тело правки из аргументов: <КЛЮЧ> <настройка> <значение>.
// Форму проверяет CLI, пределы окна — сервер: правило одно, в движке.
func usagePolicyBody(args []string) (map[string]any, error) {
	if len(args) != 3 || strings.HasPrefix(args[0], "-") {
		return nil, fmt.Errorf("%s", usagePolicySetUsage)
	}
	name, value := args[1], strings.TrimSpace(args[2])
	switch name {
	case "usage-policy":
		if value != "required" && value != "off" {
			return nil, fmt.Errorf("usage-policy: %q — допустимо required или off", value)
		}
		return map[string]any{"mode": value}, nil
	case "usage-grace":
		if value == "default" {
			return map[string]any{"grace_minutes": nil}, nil
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("usage-grace: %q — нужно целое число минут (0 — без окна) или default", value)
		}
		return map[string]any{"grace_minutes": n}, nil
	}
	return nil, fmt.Errorf("неизвестная настройка %q: usage-policy, usage-grace\n%s", name, usagePolicySetUsage)
}

func renderUsagePolicy(raw json.RawMessage) error {
	var p struct {
		Project               string     `json:"project"`
		Mode                  string     `json:"mode"`
		GraceMinutes          *int       `json:"grace_minutes"`
		EffectiveGraceMinutes int        `json:"effective_grace_minutes"`
		EnabledAt             *time.Time `json:"enabled_at"`
	}
	if err := decode(raw, &p); err != nil {
		return err
	}
	mode := "off — захват без учёта разрешён"
	if p.Mode == "required" {
		mode = "required — захват без учёта запрещён"
	}
	grace := fmt.Sprintf("%d мин (по умолчанию)", p.EffectiveGraceMinutes)
	if p.GraceMinutes != nil {
		grace = fmt.Sprintf("%d мин", *p.GraceMinutes)
		if *p.GraceMinutes == 0 {
			grace = "0 — без окна"
		}
	}
	fmt.Printf("Проект:            %s\n", p.Project)
	fmt.Printf("Политика учёта:    %s\n", mode)
	fmt.Printf("Льготное окно:     %s\n", grace)
	if p.EnabledAt != nil {
		fmt.Printf("Включена:          %s\n", p.EnabledAt.Local().Format("2006-01-02 15:04"))
	}
	fmt.Println(usagePolicyNote)
	return nil
}
