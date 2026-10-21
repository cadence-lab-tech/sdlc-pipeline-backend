package mcp

import (
	"context"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Токен задачи через MCP (CLAB-212, решение 0049): те же правила, что у
// REST, — своя задача и свой прогон на запись, круг задачи на чтение,
// сводка списка, whoami с привязкой.
func TestTaskTokenOverMCP(t *testing.T) {
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
	for _, key := range []string{"SDLC-1", "SDLC-3"} {
		if _, err := engine.Execute(ctx, key, "start", root.Principal.ID, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.ClaimWith(ctx, key, pool.Principal.ID, "pool", workflow.ClaimOptions{}); err != nil {
			t.Fatal(err)
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
	z, err := engine.CreateTask(ctx, "SDLC", root.Principal.ID, workflow.CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := connectAs(t, srv, tid)

	// Своя задача (критерий 8).
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"sdlc_get_task", map[string]any{"task_key": "SDLC-1"}},
		{"sdlc_claim_task", map[string]any{"task_key": "SDLC-1"}},
		{"sdlc_heartbeat", map[string]any{"task_key": "SDLC-1"}},
		{"sdlc_submit_artifact", map[string]any{"task_key": "SDLC-1", "kind": "analysis.brief", "title": "b", "content": "b"}},
		{"sdlc_link_repo_ref", map[string]any{"task_key": "SDLC-1", "repository": "cadence-lab-tech/sdlc-pipeline-backend", "ref": "12"}},
		{"sdlc_check_gates", map[string]any{"task_key": "SDLC-1"}},
		{"sdlc_report_usage", map[string]any{"task_key": "SDLC-1", "model": "m"}},
		{"sdlc_create_task", map[string]any{"title": "D", "parent": "SDLC-1"}},
		{"sdlc_get_task", map[string]any{"task_key": "SDLC-4"}},
		{"sdlc_get_drift", map[string]any{"task_key": "SDLC-2"}},
	} {
		if out, isErr := call(c.tool, c.args); isErr {
			t.Errorf("%s %v: %s", c.tool, c.args, errorMessage(out))
		}
	}
	// Подтверждение аренды — тот же прогон.
	if out, _ := call("sdlc_claim_task", map[string]any{"task_key": "SDLC-1"}); out["stage_run_id"] != taskTok.StageRunID.String() {
		t.Errorf("claim токеном задачи: %v", out["stage_run_id"])
	}

	// Чужая задача пула и задача вне круга (критерии 9, 11).
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"sdlc_claim_task", map[string]any{"task_key": "SDLC-3"}},
		{"sdlc_heartbeat", map[string]any{"task_key": "SDLC-3"}},
		{"sdlc_release", map[string]any{"task_key": "SDLC-3"}},
		{"sdlc_submit_artifact", map[string]any{"task_key": "SDLC-3", "kind": "analysis.brief", "title": "b", "content": "b"}},
		{"sdlc_request_transition", map[string]any{"task_key": "SDLC-3", "transition": "analysis_done"}},
		{"sdlc_report_usage", map[string]any{"task_key": "SDLC-3", "model": "m"}},
		{"sdlc_create_task", map[string]any{"title": "x", "parent": "SDLC-3"}},
		{"sdlc_create_task", map[string]any{"title": "x"}},
		{"sdlc_get_task", map[string]any{"task_key": z.Key}},
		{"sdlc_get_task", map[string]any{"task_key": "SDLC-3"}},
	} {
		out, isErr := call(c.tool, c.args)
		if !isErr || out["error"].(map[string]any)["code"] != "forbidden" {
			t.Errorf("%s %v токеном задачи SDLC-1: %v", c.tool, c.args, out)
		}
	}

	// Список — без описаний (критерий 12), whoami — с привязкой (14).
	out, _ := call("sdlc_list_tasks", map[string]any{})
	for _, it := range out["tasks"].([]any) {
		if _, has := it.(map[string]any)["description"]; has {
			t.Fatalf("в списке описание: %v", it)
		}
	}
	who, _ := call("sdlc_whoami", map[string]any{})
	tokView, _ := who["token"].(map[string]any)
	if tokView["token_kind"] != domain.TokenTask || tokView["task_key"] != "SDLC-1" {
		t.Fatalf("whoami токеном задачи: %v", who)
	}

	// Личный токен того же принципала — как раньше.
	personal := connectAs(t, srv, pool)
	if out, isErr := personal("sdlc_get_task", map[string]any{"task_key": z.Key}); isErr {
		t.Fatalf("личный токен не читает Z: %s", errorMessage(out))
	}
	if who, _ := personal("sdlc_whoami", map[string]any{}); who["token"] != nil {
		t.Fatalf("личный токен показан как токен задачи: %v", who["token"])
	}
}
