package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// CLAB-220: разрез closed_cost в команде метрик. По умолчанию вывод прежний —
// JSON; таблица — только по --closed-cost; --json отдаёт тело ответа.

const metricsReply = `{"since":"2026-09-26T10:00:00Z","until":"2026-10-26T10:00:00Z",
"throughput":{"closed":4},"cost":[],
"closed_cost":[
 {"task_type":"epic","closed_tasks":1,"incomplete_tasks":0,"runs":6,"reported_runs":5,"silent_runs":0,"silent_share":0,
  "api_cost_usd":2.5,"avg_api_cost_usd":2.5,"median_api_cost_usd":2.5,"unspecified_cost_usd":0,
  "subscription_cost_usd":0,"avg_subscription_cost_usd":0,"median_subscription_cost_usd":0},
 {"task_type":"task","closed_tasks":3,"incomplete_tasks":1,"runs":9,"reported_runs":5,"silent_runs":3,"silent_share":0.375,
  "api_cost_usd":1.2,"avg_api_cost_usd":0.4,"median_api_cost_usd":0.35,"unspecified_cost_usd":0.1,
  "subscription_cost_usd":3,"avg_subscription_cost_usd":1,"median_subscription_cost_usd":0.5}]}`

func runMetrics(t *testing.T, reply string, args ...string) (string, *inviteServer) {
	t.Helper()
	c, rec := newInviteClient(t, 200, reply)
	out, _, err := capture(t, func() error {
		return cmdMetrics(context.Background(), c, append([]string{"--project", "CLAB"}, args...))
	})
	if err != nil {
		t.Fatalf("metrics %v: %v", args, err)
	}
	return out, rec
}

// Пункт 8: таблица по флагу — период, колонки, доли, подсказка о неполных.
func TestMetricsClosedCostTable(t *testing.T) {
	withJSON(t, false)
	out, rec := runMetrics(t, metricsReply, "--period", "30d", "--closed-cost")
	if rec.method != "GET" || rec.path != "/v1/projects/CLAB/metrics" {
		t.Fatalf("запрос %s %s", rec.method, rec.path)
	}
	for _, want := range []string{
		"Период:", "ТИП", "ЗАКРЫТО", "НЕПОЛНЫХ", "ДЕНЬГИ Σ", "УСЛОВНО Σ", "БЕЗ УЧЁТА",
		"epic", "task", "$2.50", "$0.35", "$3.00",
		"0 из 5 (0%)", "3 из 8 (38%)", "нижняя граница",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Период:") > strings.Index(out, "ТИП") {
		t.Errorf("период напечатан после таблицы:\n%s", out)
	}
}

// Без неполных задач подсказки нет; пустой разрез — «пусто».
func TestMetricsClosedCostEmpty(t *testing.T) {
	withJSON(t, false)
	out, _ := runMetrics(t, `{"since":"2026-09-26T10:00:00Z","until":"2026-10-26T10:00:00Z","closed_cost":[]}`, "--closed-cost")
	if !strings.Contains(out, "Период:") || !strings.Contains(out, "пусто") || strings.Contains(out, "нижняя граница") {
		t.Fatalf("пустой разрез:\n%s", out)
	}
}

// Решение человека (Q2): без флага вывод прежний — JSON, не таблица.
func TestMetricsDefaultStaysJSON(t *testing.T) {
	withJSON(t, false)
	out, _ := runMetrics(t, metricsReply)
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("без флага не JSON: %v\n%s", err, out)
	}
	if _, ok := got["closed_cost"]; !ok || strings.Contains(out, "ЗАКРЫТО") {
		t.Fatalf("без флага: %s", out)
	}
}

// --json при флаге — тело ответа, поля API как есть.
func TestMetricsClosedCostJSON(t *testing.T) {
	withJSON(t, true)
	out, _ := runMetrics(t, metricsReply, "--closed-cost")
	var got struct {
		ClosedCost []map[string]any `json:"closed_cost"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("не JSON: %v\n%s", err, out)
	}
	if len(got.ClosedCost) != 2 || got.ClosedCost[1]["silent_share"] != 0.375 || strings.Contains(out, "ЗАКРЫТО") {
		t.Fatalf("--json: %s", out)
	}
}
