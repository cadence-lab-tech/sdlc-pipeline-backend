package main

import (
	"context"
	"strings"
	"testing"
)

const repoRemoved = `{"repository":{"owner":"acme","name":"api"},
	"removed":{"index_runs":3,"code_files":120,"code_chunks":900,"documents":1,"drift_signals_dismissed":2}}`

// Отвязка идёт DELETE'ом по координатам в пути, а печатает цену действия:
// сколько знания ушло вместе с репозиторием.
func TestRepoRemoveCallsDeleteAndPrintsCounters(t *testing.T) {
	c, rec := newInviteClient(t, 200, repoRemoved)
	t.Setenv("SDLC_PROJECT", "CLAB")
	withJSON(t, false)

	out, errOut, err := capture(t, func() error {
		return cmdRepo(context.Background(), c, []string{"remove", "acme/api"})
	})
	if err != nil {
		t.Fatalf("отвязка: %v", err)
	}
	if rec.method != "DELETE" || rec.path != "/v1/projects/CLAB/repositories/acme/api" {
		t.Fatalf("запрос: %s %s", rec.method, rec.path)
	}
	for _, want := range []string{"acme/api", "120", "900"} {
		if !strings.Contains(out, want) {
			t.Fatalf("вывод не содержит %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "необратима") {
		t.Fatalf("предупреждение об необратимости не напечатано: %q", errOut)
	}
}

func TestRepoRemoveChecksArguments(t *testing.T) {
	c, rec := newInviteClient(t, 200, repoRemoved)
	t.Setenv("SDLC_PROJECT", "CLAB")

	if err := cmdRepo(context.Background(), c, []string{"remove"}); err == nil {
		t.Fatal("отвязка без репозитория прошла")
	}
	if err := cmdRepo(context.Background(), c, []string{"remove", "api"}); err == nil {
		t.Fatal("отвязка без owner прошла")
	}
	if err := cmdRepo(context.Background(), c, []string{"drop", "acme/api"}); err == nil ||
		!strings.Contains(err.Error(), "add, remove") {
		t.Fatalf("неизвестная подкоманда: %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("до сервера дошло %d запросов", rec.calls)
	}
}

// Отвязка обязана быть видна в подсказке, причём напечатанной: проверять поле
// структуры бессмысленно, человек видит только вывод команды.
func TestRepoRemoveIsPrintedInHelp(t *testing.T) {
	c, rec := newInviteClient(t, 200, repoRemoved)

	for _, args := range [][]string{{}, {"--help"}} {
		_, errOut, err := capture(t, func() error {
			return cmdRepo(context.Background(), c, args)
		})
		if err == nil {
			t.Fatalf("clab repo %v завершилась успехом", args)
		}
		for _, want := range []string{"clab repo add", "clab repo remove"} {
			if !strings.Contains(errOut, want) {
				t.Fatalf("подсказка clab repo %v не называет %q:\n%s", args, want, errOut)
			}
		}
	}
	if rec.calls != 0 {
		t.Fatalf("подсказка сходила на сервер %d раз", rec.calls)
	}
}

// Отказ приложения инстанса (CLAB-78) печатается `<код>: <текст>` — тем же
// путём, что любая ошибка домена; код приходит из ответа как есть.
func TestRepoAddPrintsProviderMisconfigured(t *testing.T) {
	const reply = `{"error":{"code":"provider_misconfigured","message":"приложение GitHub инстанса не принято провайдером: github 401"}}`
	c, rec := newInviteClient(t, 502, reply)
	t.Setenv("SDLC_PROJECT", "CLAB")

	err := cmdRepo(context.Background(), c, []string{"add", "acme/api", "--installation", "42"})
	if rec.method != "POST" || rec.path != "/v1/projects/CLAB/repositories" {
		t.Fatalf("запрос: %s %s", rec.method, rec.path)
	}
	if err == nil || err.Error() != "provider_misconfigured: приложение GitHub инстанса не принято провайдером: github 401" {
		t.Fatalf("ошибка: %v", err)
	}
}
