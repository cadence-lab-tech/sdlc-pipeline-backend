package main

import (
	"context"
	"net/http"
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
}
