package mcp

import (
	"context"
	"log/slog"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Клиент видит в initialize версию сборки, а не строку, зашитую в пакет
// (CLAB-87): одна и та же версия у REST, MCP и логов.
func TestInitializeReportsBuildVersion(t *testing.T) {
	cases := map[string]string{"v0.5.0": "v0.5.0", "": "dev"}
	for build, want := range cases {
		srv := NewServer(slog.New(slog.DiscardHandler), nil, nil, nil, nil, build)
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
		got := cs.InitializeResult().ServerInfo.Version
		_ = cs.Close()
		_ = ss.Wait()
		if got != want {
			t.Fatalf("сборка %q: serverInfo.version = %q, ждали %q", build, got, want)
		}
	}
}
