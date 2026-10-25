package httpapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
)

// CLAB-216, решение 0062: признак оплаты на обоих транспортах — обе ручки
// REST и sdlc_report_usage. Проверка одна, в движке, поэтому ответы и отказы
// у транспортов одинаковые; чтение отдаёт деньги и условную стоимость рядом
// с прежним cost_usd.
func TestReportUsageBillingTransports(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()

	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/start", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	code, claim := f.json("POST", "/v1/tasks/SDLC-1/claim", f.root, map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("claim: %d %v", code, claim)
	}
	runID, _ := claim["stage_run_id"].(string)
	if runID == "" {
		t.Fatalf("в ответе захвата нет stage_run_id: %v", claim)
	}
	rows := func() int {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM stage_run_usage WHERE run_id = $1`, runID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// REST: обе ручки, значение как есть; без поля и пустое — null.
	for _, tc := range []struct {
		path    string
		billing any // nil — поля в теле нет
		want    any
	}{
		{"/v1/tasks/SDLC-1/usage", "api", "api"},
		{"/v1/tasks/SDLC-1/runs/" + runID + "/usage", "subscription", "subscription"},
		{"/v1/tasks/SDLC-1/usage", nil, nil},
		{"/v1/tasks/SDLC-1/runs/" + runID + "/usage", "", nil},
	} {
		body := map[string]any{"model": "m", "cost_usd": 1}
		if tc.billing != nil {
			body["billing"] = tc.billing
		}
		code, out := f.json("POST", tc.path, f.root, body)
		if code != http.StatusCreated {
			t.Fatalf("%s %v: %d %v", tc.path, tc.billing, code, out)
		}
		got, has := out["billing"]
		if !has || got != tc.want {
			t.Fatalf("%s %v: billing в ответе %v (есть: %v), ждали %v", tc.path, tc.billing, got, has, tc.want)
		}
	}
	before := rows()
	for _, bad := range []string{"x", "API"} {
		for _, path := range []string{"/v1/tasks/SDLC-1/usage", "/v1/tasks/SDLC-1/runs/" + runID + "/usage"} {
			code, out := f.json("POST", path, f.root, map[string]any{"model": "m", "cost_usd": 1, "billing": bad})
			e, _ := out["error"].(map[string]any)
			msg, _ := e["message"].(string)
			if code != http.StatusBadRequest || e["code"] != "invalid_input" || !strings.Contains(msg, "api, subscription") {
				t.Fatalf("%s %q: %d %v", path, bad, code, out)
			}
		}
	}

	// MCP: тот же признак и тот же отказ.
	for _, tc := range []struct {
		billing any
		want    any
	}{{nil, nil}, {"api", "api"}, {"subscription", "subscription"}} {
		args := map[string]any{"task_key": "SDLC-1", "model": "m", "cost_usd": 1}
		if tc.billing != nil {
			args["billing"] = tc.billing
		}
		out, isErr := f.mcp(f.root, "sdlc_report_usage", args)
		if isErr {
			t.Fatalf("sdlc_report_usage %v: %v", tc.billing, out)
		}
		recorded, _ := out["recorded"].(map[string]any)
		if got, has := recorded["billing"]; !has || got != tc.want {
			t.Fatalf("sdlc_report_usage %v: billing %v, ждали %v", tc.billing, got, tc.want)
		}
		if _, ok := out["task_usage"].(map[string]any)["subscription_cost_usd"]; !ok {
			t.Fatalf("task_usage без раздельных сумм: %v", out["task_usage"])
		}
	}
	out, isErr := f.mcp(f.root, "sdlc_report_usage", map[string]any{"task_key": "SDLC-1", "model": "m", "billing": "x"})
	raw, _ := json.Marshal(out)
	if !isErr || !strings.Contains(string(raw), "api, subscription") {
		t.Fatalf("sdlc_report_usage с billing=x: ошибка %v, %s", isErr, raw)
	}
	if rows() != before+3 {
		t.Fatalf("отказанные отчёты записали строки: было %d, стало %d", before, rows())
	}

	// Чтение: REST api 1 + subscription 1 + два без признака, MCP — по одной
	// строке каждого вида. Всё 7, деньги 5, из них без признака 3, подписка 2.
	want := map[string]float64{
		"cost_usd": 7, "api_cost_usd": 5, "unspecified_cost_usd": 3, "subscription_cost_usd": 2,
		"subtree_cost_usd": 7, "subtree_api_cost_usd": 5, "subtree_unspecified_cost_usd": 3,
		"subtree_subscription_cost_usd": 2,
	}
	code, usage := f.json("GET", "/v1/tasks/SDLC-1/usage", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("usage: %d %v", code, usage)
	}
	code, card := f.json("GET", "/v1/tasks/SDLC-1", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("карточка: %d %v", code, card)
	}
	for name, got := range map[string]map[string]any{"GET usage": usage, "карточка": card["usage"].(map[string]any)} {
		for k, w := range want {
			g, ok := got[k].(float64)
			if !ok || math.Abs(g-w) > 1e-9 {
				t.Errorf("%s: %s = %v, ждали %v", name, k, got[k], w)
			}
		}
	}
	for _, raw := range card["stage_runs"].([]any) {
		r := raw.(map[string]any)
		if r["id"] != runID {
			continue
		}
		u := r["usage"].(map[string]any)
		for k, w := range map[string]float64{"cost_usd": 7, "api_cost_usd": 5, "unspecified_cost_usd": 3, "subscription_cost_usd": 2} {
			if g, ok := u[k].(float64); !ok || math.Abs(g-w) > 1e-9 {
				t.Errorf("учёт прогона: %s = %v, ждали %v", k, u[k], w)
			}
		}
	}
}
