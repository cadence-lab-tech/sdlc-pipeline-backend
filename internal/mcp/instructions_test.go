package mcp

import (
	"context"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// CLAB-351, AC-13: тулы библиотеки с правами REST, sdlc_put_role принимает
// закрепление, sdlc_get_role его отдаёт.
func TestMCPInstructionTools(t *testing.T) {
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
	if _, err := auth.SetMember(ctx, root, "SDLC", "agent:bot", domain.MemberAgent, nil); err != nil {
		t.Fatal(err)
	}
	issued, err := auth.IssueToken(ctx, root, workflow.IssueTokenInput{Name: "bot", Principal: "agent:bot", ProjectKey: "SDLC"})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := auth.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	admin, agent := connectAs(t, srv, root), connectAs(t, srv, bot)

	put := map[string]any{"key": "git", "name": "Правила git", "content": "Ветки clab/<ключ>.\n"}
	if out, isErr := agent("sdlc_put_instruction", put); !isErr {
		t.Fatalf("agent завёл инструкцию: %v", out)
	}
	out, isErr := admin("sdlc_put_instruction", put)
	if isErr {
		t.Fatalf("maintainer: %s", errorMessage(out))
	}
	if v := out["version"].(map[string]any); v["version"] != float64(1) || out["created"] != true {
		t.Fatalf("put: %v", out)
	}
	if out, isErr := admin("sdlc_put_instruction", put); !isErr {
		t.Fatalf("тот же текст — новая версия: %v", out)
	}
	put["content"] = "Ветки clab/<ключ>-<slug>.\n"
	if out, isErr := admin("sdlc_put_instruction", put); isErr || out["version"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("версия 2: %v", out)
	}

	if out, isErr := agent("sdlc_list_instructions", map[string]any{}); isErr || len(out["instructions"].([]any)) != 1 {
		t.Fatalf("agent читает список: %v", out)
	}
	if out, isErr := agent("sdlc_get_instruction", map[string]any{"key": "git", "version": 1}); isErr ||
		out["version"].(map[string]any)["content"] != "Ветки clab/<ключ>.\n" {
		t.Fatalf("agent читает версию 1: %v", out)
	}

	role := map[string]any{"key": "builder", "name": "Сборщик", "documents": []any{
		map[string]any{"name": "main.md", "content": "Ты сборщик."},
		map[string]any{"instruction": "git", "instruction_version": 1},
	}}
	if out, isErr := admin("sdlc_put_role", role); isErr {
		t.Fatalf("put_role с закреплением: %s", errorMessage(out))
	}
	out, isErr = agent("sdlc_get_role", map[string]any{"key": "builder"})
	if isErr {
		t.Fatalf("get_role: %s", errorMessage(out))
	}
	docs := out["latest_version"].(map[string]any)["documents"].([]any)
	pin, _ := docs[1].(map[string]any)["instruction"].(map[string]any)
	if docs[1].(map[string]any)["name"] != "git.md" || pin["key"] != "git" || pin["version"] != float64(1) {
		t.Fatalf("закрепление в get_role: %v", docs[1])
	}
	if lag, _ := out["role"].(map[string]any)["instruction_lag"].([]any); len(lag) != 1 {
		t.Fatalf("отставание в get_role: %v", out["role"])
	}

	apply := map[string]any{"key": "git", "roles": []any{map[string]any{"key": "builder"}}}
	if out, isErr := agent("sdlc_apply_instruction", apply); !isErr {
		t.Fatalf("agent обновил роли: %v", out)
	}
	out, isErr = admin("sdlc_apply_instruction", apply)
	if isErr {
		t.Fatalf("apply: %s", errorMessage(out))
	}
	if r := out["roles"].([]any)[0].(map[string]any); r["outcome"] != "created" || r["role_version"] != float64(2) {
		t.Fatalf("итог apply: %v", out)
	}
}
