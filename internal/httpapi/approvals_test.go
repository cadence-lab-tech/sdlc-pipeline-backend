package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
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
