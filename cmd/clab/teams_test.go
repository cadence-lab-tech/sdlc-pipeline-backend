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

// Выделенный инстанс (CLAB-88, К13): тарифов нет, и `clab team plan`
// говорит это сам — по ответу, без вопроса серверу о профиле.
func TestTeamPlanWithoutPlans(t *testing.T) {
	withJSON(t, false)

	// Без --set: в ответе команды нет ключа plan.
	c, rec := newInviteClient(t, 200, `{"team":{"key":"acme"},"members":[],"projects":[],"installations":[]}`)
	stdout, _, err := capture(t, func() error {
		return cmdTeam(context.Background(), c, []string{"plan", "acme"})
	})
	if err == nil || !strings.Contains(err.Error(), "тарифов на этом инстансе нет") {
		t.Fatalf("без тарифов: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("в stdout ушло %q", stdout)
	}
	if rec.calls != 1 || rec.method != "GET" || rec.path != "/v1/teams/acme" {
		t.Fatalf("запросы: %d, последний %s %s", rec.calls, rec.method, rec.path)
	}

	// С --set: 404 сервера с тем же текстом доходит до человека.
	c, rec = newInviteClient(t, 404, `{"error":{"code":"not_found","message":"тарифов на этом инстансе нет"}}`)
	_, _, err = capture(t, func() error {
		return cmdTeam(context.Background(), c, []string{"plan", "acme", "--set", "business"})
	})
	if err == nil || !strings.Contains(err.Error(), "тарифов на этом инстансе нет") {
		t.Fatalf("перевод без тарифов: %v", err)
	}
	if rec.calls != 1 || rec.method != "PUT" || rec.path != "/v1/teams/acme/plan" {
		t.Fatalf("запросы: %d, последний %s %s", rec.calls, rec.method, rec.path)
	}
}

// Общий инстанс (К16): тариф печатается как раньше.
func TestTeamPlanPrintsPlan(t *testing.T) {
	withJSON(t, false)
	c, _ := newInviteClient(t, 200, `{"team":{"key":"acme"},"plan":{"code":"free","members_max":3}}`)
	stdout, _, err := capture(t, func() error {
		return cmdTeam(context.Background(), c, []string{"plan", "acme"})
	})
	if err != nil || !strings.Contains(stdout, "free") {
		t.Fatalf("тариф: %v %q", err, stdout)
	}
}

// К10: отказ сервера в заведении и превращении команды доходит до человека
// текстом сервера, команда завершается ошибкой.
func TestTeamCreateConvertForbidden(t *testing.T) {
	const refusal = `{"error":{"code":"forbidden","message":"команды на этом инстансе заводит администратор"}}`
	for _, args := range [][]string{
		{"create", "acme", "--name", "Acme"},
		{"convert", "alice", "--key", "acme"},
	} {
		c, _ := newInviteClient(t, 403, refusal)
		_, _, err := capture(t, func() error { return cmdTeam(context.Background(), c, args) })
		if err == nil || !strings.Contains(err.Error(), "заводит администратор") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
