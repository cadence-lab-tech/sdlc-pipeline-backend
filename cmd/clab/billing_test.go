package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// CLAB-216: clab usage --billing (решение 0062). Флаг передаёт значение как
// есть, без флага поля в теле нет; проверяет значение сервер и отвечает
// перечнем допустимых.

func TestUsageCarriesBilling(t *testing.T) {
	withJSON(t, false)
	for _, billing := range []string{"subscription", "api", ""} {
		c, rec := newInviteClient(t, http.StatusCreated, `{"id":"u-1","billing":null}`)
		args := []string{"CLAB-7", "--model", "m", "--cost", "1.5"}
		if billing != "" {
			args = append(args, "--billing", billing)
		}
		if _, _, err := capture(t, func() error { return cmdUsage(context.Background(), c, args) }); err != nil {
			t.Fatalf("%q: %v", billing, err)
		}
		want := map[string]any{"model": "m", "cost_usd": 1.5}
		if billing != "" {
			want["billing"] = billing
		}
		if rec.method != "POST" || rec.path != "/v1/tasks/CLAB-7/usage" || !equalJSON(rec.body, want) {
			t.Fatalf("%q: %s %s тело %v, ждали %v", billing, rec.method, rec.path, rec.body, want)
		}
	}
}

// AC4: отказ сервера доходит до пользователя с перечнем и ненулевым кодом.
func TestUsageBillingRefusalShowsAllowed(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusBadRequest,
		`{"error":{"code":"invalid_input","message":"billing: \"x\" не поддерживается, допустимы api, subscription; пусто — не указано, считается как api"}}`)
	_, _, err := capture(t, func() error {
		return cmdUsage(context.Background(), c, []string{"CLAB-7", "--model", "m", "--billing", "x"})
	})
	if err == nil {
		t.Fatal("отказ сервера не стал ошибкой команды")
	}
	for _, part := range []string{"api", "subscription"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("в отказе нет %q: %v", part, err)
		}
	}
	if rec.body["billing"] != "x" {
		t.Fatalf("значение ушло не как есть: %v", rec.body)
	}
}
