package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// «Ждут меня» по REST (CLAB-326, AC-13): ответ объектом, решать — с
// maintainer; ниже — пустой список и can_decide=false.
func TestListApprovalsByLevel(t *testing.T) {
	f := newAuditFixture(t)
	for _, c := range []struct {
		who       string
		token     string
		canDecide bool
	}{{"администратор", f.root, true}, {"агент", f.bot, false}} {
		code, body := f.json(http.MethodGet, "/v1/projects/SDLC/approvals", c.token, nil)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %v", c.who, code, body)
		}
		list, ok := body["approvals"].([]any)
		if !ok || body["can_decide"] != c.canDecide || body["truncated"] != false {
			t.Fatalf("%s: %v", c.who, body)
		}
		if !c.canDecide && len(list) != 0 {
			t.Fatalf("%s без права видит %d элементов", c.who, len(list))
		}
	}
	// Без входа — 401, как у любой ручки проекта.
	if code, _ := f.json(http.MethodGet, "/v1/projects/SDLC/approvals", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", code)
	}
}

// AC-20: элемент списка задач несёт awaiting_approval после полей задачи,
// прежние поля не тронуты.
func TestListTasksAwaitingField(t *testing.T) {
	f := newAuditFixture(t)
	rec := f.do(http.MethodGet, "/v1/projects/SDLC/tasks", f.root, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("список: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Tasks) == 0 {
		t.Fatalf("список: %v %s", err, rec.Body)
	}
	for _, raw := range out.Tasks {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		if _, ok := item["awaiting_approval"].(bool); !ok {
			t.Fatalf("нет awaiting_approval: %s", raw)
		}
		for _, k := range []string{"key", "title", "status_key", "workflow_id", "optional_stages"} {
			if _, ok := item[k]; !ok {
				t.Fatalf("пропало поле %s: %s", k, raw)
			}
		}
		if bytes.Index(raw, []byte(`"optional_stages"`)) > bytes.Index(raw, []byte(`"awaiting_approval"`)) {
			t.Fatalf("новое поле не после полей задачи: %s", raw)
		}
	}
}

// AC-4 по REST: seen_artifact_id расходится с этапом — 409 с причиной,
// решения нет; без поля — как раньше.
func TestDecideGateSeenREST(t *testing.T) {
	f := newAuditFixture(t)
	code, task := f.json(http.MethodPost, "/v1/projects/SDLC/tasks", f.root, map[string]any{"title": "сверка"})
	if code != http.StatusCreated {
		t.Fatalf("задача: %d %v", code, task)
	}
	key := task["key"].(string)
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	path := "/v1/tasks/" + key + "/transitions/analysis_done/gates/human_approval/decision"
	code, body := f.json(http.MethodPost, path, f.root, map[string]any{"status": "passed", "seen_artifact_id": uuid.NewString()})
	errBody, _ := body["error"].(map[string]any)
	details, _ := errBody["details"].(map[string]any)
	if code != http.StatusConflict || errBody["code"] != "conflict" || details["reason"] != "stage_result_changed" {
		t.Fatalf("сверка не совпала: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPost, path, f.root, map[string]any{"status": "passed", "seen_artifact_id": ""}); code != http.StatusOK {
		t.Fatalf("сверка совпала: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPost, path, f.root, map[string]any{"status": "passed"}); code != http.StatusOK {
		t.Fatalf("без условия: %d %v", code, body)
	}
}

// «Не пройден» по ручному чек-листу по REST: гейт в предпросмотре
// переходов — failed с причиной, строка уходит из «Ждут меня»; повторная
// отметка всех пунктов — passed.
func TestChecklistFailedREST(t *testing.T) {
	f := newAuditFixture(t)
	code, task := f.json(http.MethodPost, "/v1/projects/SDLC/tasks", f.root, map[string]any{"title": "чек-лист"})
	if code != http.StatusCreated {
		t.Fatalf("задача: %d %v", code, task)
	}
	key := task["key"].(string)
	gates := func(transition string) map[string]map[string]any {
		t.Helper()
		code, body := f.json(http.MethodGet, "/v1/tasks/"+key+"/transitions", f.root, nil)
		if code != http.StatusOK {
			t.Fatalf("переходы: %d %v", code, body)
		}
		out := map[string]map[string]any{}
		for _, raw := range body["transitions"].([]any) {
			tr := raw.(map[string]any)
			if tr["key"] != transition {
				continue
			}
			for _, g := range tr["gates"].([]any) {
				gate := g.(map[string]any)
				out[gate["gate_key"].(string)] = gate
			}
		}
		return out
	}
	decide := func(transition, gate string, body map[string]any) {
		t.Helper()
		path := "/v1/tasks/" + key + "/transitions/" + transition + "/gates/" + gate + "/decision"
		if code, resp := f.json(http.MethodPost, path, f.root, body); code != http.StatusOK {
			t.Fatalf("%s/%s: %d %v", transition, gate, code, resp)
		}
	}
	// Всё, кроме чек-листа, снимаем руками: проверяется только он.
	clear := func(transition string) {
		t.Helper()
		for k, g := range gates(transition) {
			if g["type"] == "checklist" || g["status"] == "passed" {
				continue
			}
			if g["type"] == "human_approval" {
				decide(transition, k, map[string]any{"status": "passed"})
				continue
			}
			decide(transition, k, map[string]any{"status": "waived"})
		}
	}
	listed := func() bool {
		t.Helper()
		code, body := f.json(http.MethodGet, "/v1/projects/SDLC/approvals", f.root, nil)
		if code != http.StatusOK {
			t.Fatalf("выборка: %d %v", code, body)
		}
		for _, raw := range body["approvals"].([]any) {
			a := raw.(map[string]any)
			if a["task"].(map[string]any)["key"] == key && a["gate"].(map[string]any)["gate_key"] == "traceability" {
				return true
			}
		}
		return false
	}

	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	clear("analysis_done")
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/analysis_done", f.root, nil); code != http.StatusOK {
		t.Fatalf("analysis_done: %d %v", code, body)
	}
	clear("architecture_done")
	if g := gates("architecture_done")["traceability"]; g["status"] != "pending" || !listed() {
		t.Fatalf("до решения: %v, в выборке %v", g, listed())
	}

	decide("architecture_done", "traceability", map[string]any{"status": "failed", "details": map[string]any{"reason": "ADR без критерия AC-3"}})
	g := gates("architecture_done")["traceability"]
	if g["status"] != "failed" || !strings.Contains(g["reason"].(string), "ADR без критерия AC-3") || listed() {
		t.Fatalf("после «Не пройден»: %v, в выборке %v", g, listed())
	}

	decide("architecture_done", "traceability", map[string]any{"status": "passed", "details": map[string]any{"checked": []string{"ac_covered", "impact_complete"}}})
	if g := gates("architecture_done")["traceability"]; g["status"] != "passed" || listed() {
		t.Fatalf("повторная отметка: %v, в выборке %v", g, listed())
	}
}
