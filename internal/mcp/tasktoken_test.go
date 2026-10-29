package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Токен задачи через MCP (CLAB-212, решение 0049; чтение — CLAB-305, решение
// 0065): те же правила, что у REST, — своя задача и свой прогон на запись,
// весь проект на чтение, граница проекта, поиск, whoami с привязкой.
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
	zArt := testdb.Artifact(t, store, z, "секрет Z")
	foreign, foreignArt := testdb.ForeignTask(t, store)
	testdb.SearchCorpus(t, store, "quasar", "SDLC-1", "SDLC-3")
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
		// Чтение чужих задач проекта (CLAB-305, AC-1, AC-2).
		{"sdlc_get_task", map[string]any{"task_key": z.Key}},
		{"sdlc_get_task", map[string]any{"task_key": "SDLC-3"}},
		{"sdlc_get_drift", map[string]any{"task_key": z.Key}},
		{"sdlc_check_gates", map[string]any{"task_key": "SDLC-3"}},
		{"sdlc_get_artifact", map[string]any{"artifact_id": zArt.ID.String()}},
		// Связь своей задачи с любой задачей проекта (AC-6).
		{"sdlc_link_tasks", map[string]any{"from": "SDLC-1", "to": z.Key, "rel": "relates_to"}},
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
		// Запись в задачу, которую токен читает (AC-5, AC-6).
		{"sdlc_update_task", map[string]any{"task_key": z.Key, "priority": "high"}},
		{"sdlc_assign_task", map[string]any{"task_key": z.Key, "assignee": "agent:pool"}},
		{"sdlc_submit_artifact", map[string]any{"task_key": z.Key, "kind": "analysis.brief", "title": "b", "content": "b"}},
		{"sdlc_link_repo_ref", map[string]any{"task_key": z.Key, "repository": "cadence-lab-tech/sdlc-pipeline-backend", "ref": "13"}},
		{"sdlc_create_task", map[string]any{"title": "x", "parent": z.Key}},
		{"sdlc_link_tasks", map[string]any{"from": z.Key, "to": "SDLC-1", "rel": "blocks"}},
	} {
		out, isErr := call(c.tool, c.args)
		if !isErr || out["error"].(map[string]any)["code"] != "forbidden" {
			t.Errorf("%s %v токеном задачи SDLC-1: %v", c.tool, c.args, out)
		}
	}
	if arts, _ := store.ListArtifacts(ctx, store.DB(), z.ID, true); len(arts) != 1 {
		t.Errorf("у Z после отказов %d артефактов", len(arts))
	}
	if zt, _ := store.GetTaskByKey(ctx, store.DB(), z.Key); zt.Priority != "normal" || zt.AssigneeID != nil {
		t.Errorf("Z тронута отказами: %+v", zt)
	}
	// Содержимое артефакта чужой задачи проекта — как у своей.
	if out, _ := call("sdlc_get_artifact", map[string]any{"artifact_id": zArt.ID.String()}); !strings.Contains(mustText(out), "секрет Z") {
		t.Errorf("содержимое артефакта Z: %v", out)
	}

	// Граница проекта (AC-7): задача и артефакт другого проекта — not_found.
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"sdlc_get_task", map[string]any{"task_key": foreign.Key}},
		{"sdlc_get_drift", map[string]any{"task_key": foreign.Key}},
		{"sdlc_get_artifact", map[string]any{"artifact_id": foreignArt.ID.String()}},
		{"sdlc_link_tasks", map[string]any{"from": "SDLC-1", "to": foreign.Key, "rel": "relates_to"}},
	} {
		out, isErr := call(c.tool, c.args)
		if code := out["error"].(map[string]any)["code"]; !isErr || (code != "not_found" && code != "forbidden") {
			t.Errorf("%s %v другого проекта: %v", c.tool, c.args, out)
		}
		if strings.Contains(mustText(out), "OTHER-1") && strings.Contains(mustText(out), "описание") {
			t.Errorf("%s отдал данные другого проекта: %v", c.tool, out)
		}
	}

	// Поиск (AC-4): та же выдача, что личному токену, фильтр вне области
	// сужает, а не отбрасывается.
	personal := connectAs(t, srv, pool)
	hits := func(call mcpCaller, repos ...string) string {
		t.Helper()
		out, isErr := call("sdlc_search_context", map[string]any{"query": "quasar", "repositories": repos, "top_k": 50})
		if isErr {
			t.Fatalf("поиск: %s", errorMessage(out))
		}
		var got []string
		for _, h := range out["results"].([]any) {
			m := h.(map[string]any)
			got = append(got, fmt.Sprint(m["scope"], " ", m["repository"], m["task"]))
		}
		sort.Strings(got)
		return strings.Join(got, "; ")
	}
	if mine, theirs := hits(call), hits(personal); mine != theirs || strings.Count(mine, "code") != 3 ||
		strings.Count(mine, "artifacts") != 2 {
		t.Errorf("поиск токеном задачи: %s; личным: %s", mine, theirs)
	}
	if got := hits(call, "cadence-lab-tech/sdlc-pipeline-frontend"); !strings.Contains(got, "code cadence-lab-tech/sdlc-pipeline-frontend") ||
		strings.Contains(got, "sdlc-pipeline-backend") {
		t.Errorf("поиск по репозиторию вне области: %s", got)
	}

	// Список — одна компактная форма для всех (AC-3: без изменений), whoami
	// — с привязкой (критерий 14).
	out, _ := call("sdlc_list_tasks", map[string]any{})
	theirs, _ := personal("sdlc_list_tasks", map[string]any{})
	if len(out["tasks"].([]any)) == 0 || mustText(out) != mustText(theirs) {
		t.Fatalf("список токену задачи не тот, что личному: %v", out)
	}
	who, _ := call("sdlc_whoami", map[string]any{})
	tokView, _ := who["token"].(map[string]any)
	if tokView["token_kind"] != domain.TokenTask || tokView["task_key"] != "SDLC-1" {
		t.Fatalf("whoami токеном задачи: %v", who)
	}

	// Личный токен того же принципала — как раньше.
	if out, isErr := personal("sdlc_get_task", map[string]any{"task_key": z.Key}); isErr {
		t.Fatalf("личный токен не читает Z: %s", errorMessage(out))
	}
	if who, _ := personal("sdlc_whoami", map[string]any{}); who["token"] != nil {
		t.Fatalf("личный токен показан как токен задачи: %v", who["token"])
	}
}

func mustText(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
