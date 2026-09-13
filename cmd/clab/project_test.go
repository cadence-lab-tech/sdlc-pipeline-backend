package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

const projectDeleted = `{"project":{"key":"ACME","name":"Проект"},
	"removed":{"tasks":12,"stage_runs":30,"artifacts":41,"repositories":2,"index_runs":7,
	"code_files":120,"code_chunks":900,"documents":3,"members":4,"tokens":1,"roles":5,
	"workflows":2,"invites":1,"cost_usd":12.5}}`

// Удаление идёт DELETE'ом по ключу в пути и печатает цену действия.
// Подтверждение — флагом: в тесте stdin не терминал, как и в CI.
func TestProjectDeleteCallsDeleteAndPrintsCounters(t *testing.T) {
	c, rec := newInviteClient(t, 200, projectDeleted)
	withJSON(t, false)

	out, errOut, err := capture(t, func() error {
		return cmdProject(context.Background(), c, []string{"delete", "ACME", "--confirm", "ACME"})
	})
	if err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if rec.method != "DELETE" || rec.path != "/v1/projects/ACME" {
		t.Fatalf("запрос: %s %s", rec.method, rec.path)
	}
	for _, want := range []string{"ACME", "12", "41", "900", "$12.50"} {
		if !strings.Contains(out, want) {
			t.Fatalf("вывод не содержит %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "необратимо") {
		t.Fatalf("предупреждение о необратимости не напечатано: %q", errOut)
	}
}

// Ключ проекта — единственное подтверждение, и не совпавшее до сервера не
// доходит. Пустой stdin — тоже отказ: удалить проект строкой в скрипте, не
// дописав --confirm, нельзя.
func TestProjectDeleteRequiresConfirmation(t *testing.T) {
	c, rec := newInviteClient(t, 200, projectDeleted)

	if err := cmdProject(context.Background(), c, []string{"delete", "ACME", "--confirm", "acme"}); err == nil ||
		!strings.Contains(err.Error(), "не совпало") {
		t.Fatalf("подтверждение чужим ключом: %v", err)
	}
	_, _, err := capture(t, func() error {
		return cmdProject(context.Background(), c, []string{"delete", "ACME"})
	})
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("удаление с пустым stdin: %v", err)
	}
	if err := cmdProject(context.Background(), c, []string{"delete"}); err == nil {
		t.Fatal("удаление без ключа прошло")
	}
	if err := cmdProject(context.Background(), c, []string{"drop", "ACME"}); err == nil ||
		!strings.Contains(err.Error(), "delete") {
		t.Fatalf("неизвестная подкоманда не называет delete: %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("до сервера дошло %d запросов", rec.calls)
	}
}

// Ключ, введённый в ответ на вопрос, — то же подтверждение, что и флаг.
func TestProjectDeleteReadsConfirmationFromStdin(t *testing.T) {
	c, rec := newInviteClient(t, 200, projectDeleted)
	withJSON(t, false)

	withStdin(t, "ACME\n")
	if _, _, err := capture(t, func() error {
		return cmdProject(context.Background(), c, []string{"delete", "ACME"})
	}); err != nil {
		t.Fatalf("подтверждение из stdin: %v", err)
	}
	if rec.calls != 1 || rec.method != "DELETE" {
		t.Fatalf("запросов %d: %s %s", rec.calls, rec.method, rec.path)
	}

	// Введено не то — до сервера не доходит.
	withStdin(t, "acme\n")
	if _, _, err := capture(t, func() error {
		return cmdProject(context.Background(), c, []string{"delete", "ACME"})
	}); err == nil || !strings.Contains(err.Error(), "не удалён") {
		t.Fatalf("подтверждение чужим ключом из stdin: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("до сервера дошло %d запросов", rec.calls)
	}
}

// withStdin подставляет команде ответ человека на время теста.
func withStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	prev := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = prev; _ = r.Close() })
}

// --json отдаёт ответ ручки целиком: скрипту нужны счётчики, а не таблица.
func TestProjectDeleteJSON(t *testing.T) {
	c, _ := newInviteClient(t, 200, projectDeleted)
	withJSON(t, true)

	out, _, err := capture(t, func() error {
		return cmdProject(context.Background(), c, []string{"delete", "ACME", "--confirm", "ACME"})
	})
	if err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if !strings.Contains(out, `"cost_usd"`) || !strings.Contains(out, `"tasks"`) {
		t.Fatalf("--json не отдал ответ ручки:\n%s", out)
	}
}

// Удаление обязано быть видно в подсказке команды.
func TestProjectDeleteIsPrintedInHelp(t *testing.T) {
	if !strings.Contains(commandUsage(t, "project"), "clab project delete") {
		t.Fatal("подсказка clab project не называет delete")
	}
}

func commandUsage(t *testing.T, name string) string {
	t.Helper()
	for _, cmd := range commands() {
		if cmd.name == name {
			return cmd.usage
		}
	}
	t.Fatalf("команды %q нет", name)
	return ""
}
