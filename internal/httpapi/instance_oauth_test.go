package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/oauth"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Инстансовый адрес и OAuth (CLAB-483): 401 с resource_metadata, метаданные
// ресурса /mcp и полный цикл выдачи по resource=<AS>/mcp. Номера — критерии
// приёмки анализа CLAB-483.

const instanceChallenge = `Bearer resource_metadata="` + oauthIssuer + `/.well-known/oauth-protected-resource/mcp"`

// AC-1: 401 на /mcp — заголовок с метаданными /mcp, тело как у адреса проекта.
func TestInstanceMCPChallenge(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	rec := f.mcpCall("/mcp", "")
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != instanceChallenge ||
		strings.TrimSpace(rec.Body.String()) != unauthenticatedBody {
		t.Fatalf("без токена: %d %q %s", rec.Code, rec.Header().Get("WWW-Authenticate"), rec.Body)
	}
	project := f.mcpCall("/mcp/projects/SDLC", "")
	if rec.Body.String() != project.Body.String() {
		t.Fatalf("тело 401 разошлось с адресом проекта:\n%s\n%s", rec.Body, project.Body)
	}
	rec = f.mcpCall("/mcp", "sdlc_nope")
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != instanceChallenge+`, error="invalid_token"` {
		t.Fatalf("неверный токен: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	// OAuth выключен — 401 без заголовка; режим заголовка — без 401.
	off := newOAuthFixture(t, oauthSetup{enabled: false, store: f.store})
	if rec := off.mcpCall("/mcp", ""); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("OAuth выключен: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	header := newOAuthFixture(t, oauthSetup{mode: workflow.AuthHeader, enabled: true, store: f.store})
	initParams := map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "t", "version": "1"}}
	if rec, out := rpc(header.h, "/mcp", "actor:user:alice", "initialize", initParams); rec.Code != http.StatusOK || out["result"] == nil {
		t.Fatalf("режим заголовка initialize: %d %s", rec.Code, rec.Body)
	}
	if rec, out := rpc(header.h, "/mcp", "actor:user:alice", "tools/call",
		map[string]any{"name": "sdlc_whoami", "arguments": map[string]any{"project": "SDLC"}}); rec.Code != http.StatusOK ||
		strings.Contains(rec.Body.String(), `"isError":true`) || out["result"] == nil {
		t.Fatalf("режим заголовка whoami: %d %s", rec.Code, rec.Body)
	}
}

// AC-2: метаданные ресурса инстансового адреса — по пути /mcp и в корне.
func TestInstanceResourceMetadata(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	for _, p := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		rec := f.get(p, nil)
		var m oauth.ResourceMetadata
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		if rec.Code != http.StatusOK || m.Resource != oauthIssuer+"/mcp" || m.AuthorizationServers[0] != oauthIssuer ||
			strings.Join(m.BearerMethodsSupported, ",") != "header" || strings.Join(m.ScopesSupported, " ") != strings.Join(oauth.SupportedScopes, " ") {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body)
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s заголовки: %v", p, rec.Header())
		}
		pre := f.send(httptest.NewRequest("OPTIONS", p, nil))
		if pre.Code != http.StatusNoContent || pre.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s preflight: %d", p, pre.Code)
		}
	}
	for _, p := range []string{"/.well-known/oauth-protected-resource/mcp/", "/.well-known/oauth-protected-resource/mcp/x",
		"/.well-known/oauth-protected-resource/mcpx", "/.well-known/oauth-protected-resource/mcp/projects/"} {
		if rec := f.get(p, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	// Адрес из Host и X-Forwarded-Proto без SDLC_PUBLIC_URL.
	open := newOAuthFixture(t, oauthSetup{enabled: true, publicURL: "-", store: f.store})
	req := httptest.NewRequest("GET", "/.well-known/oauth-protected-resource", nil)
	req.Host = "cadence.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	if rec := open.send(req); !strings.Contains(rec.Body.String(), `"resource":"https://cadence.example/mcp"`) {
		t.Fatalf("из заголовков: %s", rec.Body)
	}
	// Выключенный OAuth и режим заголовка — 404.
	for _, setup := range []oauthSetup{{enabled: false, store: f.store}, {mode: workflow.AuthHeader, enabled: true, store: f.store}} {
		g := newOAuthFixture(t, setup)
		for _, p := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
			if rec := g.get(p, nil); rec.Code != http.StatusNotFound {
				t.Errorf("%+v %s: %d", setup.mode, p, rec.Code)
			}
		}
	}
}

// AC-12, AC-2: цикл «401 → метаданные → authorize → token → вызов» для
// resource=<AS>/mcp; токен без привязки работает на /mcp со всеми проектами
// участия. Токен по адресу проекта на /mcp — только свой проект (AC-7).
func TestInstanceOAuthFlow(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	rec := f.mcpCall("/mcp", "")
	meta := strings.TrimSuffix(strings.TrimPrefix(rec.Header().Get("WWW-Authenticate"), `Bearer resource_metadata="`), `"`)
	var m oauth.ResourceMetadata
	_ = json.Unmarshal(f.get(strings.TrimPrefix(meta, oauthIssuer), nil).Body.Bytes(), &m)
	if m.Resource != oauthIssuer+"/mcp" {
		t.Fatalf("метаданные из 401: %q %v", meta, m)
	}
	client := f.register("Codex", "http://127.0.0.1/callback")
	conn := f.connectWith(client, m.Resource)
	_, tok := f.grantOf(conn.access)
	if tok.ProjectID != nil {
		t.Fatalf("токен по resource=/mcp привязан к проекту: %v", tok.ProjectID)
	}
	list := func(token string) string {
		rec, _ := rpc(f.h, "/mcp", token, "tools/call", map[string]any{"name": "sdlc_list_projects", "arguments": map[string]any{}})
		if rec.Code != http.StatusOK {
			t.Fatalf("list_projects: %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	if body := list(conn.access); !strings.Contains(body, `\"key\": \"SDLC\"`) {
		t.Fatalf("выдача по /mcp: %s", body)
	}
	if rec, _ := rpc(f.h, "/mcp", conn.access, "initialize", map[string]any{"protocolVersion": "2025-06-18",
		"capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}}); rec.Code != http.StatusOK {
		t.Fatalf("initialize выдачей: %d", rec.Code)
	}
	bound := f.connectWith(client, oauthIssuer+"/mcp/projects/SDLC")
	if _, tok := f.grantOf(bound.access); tok.ProjectID == nil {
		t.Fatal("выдача по адресу проекта без привязки")
	}
	if rec, _ := rpc(f.h, "/mcp", bound.access, "tools/call", map[string]any{"name": "sdlc_whoami",
		"arguments": map[string]any{}}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `\"key\": \"SDLC\"`) {
		t.Fatalf("привязанная выдача на /mcp: %d %s", rec.Code, rec.Body)
	}
}
