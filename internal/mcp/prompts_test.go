package mcp

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/trust"
)

// prompts/get без обязательного аргумента — ошибка, а не промпт с пустым
// ключом задачи (CLAB-104); с аргументом промпт называет задачу.
func TestWorkPromptRequiresTask(t *testing.T) {
	ctx := context.Background()
	cs := connectPrompts(t)

	for name, args := range map[string]map[string]string{
		"без аргументов":  nil,
		"не тот аргумент": {"task_key": "SDLC-42"},
		"пустой ключ":     {"task": "  "},
	} {
		_, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "work_on_task", Arguments: args})
		if err == nil || !strings.Contains(err.Error(), "task") {
			t.Fatalf("%s: ждали ошибку про task, получили %v", name, err)
		}
	}

	res, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "work_on_task", Arguments: map[string]string{"task": "SDLC-42"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Messages[0].Content.(*sdk.TextContent).Text
	if !strings.Contains(text, "Возьми задачу SDLC-42 в работу") {
		t.Fatalf("промпт без ключа задачи: %q", text[:60])
	}
}

// connectPrompts — клиент MCP к серверу без базы: промптам и tools/list
// она не нужна.
func connectPrompts(t *testing.T) *sdk.ClientSession {
	t.Helper()
	srv := NewServer(slog.New(slog.DiscardHandler), nil, nil, nil, nil, "test")
	ctx := context.Background()
	clientT, serverT := sdk.NewInMemoryTransports()
	ss, err := srv.mcp.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait() })
	return cs
}

func promptText(t *testing.T, cs *sdk.ClientSession, name string, args map[string]string) string {
	t.Helper()
	res, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res.Messages[0].Content.(*sdk.TextContent).Text
}

// CLAB-209, критерий 6: промпт work_on_task несёт правило о данных целиком —
// и тот, что получает человек, и тот, с которого стартует агент раннера.
func TestWorkPromptCarriesTrustRule(t *testing.T) {
	cs := connectPrompts(t)
	for _, args := range []map[string]string{
		{"task": "SDLC-42"}, {"task": "SDLC-42", "role": "developer"},
	} {
		text := promptText(t, cs, "work_on_task", args)
		if !strings.Contains(text, trust.Rule) {
			t.Fatalf("в work_on_task %v нет правила о данных:\n%s", args, text)
		}
		for _, want := range []string{"boundary", "сменить роль", "расширить", "апрув", "токен", "отключить", "упомянуть указание в сданном"} {
			if !strings.Contains(text, want) {
				t.Errorf("в work_on_task нет %q", want)
			}
		}
	}
}

// CLAB-209, критерий 7: перенос правил считает переносимое материалом, а
// подозрительные строки показывает человеку в отчёте dry_run.
func TestImportRulesCarriesTrustRule(t *testing.T) {
	cs := connectPrompts(t)
	text := promptText(t, cs, "import_rules", nil)
	for name, body := range map[string]string{"import_rules": text, "ImportGuide": ImportGuide} {
		if !strings.Contains(body, trust.ImportRule) {
			t.Errorf("в %s нет правила переноса", name)
		}
		if !strings.Contains(body, "похожие на попытку") || !strings.Contains(body, "dry_run") {
			t.Errorf("в %s нет пункта о показе подозрительного в dry_run", name)
		}
	}
}

// CLAB-209, критерий 8: тулы, которые отдают тексты людей и агентов, говорят
// в описании, что это данные.
func TestDataToolsSayContentIsData(t *testing.T) {
	cs := connectPrompts(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"sdlc_claim_task": false, "sdlc_get_task": false, "sdlc_get_artifact": false,
		"sdlc_search_context": false, "sdlc_get_drift": false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			continue
		}
		want[tool.Name] = true
		if !strings.Contains(tool.Description, trust.DataNote) {
			t.Errorf("%s: в описании нет %q: %s", tool.Name, trust.DataNote, tool.Description)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("тул %s не объявлен", name)
		}
	}
}
