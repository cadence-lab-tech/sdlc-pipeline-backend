package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// CLAB-426, AC-11: clab usage печатает ответ сервера целиком — источник
// суммы, версия прайса, оценка агента и предупреждения доходят до stdout, а
// тело запроса прежнее: --cost по-прежнему cost_usd.
func TestUsagePrintsCostSourceFields(t *testing.T) {
	withJSON(t, false)
	const answer = `{"id":"u-1","model":"M","cost_usd":18,"cost_source":"pricing","pricing_version":3,
		"reported_cost_usd":20,"billing":null,"warnings":[]}`
	c, rec := newInviteClient(t, http.StatusCreated, answer)
	stdout, _, err := capture(t, func() error {
		return cmdUsage(context.Background(), c, []string{
			"CLAB-7", "--model", "M", "--input", "1000000", "--output", "1000000", "--cost", "20"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var printed map[string]any
	if err := json.Unmarshal([]byte(stdout), &printed); err != nil {
		t.Fatalf("stdout не JSON: %v\n%s", err, stdout)
	}
	for k, want := range map[string]any{"cost_usd": 18.0, "cost_source": "pricing", "pricing_version": 3.0,
		"reported_cost_usd": 20.0} {
		if printed[k] != want {
			t.Errorf("в stdout %s = %v, ждали %v", k, printed[k], want)
		}
	}
	if ws, ok := printed["warnings"].([]any); !ok || len(ws) != 0 {
		t.Errorf("warnings: %v", printed["warnings"])
	}
	// Тело запроса не изменилось: оценка агента уходит как cost_usd.
	want := map[string]any{"model": "M", "input_tokens": 1000000.0, "output_tokens": 1000000.0, "cost_usd": 20.0}
	if rec.method != "POST" || rec.path != "/v1/tasks/CLAB-7/usage" || !equalJSON(rec.body, want) {
		t.Fatalf("%s %s тело %v, ждали %v", rec.method, rec.path, rec.body, want)
	}
}

// Предупреждение сервера (модель в прайсе, токенов нет) доходит до stdout, а
// сервер прежней версии без новых полей работает как раньше.
func TestUsagePrintsWarningsAndOldServer(t *testing.T) {
	withJSON(t, false)
	c, _ := newInviteClient(t, http.StatusCreated,
		`{"id":"u-2","cost_usd":7,"cost_source":"reported","pricing_version":null,"reported_cost_usd":7,
		"warnings":["модель \"M\" есть в прайсе версии 3, но не даны input_tokens и output_tokens"]}`)
	stdout, _, err := capture(t, func() error {
		return cmdUsage(context.Background(), c, []string{"CLAB-7", "--model", "M", "--cost", "7"})
	})
	if err != nil || !strings.Contains(stdout, "input_tokens и output_tokens") || !strings.Contains(stdout, `"cost_source": "reported"`) {
		t.Fatalf("предупреждение не дошло: %v\n%s", err, stdout)
	}

	c, _ = newInviteClient(t, http.StatusCreated, `{"id":"u-3","cost_usd":1.5,"billing":null}`)
	stdout, _, err = capture(t, func() error {
		return cmdUsage(context.Background(), c, []string{"CLAB-7", "--model", "m", "--cost", "1.5"})
	})
	if err != nil || !strings.Contains(stdout, `"cost_usd": 1.5`) || strings.Contains(stdout, "cost_source") {
		t.Fatalf("сервер прежней версии: %v\n%s", err, stdout)
	}
}
