package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// Карточка с детьми (CLAB-324, AC-7): у каждого элемента children[] —
// status_category, awaiting_approval и blocked_by после прежних полей задачи;
// задача без детей отдаёт children: [].
func TestGetTaskChildrenFields(t *testing.T) {
	f := newAuditFixture(t)
	create := func(body map[string]any) string {
		t.Helper()
		code, task := f.json(http.MethodPost, "/v1/projects/SDLC/tasks", f.root, body)
		if code != http.StatusCreated {
			t.Fatalf("задача: %d %v", code, task)
		}
		return task["key"].(string)
	}
	first := create(map[string]any{"title": "первый", "parent_key": "SDLC-1"})
	second := create(map[string]any{"title": "второй", "parent_key": "SDLC-1"})
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+first+"/links", f.root, map[string]any{"task": second}); code != http.StatusOK {
		t.Fatalf("связь: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+first+"/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}

	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-1/repo-refs", f.root, map[string]any{
		"repository": "cadence-lab-tech/sdlc-pipeline-backend", "ref_type": "pull_request", "ref": "13",
		"url": "https://github.com/cadence-lab-tech/sdlc-pipeline-backend/pull/13"}); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("PR: %d %v", code, body)
	}

	rec := f.do(http.MethodGet, "/v1/tasks/SDLC-1", f.root, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("карточка: %d %s", rec.Code, rec.Body)
	}
	var card struct {
		Children []json.RawMessage `json:"children"`
		RepoRefs []struct {
			RefType string `json:"ref_type"`
			Ref     string `json:"ref"`
			URL     string `json:"url"`
		} `json:"repo_refs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatal(err)
	}
	if len(card.RepoRefs) != 1 || card.RepoRefs[0].Ref != "13" || card.RepoRefs[0].RefType != "pull_request" || card.RepoRefs[0].URL == "" {
		t.Fatalf("repo_refs карточки: %+v", card.RepoRefs)
	}
	if len(card.Children) != 2 {
		t.Fatalf("детей %d", len(card.Children))
	}
	want := map[string]struct {
		category  string
		blockedBy []any
	}{first: {"active", []any{}}, second: {"backlog", []any{first}}}
	for _, raw := range card.Children {
		var kid map[string]any
		if err := json.Unmarshal(raw, &kid); err != nil {
			t.Fatal(err)
		}
		w := want[kid["key"].(string)]
		if kid["status_category"] != w.category || kid["awaiting_approval"] != false {
			t.Errorf("%s: %v %v", kid["key"], kid["status_category"], kid["awaiting_approval"])
		}
		got, _ := json.Marshal(kid["blocked_by"])
		exp, _ := json.Marshal(w.blockedBy)
		if !bytes.Equal(got, exp) {
			t.Errorf("%s: blocked_by %s, want %s", kid["key"], got, exp)
		}
		// Поля задачи — первыми, новые — за ними.
		if bytes.Index(raw, []byte(`"optional_stages"`)) > bytes.Index(raw, []byte(`"status_category"`)) {
			t.Errorf("новые поля не после полей задачи: %s", raw)
		}
	}

	rec = f.do(http.MethodGet, "/v1/tasks/"+second, f.root, nil)
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"children":[]`)) {
		t.Fatalf("без детей: %s", rec.Body)
	}
	// PR и ветки — в той же карточке (repo_refs), без привязок — [].
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"repo_refs":[]`)) {
		t.Fatalf("repo_refs: %s", rec.Body)
	}
}
