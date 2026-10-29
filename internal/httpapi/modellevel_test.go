package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Уровень модели по REST и MCP (CLAB-259, решение 0060): версия роли, уровни
// задачи в круге записи токена задачи с журналом отказов, execution в ответе
// захвата и состояния одинаковый в обоих транспортах.

func (f *auditFixture) json(method, path, token string, body any) (int, map[string]any) {
	f.t.Helper()
	rec := f.do(method, path, token, body)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// mcp зовёт тул через HTTP-транспорт MCP и отдаёт разобранный текст
// результата и признак ошибки.
func (f *auditFixture) mcp(token, tool string, args map[string]any) (map[string]any, bool) {
	f.t.Helper()
	call, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	req := httptest.NewRequest("POST", "/mcp/projects/SDLC", strings.NewReader(string(call)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if i := strings.LastIndex(body, "data: "); i >= 0 {
		body = strings.TrimSpace(strings.SplitN(body[i+len("data: "):], "\n", 2)[0])
	}
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &rpc); err != nil || len(rpc.Result.Content) == 0 {
		f.t.Fatalf("%s: ответ MCP не разобран: %d %s", tool, rec.Code, rec.Body)
	}
	out := map[string]any{}
	_ = json.Unmarshal([]byte(rpc.Result.Content[0].Text), &out)
	return out, rpc.Result.IsError
}

func execOf(body map[string]any) string {
	e, ok := body["execution"].(map[string]any)
	if !ok {
		return "нет execution"
	}
	level, _ := e["model_level"].(string)
	if level == "" {
		level = "-"
	}
	source, _ := e["model_level_source"].(string)
	return level + "/" + source
}

// AC-3: версия роли с уровнем по REST; model_hint как раньше.
func TestRoleModelLevelOverREST(t *testing.T) {
	f := newAuditFixture(t)
	code, body := f.json("POST", "/v1/projects/SDLC/roles", f.root, map[string]any{
		"key": "writer", "name": "Техписатель",
		"version": map[string]any{"instructions": "Пиши.", "model_level": "medium", "model_hint": "sonnet"},
	})
	if code != http.StatusCreated {
		t.Fatalf("роль с уровнем: %d %v", code, body)
	}
	latest := func() map[string]any {
		t.Helper()
		code, body := f.json("GET", "/v1/projects/SDLC/roles/writer", f.root, nil)
		if code != http.StatusOK {
			t.Fatalf("роль: %d %v", code, body)
		}
		return body["latest_version"].(map[string]any)
	}
	if v := latest(); v["model_level"] != "medium" || v["model_hint"] != "sonnet" {
		t.Fatalf("уровень версии роли: %v", v)
	}
	if code, body := f.json("GET", "/v1/projects/SDLC/roles/writer/versions/1", f.root, nil); code != http.StatusOK ||
		body["model_level"] != "medium" {
		t.Fatalf("версия роли: %d %v", code, body)
	}

	code, body = f.json("POST", "/v1/projects/SDLC/roles/writer/versions", f.root, map[string]any{
		"instructions": "Пиши иначе.", "model_level": "huge",
	})
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if code != http.StatusBadRequest || errCode(body) != "invalid_input" || !strings.Contains(msg, "low, medium, high") {
		t.Fatalf("неверный уровень: %d %v", code, body)
	}
	if v := latest(); v["version"].(float64) != 1 {
		t.Fatalf("отказ завёл версию: %v", v)
	}
	for _, level := range []any{"", nil} {
		req := map[string]any{"instructions": "Пиши кратко.", "model_hint": "sonnet"}
		if level != nil {
			req["model_level"] = level
		}
		if code, body := f.json("POST", "/v1/projects/SDLC/roles/writer/versions", f.root, req); code != http.StatusCreated {
			t.Fatalf("версия без уровня: %d %v", code, body)
		}
		if v := latest(); v["model_level"] != nil || v["model_hint"] != "sonnet" {
			t.Fatalf("версия без уровня: %v", v)
		}
	}
}

// AC-7…AC-10, AC-13: уровни задачи в круге записи токена задачи, отказы в
// журнале, execution одинаковый в REST и MCP.
func TestModelLevelOverRESTAndMCP(t *testing.T) {
	f := newAuditFixture(t)
	for _, path := range []string{"/v1/tasks/SDLC-1/transitions/start", "/v1/tasks/SDLC-1/claim"} {
		if code, body := f.json("POST", path, f.root, map[string]any{}); code != http.StatusOK {
			t.Fatalf("%s: %d %v", path, code, body)
		}
	}
	code, issued := f.json("POST", "/v1/tasks/SDLC-1/task-token", f.root, nil)
	if code != http.StatusCreated {
		t.Fatalf("токен задачи: %d %v", code, issued)
	}
	tt := issued["token"].(string)

	// Ребёнок своей задачи с уровнями — 201.
	code, child := f.json("POST", "/v1/projects/SDLC/tasks", tt, map[string]any{
		"title": "Ребёнок", "parent_key": "SDLC-1", "model_level": "high",
		"stage_settings": map[string]any{"development": map[string]any{"model_level": "low"}},
	})
	if code != http.StatusCreated || child["model_level"] != "high" {
		t.Fatalf("ребёнок с уровнями: %d %v", code, child)
	}
	childKey := child["key"].(string)
	if code, body := f.json("POST", "/v1/projects/SDLC/tasks", tt, map[string]any{
		"title": "Плохой ключ", "parent_key": "SDLC-1",
		"stage_settings": map[string]any{"done": map[string]any{"model_level": "low"}},
	}); code != http.StatusBadRequest || errCode(body) != "invalid_input" {
		t.Fatalf("ключ терминального статуса: %d %v", code, body)
	}

	// Своя задача и прямой ребёнок — 200.
	if code, body := f.json("PATCH", "/v1/tasks/SDLC-1", tt, map[string]any{
		"stage_settings": map[string]any{"analysis": map[string]any{"model_level": "high"}},
	}); code != http.StatusOK || body["stage_settings"].(map[string]any)["analysis"] == nil {
		t.Fatalf("уровень этапа своей задачи: %d %v", code, body)
	}
	if code, body := f.json("PATCH", "/v1/tasks/"+childKey, tt, map[string]any{"model_level": "medium"}); code != http.StatusOK ||
		body["model_level"] != "medium" {
		t.Fatalf("уровень ребёнка: %d %v", code, body)
	}

	// Вне круга записи: внук, сосед по эпику, чужая задача — 403 и запись в
	// журнал. Читает их токен все (CLAB-305), поэтому отбивает каждую круг
	// записи текстом ExecutorRefusal.
	code, grand := f.json("POST", "/v1/projects/SDLC/tasks", f.root, map[string]any{"title": "Внук", "parent_key": childKey})
	if code != http.StatusCreated {
		t.Fatalf("внук: %d %v", code, grand)
	}
	code, alien := f.json("POST", "/v1/projects/SDLC/tasks", f.root, map[string]any{"title": "Чужая"})
	if code != http.StatusCreated {
		t.Fatalf("чужая: %d %v", code, alien)
	}
	outside := []string{grand["key"].(string), "SDLC-2", alien["key"].(string)}
	for _, key := range outside {
		code, body := f.json("PATCH", "/v1/tasks/"+key, tt, map[string]any{
			"model_level": "high", "stage_settings": map[string]any{"qa": map[string]any{"model_level": "high"}},
		})
		if code != http.StatusForbidden || errCode(body) != "forbidden" || !executorRefused(body) {
			t.Errorf("%s вне круга: %d %v", key, code, body)
		}
		_, task := f.json("GET", "/v1/tasks/"+key, f.root, nil)
		got := task["task"].(map[string]any)
		if got["model_level"] != nil || len(got["stage_settings"].(map[string]any)) != 0 {
			t.Errorf("%s: отказ изменил задачу: %v", key, got)
		}
	}
	if rows := f.rows("a.token_kind = 'task' AND a.outcome = 'forbidden' AND a.action = 'PATCH /v1/tasks/:taskKey'"); len(rows) != len(outside) {
		t.Fatalf("отказы в журнале: %+v", rows)
	}
	// MCP: то же правило круга.
	if out, isErr := f.mcp(tt, "sdlc_update_task", map[string]any{"task_key": childKey, "model_level": ""}); isErr || out["model_level"] != nil {
		t.Fatalf("sdlc_update_task ребёнка: %v", out)
	}
	if out, isErr := f.mcp(tt, "sdlc_update_task", map[string]any{"task_key": "SDLC-2", "model_level": "low"}); !isErr {
		t.Fatalf("sdlc_update_task вне круга: %v", out)
	}
	if out, isErr := f.mcp(tt, "sdlc_create_task", map[string]any{
		"title": "Ребёнок MCP", "parent": "SDLC-1", "model_level": "low",
		"stage_settings": map[string]any{"qa": map[string]any{"model_level": "medium"}},
	}); isErr || out["model_level"] != "low" {
		t.Fatalf("sdlc_create_task с уровнями: %v", out)
	}
	if rows := f.rows("a.token_kind = 'task' AND a.outcome = 'forbidden' AND a.action = 'sdlc_update_task'"); len(rows) != 1 {
		t.Fatalf("отказ MCP в журнале: %+v", rows)
	}
	// Уровень роли токену задачи закрыт, как вся правка ролей.
	if code, body := f.json("POST", "/v1/projects/SDLC/roles/analyst/versions", tt, map[string]any{
		"instructions": "x", "model_level": "low",
	}); code != http.StatusForbidden {
		t.Fatalf("версия роли токеном задачи: %d %v", code, body)
	}

	// Карточка: уровни задачи и снимок у прогона. Захват был до уровней —
	// none; правка уровня этапа после захвата снимок не трогает.
	_, card := f.json("GET", "/v1/tasks/SDLC-1", f.root, nil)
	task := card["task"].(map[string]any)
	if task["stage_settings"].(map[string]any)["analysis"] == nil {
		t.Fatalf("уровни в карточке: %v", task)
	}
	runs := card["stage_runs"].([]any)
	last := runs[len(runs)-1].(map[string]any)
	if last["model_level_source"] != "none" {
		t.Fatalf("снимок в stage_runs[]: %v", last)
	}

	// Одинаковый execution: REST claim (подтверждение), REST state, MCP
	// claim и sdlc_get_task.
	_, claim := f.json("POST", "/v1/tasks/SDLC-1/claim", tt, map[string]any{})
	_, state := f.json("GET", "/v1/tasks/SDLC-1/state", f.root, nil)
	mcpClaim, _ := f.mcp(tt, "sdlc_claim_task", map[string]any{"task_key": "SDLC-1"})
	mcpTask, _ := f.mcp(f.root, "sdlc_get_task", map[string]any{"task_key": "SDLC-1"})
	for name, body := range map[string]map[string]any{
		"REST claim": claim, "REST state": state, "MCP claim": mcpClaim, "MCP get_task": mcpTask,
	} {
		if got := execOf(body); got != "-/none" {
			t.Errorf("%s: execution %s, ожидали -/none", name, got)
		}
	}
	role, _ := claim["role"].(map[string]any)
	if _, has := role["model_level"]; !has {
		t.Fatalf("role.model_level нет в ответе захвата: %v", role)
	}
	view, _ := mcpTask["task"].(map[string]any)
	if view["stage_settings"].(map[string]any)["analysis"] == nil {
		t.Fatalf("уровни задачи в sdlc_get_task: %v", view)
	}
}
