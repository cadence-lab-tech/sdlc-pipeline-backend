package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Предложения версий ролей по MCP (CLAB-381, решение 0072 п. 5): те же
// права и исходы, что у REST, — одна функция Auth. Номера — критерии
// приёмки анализа CLAB-381.
func TestRoleProposalsOverMCP(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := NewServer(log, store, engine, workflow.NewAuthoring(store, log, engine), auth, "test")
	const rootToken = "bootstrap-token-clab-381-0123456789"
	if err := auth.Bootstrap(ctx, "root", rootToken); err != nil {
		t.Fatal(err)
	}
	root, err := auth.Authenticate(ctx, rootToken)
	if err != nil {
		t.Fatal(err)
	}
	member := func(raw string, level domain.MemberRole, roles []string) workflow.Identity {
		t.Helper()
		kind, handle := workflow.SplitHandle(raw, domain.PrincipalUser)
		if kind == domain.PrincipalUser {
			if _, err := store.ResolveActor(ctx, store.DB(), handle, kind); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := auth.SetMember(ctx, root, "SDLC", raw, level, roles); err != nil {
			t.Fatal(err)
		}
		issued, err := auth.IssueToken(ctx, root, workflow.IssueTokenInput{
			Name: "personal", Principal: raw, ProjectKey: "SDLC", MemberRole: level,
		})
		if err != nil {
			t.Fatal(err)
		}
		id, err := auth.Authenticate(ctx, issued.Token)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	dev := connectAs(t, srv, member("user:dev", domain.MemberAgent, []string{"analyst"}))
	viewer := connectAs(t, srv, member("user:viewer", domain.MemberViewer, nil))
	maint := connectAs(t, srv, member("user:maint", domain.MemberMaintainer, nil))
	errCode := func(out map[string]any) string {
		e, _ := out["error"].(map[string]any)
		c, _ := e["code"].(string)
		return c
	}
	proposal := func(out map[string]any) map[string]any {
		p, _ := out["proposal"].(map[string]any)
		return p
	}

	// AC-2 через MCP — тот же исход, что у движка и REST.
	for _, c := range []struct {
		call mcpCaller
		role string
		want string
	}{
		{dev, "developer", "forbidden"},
		{viewer, "analyst", "forbidden"},
		{dev, "no-such", "not_found"},
	} {
		out, isErr := c.call("sdlc_propose_role_version", map[string]any{"key": c.role, "instructions": "x", "changelog": "y"})
		if !isErr || errCode(out) != c.want {
			t.Errorf("%s: %v, ждали %s", c.role, out, c.want)
		}
	}
	out, isErr := dev("sdlc_propose_role_version", map[string]any{
		"key": "analyst", "changelog": "две части",
		"documents": []map[string]any{{"name": "main.md", "content": "Ты аналитик.\n"}, {"name": "check.md", "kind": "checklist", "content": "- [ ] x\n"}},
	})
	p := proposal(out)
	if isErr || p["state"] != "pending" {
		t.Fatalf("подача: %v", out)
	}
	id := p["id"].(string)
	out, isErr = viewer("sdlc_list_role_proposals", map[string]any{"key": "analyst", "state": "pending"})
	if list, _ := out["proposals"].([]any); isErr || len(list) != 1 {
		t.Fatalf("список: %v", out)
	}
	out, isErr = viewer("sdlc_list_role_proposals", map[string]any{"key": "analyst", "id": id})
	if docs, _ := proposal(out)["documents"].([]any); isErr || len(docs) != 2 {
		t.Fatalf("чтение: %v", out)
	}

	// AC-5: sdlc_put_role у agent — отказ с подсказкой.
	out, isErr = dev("sdlc_put_role", map[string]any{"key": "analyst", "instructions": "x"})
	if !isErr || errCode(out) != "forbidden" || !strings.Contains(errorMessage(out), "clab role propose") {
		t.Fatalf("put_role агентом: %v", out)
	}

	// AC-8: утверждение — maintainer, автор-agent — 403; отклонение без заметки — 400.
	if out, isErr := dev("sdlc_approve_role_proposal", map[string]any{"key": "analyst", "id": id}); !isErr || errCode(out) != "forbidden" {
		t.Fatalf("утверждает агент: %v", out)
	}
	if out, isErr := maint("sdlc_reject_role_proposal", map[string]any{"key": "analyst", "id": id}); !isErr || errCode(out) != "invalid_input" {
		t.Fatalf("отклонение без заметки: %v", out)
	}
	out, isErr = maint("sdlc_approve_role_proposal", map[string]any{"key": "analyst", "id": id})
	if isErr || proposal(out)["state"] != "approved" || proposal(out)["result_version"] == nil {
		t.Fatalf("утверждение: %v", out)
	}
	if out, isErr := dev("sdlc_withdraw_role_proposal", map[string]any{"key": "analyst", "id": id}); !isErr || errCode(out) != "conflict" {
		t.Fatalf("отзыв утверждённого: %v", out)
	}
}
