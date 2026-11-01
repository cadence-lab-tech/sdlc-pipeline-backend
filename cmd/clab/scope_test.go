package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const scopedTask = `{"key":"CLAB-92","seq":92,"title":"Пакет поставки","status_key":"analysis",` +
	`"properties":{"type":"task","multi_repo":true},"repositories":[]}`

// --add и --remove уходят одним запросом: замена репозитория в области не
// должна проходить через промежуточное состояние с отдельным коммитом.
func TestScopeBodyCarriesAddAndRemove(t *testing.T) {
	withJSON(t, true)
	cases := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"--add", "acme/web", "--reason", "фронт"},
			map[string]any{"add_repositories": []any{"acme/web"}, "remove_repositories": []any{}, "note": "фронт"}},
		{[]string{"--remove", "acme/api, acme/docs", "--reason", "справочник"},
			map[string]any{"add_repositories": []any{}, "remove_repositories": []any{"acme/api", "acme/docs"}, "note": "справочник"}},
		{[]string{"--add", "acme/web", "--remove", "acme/api"},
			map[string]any{"add_repositories": []any{"acme/web"}, "remove_repositories": []any{"acme/api"}, "note": ""}},
	}
	for _, tc := range cases {
		c, rec := newInviteClient(t, http.StatusOK, scopedTask)
		if _, _, err := capture(t, func() error {
			return cmdScope(context.Background(), c, append([]string{"CLAB-92"}, tc.args...))
		}); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if rec.method != "PATCH" || rec.path != "/v1/tasks/CLAB-92/scope" {
			t.Fatalf("%v: %s %s", tc.args, rec.method, rec.path)
		}
		if !equalJSON(rec.body, tc.want) {
			t.Fatalf("%v: тело %v, ждали %v", tc.args, rec.body, tc.want)
		}
	}
}

// --multi-repo уходит свойством в том же запросе, что и --add; --multi-repo=false
// шлёт false; без флага поля properties в теле нет (старый сервер и новый
// клиент, как и наоборот, не расходятся). CLAB-353.
func TestScopeBodyCarriesMultiRepo(t *testing.T) {
	withJSON(t, true)
	cases := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"--add", "acme/web", "--multi-repo", "--reason", "x"},
			map[string]any{"add_repositories": []any{"acme/web"}, "remove_repositories": []any{},
				"properties": map[string]any{"multi_repo": true}, "note": "x"}},
		{[]string{"--multi-repo", "--reason", "x"},
			map[string]any{"add_repositories": []any{}, "remove_repositories": []any{},
				"properties": map[string]any{"multi_repo": true}, "note": "x"}},
		{[]string{"--multi-repo=false", "--reason", "x"},
			map[string]any{"add_repositories": []any{}, "remove_repositories": []any{},
				"properties": map[string]any{"multi_repo": false}, "note": "x"}},
		{[]string{"--remove", "acme/api", "--multi-repo=false"},
			map[string]any{"add_repositories": []any{}, "remove_repositories": []any{"acme/api"},
				"properties": map[string]any{"multi_repo": false}, "note": ""}},
	}
	for _, tc := range cases {
		c, rec := newInviteClient(t, http.StatusOK, scopedTask)
		if _, _, err := capture(t, func() error {
			return cmdScope(context.Background(), c, append([]string{"CLAB-92"}, tc.args...))
		}); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if !equalJSON(rec.body, tc.want) {
			t.Fatalf("%v: тело %v, ждали %v", tc.args, rec.body, tc.want)
		}
	}

	// Без флага — ни ключа properties.
	c, rec := newInviteClient(t, http.StatusOK, scopedTask)
	if _, _, err := capture(t, func() error {
		return cmdScope(context.Background(), c, []string{"CLAB-92", "--add", "acme/web"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.body["properties"]; ok {
		t.Fatalf("без --multi-repo properties не шлём: %v", rec.body)
	}
}

// scope_expansion_forbidden без --multi-repo дополняется подсказкой, текст
// сервера остаётся; с --multi-repo подсказка не нужна.
func TestScopeHintsMultiRepoOnForbidden(t *testing.T) {
	withJSON(t, false)
	const forbidden = `{"error":{"code":"scope_expansion_forbidden","message":"включите свойство multi_repo"}}`

	c, _ := newInviteClient(t, http.StatusConflict, forbidden)
	_, _, err := capture(t, func() error {
		return cmdScope(context.Background(), c, []string{"CLAB-92", "--add", "acme/web", "--reason", "x"})
	})
	if err == nil || !strings.Contains(err.Error(), "scope_expansion_forbidden") ||
		!strings.Contains(err.Error(), "--multi-repo") || !strings.Contains(err.Error(), "решение человека") {
		t.Fatalf("ждали ошибку сервера и подсказку, получили %v", err)
	}

	c, _ = newInviteClient(t, http.StatusConflict, forbidden)
	_, _, err = capture(t, func() error {
		return cmdScope(context.Background(), c, []string{"CLAB-92", "--add", "acme/web", "--multi-repo"})
	})
	if err == nil || strings.Contains(err.Error(), "повторите") {
		t.Fatalf("с --multi-repo подсказки быть не должно: %v", err)
	}
}

// Без --add и --remove менять нечего — ошибка до запроса.
func TestScopeRefusesEmptyChange(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusOK, scopedTask)
	_, _, err := capture(t, func() error {
		return cmdScope(context.Background(), c, []string{"CLAB-92", "--reason", "просто так"})
	})
	if err == nil || rec.calls != 0 {
		t.Fatalf("ошибка %v, запросов %d", err, rec.calls)
	}
	for _, name := range []string{"--add", "--remove", "--multi-repo"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("ошибка не называет %s: %v", name, err)
		}
	}
}
