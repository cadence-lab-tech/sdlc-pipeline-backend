package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Настройка агентов в связке по REST и MCP (CLAB-428): исходы, сравнение не
// зависит от канала, права бота, журнал без документа. Номера AC —
// критерии приёмки аналитики задачи.

const importPath = "/v1/projects/SDLC/import"

// bundleDoc — документ настройки с числами, записанными как есть в raw:
// json.RawMessage Go не переписывает (1.0 остаётся 1.0).
func bundleDoc(model, price string) json.RawMessage {
	return json.RawMessage(`{"pricing":{"as_of":"2026-11-01","models":{"` + model + `":{"input":` + price + `,"output":2.0}}},` +
		`"agents":{"claude":{"models":["` + model + `"],"levels":{"high":{"model":"` + model + `","effort":"high"}}}}}`)
}

func agentSettingsBundle(doc json.RawMessage, changelog string) map[string]any {
	return map[string]any{"version": 3, "agent_settings": map[string]any{"document": doc, "changelog": changelog}}
}

func agentSettingsEntry(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	e, ok := body["agent_settings"].(map[string]any)
	if !ok {
		t.Fatalf("в отчёте нет agent_settings: %v", body)
	}
	return e
}

func versionsCount(t *testing.T, f *auditFixture) (total, drafts int) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*), count(*) FILTER (WHERE state = 'draft') FROM agent_settings_versions`).Scan(&total, &drafts); err != nil {
		t.Fatal(err)
	}
	return total, drafts
}

// AC-1, AC-3, AC-7–AC-9, AC-11, AC-17, AC-20, AC-21: один и тот же импорт
// по REST и MCP, сравнение не зависит от записи числа и канала.
func TestBundleAgentSettingsRESTAndMCP(t *testing.T) {
	f := newAuditFixture(t)
	lead := f.memberToken("lead", domain.MemberMaintainer, nil)

	// Экспорт проекта без настройки: ключа нет ни по REST, ни по MCP.
	rec := f.do("GET", "/v1/projects/SDLC/export", lead, nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "agent_settings") {
		t.Fatalf("экспорт без настройки: %d", rec.Code)
	}
	if out, isErr := f.mcp(lead, "sdlc_export_bundle", map[string]any{}); isErr || out["agent_settings"] != nil {
		t.Fatalf("MCP-экспорт без настройки: %v", out)
	}

	// REST: сухой прогон не пишет, боевой заводит черновик.
	rec = f.do("POST", importPath+"?dry_run=true", lead, agentSettingsBundle(bundleDoc("claude-opus-7", "1.0"), ""))
	var rep map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if rec.Code != http.StatusOK || agentSettingsEntry(t, rep)["outcome"] != "draft_created" {
		t.Fatalf("сухой прогон: %d %s", rec.Code, rec.Body)
	}
	if total, _ := versionsCount(t, f); total != 0 {
		t.Fatalf("сухой прогон записал %d версий", total)
	}
	rec = f.do("POST", importPath, lead, agentSettingsBundle(bundleDoc("claude-opus-7", "1.0"), "первый"))
	rep = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if rec.Code != http.StatusOK || agentSettingsEntry(t, rep)["outcome"] != "draft_created" || agentSettingsEntry(t, rep)["id"] == "" {
		t.Fatalf("боевой импорт: %d %s", rec.Code, rec.Body)
	}

	// AC-9 и AC-21: тот же документ через MCP, число записано иначе
	// (объект с вложенными объектами и числами), — unchanged.
	var nested any
	_ = json.Unmarshal(bundleDoc("claude-opus-7", "1"), &nested)
	out, isErr := f.mcp(lead, "sdlc_import_bundle", map[string]any{
		"bundle": map[string]any{"version": 3, "agent_settings": map[string]any{"document": nested}}})
	if isErr || agentSettingsEntry(t, out)["outcome"] != "unchanged" {
		t.Fatalf("MCP тот же документ: %v", out)
	}
	if total, drafts := versionsCount(t, f); total != 1 || drafts != 1 {
		t.Fatalf("версий %d, черновиков %d", total, drafts)
	}

	// MCP: другой документ заменяет черновик.
	_ = json.Unmarshal(bundleDoc("claude-opus-8", "3"), &nested)
	out, isErr = f.mcp(lead, "sdlc_import_bundle", map[string]any{
		"bundle": map[string]any{"version": 3, "agent_settings": map[string]any{"document": nested, "changelog": "второй"}}})
	if isErr || agentSettingsEntry(t, out)["outcome"] != "updated" {
		t.Fatalf("MCP замена: %v", out)
	}
	// Сухой прогон по MCP отдаёт тот же исход и ничего не меняет.
	_ = json.Unmarshal(bundleDoc("claude-opus-9", "3"), &nested)
	out, isErr = f.mcp(lead, "sdlc_import_bundle", map[string]any{"dry_run": true,
		"bundle": map[string]any{"version": 3, "agent_settings": map[string]any{"document": nested}}})
	if isErr || agentSettingsEntry(t, out)["outcome"] != "updated" {
		t.Fatalf("MCP сухой прогон: %v", out)
	}

	// Экспорт: черновик в связку не идёт, активная — идёт, REST и MCP одинаково.
	rec = f.do("GET", "/v1/projects/SDLC/export", lead, nil)
	if strings.Contains(rec.Body.String(), "agent_settings") {
		t.Fatal("черновик попал в экспорт")
	}
	if code, body := f.json("POST", agentSettingsPath+"/draft/activate", lead, nil); code != http.StatusOK {
		t.Fatalf("активация: %d %v", code, body)
	}
	code, exp := f.json("GET", "/v1/projects/SDLC/export", lead, nil)
	as, _ := exp["agent_settings"].(map[string]any)
	if code != http.StatusOK || as == nil || as["changelog"] != "второй" || exp["version"] != float64(3) ||
		!strings.Contains(mustJSON(t, as["document"]), "claude-opus-8") {
		t.Fatalf("REST-экспорт: %d %v", code, exp["agent_settings"])
	}
	mexp, isErr := f.mcp(lead, "sdlc_export_bundle", map[string]any{})
	if isErr || mustJSON(t, mexp["agent_settings"]) != mustJSON(t, exp["agent_settings"]) {
		t.Fatalf("MCP-экспорт отличается: %v", mexp["agent_settings"])
	}

	// Круг: экспорт → импорт назад — unchanged, активная не изменилась (AC-20).
	rec = f.do("POST", importPath, lead, exp)
	rep = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if e := agentSettingsEntry(t, rep); rec.Code != http.StatusOK || e["outcome"] != "unchanged" {
		t.Fatalf("круг: %d %s", rec.Code, rec.Body)
	}
	if total, drafts := versionsCount(t, f); total != 1 || drafts != 0 {
		t.Fatalf("после круга версий %d, черновиков %d", total, drafts)
	}

	// Связка без поля — отчёт без ключа (AC-16/17).
	rec = f.do("POST", importPath, lead, map[string]any{"version": 3})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "agent_settings") {
		t.Fatalf("связка без поля: %d %s", rec.Code, rec.Body)
	}
}

// AC-13: 400 invalid_input с путём; откат всей связки; значение секрета не
// попадает ни в ответ, ни в журнал, ни в лог.
func TestBundleAgentSettingsValidationAndJournal(t *testing.T) {
	f, logs := newLoggedFixture(t)
	ctx := context.Background()
	lead := f.memberToken("lead", domain.MemberMaintainer, nil)

	secret := "sdlc_BundleSecretValue0123"
	bad := agentSettingsBundle(json.RawMessage(`{"agents":{"claude":{"levels":{"high":{"model":"`+secret+`"}}}}}`), "")
	bad["roles"] = []any{map[string]any{"key": "ghost", "name": "Призрак", "instructions": "x"}}
	for _, dry := range []string{"", "?dry_run=true"} {
		rec := f.do("POST", importPath+dry, lead, bad)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		p, _ := detailPath(out)
		if rec.Code != http.StatusBadRequest || p != "agents.claude.levels.high.model" || strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("секрет в связке (%q): %d %s", dry, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `"bundle_field":"agent_settings"`) {
			t.Fatalf("нет bundle_field: %s", rec.Body)
		}
	}
	res, isErr := f.mcp(lead, "sdlc_import_bundle", map[string]any{"bundle": map[string]any{"version": 3,
		"agent_settings": map[string]any{"document": map[string]any{"agents": map[string]any{"claude": map[string]any{
			"levels": map[string]any{"high": map[string]any{"model": secret}}}}}}}})
	if !isErr || errCode(res) != "invalid_input" || strings.Contains(mustJSON(t, res), secret) {
		t.Fatalf("MCP секрет: %v", res)
	}
	var ghost int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM roles WHERE key = 'ghost'`).Scan(&ghost); err != nil || ghost != 0 {
		t.Fatalf("роль отбитой связки записана: %d %v", ghost, err)
	}

	// Успешный импорт с обычным документом: в журнале документа нет, есть
	// размер и sha256, а при draft_created — change.
	doc := bundleDoc("claude-opus-7", "1.5")
	if rec := f.do("POST", importPath, lead, agentSettingsBundle(doc, "ч")); rec.Code != http.StatusOK {
		t.Fatalf("импорт: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", importPath, lead, agentSettingsBundle(doc, "ч")); rec.Code != http.StatusOK {
		t.Fatalf("повтор: %d %s", rec.Code, rec.Body)
	}
	if out, isErr := f.mcp(lead, "sdlc_import_bundle", map[string]any{"bundle": map[string]any{"version": 3,
		"agent_settings": map[string]any{"document": map[string]any{"agents": map[string]any{"codex": map[string]any{
			"levels": map[string]any{"high": map[string]any{"model": "gpt-6.1-sol"}}}}}}}}); isErr {
		t.Fatalf("MCP импорт: %v", out)
	}

	rest := f.rows(`a.action = 'POST /v1/projects/:projectKey/import'`)
	mcpRows := f.rows(`a.action = 'sdlc_import_bundle'`)
	if len(rest) != 4 || len(mcpRows) != 2 {
		t.Fatalf("записей журнала: REST %d, MCP %d", len(rest), len(mcpRows))
	}
	for _, r := range append(append([]restAuditRow{}, rest...), mcpRows...) {
		if strings.Contains(r.Params, secret) || strings.Contains(r.Params, "claude-opus") || strings.Contains(r.Params, "gpt-6.1-sol") {
			t.Fatalf("документ настройки в журнале (%s): %.400s", r.Outcome, r.Params)
		}
		if strings.Contains(r.Params, `"agent_settings"`) && !strings.Contains(r.Params, `"sha256"`) {
			t.Fatalf("у документа нет размера и sha256 (%s): %.400s", r.Outcome, r.Params)
		}
	}
	// draft_created → change с after; unchanged → change нет.
	created, unchanged := rest[2], rest[3]
	if !strings.Contains(created.Params, `"change"`) || !strings.Contains(created.Params, `"draft_created"`) {
		t.Fatalf("у draft_created нет change: %.400s", created.Params)
	}
	if strings.Contains(unchanged.Params, `"change"`) {
		t.Fatalf("у unchanged есть change: %.400s", unchanged.Params)
	}
	if !strings.Contains(mcpRows[1].Params, `"change"`) || !strings.Contains(mcpRows[1].Params, `"draft_replaced"`) {
		t.Fatalf("у MCP updated нет change: %.400s", mcpRows[1].Params)
	}
	// Отказы 400 тоже без значения.
	for _, r := range f.rows(`a.outcome = 'invalid_input' AND (a.action = 'POST /v1/projects/:projectKey/import' OR a.action = 'sdlc_import_bundle')`) {
		if strings.Contains(r.Params, secret) {
			t.Fatalf("секрет в записи отказа: %.400s", r.Params)
		}
	}
	var audit string
	if err := f.pool.QueryRow(ctx, `SELECT coalesce(string_agg(params::text, E'\n'), '') FROM audit_log`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(audit, secret) || strings.Contains(logs.String(), secret) {
		t.Fatal("секрет в журнале или в логе сервера")
	}
}

// AC-14: бот с maintainer и admin отбивается 403 human_action по REST и MCP,
// в боевом и сухом прогоне; связка бота без поля применяется; человек —
// применяет.
func TestBundleAgentSettingsBotRefused(t *testing.T) {
	f := newAuditFixture(t)
	lead := f.memberToken("lead", domain.MemberMaintainer, nil)
	bots := map[string]string{
		"бот maintainer": f.memberToken("agent:keeper", domain.MemberMaintainer, nil),
		"бот admin":      f.memberToken("agent:chief", domain.MemberAdmin, nil),
	}
	n := 0
	for name, tok := range bots {
		n++
		for _, dry := range []string{"", "?dry_run=true"} {
			code, out := f.json("POST", importPath+dry, tok, agentSettingsBundle(bundleDoc("claude-opus-7", "1"), ""))
			if _, reason := errOf(out); code != http.StatusForbidden || reason != workflow.DenyHumanAction {
				t.Fatalf("%s REST %q: %d %v", name, dry, code, out)
			}
		}
		for _, dry := range []bool{false, true} {
			var doc any
			_ = json.Unmarshal(bundleDoc("claude-opus-7", "1"), &doc)
			res, isErr := f.mcp(tok, "sdlc_import_bundle", map[string]any{"dry_run": dry,
				"bundle": map[string]any{"version": 3, "agent_settings": map[string]any{"document": doc}}})
			if _, reason := errOf(res); !isErr || errCode(res) != "forbidden" || reason != workflow.DenyHumanAction {
				t.Fatalf("%s MCP dry=%v: %v", name, dry, res)
			}
		}
		// Та же роль без поля применяется.
		code, out := f.json("POST", importPath, tok, map[string]any{"version": 3,
			"roles": []any{map[string]any{"key": "helper-" + string(rune('a'+n)), "name": "Роль", "instructions": "x"}}})
		if code != http.StatusOK {
			t.Fatalf("%s: связка без настройки: %d %v", name, code, out)
		}
	}
	if total, _ := versionsCount(t, f); total != 0 {
		t.Fatalf("бот записал %d версий", total)
	}
	// Отказы — в журнале с исходом forbidden и без документа.
	denied := f.rows(`a.outcome = 'forbidden' AND (a.action = 'POST /v1/projects/:projectKey/import' OR a.action = 'sdlc_import_bundle')`)
	if len(denied) != 8 {
		t.Fatalf("отказов в журнале %d, ждали 8", len(denied))
	}
	for _, r := range denied {
		if strings.Contains(r.Params, "claude-opus") {
			t.Fatalf("документ в записи отказа: %.300s", r.Params)
		}
	}
	if code, out := f.json("POST", importPath, lead, agentSettingsBundle(bundleDoc("claude-opus-7", "1"), "")); code != http.StatusOK {
		t.Fatalf("человек: %d %v", code, out)
	}
}

// Токену задачи импорт закрыт по-прежнему (решение 0049), в том числе со
// связкой с настройкой.
func TestBundleAgentSettingsTaskTokenRefused(t *testing.T) {
	f := newAuditFixture(t)
	lead := f.memberToken("lead", domain.MemberMaintainer, nil)
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/start", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/claim", lead, map[string]any{}); code != http.StatusOK {
		t.Fatalf("claim: %d %v", code, body)
	}
	code, tt := f.json("POST", "/v1/tasks/SDLC-1/task-token", lead, nil)
	if code != http.StatusCreated {
		t.Fatalf("токен задачи: %d %v", code, tt)
	}
	code, out := f.json("POST", importPath, tt["token"].(string), agentSettingsBundle(bundleDoc("claude-opus-7", "1"), ""))
	if code != http.StatusForbidden {
		t.Fatalf("токен задачи: %d %v", code, out)
	}
	if total, _ := versionsCount(t, f); total != 0 {
		t.Fatal("токен задачи записал настройку")
	}
}
