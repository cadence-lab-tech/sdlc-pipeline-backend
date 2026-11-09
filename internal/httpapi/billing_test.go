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

// CLAB-426, AC-11, AC-12: сумма по прайсу и её источник одинаковы на трёх
// транспортах — POST …/usage, POST …/runs/{id}/usage и sdlc_report_usage;
// прежние поля ответа на месте, карточка несёт источник прогона.
func TestReportUsagePricingTransports(t *testing.T) {
	f := newAuditFixture(t)
	doc := map[string]any{"pricing": map[string]any{"as_of": "2026-11-01", "models": map[string]any{
		"M": map[string]any{"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75},
	}}}
	if code, body := f.json("POST", agentSettingsPath+"/draft", f.root, nil); code != http.StatusCreated {
		t.Fatalf("черновик: %d %v", code, body)
	}
	if code, body := f.json("PUT", agentSettingsPath+"/draft", f.root, map[string]any{"document": doc}); code != http.StatusOK {
		t.Fatalf("документ: %d %v", code, body)
	}
	if code, body := f.json("POST", agentSettingsPath+"/draft/activate", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("активация: %d %v", code, body)
	}
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/start", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	code, claim := f.json("POST", "/v1/tasks/SDLC-1/claim", f.root, map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("claim: %d %v", code, claim)
	}
	runID, _ := claim["stage_run_id"].(string)

	body := func() map[string]any {
		return map[string]any{"model": "M", "input_tokens": 1_000_000, "output_tokens": 1_000_000,
			"cache_read_tokens": 2_000_000, "cache_write_tokens": 1_000_000, "cost_usd": 20}
	}
	code, rest1 := f.json("POST", "/v1/tasks/SDLC-1/usage", f.root, body())
	if code != http.StatusCreated {
		t.Fatalf("POST usage: %d %v", code, rest1)
	}
	code, rest2 := f.json("POST", "/v1/tasks/SDLC-1/runs/"+runID+"/usage", f.root, body())
	if code != http.StatusCreated {
		t.Fatalf("POST runs usage: %d %v", code, rest2)
	}
	args := body()
	args["task_key"] = "SDLC-1"
	out, isErr := f.mcp(f.root, "sdlc_report_usage", args)
	if isErr {
		t.Fatalf("sdlc_report_usage: %v", out)
	}
	mcpRec, _ := out["recorded"].(map[string]any)

	// Новые поля: 18 + 0.6 + 3.75 по прайсу, оценка агента 20 рядом.
	want := map[string]any{"cost_usd": 22.35, "cost_source": "pricing", "pricing_version": float64(1),
		"reported_cost_usd": float64(20), "model": "M", "input_tokens": float64(1_000_000), "billing": nil}
	for name, rec := range map[string]map[string]any{"POST usage": rest1, "POST runs usage": rest2, "MCP": mcpRec} {
		for k, w := range want {
			if got, has := rec[k]; !has || got != w {
				t.Errorf("%s: %s = %v (есть: %v), ждали %v", name, k, got, has, w)
			}
		}
		if ws, ok := rec["warnings"].([]any); !ok || len(ws) != 0 {
			t.Errorf("%s: warnings = %v, ждали пустой массив", name, rec["warnings"])
		}
	}

	// Предупреждение одно и то же на трёх транспортах; отчёт принят.
	partial := func() map[string]any { return map[string]any{"model": "M", "cost_usd": 7} }
	code, w1 := f.json("POST", "/v1/tasks/SDLC-1/usage", f.root, partial())
	_, w2 := f.json("POST", "/v1/tasks/SDLC-1/runs/"+runID+"/usage", f.root, partial())
	pargs := partial()
	pargs["task_key"] = "SDLC-1"
	wout, werr := f.mcp(f.root, "sdlc_report_usage", pargs)
	w3, _ := wout["recorded"].(map[string]any)
	if code != http.StatusCreated || werr {
		t.Fatalf("неполный отчёт: %d %v", code, wout)
	}
	for name, rec := range map[string]map[string]any{"POST usage": w1, "POST runs usage": w2, "MCP": w3} {
		ws, _ := rec["warnings"].([]any)
		if len(ws) != 1 || !strings.Contains(ws[0].(string), "input_tokens и output_tokens") ||
			rec["cost_source"] != "reported" || rec["pricing_version"] != nil || rec["cost_usd"] != float64(7) {
			t.Errorf("%s: %v", name, rec)
		}
	}

	// Диапазон: 400 на REST и ошибка MCP, строки не пишутся.
	var before int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM stage_run_usage WHERE run_id = $1`, runID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	huge := map[string]any{"model": "M", "input_tokens": 0, "output_tokens": 1_000_000_000_000}
	code, bad := f.json("POST", "/v1/tasks/SDLC-1/usage", f.root, huge)
	e, _ := bad["error"].(map[string]any)
	if code != http.StatusBadRequest || e["code"] != "invalid_input" || !strings.Contains(e["message"].(string), "не помещается") {
		t.Fatalf("диапазон на REST: %d %v", code, bad)
	}
	huge["task_key"] = "SDLC-1"
	mout, merr := f.mcp(f.root, "sdlc_report_usage", huge)
	raw, _ := json.Marshal(mout)
	if !merr || !strings.Contains(string(raw), "не помещается") {
		t.Fatalf("диапазон в MCP: %v %s", merr, raw)
	}
	var after int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM stage_run_usage WHERE run_id = $1`, runID).Scan(&after); err != nil || after != before {
		t.Fatalf("строк было %d, стало %d (%v)", before, after, err)
	}

	// Карточка: cost_usd как прежде (3 × 22.35 + 3 × 7 = 88.05), источник mixed.
	code, card := f.json("GET", "/v1/tasks/SDLC-1", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("карточка: %d %v", code, card)
	}
	for _, raw := range card["stage_runs"].([]any) {
		r := raw.(map[string]any)
		if r["id"] != runID {
			continue
		}
		u := r["usage"].(map[string]any)
		versions, _ := u["pricing_versions"].([]any)
		if u["cost_source"] != "mixed" || len(versions) != 1 || versions[0] != float64(1) ||
			math.Abs(u["cost_usd"].(float64)-88.05) > 1e-9 || math.Abs(u["reported_cost_usd"].(float64)-81) > 1e-9 {
			t.Errorf("учёт прогона в карточке: %v", u)
		}
	}
	// Прогон без учёта: источник null, версий нет.
	if code, task := f.json("GET", "/v1/tasks/SDLC-1", f.root, nil); code == http.StatusOK {
		for _, raw := range task["stage_runs"].([]any) {
			r := raw.(map[string]any)
			if r["id"] == runID {
				continue
			}
			u, _ := r["usage"].(map[string]any)
			if _, has := u["cost_source"]; !has || u["cost_source"] != nil {
				t.Errorf("прогон без учёта: %v", u)
			}
			if vs, ok := u["pricing_versions"].([]any); !ok || len(vs) != 0 {
				t.Errorf("прогон без учёта: pricing_versions %v", u["pricing_versions"])
			}
		}
	}
}
