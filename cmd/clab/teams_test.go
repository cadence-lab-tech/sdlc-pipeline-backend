package main

import (
	"context"
	"strings"
	"testing"
)

// Удаление арендатора из CLI не делается (CLAB-13): оно уносит проекты команды
// со всем содержимым, и подтверждается диалогом в интерфейсе, а не флагом в
// скрипте. Тест держит это: ни одна подкоманда `clab team` не шлёт DELETE по
// самой команде, и прежняя `archive` не работает молча по-старому.
func TestTeamHasNoRemovalSubcommand(t *testing.T) {
	c, rec := newInviteClient(t, 200, `{}`)

	for _, sub := range []string{"archive", "delete", "remove-team"} {
		err := cmdTeam(context.Background(), c, []string{sub, "acme"})
		if err == nil {
			t.Fatalf("подкоманда %q выполнилась", sub)
		}
		if !strings.Contains(err.Error(), "неизвестная подкоманда") {
			t.Fatalf("подкоманда %q: %v", sub, err)
		}
		// Само имя подкоманды стоит в тексте ошибки; смотрим только подсказку
		// после двоеточия — в ней archive быть не должно.
		_, hint, _ := strings.Cut(err.Error(), ": ")
		if strings.Contains(hint, "archive") || strings.Contains(hint, "delete") {
			t.Fatalf("подсказка всё ещё предлагает удаление команды: %v", err)
		}
	}
	if rec.calls != 0 {
		t.Fatalf("до сервера дошло %d запросов", rec.calls)
	}
}

// `clab team remove` остаётся выводом человека из состава — это не удаление
// команды, и путь у него другой.
func TestTeamRemoveTakesMemberOutOfTeam(t *testing.T) {
	c, rec := newInviteClient(t, 204, ``)
	withJSON(t, false)

	if _, _, err := capture(t, func() error {
		return cmdTeam(context.Background(), c, []string{"remove", "acme", "user:alice"})
	}); err != nil {
		t.Fatalf("вывод участника: %v", err)
	}
	if rec.method != "DELETE" || rec.path != "/v1/teams/acme/members/user:alice" {
		t.Fatalf("запрос: %s %s", rec.method, rec.path)
	}
}
