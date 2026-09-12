package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const editedTask = `{"key":"CLAB-58","seq":58,"title":"Новый заголовок","description":"Новое описание",` +
	`"status_key":"development","priority":"high","labels":["bug"],"properties":{"type":"task"},` +
	`"created_at":"2026-09-12T10:00:00Z","updated_at":"2026-09-12T12:00:00Z"}`

// Переданный флаг и не переданный — разные вещи: `--description ""` очищает
// описание, а отсутствие флага его не трогает. Тело запроса собирается по
// фактически переданным флагам, поэтому проверяется именно оно.
func TestEditBodyCarriesOnlyPassedFlags(t *testing.T) {
	withJSON(t, false)
	cases := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"--title", "Новый заголовок"}, map[string]any{"title": "Новый заголовок"}},
		{[]string{"--description", "Текст"}, map[string]any{"description": "Текст"}},
		{[]string{"--description", ""}, map[string]any{"description": ""}},
		{[]string{"--priority", "high"}, map[string]any{"priority": "high"}},
		{[]string{"--labels", "bug, phase8"}, map[string]any{"labels": []any{"bug", "phase8"}}},
		{[]string{"--labels", ""}, map[string]any{"labels": []any{}}},
		{[]string{"--title", "И то", "--priority", "low"},
			map[string]any{"title": "И то", "priority": "low"}},
	}
	for _, tc := range cases {
		c, rec := newInviteClient(t, http.StatusOK, editedTask)
		stdout, _, err := capture(t, func() error {
			return cmdEdit(context.Background(), c, append([]string{"CLAB-58"}, tc.args...))
		})
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if rec.method != "PATCH" || rec.path != "/v1/tasks/CLAB-58" {
			t.Fatalf("%v: %s %s", tc.args, rec.method, rec.path)
		}
		if !equalJSON(rec.body, tc.want) {
			t.Fatalf("%v: тело %v, ждали %v", tc.args, rec.body, tc.want)
		}
		// Сводка человеку — ключ, заголовок, статус и постановка целиком:
		// у правки приоритета и меток иначе не видно, что она применилась.
		for _, want := range []string{"CLAB-58", "Новый заголовок", "development", "high", "bug"} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("%v: в сводке нет %q:\n%s", tc.args, want, stdout)
			}
		}
	}
}

// Описание — это абзацы: оно читается из файла и со stdin, а не только из
// аргумента командной строки.
func TestEditDescriptionFromFileAndStdin(t *testing.T) {
	withJSON(t, false)
	text := "# Уточнение\n\nРучка уже есть в коде.\n"

	path := filepath.Join(t.TempDir(), "d.md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	c, rec := newInviteClient(t, http.StatusOK, editedTask)
	if _, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--description-file", path})
	}); err != nil {
		t.Fatal(err)
	}
	if !equalJSON(rec.body, map[string]any{"description": text}) {
		t.Fatalf("описание из файла: %v", rec.body)
	}

	stdin := filepath.Join(t.TempDir(), "stdin.md")
	if err := os.WriteFile(stdin, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(stdin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()

	c, rec = newInviteClient(t, http.StatusOK, editedTask)
	if _, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--description", "-"})
	}); err != nil {
		t.Fatal(err)
	}
	if !equalJSON(rec.body, map[string]any{"description": text}) {
		t.Fatalf("описание со stdin: %v", rec.body)
	}

	// Несуществующий файл — ошибка до запроса: сервер не при чём.
	c, rec = newInviteClient(t, http.StatusOK, editedTask)
	if _, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--description-file", path + ".нет"})
	}); err == nil || rec.calls != 0 {
		t.Fatalf("чтение отсутствующего файла: %v, запросов %d", err, rec.calls)
	}
}

// Ошибки формы команды ловятся до запроса.
func TestEditRefusesEmptyAndConflictingFlags(t *testing.T) {
	withJSON(t, false)
	cases := [][]string{
		{"CLAB-58"},
		{},
		{"CLAB-58", "--description", "Текст", "--description-file", "d.md"},
	}
	for _, args := range cases {
		c, rec := newInviteClient(t, http.StatusOK, editedTask)
		_, stderr, err := capture(t, func() error {
			return cmdEdit(context.Background(), c, args)
		})
		if err == nil || rec.calls != 0 {
			t.Fatalf("%v: ошибка %v, запросов %d", args, err, rec.calls)
		}
		if !strings.Contains(stderr, "clab edit") {
			t.Fatalf("%v: нет usage в stderr:\n%s", args, stderr)
		}
	}
}

// --json отдаёт ответ сервера как есть: на него смотрят скрипты.
func TestEditJSONIsServerReply(t *testing.T) {
	withJSON(t, true)
	c, _ := newInviteClient(t, http.StatusOK, editedTask)
	stdout, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--title", "Новый заголовок"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := decode([]byte(stdout), &got); err != nil {
		t.Fatalf("не JSON: %s", stdout)
	}
	if err := decode([]byte(editedTask), &want); err != nil {
		t.Fatal(err)
	}
	if !equalJSON(got, want) {
		t.Fatalf("JSON %v, ждали %v", got, want)
	}
}

// Команда видна в общем списке: иначе о ней узнают только из кода.
func TestEditIsListedInHelp(t *testing.T) {
	for _, c := range commands() {
		if c.name == "edit" {
			if !strings.Contains(c.usage, "--title") || !strings.Contains(c.usage, "--description") {
				t.Fatalf("строка использования: %s", c.usage)
			}
			return
		}
	}
	t.Fatal("команды edit нет в списке")
}
