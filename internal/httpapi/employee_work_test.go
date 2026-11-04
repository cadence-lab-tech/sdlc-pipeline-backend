package httpapi

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Сотрудник работает с любой задачей проекта без аренды (CLAB-411): AC-1 —
// правка в gating, AC-2 — выбор этапов на апруве и отказы 409, AC-3 —
// заведение в бэклог, AC-4 — ребёнок чужой задачи с выбором этапов, связью и
// назначением, назначение и связи, AC-17 — прежние поля карточки на месте;
// R-8 — тот же результат тулами MCP.
func TestEmployeeWorksAnyTask(t *testing.T) {
	f := newAuditFixture(t)
	f.activateSkipCycle()
	employee := f.memberToken("user:anna", domain.MemberAgent, nil)
	const backend, docs = "cadence-lab-tech/sdlc-pipeline-backend", "cadence-lab-tech/sdlc-pipeline-docs"

	newTask := func(token string, body map[string]any) (int, map[string]any) {
		t.Helper()
		return f.json(http.MethodPost, "/v1/projects/SDLC/tasks", token, body)
	}
	events := func(key, typ string) []map[string]any {
		t.Helper()
		_, body := f.json(http.MethodGet, "/v1/tasks/"+key+"/timeline", f.root, nil)
		var out []map[string]any
		for _, raw := range body["events"].([]any) {
			if e := raw.(map[string]any); e["type"] == typ {
				out = append(out, e)
			}
		}
		return out
	}
	byAnna := func(list []map[string]any) bool {
		for _, e := range list {
			if e["actor_handle"] == "anna" {
				return true
			}
		}
		return false
	}

	// AC-1, AC-2: этап аналитики в gating, аренды у сотрудника нет.
	code, created := newTask(f.root, map[string]any{"title": "На апруве", "repositories": []string{backend}})
	if code != http.StatusCreated {
		t.Fatalf("заведение: %d %v", code, created)
	}
	G := created["key"].(string)
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+G+"/transitions/start", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("старт: %d %v", code, body)
	}
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE stage_runs SET state = 'gating' FROM tasks t
		WHERE t.key = $1 AND stage_runs.task_id = t.id AND stage_runs.status_id = t.status_id`, G); err != nil {
		t.Fatal(err)
	}
	if code, body := f.json(http.MethodPatch, "/v1/tasks/"+G, employee, map[string]any{
		"title": "На апруве, уточнено", "description": "новое", "priority": "high", "labels": []string{"bug"},
		"external_ref":   map[string]any{"jira": map[string]any{"key": "PROJ-1", "url": "https://jira.example.com/browse/PROJ-1"}},
		"model_level":    "medium",
		"stage_settings": map[string]any{"development": map[string]any{"model_level": "high"}},
	}); code != http.StatusOK {
		t.Fatalf("AC-1 правка в gating: %d %v", code, body)
	}
	if !byAnna(events(G, domain.EventTaskUpdated)) {
		t.Fatal("AC-1: в ленте нет task.updated сотрудника")
	}
	code, body := f.json(http.MethodPatch, "/v1/tasks/"+G, employee, map[string]any{"optional_stages": map[string]any{"design": true}})
	if code != http.StatusOK || !reflect.DeepEqual(stagesOf(body), map[string]any{"design": true, "docs": true}) {
		t.Fatalf("AC-2 выбор этапов на апруве: %d %v", code, body)
	}
	if _, card := f.json(http.MethodGet, "/v1/tasks/"+G, employee, nil); stagesOf(card)["design"] != true {
		t.Fatalf("AC-2 карточка: %v", card["task"])
	}
	// Выключить текущий статус — 409, данные прежние.
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+G+"/transitions/analysis_done", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("analysis_done: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPatch, "/v1/tasks/"+G, employee, map[string]any{"optional_stages": map[string]any{"design": false}}); code != http.StatusConflict {
		t.Fatalf("AC-2 выключить текущий: %d %v", code, body)
	}
	if _, card := f.json(http.MethodGet, "/v1/tasks/"+G, f.root, nil); stagesOf(card)["design"] != true {
		t.Fatalf("AC-2 отказ изменил выбор: %v", card["task"])
	}

	// AC-3: сотрудник без parent — начальный статус, конец бэклога.
	code, top := newTask(employee, map[string]any{"title": "Баг", "repositories": []string{backend}, "labels": []string{"bug"}, "priority": "high"})
	if code != http.StatusCreated || top["status_key"] != "new" || top["parent_key"] != nil {
		t.Fatalf("AC-3: %d %v", code, top)
	}
	_, list := f.json(http.MethodGet, "/v1/projects/SDLC/tasks?order=backlog&top_level=true&terminal=false&limit=500", f.root, nil)
	items := list["tasks"].([]any)
	if last := items[len(items)-1].(map[string]any); last["key"] != top["key"] {
		t.Fatalf("AC-3: в конце бэклога %v, а не %v", last["key"], top["key"])
	}
	if !byAnna(events(top["key"].(string), domain.EventTaskCreated)) {
		t.Fatal("AC-3: в ленте нет task.created сотрудника")
	}

	// AC-4: ребёнок чужого эпика с выбором этапов, blocks и назначением; вне
	// области родителя — тоже.
	code, epic := newTask(f.root, map[string]any{"title": "Эпик коллеги", "repositories": []string{backend}})
	if code != http.StatusCreated {
		t.Fatalf("родитель: %d %v", code, epic)
	}
	E := epic["key"].(string)
	code, kid := newTask(employee, map[string]any{
		"title": "Ребёнок", "parent_key": E, "repositories": []string{docs},
		"optional_stages": map[string]any{"design": false}, "blocks": []string{G}, "assignee": "user:anna"})
	if code != http.StatusCreated || kid["parent_key"] != E || kid["assignee_handle"] != "anna" ||
		!reflect.DeepEqual(stagesOf(kid), map[string]any{"design": false, "docs": true}) {
		t.Fatalf("AC-4 ребёнок: %d %v", code, kid)
	}
	K := kid["key"].(string)
	_, card := f.json(http.MethodGet, "/v1/tasks/"+K, employee, nil)
	blocksG := false
	for _, raw := range card["links"].([]any) {
		if l := raw.(map[string]any); l["rel"] == "blocks" && l["direction"] == "outgoing" && l["task_key"] == G {
			blocksG = true
		}
	}
	if !blocksG {
		t.Fatalf("AC-4: связи blocks %s нет: %v", G, card["links"])
	}
	if !byAnna(events(K, domain.EventTaskCreated)) {
		t.Fatal("AC-4: task.created ребёнка без сотрудника")
	}
	// Назначение и связь любой задачи проекта.
	if code, body := f.json(http.MethodPut, "/v1/tasks/"+E+"/assignee", employee, map[string]any{"assignee": "user:anna"}); code != http.StatusOK {
		t.Fatalf("AC-4 назначение: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+E+"/links", employee, map[string]any{"task": top["key"], "rel": "blocks"}); code != http.StatusOK {
		t.Fatalf("AC-4 связь: %d %v", code, body)
	}
	if !byAnna(events(E, "task.assigned")) || !byAnna(events(E, "task.linked")) {
		t.Fatal("AC-4: нет task.assigned или task.linked сотрудника")
	}
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+E+"/links", employee, map[string]any{"task": E, "rel": "blocks"}); code != http.StatusBadRequest {
		t.Fatalf("связь с собой: %d %v", code, body)
	}

	// R-8: те же действия тулами MCP.
	if out, isErr := f.mcp(employee, "sdlc_update_task", map[string]any{"task_key": E, "description": "MCP"}); isErr {
		t.Fatalf("sdlc_update_task: %v", out)
	}
	out, isErr := f.mcp(employee, "sdlc_create_task", map[string]any{"title": "MCP без родителя", "repositories": []string{backend}})
	if isErr || out["parent"] != nil {
		t.Fatalf("sdlc_create_task: %v", out)
	}
	if out, isErr := f.mcp(employee, "sdlc_assign_task", map[string]any{"task_key": E, "assignee": ""}); isErr {
		t.Fatalf("sdlc_assign_task: %v", out)
	}
	if out, isErr := f.mcp(employee, "sdlc_link_tasks", map[string]any{"from": E, "to": G, "rel": "relates_to"}); isErr {
		t.Fatalf("sdlc_link_tasks: %v", out)
	}
	// Бот тулом — тот же отказ, что по REST.
	_, rest := newTask(f.bot, map[string]any{"title": "Бот без родителя"})
	mcpOut, isErr := f.mcp(f.bot, "sdlc_create_task", map[string]any{"title": "Бот без родителя"})
	restMsg, _ := errOf(rest)
	if e, _ := mcpOut["error"].(map[string]any); !isErr || e["code"] != "forbidden" || e["message"] != restMsg {
		t.Fatalf("бот sdlc_create_task: %v, REST %v", mcpOut, rest)
	}

	// AC-17: actor_rights в sdlc_get_task и /state, прежние поля карточки на
	// месте — к ним добавлено одно поле.
	got, _ := f.mcp(employee, "sdlc_get_task", map[string]any{"task_key": E})
	if r, _ := got["actor_rights"].(map[string]any); r["edit"] != true || r["scope"] != true {
		t.Fatalf("sdlc_get_task actor_rights: %v", got["actor_rights"])
	}
	_, state := f.json(http.MethodGet, "/v1/tasks/"+E+"/state", f.bot, nil)
	if r, _ := state["actor_rights"].(map[string]any); r["edit"] != false || r["assign"] != true {
		t.Fatalf("/state actor_rights бота: %v", state["actor_rights"])
	}
	keys := make([]string, 0, len(card))
	for k := range card {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"actor_rights", "artifacts", "attachments", "children", "links", "repo_refs", "stage_limit",
		"stage_runs", "task", "transitions", "usage"}
	if !slices.Equal(keys, want) {
		t.Fatalf("поля карточки %v, ждали %v", keys, want)
	}
	for _, raw := range card["transitions"].([]any) {
		p := raw.(map[string]any)
		if _, ok := p["actor_may"]; !ok {
			t.Fatalf("переход без actor_may: %v", p)
		}
	}
}
