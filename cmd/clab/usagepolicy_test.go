package main

import (
	"reflect"
	"strings"
	"testing"
)

// Форма clab project set (CLAB-219): тело правки политики учёта. Пределы окна
// проверяет сервер, здесь — только разбор.
func TestUsagePolicyBody(t *testing.T) {
	ok := map[string]struct {
		args []string
		want map[string]any
	}{
		"включить":   {[]string{"ACME", "usage-policy", "required"}, map[string]any{"mode": "required"}},
		"выключить":  {[]string{"ACME", "usage-policy", "off"}, map[string]any{"mode": "off"}},
		"окно":       {[]string{"ACME", "usage-grace", "30"}, map[string]any{"grace_minutes": 30}},
		"без окна":   {[]string{"ACME", "usage-grace", "0"}, map[string]any{"grace_minutes": 0}},
		"по умолчан": {[]string{"ACME", "usage-grace", "default"}, map[string]any{"grace_minutes": nil}},
		// Отрицательное и слишком большое окно отбивает сервер с подсказкой.
		"минус": {[]string{"ACME", "usage-grace", "-1"}, map[string]any{"grace_minutes": -1}},
	}
	for name, tc := range ok {
		got, err := usagePolicyBody(tc.args)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v %v, ожидали %v", name, got, err, tc.want)
		}
	}
	bad := map[string][]string{
		"без ключа":      {"usage-policy", "required"},
		"флаг вместо":    {"--project", "usage-policy", "required"},
		"режим":          {"ACME", "usage-policy", "strict"},
		"дробное окно":   {"ACME", "usage-grace", "1.5"},
		"буквы":          {"ACME", "usage-grace", "abc"},
		"чужая настройк": {"ACME", "budget", "10"},
	}
	for name, args := range bad {
		if _, err := usagePolicyBody(args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
