package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// AC-27, AC-28, AC-29, AC-30: «OAuth действует» ⇔ выключатель включён и
// режим токенов. Одна база, одна выдача, четыре сервера.
func TestOAuthModeMatrix(t *testing.T) {
	base := newOAuthFixture(t, oauthSetup{enabled: true})
	client := base.register("Codex", "http://127.0.0.1/callback")
	grant := base.connectWith(client, oauthIssuer+"/mcp/projects/SDLC")
	grantsBefore := base.count(`SELECT count(*) FROM oauth_grants g JOIN agent_tokens t ON t.id = g.token_id WHERE t.revoked_at IS NULL`)

	type row struct {
		meta, handlers, mcpNoToken, grantToken, manual int
		challenge                                      bool
	}
	cases := []struct {
		mode    string
		enabled bool
		want    row
	}{
		{workflow.AuthToken, true, row{200, 200, 401, 200, 200, true}},
		{workflow.AuthToken, false, row{404, 404, 401, 401, 200, false}},
		{workflow.AuthHeader, true, row{404, 404, 200, 401, 200, false}},
		{workflow.AuthHeader, false, row{404, 404, 200, 401, 200, false}},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/%v", c.mode, c.enabled), func(t *testing.T) {
			f := newOAuthFixture(t, oauthSetup{mode: c.mode, enabled: c.enabled, store: base.store})
			var got row
			got.meta = f.get("/.well-known/oauth-authorization-server", nil).Code
			if r := f.get("/.well-known/oauth-protected-resource/mcp/projects/SDLC", nil); r.Code != got.meta {
				t.Errorf("метаданные ресурса: %d", r.Code)
			}
			got.handlers, _ = f.jsonCall("GET", "/v1/oauth/grants", f.alice, nil)
			for _, p := range []string{"/v1/oauth/token", "/v1/oauth/register", "/v1/oauth/revoke"} {
				if code, _ := f.jsonCall("POST", p, "", map[string]any{}); (code == http.StatusNotFound) != (got.handlers == http.StatusNotFound) {
					t.Errorf("%s: %d", p, code)
				}
			}
			if r := f.get("/v1/oauth/authorize?client_id=x", nil); (r.Code == http.StatusNotFound) != (got.handlers == http.StatusNotFound) {
				t.Errorf("authorize: %d", r.Code)
			}
			rec := f.mcpCall("/mcp/projects/SDLC", "")
			got.mcpNoToken = rec.Code
			got.challenge = rec.Header().Get("WWW-Authenticate") != ""
			rec = f.mcpCall("/mcp/projects/SDLC", grant.access)
			got.grantToken = rec.Code
			if rec.Code == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != "" && !c.want.challenge {
				t.Errorf("токен выдачи: WWW-Authenticate при недействующем OAuth")
			}
			if code, _ := f.jsonCall("GET", "/v1/auth/me", grant.access, nil); code != got.grantToken {
				t.Errorf("токен выдачи на /v1: %d, на MCP %d", code, got.grantToken)
			}
			got.manual, _ = f.jsonCall("GET", "/v1/auth/me", base.alice, nil)
			if got != c.want {
				t.Fatalf("матрица: %+v, ожидали %+v", got, c.want)
			}

			// AC-30: сведения об инстансе, одинаково на single и shared.
			for _, tenancy := range []string{"shared", "single"} {
				f.auth.Tenancy(tenancy)
				var inst struct {
					Tenancy string                     `json:"tenancy"`
					OAuth   map[string]json.RawMessage `json:"oauth"`
				}
				_ = json.Unmarshal(f.get("/v1/instance", nil).Body.Bytes(), &inst)
				if string(inst.OAuth["enabled"]) != fmt.Sprint(c.want.meta == 200) ||
					string(inst.OAuth["dcr"]) != "true" || string(inst.OAuth["cimd"]) != "true" {
					t.Fatalf("instance %s: %v", tenancy, inst.OAuth)
				}
			}
		})
	}
	// Выдачи целы, включение возвращает рабочий токен той же выдачи.
	if n := base.count(`SELECT count(*) FROM oauth_grants g JOIN agent_tokens t ON t.id = g.token_id WHERE t.revoked_at IS NULL`); n != grantsBefore {
		t.Fatalf("выдачи изменились: %d → %d", grantsBefore, n)
	}
	if rec := base.mcpCall("/mcp/projects/SDLC", grant.access); rec.Code != http.StatusOK {
		t.Fatalf("включение не вернуло выдачу: %d", rec.Code)
	}
}
