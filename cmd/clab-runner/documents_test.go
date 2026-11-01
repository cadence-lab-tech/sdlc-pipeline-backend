package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CLAB-344, AC-10: ответ захвата с role.documents (решение 0068, п. 4)
// раннер разбирает как раньше — ему нужны role.key, model_hint и
// tool_policy, новое поле он не замечает.
func TestClaimWithRoleDocuments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stage_run_id": "run-1",
			"role": {"key": "developer", "name": "Разработчик", "version": 20, "model_hint": "opus",
				"instructions": "<!-- document: main.md -->\nГлавное.\n\n<!-- document: git.md -->\nВетки.\n",
				"documents": [
					{"name": "main.md", "kind": "instructions", "position": 0, "sha256": "aa", "size_bytes": 9, "content": "Главное."},
					{"name": "git.md", "kind": "reference", "position": 1, "sha256": "bb", "size_bytes": 11, "content": "Ветки.\n"}],
				"tool_policy": {"allow": ["Read"], "deny": ["WebFetch"]}},
			"context": {"repositories": [{"owner": "acme", "name": "api", "default_branch": "main"}]},
			"execution": {"model_level": "high", "model_level_source": "stage"}}`))
	}))
	t.Cleanup(srv.Close)

	c := newClient(Config{APIURL: srv.URL + "/v1", Token: "t"})
	got, err := c.claim(context.Background(), "CLAB-1", "runner")
	if err != nil {
		t.Fatalf("ответ с role.documents не разобран: %v", err)
	}
	if got.roleKey("") != "developer" || got.modelHint() != "opus" ||
		len(got.allowTools()) != 1 || len(got.Context.Repositories) != 1 ||
		got.Execution == nil || got.Execution.ModelLevel == nil || *got.Execution.ModelLevel != "high" {
		t.Fatalf("раннер прочитал не то: %+v", got)
	}
}
