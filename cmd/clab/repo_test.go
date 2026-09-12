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

// Отвязка обязана быть видна в подсказке: иначе её найдут только в исходниках.
func TestRepoRemoveIsListedInHelp(t *testing.T) {
	for _, c := range commands() {
		if c.name == "repo" {
			if !strings.Contains(c.usage, "repo remove") {
				t.Fatalf("строка использования: %s", c.usage)
			}
			return
		}
	}
	t.Fatal("команды repo нет в списке")
}
