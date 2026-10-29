package httpapi

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Выбор необязательных этапов и пропуск по REST и MCP (CLAB-315, решение
// 0066, п. 9-15, 18): заведение и правка, права токена задачи с журналом
// отказов, представление перехода, возврат в выключенный этап и набор в
// sdlc_whoami.

// skipBundle — цикл задач с design (выключен по умолчанию) и docs
// (включён) между analysis и development; возврат в design — reject.
func skipBundle() map[string]any {
	st := func(key, category string, extra map[string]any) map[string]any {
		out := map[string]any{"key": key, "name": strings.ToUpper(key[:1]) + key[1:], "category": category}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	tr := func(key, from, to, kind string) map[string]any {
		out := map[string]any{"key": key, "name": key, "to": to, "kind": kind}
		if from != "" {
			out["from"] = from
		}
		return out
	}
	return map[string]any{
		"version": 1,
		"workflows": []any{map[string]any{
			"name": "Пропуск", "task_types": []string{"task"},
			"statuses": []any{
				st("new", "backlog", map[string]any{"is_initial": true}),
				st("analysis", "active", map[string]any{"role": "analyst"}),
				st("design", "active", map[string]any{"is_optional": true, "enabled_by_default": false}),
				st("docs", "active", map[string]any{"is_optional": true}),
				st("development", "active", map[string]any{"role": "developer"}),
				st("done", "done", map[string]any{"is_terminal": true}),
				st("cancelled", "cancelled", map[string]any{"is_terminal": true}),
			},
			"transitions": []any{
				tr("start", "new", "analysis", "advance"),
				tr("analysis_done", "analysis", "design", "advance"),
				tr("design_done", "design", "docs", "advance"),
				tr("docs_done", "docs", "development", "advance"),
				tr("finish", "development", "done", "advance"),
				tr("back_to_design", "development", "design", "reject"),
				tr("cancel", "", "cancelled", "cancel"),
			},
		}},
	}
}

func (f *auditFixture) activateSkipCycle() {
	f.t.Helper()
	code, report := f.json("POST", "/v1/projects/SDLC/import", f.root, skipBundle())
	if code != http.StatusOK {
		f.t.Fatalf("импорт цикла: %d %v", code, report)
	}
	entry := report["workflows"].([]any)[0].(map[string]any)
	if code, body := f.json("POST", "/v1/workflows/"+entry["id"].(string)+"/activate", f.root, nil); code != http.StatusOK {
		f.t.Fatalf("активация: %d %v", code, body)
	}
}

func stagesOf(body map[string]any) map[string]any {
	if task, ok := body["task"].(map[string]any); ok {
		body = task
	}
	out, _ := body["optional_stages"].(map[string]any)
	return out
}

func previewByKey(t *testing.T, list any, key string) map[string]any {
	t.Helper()
	items, _ := list.([]any)
	for _, raw := range items {
		p := raw.(map[string]any)
		if p["key"] == key || p["transition"] == key {
			return p
		}
	}
	t.Fatalf("перехода %s нет: %v", key, list)
	return nil
}

// AC-2…AC-5, AC-11, AC-12, AC-14 по транспортам.
func TestOptionalStagesOverRESTAndMCP(t *testing.T) {
	f := newAuditFixture(t)
	f.activateSkipCycle()

	// AC-2: заведение, полный набор в ответе, карточке и списке.
	code, parent := f.json("POST", "/v1/projects/SDLC/tasks", f.root, map[string]any{
		"title": "С выбором", "optional_stages": map[string]any{"design": true}})
	if code != http.StatusCreated || !reflect.DeepEqual(stagesOf(parent), map[string]any{"design": true, "docs": true}) {
		t.Fatalf("заведение с выбором: %d %v", code, parent)
	}
	key := parent["key"].(string)
	if _, card := f.json("GET", "/v1/tasks/"+key, f.root, nil); !reflect.DeepEqual(stagesOf(card), map[string]any{"design": true, "docs": true}) {
		t.Fatalf("карточка: %v", card["task"])
	}
	_, list := f.json("GET", "/v1/projects/SDLC/tasks", f.root, nil)
	found := false
	for _, raw := range list["tasks"].([]any) {
		if item := raw.(map[string]any); item["key"] == key {
			found = reflect.DeepEqual(item["optional_stages"], map[string]any{"design": true, "docs": true})
		}
	}
	if !found {
		t.Fatalf("набор в списке задач: %v", list)
	}
	if mcpTask, _ := f.mcp(f.root, "sdlc_get_task", map[string]any{"task_key": key}); !reflect.DeepEqual(stagesOf(mcpTask),
		map[string]any{"design": true, "docs": true}) {
		t.Fatalf("sdlc_get_task: %v", mcpTask["task"])
	}

	// AC-3: неверный ключ — 400 с перечнем допустимых, задачи нет.
	code, bad := f.json("POST", "/v1/projects/SDLC/tasks", f.root, map[string]any{
		"title": "Плохой", "optional_stages": map[string]any{"development": false}})
	details, _ := bad["error"].(map[string]any)["details"].(map[string]any)
	if code != http.StatusBadRequest || errCode(bad) != "invalid_input" ||
		!reflect.DeepEqual(details["allowed"], []any{"design", "docs"}) {
		t.Fatalf("неверный ключ: %d %v", code, bad)
	}

	// Токен задачи parent.
	for _, path := range []string{"/v1/tasks/" + key + "/transitions/start", "/v1/tasks/" + key + "/claim"} {
		if code, body := f.json("POST", path, f.root, map[string]any{}); code != http.StatusOK {
			t.Fatalf("%s: %d %v", path, code, body)
		}
	}
	code, issued := f.json("POST", "/v1/tasks/"+key+"/task-token", f.root, nil)
	if code != http.StatusCreated {
		t.Fatalf("токен задачи: %d %v", code, issued)
	}
	tt := issued["token"].(string)

	// AC-4: дети своей задачи — заведение и правка, REST и MCP.
	code, child := f.json("POST", "/v1/projects/SDLC/tasks", tt, map[string]any{
		"title": "Ребёнок", "parent_key": key, "optional_stages": map[string]any{"docs": false}})
	if code != http.StatusCreated || !reflect.DeepEqual(stagesOf(child), map[string]any{"design": false, "docs": false}) {
		t.Fatalf("ребёнок токеном: %d %v", code, child)
	}
	childKey := child["key"].(string)
	if code, body := f.json("PATCH", "/v1/tasks/"+childKey, tt, map[string]any{
		"optional_stages": map[string]any{"design": true}}); code != http.StatusOK ||
		!reflect.DeepEqual(stagesOf(body), map[string]any{"design": true, "docs": false}) {
		t.Fatalf("правка ребёнка токеном: %d %v", code, body)
	}
	if out, isErr := f.mcp(tt, "sdlc_update_task", map[string]any{
		"task_key": childKey, "optional_stages": map[string]any{"docs": true}}); isErr ||
		!reflect.DeepEqual(out["optional_stages"], map[string]any{"design": true, "docs": true}) {
		t.Fatalf("sdlc_update_task ребёнка: %v", out)
	}
	if out, isErr := f.mcp(tt, "sdlc_create_task", map[string]any{
		"title": "Ребёнок MCP", "parent": key, "optional_stages": map[string]any{"design": true}}); isErr ||
		!reflect.DeepEqual(out["optional_stages"], map[string]any{"design": true, "docs": true}) {
		t.Fatalf("sdlc_create_task с выбором: %v", out)
	}
	_, timeline := f.json("GET", "/v1/tasks/"+childKey+"/timeline", f.root, nil)
	if !strings.Contains(mustJSON(t, timeline), `"optional_stages"`) {
		t.Fatalf("task.updated в ленте ребёнка: %v", timeline)
	}

	// AC-5: своей задаче — 403 целиком, с неверным ключом тоже; журнал.
	for _, stages := range []map[string]any{{"design": false}, {"nope": true}} {
		code, body := f.json("PATCH", "/v1/tasks/"+key, tt, map[string]any{"optional_stages": stages, "title": "Чужой заголовок"})
		msg, _ := body["error"].(map[string]any)["message"].(string)
		if code != http.StatusForbidden || !executorRefused(body) || !strings.Contains(msg, "выбор этапов своей задачи — решение человека") {
			t.Fatalf("своя задача токеном %v: %d %v", stages, code, body)
		}
	}
	if out, isErr := f.mcp(tt, "sdlc_update_task", map[string]any{
		"task_key": key, "optional_stages": map[string]any{"design": false}}); !isErr {
		t.Fatalf("sdlc_update_task своей задачи: %v", out)
	}
	_, card := f.json("GET", "/v1/tasks/"+key, f.root, nil)
	if got := card["task"].(map[string]any); got["title"] != "С выбором" ||
		!reflect.DeepEqual(stagesOf(got), map[string]any{"design": true, "docs": true}) {
		t.Fatalf("отказ изменил задачу: %v", got)
	}
	if rows := f.rows("a.token_kind = 'task' AND a.outcome = 'forbidden' AND a.action = 'PATCH /v1/tasks/:taskKey'"); len(rows) != 2 {
		t.Fatalf("отказы REST в журнале: %+v", rows)
	}
	if rows := f.rows("a.token_kind = 'task' AND a.outcome = 'forbidden' AND a.action = 'sdlc_update_task'"); len(rows) != 1 {
		t.Fatalf("отказ MCP в журнале: %+v", rows)
	}
	if code, body := f.json("PATCH", "/v1/tasks/"+key, tt, map[string]any{"title": "Свой заголовок"}); code != http.StatusOK {
		t.Fatalf("заголовок своей задачи токеном: %d %v", code, body)
	}

	// AC-11: человек выключает оба этапа — представление у всех каналов.
	if code, body := f.json("PATCH", "/v1/tasks/"+key, f.root, map[string]any{
		"optional_stages": map[string]any{"design": false, "docs": false}}); code != http.StatusOK {
		t.Fatalf("правка человеком: %d %v", code, body)
	}
	wantAhead := func(p map[string]any, where string) {
		t.Helper()
		if p["to_status"] != "design" || p["lands_in"] != "development" ||
			!reflect.DeepEqual(p["skips"], []any{"design", "docs"}) || p["unavailable"] != nil {
			t.Fatalf("%s: analysis_done %v", where, p)
		}
	}
	_, transitions := f.json("GET", "/v1/tasks/"+key+"/transitions", f.root, nil)
	wantAhead(previewByKey(t, transitions["transitions"], "analysis_done"), "REST transitions")
	if c := previewByKey(t, transitions["transitions"], "cancel"); c["lands_in"] != "cancelled" || !reflect.DeepEqual(c["skips"], []any{}) {
		t.Fatalf("переход без пропуска: %v", c)
	}
	_, card = f.json("GET", "/v1/tasks/"+key, f.root, nil)
	wantAhead(previewByKey(t, card["transitions"], "analysis_done"), "карточка")
	check, _ := f.mcp(f.root, "sdlc_check_gates", map[string]any{"task_key": key, "transition": "analysis_done"})
	wantAhead(check, "sdlc_check_gates с transition")
	all, _ := f.mcp(f.root, "sdlc_check_gates", map[string]any{"task_key": key})
	wantAhead(previewByKey(t, all["transitions"], "analysis_done"), "sdlc_check_gates")
	_, claim := f.json("POST", "/v1/tasks/"+key+"/claim", tt, map[string]any{})
	wantAhead(previewByKey(t, claim["gates_ahead"], "analysis_done"), "gates_ahead захвата")
	if !reflect.DeepEqual(stagesOf(claim), map[string]any{"design": false, "docs": false}) {
		t.Fatalf("task.optional_stages захвата: %v", claim["task"])
	}

	// AC-14: набор для создающего в sdlc_whoami, личным токеном и токеном
	// задачи.
	for name, token := range map[string]string{"личный": f.root, "токен задачи": tt} {
		who, isErr := f.mcp(token, "sdlc_whoami", map[string]any{})
		if isErr {
			t.Fatalf("%s: sdlc_whoami: %v", name, who)
		}
		byType := map[string]any{}
		for _, raw := range who["workflows"].([]any) {
			wf := raw.(map[string]any)
			for _, typ := range wf["task_types"].([]any) {
				byType[typ.(string)] = wf["optional_stages"]
			}
		}
		want := []any{
			map[string]any{"key": "design", "name": "Design", "enabled_by_default": false},
			map[string]any{"key": "docs", "name": "Docs", "enabled_by_default": true},
		}
		if !reflect.DeepEqual(byType["task"], want) || !reflect.DeepEqual(byType["epic"], []any{}) {
			t.Fatalf("%s: workflows[].optional_stages: %v", name, who["workflows"])
		}
	}

	// AC-8 по MCP: переход токеном — фактический статус и skips.
	out, isErr := f.mcp(tt, "sdlc_request_transition", map[string]any{"task_key": key, "transition": "analysis_done"})
	if isErr || out["task"].(map[string]any)["status"] != "development" || !reflect.DeepEqual(out["skips"], []any{"design", "docs"}) {
		t.Fatalf("sdlc_request_transition с пропуском: %v", out)
	}

	// AC-11, AC-12: возврат в выключенный — недоступен и 409.
	_, transitions = f.json("GET", "/v1/tasks/"+key+"/transitions", f.root, nil)
	back := previewByKey(t, transitions["transitions"], "back_to_design")
	if back["allowed"] != false || back["lands_in"] != nil || back["unavailable"] != "stage_disabled" {
		t.Fatalf("back_to_design: %v", back)
	}
	check, _ = f.mcp(f.root, "sdlc_check_gates", map[string]any{"task_key": key, "transition": "back_to_design"})
	if check["can_transition"] != false || check["unavailable"] != "stage_disabled" {
		t.Fatalf("sdlc_check_gates недоступного: %v", check)
	}
	code, body := f.json("POST", "/v1/tasks/"+key+"/transitions/back_to_design", f.root, map[string]any{})
	if code != http.StatusConflict || errCode(body) != "transition_not_allowed" {
		t.Fatalf("возврат в выключенный: %d %v", code, body)
	}

	// REST-результат перехода с пропуском.
	code, other := f.json("POST", "/v1/projects/SDLC/tasks", f.root, map[string]any{
		"title": "Мимо", "optional_stages": map[string]any{"docs": false}})
	if code != http.StatusCreated {
		t.Fatalf("задача для REST-перехода: %d %v", code, other)
	}
	otherKey := other["key"].(string)
	f.json("POST", "/v1/tasks/"+otherKey+"/transitions/start", f.root, map[string]any{})
	code, moved := f.json("POST", "/v1/tasks/"+otherKey+"/transitions/analysis_done", f.root, map[string]any{})
	if code != http.StatusOK || moved["task"].(map[string]any)["status_key"] != "development" ||
		!reflect.DeepEqual(moved["skips"], []any{"design", "docs"}) {
		t.Fatalf("REST-переход с пропуском: %d %v", code, moved)
	}

}
