package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Уровень модели через MCP (CLAB-259, решение 0060): sdlc_put_role и
// sdlc_get_role несут model_level, описания тулов называют новые поля, а
// model_hint помечен устаревшим.
func TestModelLevelOverMCP(t *testing.T) {
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
	call := connectAs(t, srv, root)

	out, isErr := call("sdlc_put_role", map[string]any{
		"key": "writer", "name": "Техписатель", "instructions": "Пиши.",
		"model_level": "medium", "model_hint": "sonnet",
	})
	if isErr {
		t.Fatalf("sdlc_put_role с уровнем: %v", out)
	}
	latest, _ := out["latest_version"].(map[string]any)
	if latest["model_level"] != "medium" || latest["model_hint"] != "sonnet" {
		t.Fatalf("версия после sdlc_put_role: %v", latest)
	}
	got, _ := call("sdlc_get_role", map[string]any{"key": "writer"})
	if v, _ := got["latest_version"].(map[string]any); v["model_level"] != "medium" {
		t.Fatalf("sdlc_get_role: %v", got)
	}
	if out, isErr := call("sdlc_put_role", map[string]any{
		"key": "writer", "instructions": "Пиши иначе.", "model_level": "huge",
	}); !isErr || out["error"].(map[string]any)["code"] != "invalid_input" {
		t.Fatalf("неверный уровень sdlc_put_role: %v", out)
	}
	// Тот же уровень и инструкция — unchanged, без поля — новая версия без уровня.
	if out, _ := call("sdlc_put_role", map[string]any{
		"key": "writer", "name": "Техписатель", "instructions": "Пиши.",
		"model_level": "medium", "model_hint": "sonnet",
	}); out["outcome"].(map[string]any)["outcome"] != "unchanged" {
		t.Fatalf("повтор sdlc_put_role: %v", out)
	}
	if out, _ := call("sdlc_put_role", map[string]any{
		"key": "writer", "name": "Техписатель", "instructions": "Пиши.", "model_hint": "sonnet",
	}); out["outcome"].(map[string]any)["outcome"] != "new_version" {
		t.Fatalf("sdlc_put_role без уровня: %v", out)
	}

	// Описания тулов (AC-3, AC-14; учёт прогона в sdlc_get_task — CLAB-264, AC-7).
	serverT, clientT := sdk.NewInMemoryTransports()
	ss, err := srv.mcp.Connect(WithIdentity(WithProject(ctx, "SDLC"), root), serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"sdlc_claim_task":  {"execution", "model_level_source", "role.model_level", "model_hint устарел"},
		"sdlc_get_task":    {"model_level", "stage_settings", "execution", "usage", "reported: false"},
		"sdlc_create_task": {"model_level", "stage_settings"},
		"sdlc_update_task": {"model_level", "stage_settings", "сливается"},
		"sdlc_put_role":    {"model_level", "model_hint устарел"},
	}
	for _, tool := range tools.Tools {
		for _, phrase := range want[tool.Name] {
			if !strings.Contains(tool.Description, phrase) {
				t.Errorf("описание %s не называет %q", tool.Name, phrase)
			}
		}
		if tool.Name == "sdlc_put_role" {
			schema, _ := tool.InputSchema.(map[string]any)
			props, _ := schema["properties"].(map[string]any)
			hint, _ := props["model_hint"].(map[string]any)
			if desc, _ := hint["description"].(string); !strings.Contains(desc, "устарел") {
				t.Errorf("поле model_hint в схеме sdlc_put_role не помечено устаревшим: %v", hint)
			}
		}
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		t.Errorf("тулов нет: %v", want)
	}
}
