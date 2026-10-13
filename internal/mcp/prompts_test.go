package mcp

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// prompts/get без обязательного аргумента — ошибка, а не промпт с пустым
// ключом задачи (CLAB-104); с аргументом промпт называет задачу.
func TestWorkPromptRequiresTask(t *testing.T) {
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
	defer func() { _ = cs.Close(); _ = ss.Wait() }()

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
