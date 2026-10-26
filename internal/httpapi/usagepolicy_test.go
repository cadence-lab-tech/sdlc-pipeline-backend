package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Политика учёта проекта по REST и MCP (CLAB-219): кто её меняет, журнал
// «было → стало», отказ захвата кодом usage_report_required с HTTP 409 и
// состояние учёта прогона в карточке. Номера — пункты критериев приёмки.
func TestUsagePolicyOverREST(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()
	const path = "/v1/projects/SDLC/usage-policy"

	token := func(handle string, kind domain.PrincipalKind, level domain.MemberRole) string {
		t.Helper()
		ref := string(kind) + ":" + handle
		if kind == domain.PrincipalUser {
			if _, err := postgres.New(f.pool).ResolveActor(ctx, f.pool, handle, kind); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.auth.SetMember(ctx, f.admin, "SDLC", ref, level, nil); err != nil {
			t.Fatal(err)
		}
		issued, err := f.auth.IssueToken(ctx, f.admin, workflow.IssueTokenInput{Name: handle, Principal: ref, ProjectKey: "SDLC"})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Token
	}
	maintainer := token("alice", domain.PrincipalUser, domain.MemberMaintainer)
	agentAdmin := token("boss", domain.PrincipalAgent, domain.MemberAdmin)

	errCode := func(body map[string]any) string {
		e, _ := body["error"].(map[string]any)
		c, _ := e["code"].(string)
		return c
	}

	// 8: по умолчанию off, окно не задано, действует 60; читает любой участник.
	code, view := f.json("GET", path, f.bot, nil)
	if code != http.StatusOK || view["mode"] != "off" || view["grace_minutes"] != nil ||
		view["effective_grace_minutes"] != float64(60) || view["enabled_at"] != nil {
		t.Fatalf("по умолчанию: %d %v", code, view)
	}

	// 10: мейнтейнер, агент (даже admin) и агент уровня agent — отказ.
	for name, tok := range map[string]string{"maintainer": maintainer, "agent admin": agentAdmin, "agent": f.bot} {
		if code, body := f.json("PATCH", path, tok, map[string]any{"mode": "required"}); code != http.StatusForbidden || errCode(body) != "forbidden" {
			t.Fatalf("%s меняет политику: %d %v", name, code, body)
		}
	}

	// 11: неверное окно — invalid_input с подсказкой.
	for _, raw := range []string{`1.5`, `"abc"`, `-1`, `10081`} {
		rec := f.do("PATCH", path, f.root, json.RawMessage(`{"grace_minutes": `+raw+`}`))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_input") ||
			!strings.Contains(rec.Body.String(), "default") {
			t.Fatalf("окно %s: %d %s", raw, rec.Code, rec.Body)
		}
	}
	if _, view := f.json("GET", path, f.bot, nil); view["mode"] != "off" || view["grace_minutes"] != nil {
		t.Fatalf("отказы изменили настройки: %v", view)
	}

	// 9: администратор включает; журнал — от его имени, было → стало.
	code, on := f.json("PATCH", path, f.root, map[string]any{"mode": "required", "grace_minutes": 0})
	if code != http.StatusOK || on["mode"] != "required" || on["grace_minutes"] != float64(0) || on["enabled_at"] == nil {
		t.Fatalf("включение: %d %v", code, on)
	}
	rows := f.rows("a.action = 'PATCH /v1/projects/:projectKey/usage-policy' AND a.outcome = 'ok'")
	if len(rows) != 1 || s(rows[0].Principal) != "root" || s(rows[0].Project) != "SDLC" {
		t.Fatalf("журнал: %+v", rows)
	}
	var params struct {
		Change struct {
			Before map[string]any `json:"before"`
			After  map[string]any `json:"after"`
		} `json:"change"`
	}
	if err := json.Unmarshal([]byte(rows[0].Params), &params); err != nil {
		t.Fatal(err)
	}
	if params.Change.Before["mode"] != "off" || params.Change.After["mode"] != "required" || params.Change.After["enabled_at"] == nil {
		t.Fatalf("было → стало в журнале: %s", rows[0].Params)
	}
	// Политика видна и в project.settings, как обещано участникам.
	if _, body := f.json("GET", "/v1/projects/SDLC", f.bot, nil); !strings.Contains(mustJSON(t, body["project"]), `"mode":"required"`) {
		t.Fatalf("project.settings: %v", body["project"])
	}

	// Долг бота: забрал SDLC-1, прогон закрыт после включения без учёта.
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/start", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	code, claimed := f.json("POST", "/v1/tasks/SDLC-1/claim", f.bot, map[string]any{})
	if code != http.StatusOK || claimed["usage_report"] != "required" {
		t.Fatalf("захват ботом при политике: %d %v", code, claimed)
	}
	run := claimed["stage_run_id"].(string)
	if _, err := f.pool.Exec(ctx, `
		UPDATE projects SET settings = jsonb_set(settings, '{usage_policy,enabled_at}', to_jsonb(now() - interval '1 day'))
		WHERE key = 'SDLC'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE stage_runs SET state = 'passed', outcome = 'advanced', finished_at = now() - interval '1 hour',
		       claimed_by = NULL, lease_expires_at = NULL WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}

	// 1, 7: состояние учёта в карточке REST и MCP одно.
	stateOf := func(runs []any) string {
		for _, raw := range runs {
			r := raw.(map[string]any)
			if r["id"] == run {
				return r["usage"].(map[string]any)["state"].(string)
			}
		}
		return ""
	}
	_, card := f.json("GET", "/v1/tasks/SDLC-1", f.root, nil)
	if got := stateOf(card["stage_runs"].([]any)); got != domain.UsageStateMissing {
		t.Fatalf("usage.state в карточке: %q", got)
	}
	mcpCard, isErr := f.mcp(f.root, "sdlc_get_task", map[string]any{"task_key": "SDLC-1"})
	if isErr || stateOf(mcpCard["stage_runs"].([]any)) != domain.UsageStateMissing {
		t.Fatalf("usage.state по MCP: %v", mcpCard)
	}

	// 13: REST — 409 usage_report_required, в тексте ключ и прогон.
	if code, body := f.json("POST", "/v1/tasks/SDLC-3/transitions/start", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("start SDLC-3: %d %v", code, body)
	}
	code, body := f.json("POST", "/v1/tasks/SDLC-3/claim", f.bot, map[string]any{})
	e, _ := body["error"].(map[string]any)
	if code != http.StatusConflict || errCode(body) != "usage_report_required" ||
		!strings.Contains(e["message"].(string), "SDLC-1") || !strings.Contains(e["message"].(string), run) {
		t.Fatalf("отказ по REST: %d %v", code, body)
	}
	det := e["details"].(map[string]any)
	if det["total"] != float64(1) || len(det["debts"].([]any)) != 1 {
		t.Fatalf("details: %v", det)
	}
	// 14: MCP — тот же код.
	out, isErr := f.mcp(f.bot, "sdlc_claim_task", map[string]any{"task_key": "SDLC-3"})
	if !isErr || errCode(out) != "usage_report_required" {
		t.Fatalf("отказ по MCP: %v", out)
	}
	// 19: человек (root) с той же политикой забирает, ему — expected.
	if code, body := f.json("POST", "/v1/tasks/SDLC-3/claim", f.root, map[string]any{}); code != http.StatusOK || body["usage_report"] != "expected" {
		t.Fatalf("захват человеком: %d %v", code, body)
	}
	if code, body := f.json("POST", "/v1/tasks/SDLC-3/release", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("release: %d %v", code, body)
	}

	// 15: отчёт о закрытом прогоне API-токеном бота без стоимости гасит долг.
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/runs/"+run+"/usage", f.bot, map[string]any{"model": "m"}); code != http.StatusCreated {
		t.Fatalf("отчёт о закрытом прогоне: %d %v", code, body)
	}
	if code, body := f.json("POST", "/v1/tasks/SDLC-3/claim", f.bot, map[string]any{}); code != http.StatusOK || body["usage_report"] != "required" {
		t.Fatalf("захват после отчёта: %d %v", code, body)
	}
	_, card = f.json("GET", "/v1/tasks/SDLC-1", f.root, nil)
	if got := stateOf(card["stage_runs"].([]any)); got != domain.UsageStateReported {
		t.Fatalf("usage.state после отчёта: %q", got)
	}
}
