package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Исполнитель этапа через MCP (CLAB-208, решение 0049 п. 3): белый список в
// обёртке тулов и промптов, область, круг записи. Те же правила, что у
// REST, — их держит workflow.Auth.
func TestExecutorOverMCP(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := NewServer(log, store, engine, workflow.NewAuthoring(store, log, engine), auth, "test")

	if err := auth.Bootstrap(ctx, "root", "bootstrap-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	root, err := auth.Authenticate(ctx, "bootstrap-token-0123456789")
	if err != nil {
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
	if _, err := engine.Execute(ctx, "SDLC-1", "start", root.Principal.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ClaimWith(ctx, "SDLC-1", pool.Principal.ID, "pool", workflow.ClaimOptions{}); err != nil {
		t.Fatal(err)
	}
	// SDLC-2 связана с SDLC-1: читается (как и любая задача проекта), но не
	// в круге записи.
	if _, err := engine.LinkTasks(ctx, "SDLC-1", "SDLC-2", "relates_to", "", root.Principal.ID, false); err != nil {
		t.Fatal(err)
	}
	taskTok, err := auth.IssueTaskToken(ctx, pool, "SDLC-1")
	if err != nil {
		t.Fatal(err)
	}
	tid, err := auth.Authenticate(ctx, taskTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	// Тот же токен у администратора: is_admin перечень не снимает.
	adminTask := tid
	adminTask.Principal.IsAdmin = true

	cs := clientAs(t, srv, tid)
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Критерий 7: каждая строка перечня вне REST — существующий тул или
	// промпт; каждый тул вне перечня отвечает отказом.
	registered := map[string]bool{}
	for _, tl := range tools.Tools {
		registered[tl.Name] = true
	}
	for _, p := range prompts.Prompts {
		registered[workflow.PromptAction(p.Name)] = true
	}
	allowed := map[string]bool{}
	for _, a := range workflow.ExecutorActions() {
		if strings.Contains(a, " /v1/") {
			continue
		}
		allowed[a] = true
		if !registered[a] {
			t.Errorf("в перечне тул или промпт, которого нет: %s", a)
		}
	}
	closed := 0
	for _, id := range []workflow.Identity{tid, adminTask} {
		call := connectAs(t, srv, id)
		for _, tl := range tools.Tools {
			if allowed[tl.Name] {
				continue
			}
			args, _ := sampleArgs(tl.InputSchema).(map[string]any)
			out, isErr := call(tl.Name, args)
			if !isErr || !strings.HasPrefix(errorMessage(out), workflow.ExecutorRefusal) {
				t.Errorf("%s токеном задачи: %v", tl.Name, out)
			}
			closed++
		}
	}
	if closed < 14 {
		t.Fatalf("закрытых тулов подозрительно мало: %d", closed)
	}
	if _, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "import_rules"}); err == nil ||
		!strings.Contains(err.Error(), workflow.ExecutorRefusal) {
		t.Errorf("import_rules токеном задачи: %v", err)
	}
	if _, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "work_on_task",
		Arguments: map[string]string{"task": "SDLC-1"}}); err != nil {
		t.Errorf("work_on_task токеном задачи: %v", err)
	}

	call := connectAs(t, srv, tid)
	// Критерий 10: область не меняется.
	if out, isErr := call("sdlc_update_task_scope", map[string]any{
		"task_key": "SDLC-1", "add": []string{"cadence-lab-tech/sdlc-pipeline-frontend"}, "reason": "нужно",
	}); !isErr || !strings.HasPrefix(errorMessage(out), workflow.ExecutorRefusal) {
		t.Errorf("область токеном задачи: %v", out)
	}
	if task, _ := store.GetTaskByKey(ctx, store.DB(), "SDLC-1"); len(task.Repositories) != 1 {
		t.Errorf("область изменилась: %+v", task.Repositories)
	}

	// Критерии 15–17: круг записи — своя задача; связанная — только читается.
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"sdlc_update_task", map[string]any{"task_key": "SDLC-1", "priority": "high"}},
		{"sdlc_assign_task", map[string]any{"task_key": "SDLC-1", "assignee": "agent:pool"}},
		{"sdlc_link_tasks", map[string]any{"from": "SDLC-1", "to": "SDLC-2", "rel": "blocks"}},
	} {
		if out, isErr := call(c.tool, c.args); isErr {
			t.Errorf("%s %v: %s", c.tool, c.args, errorMessage(out))
		}
	}
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"sdlc_update_task", map[string]any{"task_key": "SDLC-2", "priority": "high"}},
		{"sdlc_assign_task", map[string]any{"task_key": "SDLC-2", "assignee": "agent:pool"}},
		{"sdlc_link_tasks", map[string]any{"from": "SDLC-2", "to": "SDLC-1", "rel": "blocks"}},
		{"sdlc_request_transition", map[string]any{"task_key": "SDLC-1", "transition": "cancel"}},
	} {
		if out, isErr := call(c.tool, c.args); !isErr || !strings.HasPrefix(errorMessage(out), workflow.ExecutorRefusal) {
			t.Errorf("%s %v токеном задачи: %v", c.tool, c.args, out)
		}
	}

	// Личный токен того же принципала — перечня нет.
	personal := connectAs(t, srv, pool)
	if out, isErr := personal("sdlc_update_task", map[string]any{"task_key": "SDLC-2", "priority": "low"}); isErr &&
		strings.HasPrefix(errorMessage(out), workflow.ExecutorRefusal) {
		t.Errorf("личный токен попал под перечень: %v", out)
	}
}

// sampleArgs — аргументы, проходящие схему тула: обязательные поля по типу.
// До тула вызов токена задачи вне перечня не доходит, значения не важны.
func sampleArgs(schema any) any {
	raw, _ := json.Marshal(schema)
	var s map[string]any
	_ = json.Unmarshal(raw, &s)
	typ, _ := s["type"].(string)
	if list, ok := s["type"].([]any); ok && len(list) > 0 {
		typ, _ = list[0].(string)
	}
	switch typ {
	case "object":
		out := map[string]any{}
		props, _ := s["properties"].(map[string]any)
		required, _ := s["required"].([]any)
		for _, r := range required {
			name, _ := r.(string)
			out[name] = sampleArgs(props[name])
		}
		return out
	case "array":
		return []any{sampleArgs(s["items"])}
	case "boolean":
		return false
	case "integer", "number":
		return 1
	case "null":
		return nil
	}
	return "x"
}

// clientAs — клиент MCP с действующим лицом, для списков тулов и промптов.
func clientAs(t *testing.T, srv *Server, id workflow.Identity) *sdk.ClientSession {
	t.Helper()
	ctx := WithIdentity(WithProject(context.Background(), "SDLC"), id)
	serverT, clientT := sdk.NewInMemoryTransports()
	ss, err := srv.mcp.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
