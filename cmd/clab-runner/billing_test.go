package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// CLAB-216: тип оплаты прогонов раннера из SDLC_RUNNER_BILLING (решение 0062).

// AC10: разбор переменной при загрузке конфига; опечатка — отказ старта с
// именем переменной и допустимыми значениями.
func TestLoadConfigBilling(t *testing.T) {
	t.Setenv("SDLC_API_URL", "https://c.example.com/v1")
	t.Setenv("SDLC_PROJECT", "CLAB")
	for raw, want := range map[string]string{"": "", "api": "api", "subscription": "subscription", " subscription ": "subscription"} {
		t.Setenv("SDLC_RUNNER_BILLING", raw)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if cfg.Billing != want {
			t.Fatalf("%q: Billing=%q, ждали %q", raw, cfg.Billing, want)
		}
	}
	for _, raw := range []string{"x", "Subscription", "API", "sub"} {
		t.Setenv("SDLC_RUNNER_BILLING", raw)
		_, err := loadConfig()
		if err == nil {
			t.Fatalf("%q: раннер стартовал бы с неверным типом оплаты", raw)
		}
		for _, part := range []string{"SDLC_RUNNER_BILLING", `"` + raw + `"`, "api", "subscription"} {
			if !strings.Contains(err.Error(), part) {
				t.Fatalf("%q: в ошибке нет %q: %v", raw, part, err)
			}
		}
	}
}

// AC8, AC9: признак идёт в тело отчёта о стоимости рядом с reported_by
// раннера и виден в журнале запуска; без переменной ключа billing в теле нет.
func TestRunnerReportsBilling(t *testing.T) {
	for _, billing := range []string{"subscription", "api", ""} {
		t.Run("billing="+billing, func(t *testing.T) {
			api := newFakeAPI(t)
			command, _ := stubAgent(t, agentOK)
			var logs bytes.Buffer
			r := testRunner(t, api, command, &logs)
			r.cfg.Billing = billing
			if err := r.run(context.Background()); err != nil {
				t.Fatal(err)
			}

			calls := api.find(http.MethodPost, "/v1/tasks/CLAB-1/runs/run-1/usage")
			if len(calls) != 1 {
				t.Fatalf("отчётов о стоимости %d, ждали один; журнал:\n%s", len(calls), logs.String())
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(calls[0].Body), &body); err != nil {
				t.Fatal(err)
			}
			got, has := body["billing"]
			if billing == "" {
				if has {
					t.Fatalf("без SDLC_RUNNER_BILLING в теле billing: %s", calls[0].Body)
				}
			} else if got != billing {
				t.Fatalf("billing в теле %v, ждали %q: %s", got, billing, calls[0].Body)
			}
			details, _ := body["details"].(map[string]any)
			if details["reported_by"] != "clab-runner" {
				t.Fatalf("details отчёта: %s", calls[0].Body)
			}

			launch := lineWith(logs.String(), "запуск агента")
			if billing != "" && !strings.Contains(launch, "billing="+billing) {
				t.Fatalf("тип оплаты не виден в строке запуска: %q", launch)
			}
			if billing == "" && strings.Contains(launch, "billing=") {
				t.Fatalf("без переменной в журнале billing: %q", launch)
			}
		})
	}
}

// AC8: сухой прогон показывает тип оплаты в своей строке.
func TestDryRunShowsBilling(t *testing.T) {
	api := newFakeAPI(t)
	command, _ := stubAgent(t, agentOK)
	var logs bytes.Buffer
	r := testRunner(t, api, command, &logs)
	r.cfg.DryRun = true
	r.cfg.Billing = "subscription"
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if line := lineWith(logs.String(), "сухой прогон"); !strings.Contains(line, "billing=subscription") {
		t.Fatalf("тип оплаты не виден в сухом прогоне: %q\n%s", line, logs.String())
	}
}
