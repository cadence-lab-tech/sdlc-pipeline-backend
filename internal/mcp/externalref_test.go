package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Ссылка на основной трекер через MCP (CLAB-95). Тулы зовутся настоящим
// клиентом SDK: аргументы проходят ту же схему, что у агента, — а схема
// выводится из Go-типов и для поля открытой формы легко оказывается не той.

type mcpCaller func(name string, args map[string]any) (map[string]any, bool)

// connectAs поднимает сессию к серверу от имени id в проекте SDLC.
func connectAs(t *testing.T, srv *Server, id workflow.Identity) mcpCaller {
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

	return func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := map[string]any{}
		text := ""
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				text += tc.Text
			}
		}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			out["raw"] = text
		}
		return out, res.IsError
	}
}

func errorMessage(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		return msg
	}
	if raw, ok := out["raw"].(string); ok {
		return raw
	}
	body, _ := json.Marshal(out)
	return string(body)
}

func TestExternalRefOverMCP(t *testing.T) {
	ctx := context.Background()
	store, _ := testdb.Store(t)
	log := testdb.Logger()
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := NewServer(log, store, engine, workflow.NewAuthoring(store, log, engine), auth, "test")

	if err := auth.Bootstrap(ctx, "root", "bootstrap-token-0123456789"); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, "bootstrap-token-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetMember(ctx, admin, "SDLC", "agent:bot", domain.MemberAgent, nil); err != nil {
		t.Fatal(err)
	}
	issued, err := auth.IssueToken(ctx, admin, workflow.IssueTokenInput{
		Name: "bot", Principal: "agent:bot", ProjectKey: "SDLC",
	})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := auth.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	asAdmin, asBot := connectAs(t, srv, admin), connectAs(t, srv, bot)

	jira := map[string]any{"jira": map[string]any{
		"key": "PROJ-12", "url": "https://jira.example.com/browse/PROJ-12"}}
	refOf := func(out map[string]any) string {
		raw, _ := json.Marshal(out["external_ref"])
		return string(raw)
	}
	wantJira, _ := json.Marshal(jira)

	created, isErr := asAdmin("sdlc_create_task", map[string]any{"title": "По тикету", "external_ref": jira})
	if isErr || refOf(created) != string(wantJira) {
		t.Fatalf("sdlc_create_task: %v", created)
	}
	key, _ := created["key"].(string)

	next := map[string]any{"youtrack": map[string]any{"key": "YT-7", "url": "https://yt.example.com/issue/YT-7"}}
	wantNext, _ := json.Marshal(next)
	updated, isErr := asAdmin("sdlc_update_task", map[string]any{"task_key": key, "external_ref": next})
	if isErr || refOf(updated) != string(wantNext) {
		t.Fatalf("sdlc_update_task замена: %v", updated)
	}
	// Без поля ссылка не меняется.
	updated, isErr = asAdmin("sdlc_update_task", map[string]any{"task_key": key, "title": "Новый заголовок"})
	if isErr || refOf(updated) != string(wantNext) {
		t.Fatalf("sdlc_update_task без поля: %v", updated)
	}

	// Поиск по ключу: в любой системе, без учёта регистра, с системой.
	listed := func(args map[string]any) []string {
		t.Helper()
		out, isErr := asAdmin("sdlc_list_tasks", args)
		if isErr {
			t.Fatalf("sdlc_list_tasks %v: %v", args, out)
		}
		keys := []string{}
		items, _ := out["tasks"].([]any)
		for _, item := range items {
			keys = append(keys, item.(map[string]any)["key"].(string))
		}
		return keys
	}
	for _, q := range []string{"YT-7", "yt-7", "youtrack:YT-7"} {
		if got := listed(map[string]any{"external": q}); len(got) != 1 || got[0] != key {
			t.Fatalf("external=%s: %v", q, got)
		}
	}
	if got := listed(map[string]any{"external": "jira:YT-7"}); len(got) != 0 {
		t.Fatalf("external=jira:YT-7: %v", got)
	}

	// Ошибки — тем же текстом, что у REST: правило одно, в движке.
	restErr := engineError(t, engine, key, `{"jira":{"key":"P-1","url":"ftp://jira.example.com/P-1"}}`)
	out, isErr := asAdmin("sdlc_update_task", map[string]any{"task_key": key,
		"external_ref": map[string]any{"jira": map[string]any{"key": "P-1", "url": "ftp://jira.example.com/P-1"}}})
	if !isErr || errorMessage(out) != restErr {
		t.Fatalf("текст ошибки MCP %q, у движка для REST %q", errorMessage(out), restErr)
	}
	out, isErr = asAdmin("sdlc_update_task", map[string]any{"task_key": key, "external_ref": "jira:P-1"})
	if !isErr || !strings.Contains(errorMessage(out), "объектом") {
		t.Fatalf("строка вместо объекта: %v", out)
	}

	// Агент без аренды ссылку не правит.
	out, isErr = asBot("sdlc_update_task", map[string]any{"task_key": key, "external_ref": jira})
	if !isErr || !strings.Contains(errorMessage(out), key) {
		t.Fatalf("агент без аренды поправил ссылку: %v", out)
	}

	// {} снимает ссылку.
	updated, isErr = asAdmin("sdlc_update_task", map[string]any{"task_key": key, "external_ref": map[string]any{}})
	if isErr || refOf(updated) != `{}` {
		t.Fatalf("sdlc_update_task {}: %v", updated)
	}
	if got := listed(map[string]any{"external": "YT-7"}); len(got) != 0 {
		t.Fatalf("снятая ссылка находится: %v", got)
	}
}

// engineError — текст, которым движок отбивает ссылку; его же отдаёт REST.
// Валидация идёт до задачи и актора, поэтому они здесь не нужны.
func engineError(t *testing.T, engine *workflow.Engine, key, ref string) string {
	t.Helper()
	raw := json.RawMessage(ref)
	_, err := engine.UpdateTask(context.Background(), key, uuid.Nil, workflow.UpdateTaskInput{ExternalRef: &raw})
	if err == nil {
		t.Fatalf("движок принял %s", ref)
	}
	return apperr.As(err).Message
}
