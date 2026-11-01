package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// CLAB-344, AC-3: в MCP роль приходит документами без склейки — ключа
// role.instructions нет; прочие ключи role, execution и boundary те же,
// что в REST, и захват токеном задачи работает.

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func restJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMCPRoleWithoutInstructions(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	authoring := workflow.NewAuthoring(store, log, engine)
	srv := NewServer(log, store, engine, authoring, auth, "test")
	if err := auth.Bootstrap(ctx, "root", "bootstrap-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	root, err := auth.Authenticate(ctx, "bootstrap-token-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	docs := []workflow.RoleDocumentInput{
		{Name: "main.md", Content: "Ты аналитик.\n"},
		{Name: "git.md", Kind: "reference", Content: "Ветки clab/<ключ>."},
		{Name: "selfhosted.md", Kind: "checklist", Content: "- пакет учтён\n"},
	}
	if _, err := authoring.AddRoleVersion(ctx, "SDLC", "analyst", root.Principal.ID,
		workflow.RoleVersionInput{Documents: docs}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(ctx, "SDLC-1", "start", root.Principal.ID, ""); err != nil {
		t.Fatal(err)
	}
	issued, err := auth.IssueToken(ctx, root, workflow.IssueTokenInput{
		Name: "runner", Principal: "agent:pool", ProjectKey: "SDLC",
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := auth.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := engine.ClaimWith(ctx, "SDLC-1", pool.Principal.ID, "pool", workflow.ClaimOptions{})
	if err != nil {
		t.Fatal(err)
	}
	restDoc := restJSON(t, rest)
	restRole := restDoc["role"].(map[string]any)
	if _, ok := restRole["instructions"]; !ok {
		t.Fatal("REST-ответ захвата потерял role.instructions")
	}
	wantRoleKeys := []string{}
	for _, k := range sortedKeys(restRole) {
		if k != "instructions" {
			wantRoleKeys = append(wantRoleKeys, k)
		}
	}

	taskTok, err := auth.IssueTaskToken(ctx, pool, "SDLC-1")
	if err != nil {
		t.Fatal(err)
	}
	tid, err := auth.Authenticate(ctx, taskTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	for who, call := range map[string]mcpCaller{"pool": connectAs(t, srv, pool), "task": connectAs(t, srv, tid)} {
		for _, tool := range []string{"sdlc_claim_task", "sdlc_get_task"} {
			out, isErr := call(tool, map[string]any{"task_key": "SDLC-1"})
			if isErr {
				t.Fatalf("%s %s: %s", who, tool, errorMessage(out))
			}
			role, ok := out["role"].(map[string]any)
			if !ok {
				t.Fatalf("%s %s: нет role: %v", who, tool, out)
			}
			if _, has := role["instructions"]; has {
				t.Errorf("%s %s: в role есть ключ instructions", who, tool)
			}
			if got := sortedKeys(role); !reflect.DeepEqual(got, wantRoleKeys) {
				t.Errorf("%s %s: ключи role %v, ждали %v", who, tool, got, wantRoleKeys)
			}
			list, _ := role["documents"].([]any)
			if len(list) != len(docs) {
				t.Fatalf("%s %s: документов %d", who, tool, len(list))
			}
			for i, d := range list {
				doc := d.(map[string]any)
				if doc["name"] != docs[i].Name || doc["content"] != docs[i].Content || doc["position"] != float64(i) {
					t.Errorf("%s %s: документ %d %v", who, tool, i, doc)
				}
				if got := sortedKeys(doc); !reflect.DeepEqual(got, []string{"content", "kind", "name", "position", "sha256", "size_bytes"}) {
					t.Errorf("%s %s: поля документа %v", who, tool, got)
				}
			}
			if !reflect.DeepEqual(out["execution"], restDoc["execution"]) {
				t.Errorf("%s %s: execution %v, в REST %v", who, tool, out["execution"], restDoc["execution"])
			}
			b, _ := out["boundary"].(map[string]any)
			if !reflect.DeepEqual(b, restDoc["boundary"]) {
				t.Errorf("%s %s: boundary расходится с REST", who, tool)
			}
			if rules, _ := b["rules"].([]any); len(rules) == 0 || rules[0] != "role" {
				t.Errorf("%s %s: boundary.rules без role: %v", who, tool, b["rules"])
			}
			if tool == "sdlc_claim_task" {
				for _, k := range sortedKeys(restDoc) {
					if _, ok := out[k]; !ok {
						t.Errorf("%s: в MCP-захвате нет ключа %q", who, k)
					}
				}
			}
		}
	}
}

// Описания тулов и промпт work_on_task говорят о документах роли.
func TestToolTextsSpeakOfRoleDocuments(t *testing.T) {
	cs := connectPrompts(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool.Description
	}
	for _, name := range []string{"sdlc_claim_task", "sdlc_get_task"} {
		desc := byName[name]
		if !strings.Contains(desc, "role.documents") {
			t.Errorf("%s: описание не говорит о role.documents: %s", name, desc)
		}
		if strings.Contains(desc, "инструкция роли, результаты") {
			t.Errorf("%s: описание обещает инструкцию роли одной строкой", name)
		}
	}
	prompt := workPromptText("SDLC-1", "", false)
	if !strings.Contains(prompt, "role.documents") || strings.Contains(prompt, "приедут инструкция") {
		t.Errorf("промпт work_on_task: %s", prompt)
	}
}
