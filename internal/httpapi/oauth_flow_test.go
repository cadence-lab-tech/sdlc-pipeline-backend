package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

const loopback = "http://127.0.0.1:7000/callback"

// AC-14, AC-15, AC-16, AC-18, AC-19, AC-20, AC-21, AC-23.
func TestOAuthTokenFlow(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	client := f.register("Codex\x07 CLI", "http://127.0.0.1/callback")
	other := f.register("Other", "http://127.0.0.1/callback")
	resource := oauthIssuer + "/mcp/projects/SDLC"

	// AC-14: только форма.
	verifier, challenge := pkcePair("t")
	reqID := f.requestID(f.authorize(defaultAuthz(client, loopback, challenge, resource)))
	code := f.allow(reqID, f.alice)
	for _, ct := range []string{"application/json", "text/plain", ""} {
		body := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
			"client_id": {client}, "redirect_uri": {loopback}}.Encode()
		if ct == "application/json" {
			body = fmt.Sprintf(`{"grant_type":"authorization_code","code":%q}`, code)
		}
		req := newReq("POST", "/v1/oauth/token", body, ct)
		rec := f.send(req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_request"`) {
			t.Fatalf("AC-14 %q: %d %s", ct, rec.Code, rec.Body)
		}
	}

	// AC-15: каждое нарушение — invalid_grant, код не гасится.
	for name, v := range map[string]url.Values{
		"чужой verifier": {"code_verifier": {strings.Repeat("x", 64)}},
		"redirect_uri":   {"redirect_uri": {"http://127.0.0.1:7001/callback"}},
		"client_id":      {"client_id": {other}},
		"resource":       {"resource": {oauthIssuer + "/mcp/projects/OTHER"}},
	} {
		form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
			"client_id": {client}, "redirect_uri": {loopback}}
		for k, vs := range v {
			form[k] = vs
		}
		rec, out := f.form("/v1/oauth/token", form)
		if rec.Code != http.StatusBadRequest || out["error"] != "invalid_grant" {
			t.Errorf("AC-15 %s: %d %v", name, rec.Code, out)
		}
	}
	rec, out := f.form("/v1/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {verifier}, "client_id": {client}, "redirect_uri": {loopback}, "resource": {resource}})
	if rec.Code != http.StatusOK || out["token_type"] != "Bearer" || out["expires_in"] != float64(3600) ||
		out["scope"] != "cadence offline_access" || out["refresh_token"] == nil || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("AC-14 обмен: %d %v %v", rec.Code, out, rec.Header())
	}
	first := tokens{access: out["access_token"].(string), refresh: out["refresh_token"].(string)}

	// AC-21: токен человека с меткой и привязкой к проекту.
	_, me := f.jsonCall("GET", "/v1/auth/me", first.access, nil)
	if me["principal"].(map[string]any)["handle"] != "alice" || me["principal"].(map[string]any)["kind"] != "user" {
		t.Fatalf("AC-21 /auth/me: %v", me)
	}
	g, tok := f.grantOf(first.access)
	if tok.TokenKind != domain.TokenAPI || tok.PrincipalID != f.alicePrincipal || tok.Name != "Codex CLI · 127.0.0.1" ||
		len(tok.RoleKeys) != 0 || tok.ProjectKey == nil || *tok.ProjectKey != "SDLC" {
		t.Fatalf("AC-21 строка токена: %+v", tok)
	}
	var createdBy string
	if err := f.store.DB().QueryRow(f.ctx, `SELECT p.handle FROM agent_tokens t JOIN principals p ON p.id = t.created_by WHERE t.id = $1`,
		tok.ID).Scan(&createdBy); err != nil || createdBy != "alice" {
		t.Fatalf("AC-21 created_by: %q %v", createdBy, err)
	}
	// В таблице запросов — sha256 кода и погашение, самого кода нет.
	if n := f.count(`SELECT count(*) FROM oauth_authorization_requests WHERE code_hash = $1 AND code_used_at IS NOT NULL`,
		workflow.HashToken(code)); n != 1 {
		t.Fatal("AC-15: нет хеша кода с погашением")
	}
	if n := f.count(`SELECT count(*) FROM oauth_authorization_requests r WHERE row_to_json(r)::text LIKE '%' || $1 || '%'`, code); n != 0 {
		t.Fatal("AC-15: код открытым текстом в базе")
	}
	if rec := f.mcpCall("/mcp/projects/SDLC", first.access); rec.Code != http.StatusOK {
		t.Fatalf("MCP с access: %d %s", rec.Code, rec.Body)
	}

	// AC-18: ротация на месте.
	rows := f.count(`SELECT count(*) FROM agent_tokens`)
	f.now = f.now.Add(time.Minute)
	status, out := f.refresh(client, first.refresh)
	if status != http.StatusOK {
		t.Fatalf("AC-18: %d %v", status, out)
	}
	second := tokens{access: out["access_token"].(string), refresh: out["refresh_token"].(string)}
	g2, tok2 := f.grantOf(second.access)
	if g2.ID != g.ID || tok2.ID != tok.ID || tok2.Prefix == tok.Prefix || *tok2.ExpiresAt == *tok.ExpiresAt ||
		f.count(`SELECT count(*) FROM agent_tokens`) != rows {
		t.Fatalf("AC-18 строка: %+v → %+v", tok, tok2)
	}
	if rec := f.mcpCall("/mcp/projects/SDLC", first.access); rec.Code != http.StatusUnauthorized {
		t.Fatalf("AC-18 прежний access: %d", rec.Code)
	}

	// AC-19: повтор непосредственно предыдущего refresh в окне — без отзыва.
	f.now = f.now.Add(10 * time.Second)
	if status, out := f.refresh(client, first.refresh); status != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("AC-19 гонка: %d %v", status, out)
	}
	if rec := f.mcpCall("/mcp/projects/SDLC", second.access); rec.Code != http.StatusOK {
		t.Fatalf("AC-19 гонка отозвала выдачу: %d", rec.Code)
	}
	// Ещё два refresh — для AC-23 (после трёх строка токена одна).
	cur := second
	for range 2 {
		f.now = f.now.Add(time.Minute)
		status, out := f.refresh(client, cur.refresh)
		if status != http.StatusOK {
			t.Fatalf("refresh: %d %v", status, out)
		}
		cur = tokens{access: out["access_token"].(string), refresh: out["refresh_token"].(string)}
	}
	if n := f.count(`SELECT count(*) FROM agent_tokens WHERE id = $1`, tok.ID); n != 1 ||
		f.count(`SELECT count(*) FROM agent_tokens WHERE principal_id = $1 AND name LIKE 'Codex%'`, f.alicePrincipal) != 1 {
		t.Fatal("AC-23: строк токена не одна")
	}
	// AC-23: список токенов ссылается на выдачу; журнал вызова — token_id.
	_, list := f.jsonCall("GET", "/v1/auth/tokens", f.alice, nil)
	found := false
	for _, it := range list["tokens"].([]any) {
		m := it.(map[string]any)
		if m["id"] == tok.ID.String() {
			og, _ := m["oauth_grant"].(map[string]any)
			found = og["id"] == g.ID.String() && og["client_name"] == "Codex CLI" && og["redirect_host"] == "127.0.0.1"
		}
	}
	if !found {
		t.Fatalf("AC-23 список токенов: %v", list)
	}
	if code, out := f.jsonCall("PATCH", "/v1/tasks/SDLC-2", cur.access, map[string]any{"title": "правка клиентом"}); code != http.StatusOK {
		t.Fatalf("запись по /v1 токеном выдачи: %d %v", code, out)
	}
	if n := f.count(`SELECT count(*) FROM audit_log WHERE token_id = $1 AND channel = 'mcp'`, tok.ID); n == 0 {
		t.Fatal("AC-23: вызов MCP без token_id выдачи")
	}
	if n := f.count(`SELECT count(*) FROM audit_log WHERE token_id = $1 AND action = 'PATCH /v1/tasks/:taskKey'`, tok.ID); n != 1 {
		t.Fatal("AC-23: вызов /v1 без token_id выдачи")
	}

	// AC-19: повтор погашенного не в окне — отзыв выдачи.
	f.now = f.now.Add(time.Minute)
	if status, _ := f.refresh(client, second.refresh); status != http.StatusBadRequest {
		t.Fatalf("AC-19 повтор: %d", status)
	}
	if rec := f.mcpCall("/mcp/projects/SDLC", cur.access); rec.Code != http.StatusUnauthorized {
		t.Fatalf("AC-19 access после повтора: %d", rec.Code)
	}
	if status, out := f.refresh(client, cur.refresh); status != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("AC-19 актуальный refresh: %d %v", status, out)
	}
	if n := f.count(`SELECT count(*) FROM oauth_grants WHERE id = $1 AND revoke_reason = 'refresh_reuse'`, g.ID); n != 1 {
		t.Fatal("AC-19: причина отзыва")
	}

	// AC-16: повтор погашенного кода отзывает выдачу.
	t2 := f.connectWith(client, resource)
	f.now = f.now.Add(10 * time.Second)
	if status, out := f.exchange(client, loopback, t2.code, t2.verifier); status != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("AC-16 повтор кода: %d %v", status, out)
	}
	if rec := f.mcpCall("/mcp/projects/SDLC", t2.access); rec.Code != http.StatusUnauthorized {
		t.Fatalf("AC-16 access: %d", rec.Code)
	}
	if status, _ := f.refresh(client, t2.refresh); status != http.StatusBadRequest {
		t.Fatalf("AC-16 refresh: %d", status)
	}
	if n := f.count(`SELECT count(*) FROM audit_log WHERE action = 'oauth.revoke' AND params->>'reason' = 'code_reuse'`); n != 1 {
		t.Fatal("AC-16: в журнале нет отзыва «повтор кода»")
	}

	// AC-15: код старше двух минут.
	verifier, challenge = pkcePair("late")
	code = f.allow(f.requestID(f.authorize(defaultAuthz(client, loopback, challenge, resource))), f.alice)
	f.now = f.now.Add(2*time.Minute + time.Second)
	if status, out := f.exchange(client, loopback, code, verifier); status != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("AC-15 просроченный код: %d %v", status, out)
	}

	// AC-20: refresh без использования дольше срока — invalid_grant;
	// использованный в срок продлевается. Refresh выдан и без offline_access.
	verifier, challenge = pkcePair("ttl")
	p := defaultAuthz(client, loopback, challenge, resource)
	p["scope"] = "cadence"
	code = f.allow(f.requestID(f.authorize(p)), f.alice)
	status, out = f.exchange(client, loopback, code, verifier)
	if status != http.StatusOK || out["refresh_token"] == nil || out["scope"] != "cadence" {
		t.Fatalf("AC-18 без offline_access: %d %v", status, out)
	}
	r := out["refresh_token"].(string)
	f.now = f.now.Add(700 * time.Hour)
	status, out = f.refresh(client, r)
	if status != http.StatusOK {
		t.Fatalf("AC-20 в срок: %d %v", status, out)
	}
	f.now = f.now.Add(700 * time.Hour)
	if status, _ := f.refresh(client, out["refresh_token"].(string)); status != http.StatusOK {
		t.Fatalf("AC-20 продлён: %d", status)
	}
	verifier, challenge = pkcePair("old")
	code = f.allow(f.requestID(f.authorize(defaultAuthz(client, loopback, challenge, resource))), f.alice)
	_, out = f.exchange(client, loopback, code, verifier)
	f.now = f.now.Add(721 * time.Hour)
	if status, out := f.refresh(client, out["refresh_token"].(string)); status != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("AC-20 истёк: %d %v", status, out)
	}
}

type connected struct {
	access, refresh, code, verifier string
}

func (f *oauthFixture) connectWith(client, resource string) connected {
	f.t.Helper()
	verifier, challenge := pkcePair(fmt.Sprint(f.now.UnixNano() % 97))
	code := f.allow(f.requestID(f.authorize(defaultAuthz(client, loopback, challenge, resource))), f.alice)
	status, out := f.exchange(client, loopback, code, verifier)
	if status != http.StatusOK {
		f.t.Fatalf("обмен: %d %v", status, out)
	}
	return connected{access: out["access_token"].(string), refresh: out["refresh_token"].(string), code: code, verifier: verifier}
}

// AC-24, AC-25: отзыв тремя путями и список выдач.
func TestOAuthRevoke(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	client := f.register("Codex", "http://127.0.0.1/callback")
	other := f.register("Other", "http://127.0.0.1/callback")
	resource := oauthIssuer + "/mcp/projects/SDLC"

	check := func(label string, c connected) {
		t.Helper()
		g, tok := f.grantOf(c.access)
		if tok.RevokedAt == nil {
			t.Fatalf("%s: revoked_at не выставлен", label)
		}
		if rec := f.mcpCall("/mcp/projects/SDLC", c.access); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: access %d", label, rec.Code)
		}
		if status, out := f.refresh(client, c.refresh); status != http.StatusBadRequest || out["error"] != "invalid_grant" {
			t.Fatalf("%s: refresh %d %v", label, status, out)
		}
		if n := f.count(`SELECT count(*) FROM audit_log WHERE action = 'oauth.revoke' AND params->>'grant_id' = $1
			AND params->>'reason' = 'user' AND token_id = $2`, g.ID.String(), tok.ID); n != 1 {
			t.Fatalf("%s: в журнале нет отзыва «человек»", label)
		}
	}

	a := f.connectWith(client, resource)
	g, _ := f.grantOf(a.access)
	if code, out := f.jsonCall("DELETE", "/v1/oauth/grants/"+g.ID.String(), f.bob, nil); code != http.StatusNotFound {
		t.Fatalf("AC-24 чужая выдача: %d %v", code, out)
	}
	if code, out := f.jsonCall("DELETE", "/v1/oauth/grants/"+g.ID.String(), f.alice, nil); code != http.StatusNoContent {
		t.Fatalf("AC-24 grants: %d %v", code, out)
	}
	check("DELETE /v1/oauth/grants", a)
	if code, _ := f.jsonCall("DELETE", "/v1/oauth/grants/"+g.ID.String(), f.alice, nil); code != http.StatusNoContent {
		t.Fatal("AC-24 повторный отзыв")
	}

	b := f.connectWith(client, resource)
	_, tok := f.grantOf(b.access)
	if code, out := f.jsonCall("DELETE", "/v1/auth/tokens/"+tok.ID.String(), f.alice, nil); code != http.StatusNoContent {
		t.Fatalf("AC-24 tokens: %d %v", code, out)
	}
	check("DELETE /v1/auth/tokens", b)
	if n := f.count(`SELECT count(*) FROM oauth_grants WHERE token_id = $1 AND revoke_reason = 'user'`, tok.ID); n != 1 {
		t.Fatal("AC-24: причина отзыва через токены")
	}

	// AC-25: RFC 7009.
	for _, which := range []string{"access", "refresh"} {
		c := f.connectWith(client, resource)
		token := c.access
		if which == "refresh" {
			token = c.refresh
		}
		rec, _ := f.form("/v1/oauth/revoke", url.Values{"token": {token}, "client_id": {other}})
		if _, tk := f.grantOf(c.access); rec.Code != http.StatusOK || tk.RevokedAt != nil {
			t.Fatalf("AC-25 чужой клиент %s: %d", which, rec.Code)
		}
		rec, _ = f.form("/v1/oauth/revoke", url.Values{"token": {token}, "client_id": {client}, "token_type_hint": {which + "_token"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("AC-25 %s: %d", which, rec.Code)
		}
		check("revoke "+which, c)
	}
	if rec, _ := f.form("/v1/oauth/revoke", url.Values{"token": {"nope"}, "client_id": {client}}); rec.Code != http.StatusOK {
		t.Fatalf("AC-25 неизвестный: %d", rec.Code)
	}

	// Список — только свои, без хешей.
	_, list := f.jsonCall("GET", "/v1/oauth/grants", f.alice, nil)
	grants := list["grants"].([]any)
	if len(grants) != 4 || strings.Contains(fmt.Sprint(list), "hash") {
		t.Fatalf("AC-25 список: %v", list)
	}
	if _, list := f.jsonCall("GET", "/v1/oauth/grants", f.bob, nil); len(list["grants"].([]any)) != 0 {
		t.Fatalf("AC-25 чужие видны: %v", list)
	}
}

// AC-22: права токена выдачи — права человека; проверка на проекте со
// своим набором статусов и ролей.
func TestOAuthSameRightsAsPerson(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	ok := func(method, path, token string, body any) map[string]any {
		t.Helper()
		code, out := f.jsonCall(method, path, token, body)
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("%s %s: %d %v", method, path, code, out)
		}
		return out
	}
	ok("POST", "/v1/projects", f.root, map[string]any{"key": "ACME", "name": "Acme"})
	report := ok("POST", "/v1/projects/ACME/import", f.root, map[string]any{
		"version": 1,
		"roles":   []any{map[string]any{"key": "builder", "name": "Сборщик", "instructions": "собирать"}},
		"workflows": []any{map[string]any{
			"name": "Свой", "task_types": []string{"task"},
			"statuses": []any{
				map[string]any{"key": "inbox", "name": "Входящие", "category": "backlog", "is_initial": true},
				map[string]any{"key": "build", "name": "Сборка", "category": "active", "role": "builder"},
				map[string]any{"key": "shipped", "name": "Отгружено", "category": "done", "is_terminal": true},
			},
			"transitions": []any{
				map[string]any{"key": "go", "name": "go", "from": "inbox", "to": "build", "kind": "advance"},
				map[string]any{"key": "ship", "name": "ship", "from": "build", "to": "shipped", "kind": "advance"},
			},
		}},
	})
	ok("POST", "/v1/workflows/"+report["workflows"].([]any)[0].(map[string]any)["id"].(string)+"/activate", f.root, nil)
	ok("PUT", "/v1/projects/ACME/members", f.root, map[string]any{"principal": "user:alice", "role": "viewer"})
	task := ok("POST", "/v1/projects/ACME/tasks", f.root, map[string]any{"title": "Своя задача"})["key"].(string)

	client := f.register("Codex", "http://127.0.0.1/callback")
	unbound := f.connectWith(client, "")
	bound := f.connectWith(client, oauthIssuer+"/mcp/projects/SDLC")
	manual := f.alice

	type call struct {
		method, path string
		body         any
	}
	calls := []call{
		{"GET", "/v1/projects", nil},
		{"GET", "/v1/tasks/" + task, nil},
		{"PATCH", "/v1/tasks/" + task, map[string]any{"title": "правка viewer"}},
		{"POST", "/v1/tasks/" + task + "/transitions/go", map[string]any{}},
		{"GET", "/v1/projects/OTHER", nil},
	}
	for _, c := range calls {
		want, wantBody := f.jsonCall(c.method, c.path, manual, c.body)
		got, gotBody := f.jsonCall(c.method, c.path, unbound.access, c.body)
		if want != got || errCode(wantBody) != errCode(gotBody) {
			t.Errorf("AC-22 %s %s: ручной %d %v, выдача %d %v", c.method, c.path, want, wantBody, got, gotBody)
		}
	}
	if rec := f.mcpCall("/mcp/projects/ACME", unbound.access); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "viewer") {
		t.Fatalf("AC-22 MCP: %d %s", rec.Code, rec.Body)
	}
	// Выдача с привязкой к SDLC вне него недействительна — как ручной токен
	// с привязкой, и у администратора тоже.
	if code, _ := f.jsonCall("GET", "/v1/tasks/"+task, bound.access, nil); code != http.StatusForbidden {
		t.Fatalf("AC-22 привязка: %d", code)
	}
	adminClient := f.register("Admin", "http://127.0.0.1/callback")
	verifier, challenge := pkcePair("adm")
	code := f.allow(f.requestID(f.authorize(defaultAuthz(adminClient, loopback, challenge, oauthIssuer+"/mcp/projects/SDLC"))), f.root)
	_, out := f.exchange(adminClient, loopback, code, verifier)
	if code, _ := f.jsonCall("GET", "/v1/tasks/"+task, out["access_token"].(string), nil); code != http.StatusForbidden {
		t.Fatalf("AC-22 привязка администратора: %d", code)
	}
}

// AC-32, AC-37: журнал и лог после полного потока — без секретов.
func TestOAuthAuditAndLogs(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	client := f.register("Codex", "http://127.0.0.1/callback")
	resource := oauthIssuer + "/mcp/projects/SDLC"
	var secrets []string

	c := f.connectWith(client, resource)
	secrets = append(secrets, c.access, c.refresh, c.code, c.verifier)
	cur := tokens{c.access, c.refresh}
	for range 3 {
		f.now = f.now.Add(time.Minute)
		_, out := f.refresh(client, cur.refresh)
		cur = tokens{out["access_token"].(string), out["refresh_token"].(string)}
		secrets = append(secrets, cur.access, cur.refresh)
	}
	g, _ := f.grantOf(cur.access)
	f.jsonCall("DELETE", "/v1/oauth/grants/"+g.ID.String(), f.alice, nil)

	second := f.register("Second", "http://127.0.0.1/callback")
	d := f.connectWith(second, resource)
	secrets = append(secrets, d.access, d.refresh, d.code, d.verifier)
	f.now = f.now.Add(time.Hour)
	_, out := f.refresh(second, d.refresh)
	secrets = append(secrets, out["access_token"].(string), out["refresh_token"].(string))
	f.now = f.now.Add(time.Hour)
	f.refresh(second, d.refresh) // повтор вне окна

	e := f.connectWith(second, resource)
	secrets = append(secrets, e.access, e.refresh, e.code, e.verifier)
	f.exchange(second, loopback, e.code, e.verifier) // повтор кода

	// Истечение — проход воркера.
	h := f.connectWith(second, resource)
	secrets = append(secrets, h.access, h.refresh, h.code, h.verifier)
	f.now = f.now.Add(721 * time.Hour)
	if _, err := f.oa.Sweep(f.ctx); err != nil {
		t.Fatal(err)
	}

	for _, reason := range []string{"user", "refresh_reuse", "code_reuse", "expired"} {
		if n := f.count(`SELECT count(*) FROM audit_log WHERE action = 'oauth.revoke' AND params->>'reason' = $1
			AND principal_id IS NOT NULL AND token_id IS NOT NULL`, reason); n == 0 {
			t.Errorf("AC-32: нет отзыва %q", reason)
		}
	}
	if n := f.count(`SELECT count(*) FROM audit_log WHERE action = 'oauth.grant' AND principal_id = $1 AND token_id IS NOT NULL`, f.alicePrincipal); n != 4 {
		t.Errorf("AC-32: выдач в журнале %d, ожидали 4", n)
	}
	if n := f.count(`SELECT count(*) FROM audit_log WHERE action = 'oauth.register'`); n != 2 {
		t.Errorf("AC-32: регистраций %d", n)
	}
	// Обычный refresh записей не создаёт: записей о /v1/oauth/token нет вовсе.
	if n := f.count(`SELECT count(*) FROM audit_log WHERE action LIKE '%/oauth/token%'`); n != 0 {
		t.Errorf("AC-32: refresh в журнале: %d", n)
	}
	var all string
	if err := f.store.DB().QueryRow(f.ctx, `SELECT coalesce(string_agg(params::text, ' '), '') FROM audit_log`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	logs := f.logs.String()
	for _, key := range []string{`"code_verifier"`, `"refresh_token"`, `"access_token"`, `"code":`} {
		if strings.Contains(all, key) {
			t.Errorf("AC-32: ключ %s в журнале", key)
		}
	}
	for _, sec := range secrets {
		for where, text := range map[string]string{"журнал": all, "лог": logs} {
			if strings.Contains(text, sec) || strings.Contains(text, workflow.HashToken(sec)) {
				t.Errorf("AC-32/AC-37: секрет %s… в %s", sec[:6], where)
			}
		}
	}
	// AC-37: в таблицах OAuth нет открытых секретов.
	for _, sec := range secrets {
		if n := f.count(`SELECT (SELECT count(*) FROM oauth_authorization_requests r WHERE row_to_json(r)::text LIKE '%' || $1 || '%')
			+ (SELECT count(*) FROM oauth_refresh_tokens r WHERE row_to_json(r)::text LIKE '%' || $1 || '%')
			+ (SELECT count(*) FROM oauth_grants r WHERE row_to_json(r)::text LIKE '%' || $1 || '%')`, sec); n != 0 {
			t.Errorf("AC-37: секрет %s… в таблицах OAuth", sec[:6])
		}
	}
}

// AC-33: один проход задания на подменённых часах.
func TestOAuthSweep(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	resource := oauthIssuer + "/mcp/projects/SDLC"
	live := f.register("Live", "http://127.0.0.1/callback")
	idle := f.register("Idle", "http://127.0.0.1/callback")
	ended := f.register("Ended", "http://127.0.0.1/callback")

	keep := f.connectWith(live, resource)
	gone := f.connectWith(ended, resource)
	g, _ := f.grantOf(gone.access)
	f.jsonCall("DELETE", "/v1/oauth/grants/"+g.ID.String(), f.alice, nil)
	_, challenge := pkcePair("sw")
	f.authorize(defaultAuthz(live, loopback, challenge, "")) // запрос без решения

	// 6 суток: клиенты живы, запросы — убраны; refresh выдачи keep истёк бы
	// только через 30.
	f.now = f.now.Add(6 * 24 * time.Hour)
	st, err := f.oa.Sweep(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM oauth_authorization_requests`) != 0 || st.Requests == 0 {
		t.Fatalf("AC-33 запросы: %+v", st)
	}
	if f.count(`SELECT count(*) FROM oauth_refresh_tokens WHERE grant_id = $1`, g.ID) != 0 {
		t.Fatal("AC-33: refresh погасшей выдачи остались")
	}
	if f.count(`SELECT count(*) FROM oauth_clients`) != 3 {
		t.Fatal("AC-33: клиенты ушли раньше 7 суток")
	}
	f.now = f.now.Add(2 * 24 * time.Hour)
	if _, err := f.oa.Sweep(f.ctx); err != nil {
		t.Fatal(err)
	}
	// idle и ended: регистрация и погасание последней выдачи — 8 суток
	// назад; у live выдача жива.
	if f.count(`SELECT count(*) FROM oauth_clients WHERE id = ANY($1)`, []string{idle, ended}) != 0 ||
		f.count(`SELECT count(*) FROM oauth_clients WHERE id = $1`, live) != 1 {
		t.Fatal("AC-33: клиент без выдачи остался или клиент с живой выдачей ушёл")
	}
	// Истёкший refresh — выдача отозвана с причиной «истекла».
	f.now = f.now.Add(30 * 24 * time.Hour)
	st, err = f.oa.Sweep(f.ctx)
	if err != nil || st.Expired != 1 {
		t.Fatalf("AC-33 истечение: %+v %v", st, err)
	}
	kg, _ := f.grantOf(keep.access)
	if kg.RevokeReason == nil || *kg.RevokeReason != "expired" {
		t.Fatalf("AC-33 причина: %+v", kg)
	}
	if rec := f.mcpCall("/mcp/projects/SDLC", keep.access); rec.Code != http.StatusUnauthorized {
		t.Fatalf("AC-33 токен истёкшей: %d", rec.Code)
	}
}

// AC-34: миграция поверх 000048 с живыми токенами и журналом; down — только
// таблицы OAuth.
func TestOAuthMigrationOnFilledBase(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	f.connectWith(f.register("Codex", "http://127.0.0.1/callback"), "")
	read := func(name string) string {
		body, err := migrations.Schema.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	tables := func() int {
		return f.count(`SELECT count(*) FROM information_schema.tables WHERE table_name LIKE 'oauth\_%'`)
	}
	counts := func() string {
		return fmt.Sprint(f.count(`SELECT count(*) FROM agent_tokens`), f.count(`SELECT count(*) FROM audit_log`),
			f.count(`SELECT count(*) FROM principals`))
	}
	before := counts()
	if tables() != 4 {
		t.Fatalf("таблиц OAuth %d", tables())
	}
	if _, err := f.store.DB().Exec(f.ctx, read("000049_oauth.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if tables() != 0 || counts() != before {
		t.Fatalf("down: таблиц %d, счёт %s → %s", tables(), before, counts())
	}
	if _, err := f.store.DB().Exec(f.ctx, read("000049_oauth.up.sql")); err != nil {
		t.Fatalf("up на заполненной базе: %v", err)
	}
	if tables() != 4 || counts() != before {
		t.Fatalf("up: таблиц %d, счёт %s → %s", tables(), before, counts())
	}
	if n := f.count(`SELECT count(*) FROM information_schema.columns WHERE table_name LIKE 'oauth\_%'
		AND (column_name ILIKE '%secret%' OR column_name IN ('code', 'refresh_token', 'access_token'))`); n != 0 {
		t.Fatalf("колонки секретов: %d", n)
	}
	// Ограничение вида токенов не тронуто.
	if n := f.count(`SELECT count(*) FROM pg_constraint WHERE conname = 'agent_tokens_kind_check'
		AND pg_get_constraintdef(oid) LIKE '%api%session%task%'`); n != 1 {
		t.Fatal("CHECK по виду токена изменён")
	}
	// Один действующий refresh на выдачу — индекс держит.
	g, err := f.store.CreateOAuthGrant(f.ctx, f.store.DB(), postgresGrant(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateOAuthRefresh(f.ctx, f.store.DB(), g.ID, "h1", nil, f.now, f.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CreateOAuthRefresh(f.ctx, f.store.DB(), g.ID, "h2", nil, f.now, f.now.Add(time.Hour)); err == nil {
		t.Fatal("два действующих refresh у одной выдачи")
	}
}

func newReq(method, path, body, contentType string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

// postgresGrant — вход выдачи на новом токене alice, мимо протокола: только
// для проверки ограничений схемы.
func postgresGrant(t *testing.T, f *oauthFixture) postgres.CreateOAuthGrantInput {
	t.Helper()
	tok, err := f.store.CreateToken(f.ctx, f.store.DB(), postgres.CreateTokenInput{
		PrincipalID: f.alicePrincipal, Name: "схема", TokenHash: "schema-hash", Prefix: "sdlc_schema", CreatedBy: f.alicePrincipal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return postgres.CreateOAuthGrantInput{TokenID: tok.ID, PrincipalID: f.alicePrincipal, ClientID: "c", ClientKind: "dcr",
		ClientName: "c", RedirectHost: "127.0.0.1", Resource: oauthIssuer + "/mcp", Scope: "cadence", CreatedAt: f.now}
}
