package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/oauth"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Сервер авторизации OAuth 2.1 (CLAB-481, решение 0079). Номера AC-N —
// критерии приёмки задачи. Все проверки протокола — в режиме токенов, часы
// сервиса подменяются, документ клиента отдаёт заглушка с TLS за подставным
// резолвером.

const oauthIssuer = "https://cadence.test"

// logSink собирает строки лога процесса: AC-5 (одно предупреждение) и AC-37
// (секретов в логе нет).
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type oauthFixture struct {
	t              *testing.T
	ctx            context.Context
	store          *postgres.Store
	srv            *Server
	h              http.Handler
	oa             *workflow.OAuth
	auth           *workflow.Auth
	logs           *logSink
	now            time.Time
	cimd           *cimdStub
	root           string
	alice          string // человек, участник SDLC
	bob            string // человек без участия
	alicePrincipal uuid.UUID
}

type oauthSetup struct {
	mode      string
	enabled   bool
	noDCR     bool
	noCIMD    bool
	publicURL string
	store     *postgres.Store
}

func newOAuthFixture(t *testing.T, in oauthSetup) *oauthFixture {
	t.Helper()
	ctx := context.Background()
	store := in.store
	if store == nil {
		store, _ = testdb.Store(t)
	}
	if in.mode == "" {
		in.mode = workflow.AuthToken
	}
	if in.publicURL == "-" {
		in.publicURL = ""
	} else if in.publicURL == "" {
		in.publicURL = oauthIssuer
	}
	logs := &logSink{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, in.mode)
	authoring := workflow.NewAuthoring(store, log, engine)
	mcpServer := mcp.NewServer(log, store, engine, authoring, auth, "test")
	cfg := config.Config{Env: "test", AuthMode: in.mode, PublicURL: in.publicURL,
		OAuthEnabled: in.enabled, OAuthDCREnabled: !in.noDCR, OAuthCIMDEnabled: !in.noCIMD}
	srv := NewServer(cfg, log, store, engine, authoring, auth, mcpServer)
	f := &oauthFixture{t: t, ctx: ctx, store: store, srv: srv, auth: auth, logs: logs,
		now: time.Now().UTC().Truncate(time.Second), root: rootToken}
	f.cimd = newCIMDStub(t)
	f.oa = workflow.NewOAuth(store, auth, srv.audit, log, workflow.OAuthOptions{
		Enabled: in.enabled, DCR: !in.noDCR, CIMD: !in.noCIMD,
		Fetcher: f.cimd.fetcher, Now: func() time.Time { return f.now },
	})
	srv.WithOAuth(f.oa)
	f.h = srv.Handler()

	if err := auth.Bootstrap(ctx, "root", rootToken); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, rootToken)
	if err != nil && in.mode == workflow.AuthToken {
		t.Fatal(err)
	}
	if in.mode != workflow.AuthToken {
		// В режиме заголовка Authenticate тоже работает: токен проверяется
		// всегда; root — ручной токен.
		admin, _ = auth.Authenticate(ctx, rootToken)
	}
	for _, h := range []string{"alice", "bob"} {
		if _, err := store.ResolveActor(ctx, store.DB(), h, domain.PrincipalUser); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := auth.SetMember(ctx, admin, "SDLC", "user:alice", domain.MemberAgent, nil); err != nil {
		t.Fatal(err)
	}
	issue := func(h string) string {
		out, err := auth.IssueToken(ctx, admin, workflow.IssueTokenInput{Name: "вход " + h, Principal: "user:" + h})
		if err != nil {
			t.Fatal(err)
		}
		return out.Token
	}
	f.alice, f.bob = issue("alice"), issue("bob")
	p, err := store.GetPrincipalByHandle(ctx, store.DB(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	f.alicePrincipal = p.ID
	return f
}

// ── транспорт ───────────────────────────────────────────────────────────────

func (f *oauthFixture) send(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *oauthFixture) get(path string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return f.send(req)
}

func (f *oauthFixture) jsonCall(method, path, token string, body any) (int, map[string]any) {
	f.t.Helper()
	var r io.Reader = http.NoBody
	if body != nil {
		raw, _ := json.Marshal(body)
		r = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := f.send(req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (f *oauthFixture) form(path string, v url.Values) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := f.send(req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (f *oauthFixture) mcpCall(path, token string) *httptest.ResponseRecorder {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sdlc_whoami","arguments":{}}}`
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return f.send(req)
}

func (f *oauthFixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.store.DB().QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// ── клиенты и поток ─────────────────────────────────────────────────────────

func pkcePair(seed string) (verifier, challenge string) {
	verifier = (seed + "-" + strings.Repeat("0", 64))[:64]
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (f *oauthFixture) register(name string, redirects ...string) string {
	f.t.Helper()
	code, out := f.jsonCall("POST", "/v1/oauth/register", "", map[string]any{
		"client_name": name, "redirect_uris": redirects,
	})
	if code != http.StatusCreated {
		f.t.Fatalf("регистрация: %d %v", code, out)
	}
	return out["client_id"].(string)
}

type authzParams map[string]string

func (f *oauthFixture) authorize(p authzParams) *httptest.ResponseRecorder {
	q := url.Values{}
	for k, v := range p {
		q.Set(k, v)
	}
	return f.get("/v1/oauth/authorize?"+q.Encode(), nil)
}

func defaultAuthz(clientID, redirect, challenge, resource string) authzParams {
	p := authzParams{
		"client_id": clientID, "redirect_uri": redirect, "response_type": "code",
		"code_challenge": challenge, "code_challenge_method": "S256", "state": "st-1",
	}
	if resource != "" {
		p["resource"] = resource
	}
	return p
}

// requestID — id запроса авторизации из адреса страницы согласия.
func (f *oauthFixture) requestID(rec *httptest.ResponseRecorder) string {
	f.t.Helper()
	loc := rec.Header().Get("Location")
	id, ok := strings.CutPrefix(loc, "/login/oauth/")
	if rec.Code != http.StatusFound || !ok {
		f.t.Fatalf("authorize: %d Location=%q %s", rec.Code, loc, rec.Body)
	}
	return id
}

// allow — решение человека; отдаёт код из адреса редиректа.
func (f *oauthFixture) allow(reqID, token string) string {
	f.t.Helper()
	code, out := f.jsonCall("POST", "/v1/oauth/requests/"+reqID, token, map[string]any{"decision": "allow"})
	if code != http.StatusOK {
		f.t.Fatalf("allow: %d %v", code, out)
	}
	u, err := url.Parse(out["redirect_uri"].(string))
	if err != nil {
		f.t.Fatal(err)
	}
	return u.Query().Get("code")
}

type tokens struct{ access, refresh string }

func (f *oauthFixture) exchange(clientID, redirect, code, verifier string) (int, map[string]any) {
	rec, out := f.form("/v1/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"client_id": {clientID}, "redirect_uri": {redirect},
	})
	return rec.Code, out
}

func (f *oauthFixture) refresh(clientID, refresh string) (int, map[string]any) {
	rec, out := f.form("/v1/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID},
	})
	return rec.Code, out
}

// connect проводит клиента через весь поток от имени token.
func (f *oauthFixture) connect(clientID, redirect, resource, token string) tokens {
	f.t.Helper()
	verifier, challenge := pkcePair("v" + clientID[:4])
	reqID := f.requestID(f.authorize(defaultAuthz(clientID, redirect, challenge, resource)))
	code := f.allow(reqID, token)
	status, out := f.exchange(clientID, redirect, code, verifier)
	if status != http.StatusOK {
		f.t.Fatalf("обмен кода: %d %v", status, out)
	}
	return tokens{access: out["access_token"].(string), refresh: out["refresh_token"].(string)}
}

// grantOf — выдача и строка токена по access.
func (f *oauthFixture) grantOf(access string) (domain.OAuthGrant, domain.AccessToken) {
	f.t.Helper()
	g, err := f.store.OAuthGrantByAccessHash(f.ctx, f.store.DB(), workflow.HashToken(access))
	if err != nil {
		f.t.Fatalf("выдача по access: %v", err)
	}
	tok, err := f.store.GetToken(f.ctx, f.store.DB(), g.TokenID)
	if err != nil {
		f.t.Fatal(err)
	}
	return g, tok
}

// ── заглушка документа клиента ──────────────────────────────────────────────

type cimdResolver struct{}

func (cimdResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
}

type cimdStub struct {
	srv     *httptest.Server
	fetcher *oauth.Fetcher
	mu      sync.Mutex
	hits    int
	body    string
}

const cimdClientID = "https://example.com/oauth/client.json"

func newCIMDStub(t *testing.T) *cimdStub {
	s := &cimdStub{}
	s.body = fmt.Sprintf(`{"client_id":%q,"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`, cimdClientID)
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.hits++
		body := s.body
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(s.srv.Certificate())
	addr := s.srv.Listener.Addr().String()
	s.fetcher = &oauth.Fetcher{Resolver: cimdResolver{}, RootCAs: pool,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}
	return s
}

func (s *cimdStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// ── метаданные и ответ ресурса ──────────────────────────────────────────────

// Образец 401 v0.28.0: тело ответа без токена, как на main до задачи.
const unauthenticatedBody = "{\"error\":{\"code\":\"unauthenticated\",\"message\":\"нужен токен: заголовок Authorization: Bearer \\u003ctoken\\u003e\"}}"

// AC-1, AC-2, AC-3, AC-4.
func TestOAuthMetadataAndChallenge(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})

	rec := f.mcpCall("/mcp/projects/SDLC", "")
	want := `Bearer resource_metadata="` + oauthIssuer + `/.well-known/oauth-protected-resource/mcp/projects/SDLC"`
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != want ||
		strings.TrimSpace(rec.Body.String()) != unauthenticatedBody {
		t.Fatalf("AC-1: %d %q\n%q\n%q", rec.Code, rec.Header().Get("WWW-Authenticate"), rec.Body.String(), unauthenticatedBody)
	}
	// Подпуть транспорта — тот же ресурс.
	if rec := f.mcpCall("/mcp/projects/SDLC/sse", ""); rec.Header().Get("WWW-Authenticate") != want {
		t.Fatalf("подпуть: %q", rec.Header().Get("WWW-Authenticate"))
	}

	// Неизвестный, отозванный и истёкший токен — с error="invalid_token".
	issued, err := f.auth.IssueToken(f.ctx, mustAuth(t, f.auth, f.root), workflow.IssueTokenInput{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RevokeToken(f.ctx, f.store.DB(), issued.Record.ID); err != nil {
		t.Fatal(err)
	}
	expired, err := f.auth.IssueToken(f.ctx, mustAuth(t, f.auth, f.root), workflow.IssueTokenInput{Name: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(f.ctx, `UPDATE agent_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired.Record.ID); err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"неизвестный": "sdlc_nope", "отозванный": issued.Token, "истёкший": expired.Token} {
		rec := f.mcpCall("/mcp/projects/SDLC", tok)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != want+`, error="invalid_token"` {
			t.Errorf("AC-2 %s: %d %q", name, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	}
	// /v1 — как на main: тело образца, заголовка нет.
	for _, tok := range []string{"", "sdlc_nope"} {
		h := map[string]string{}
		if tok != "" {
			h["Authorization"] = "Bearer " + tok
		}
		rec := f.get("/v1/projects", h)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "" {
			t.Fatalf("AC-2 /v1 %q: %d %q", tok, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
		if tok == "" && strings.TrimSpace(rec.Body.String()) != unauthenticatedBody {
			t.Fatalf("AC-2 тело /v1: %s", rec.Body)
		}
	}

	// AC-3: метаданные ресурса — для любого ключа; инстансовый адрес (/mcp и
	// корень) — с CLAB-483, TestInstanceResourceMetadata.
	for _, key := range []string{"SDLC", "NOPE"} {
		rec := f.get("/.well-known/oauth-protected-resource/mcp/projects/"+key, nil)
		var m oauth.ResourceMetadata
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		if rec.Code != http.StatusOK || m.Resource != oauthIssuer+"/mcp/projects/"+key || m.AuthorizationServers[0] != oauthIssuer ||
			len(m.ScopesSupported) == 0 || m.BearerMethodsSupported[0] != "header" {
			t.Fatalf("AC-3 %s: %d %s", key, rec.Code, rec.Body)
		}
	}
	for _, p := range []string{"/.well-known/oauth-protected-resource/mcp/projects/", "/.well-known/openid-configuration"} {
		if rec := f.get(p, nil); rec.Code != http.StatusNotFound {
			t.Errorf("AC-3/AC-4 %s: %d", p, rec.Code)
		}
	}

	// AC-4: метаданные сервера авторизации и CORS.
	rec = f.get("/.well-known/oauth-authorization-server", nil)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != http.StatusOK || m["issuer"] != oauthIssuer || m["registration_endpoint"] != oauthIssuer+"/v1/oauth/register" ||
		m["client_id_metadata_document_supported"] != true || m["authorization_response_iss_parameter_supported"] != true ||
		fmt.Sprint(m["code_challenge_methods_supported"]) != "[S256]" || fmt.Sprint(m["token_endpoint_auth_methods_supported"]) != "[none]" ||
		fmt.Sprint(m["scopes_supported"]) != "[cadence offline_access]" || fmt.Sprint(m["response_types_supported"]) != "[code]" ||
		fmt.Sprint(m["grant_types_supported"]) != "[authorization_code refresh_token]" ||
		m["revocation_endpoint"] != oauthIssuer+"/v1/oauth/revoke" {
		t.Fatalf("AC-4: %d %v", rec.Code, m)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("AC-4 CORS: %v", rec.Header())
	}
	for _, p := range []string{"/v1/oauth/token", "/v1/oauth/register", "/v1/oauth/revoke"} {
		req := httptest.NewRequest("OPTIONS", p, nil)
		rec := f.send(req)
		if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("preflight %s: %d %v", p, rec.Code, rec.Header())
		}
	}

	// Выключенные DCR и CIMD — их нет в метаданных.
	off := newOAuthFixture(t, oauthSetup{enabled: true, noDCR: true, noCIMD: true, store: f.store})
	rec = off.get("/.well-known/oauth-authorization-server", nil)
	m = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if _, ok := m["registration_endpoint"]; ok || m["client_id_metadata_document_supported"] == true {
		t.Fatalf("AC-4 выключенные: %v", m)
	}
}

func mustAuth(t *testing.T, a *workflow.Auth, token string) workflow.Identity {
	t.Helper()
	id, err := a.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// AC-5: адрес из запроса, no-store, одно предупреждение на процесс.
func TestOAuthPublicAddress(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true, publicURL: "-"})
	req := httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil)
	req.Host = "cadence.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := f.send(req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["issuer"] != "https://cadence.example" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("из заголовков: %v %v", m["issuer"], rec.Header())
	}
	req = httptest.NewRequest("GET", "/.well-known/oauth-protected-resource/mcp/projects/SDLC", nil)
	req.Host = "cadence.example"
	req.Header.Set("X-Forwarded-Proto", "ftp")
	rec = f.send(req)
	if !strings.Contains(rec.Body.String(), `"resource":"http://cadence.example/mcp/projects/SDLC"`) {
		t.Fatalf("иное значение X-Forwarded-Proto: %s", rec.Body)
	}
	for range 2 {
		req := httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil)
		req.Host = "cadence.example"
		f.send(req)
	}
	if n := strings.Count(f.logs.String(), "публичный адрес OAuth — http"); n != 1 {
		t.Fatalf("предупреждений %d, ожидали одно", n)
	}

	// С SDLC_PUBLIC_URL адреса не зависят от Host.
	g := newOAuthFixture(t, oauthSetup{enabled: true, publicURL: "https://a.example", store: f.store})
	req = httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil)
	req.Host = "evil.example"
	rec = g.send(req)
	if !strings.Contains(rec.Body.String(), `"issuer":"https://a.example"`) || strings.Contains(rec.Body.String(), "evil") {
		t.Fatalf("SDLC_PUBLIC_URL: %s", rec.Body)
	}
}

// ── клиенты ─────────────────────────────────────────────────────────────────

// AC-8, AC-9: регистрация DCR и её пределы по базе.
func TestOAuthDCR(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	code, out := f.jsonCall("POST", "/v1/oauth/register", "", map[string]any{
		"client_name": "ChatGPT", "redirect_uris": []string{"https://chatgpt.com/cb", "http://127.0.0.1/cb"},
		"token_endpoint_auth_method": "client_secret_basic", "grant_types": []string{"authorization_code", "refresh_token"},
	})
	id, _ := out["client_id"].(string)
	raw, _ := base64.RawURLEncoding.DecodeString(id)
	if code != http.StatusCreated || len(raw) != 32 || out["token_endpoint_auth_method"] != "none" ||
		out["client_id_issued_at"] == nil || out["client_name"] != "ChatGPT" || out["client_secret"] != nil {
		t.Fatalf("AC-8: %d %v", code, out)
	}
	for body, want := range map[string]string{
		`{"redirect_uris":["http://example.com/cb"]}`:                           "invalid_redirect_uri",
		`{"redirect_uris":[]}`:                                                  "invalid_redirect_uri",
		`{"redirect_uris":["https://x.example/cb"],"grant_types":["implicit"]}`: "invalid_client_metadata",
		`{"redirect_uris":"https://x.example/cb"}`:                              "invalid_client_metadata",
		`not json`: "invalid_client_metadata",
	} {
		req := httptest.NewRequest("POST", "/v1/oauth/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := f.send(req)
		var e map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != http.StatusBadRequest || e["error"] != want {
			t.Errorf("AC-8 %s: %d %v", body, rec.Code, e)
		}
	}
	if n := f.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'oauth_clients' AND column_name ILIKE '%secret%'`); n != 0 {
		t.Fatalf("колонки секретов у oauth_clients: %d", n)
	}
	off := newOAuthFixture(t, oauthSetup{enabled: true, noDCR: true, store: f.store})
	if code, _ := off.jsonCall("POST", "/v1/oauth/register", "", map[string]any{"redirect_uris": []string{"https://x.example/cb"}}); code != http.StatusNotFound {
		t.Fatalf("AC-8 выключенный DCR: %d", code)
	}

	// AC-9: 60 за скользящий час — 61-я 429; второй сервер на той же базе
	// видит то же число.
	if _, err := f.store.DB().Exec(f.ctx, `
		INSERT INTO oauth_clients (id, client_name, redirect_uris, created_at)
		SELECT 'c' || g, 'c', ARRAY['https://x.example/cb'], $1::timestamptz - interval '10 minutes'
		FROM generate_series(1, 58) g`, f.now); err != nil {
		t.Fatal(err)
	}
	f.register("шестидесятый", "https://x.example/cb")
	second := newOAuthFixture(t, oauthSetup{enabled: true, store: f.store})
	second.now = f.now
	for _, s := range []*oauthFixture{f, second} {
		req := httptest.NewRequest("POST", "/v1/oauth/register", strings.NewReader(`{"redirect_uris":["https://x.example/cb"]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := s.send(req)
		ra := rec.Header().Get("Retry-After")
		if rec.Code != http.StatusTooManyRequests || ra == "" || ra == "0" {
			t.Fatalf("AC-9 час: %d Retry-After=%q %s", rec.Code, ra, rec.Body)
		}
	}
	// 1000 клиентов без живой выдачи — тоже 429, уже вне часового окна.
	if _, err := f.store.DB().Exec(f.ctx, `DELETE FROM oauth_clients`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(f.ctx, `
		INSERT INTO oauth_clients (id, client_name, redirect_uris, created_at)
		SELECT 'idle' || g, 'c', ARRAY['https://x.example/cb'], $1::timestamptz - interval '2 days'
		FROM generate_series(1, 1000) g`, f.now); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/oauth/register", strings.NewReader(`{"redirect_uris":["https://x.example/cb"]}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := f.send(req); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("AC-9 1000: %d %s", rec.Code, rec.Body)
	}
}

func (f *oauthFixture) oauthRows() int {
	return f.count(`SELECT (SELECT count(*) FROM oauth_authorization_requests) + (SELECT count(*) FROM oauth_grants) +
		(SELECT count(*) FROM oauth_refresh_tokens)`)
}

// AC-6, AC-7, AC-17: клиент по документу CIMD.
func TestOAuthCIMD(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	redirect := "https://claude.ai/api/mcp/auth_callback"
	verifier, challenge := pkcePair("cimd")
	rec := f.authorize(defaultAuthz(cimdClientID, redirect, challenge, oauthIssuer+"/mcp/projects/SDLC"))
	reqID := f.requestID(rec)
	var ttl time.Duration
	var name string
	if err := f.store.DB().QueryRow(f.ctx, `SELECT expires_at - created_at, client_name FROM oauth_authorization_requests WHERE id = $1`,
		reqID).Scan(&ttl, &name); err != nil || ttl != 10*time.Minute || name != "Claude" {
		t.Fatalf("AC-6: %s %q %v", ttl, name, err)
	}

	// AC-7: отказы — страница инстанса без Location на redirect_uri.
	bad := func(label string, p authzParams) {
		t.Helper()
		before, hits := f.oauthRows(), f.cimd.count()
		rec := f.authorize(p)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" ||
			!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("AC-7 %s: %d %v", label, rec.Code, rec.Header())
		}
		if f.oauthRows() != before {
			t.Errorf("AC-7 %s: строки OAuth добавились", label)
		}
		if strings.Contains(rec.Body.String(), "auth_callback") {
			t.Errorf("AC-7 %s: тело документа в ответе", label)
		}
		_ = hits
	}
	bad("чужой redirect_uri", defaultAuthz(cimdClientID, "https://evil.example/cb", challenge, ""))
	for _, id := range []string{"http://example.com/oauth/client.json", "https://example.com/oauth/client.json#x",
		"https://u:p@example.com/oauth/client.json", "https://example.com"} {
		hits := f.cimd.count()
		bad(id, defaultAuthz(id, redirect, challenge, ""))
		if f.cimd.count() != hits {
			t.Errorf("AC-7 %s: запрос к цели", id)
		}
	}
	// Адреса IPv6 со встроенным частным IPv4 (NAT64, 6to4) — тот же отказ.
	for _, id := range []string{"https://[64:ff9b::a00:1]/c", "https://[2002:a00:1::1]/c"} {
		hits := f.cimd.count()
		bad(id, defaultAuthz(id, redirect, challenge, ""))
		if f.cimd.count() != hits {
			t.Errorf("встроенный IPv4 %s: запрос к цели", id)
		}
	}
	f.cimd.mu.Lock()
	f.cimd.body = `{"client_id":"https://example.com/other.json","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`
	f.cimd.mu.Unlock()
	bad("client_id документа", defaultAuthz("https://example.com/oauth/other.json", redirect, challenge, ""))

	off := newOAuthFixture(t, oauthSetup{enabled: true, noCIMD: true, store: f.store})
	off.authorize(defaultAuthz(cimdClientID, redirect, challenge, ""))
	if rec := off.authorize(defaultAuthz(cimdClientID, redirect, challenge, "")); rec.Code != http.StatusBadRequest ||
		rec.Header().Get("Location") != "" || off.cimd.count() != 0 {
		t.Fatalf("AC-7 CIMD выключен: %d запросов %d", rec.Code, off.cimd.count())
	}

	// AC-17: заглушка остановлена после authorize — обмен проходит быстро.
	code := f.allow(reqID, f.alice)
	f.cimd.srv.Close()
	start := time.Now()
	status, out := f.exchange(cimdClientID, redirect, code, verifier)
	if status != http.StatusOK || time.Since(start) > time.Second {
		t.Fatalf("AC-17: %d %v за %s", status, out, time.Since(start))
	}
	g, tok := f.grantOf(out["access_token"].(string))
	if g.ClientKind != domain.OAuthClientCIMD || tok.Name != "Claude · claude.ai" {
		t.Fatalf("выдача CIMD: %+v %s", g, tok.Name)
	}
}

// AC-10, AC-11: сверка redirect_uri и параметры authorize.
func TestOAuthAuthorizeParams(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	client := f.register("Codex", "http://127.0.0.1/callback", "https://x.example/cb")
	_, challenge := pkcePair("p")

	for _, r := range []string{"http://127.0.0.1:61234/callback", "http://127.0.0.1/callback", "https://x.example/cb"} {
		f.requestID(f.authorize(defaultAuthz(client, r, challenge, "")))
	}
	before := f.oauthRows()
	for _, r := range []string{"http://127.0.0.1:61234/other", "http://127.0.0.1:61234/callback?x=1", "https://127.0.0.1:61234/callback",
		"https://x.example:8443/cb", "http://localhost:61234/callback", ""} {
		p := defaultAuthz(client, r, challenge, "")
		if rec := f.authorize(p); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("AC-10 %q: %d %q", r, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := f.authorize(defaultAuthz("unknown-client", "https://x.example/cb", challenge, "")); rec.Code != http.StatusBadRequest ||
		rec.Header().Get("Location") != "" {
		t.Errorf("неизвестный клиент: %d", rec.Code)
	}
	if f.oauthRows() != before {
		t.Fatal("AC-10: отказы оставили строки")
	}

	redirectErr := func(label string, p authzParams, want string) {
		t.Helper()
		rec := f.authorize(p)
		u, err := url.Parse(rec.Header().Get("Location"))
		if rec.Code != http.StatusFound || err != nil || !strings.HasPrefix(u.String(), "https://x.example/cb?") ||
			u.Query().Get("error") != want || u.Query().Get("state") != "st-1" || u.Query().Get("iss") != oauthIssuer {
			t.Errorf("AC-11 %s: %d %q", label, rec.Code, rec.Header().Get("Location"))
		}
	}
	p := defaultAuthz(client, "https://x.example/cb", challenge, "")
	delete(p, "code_challenge")
	redirectErr("без code_challenge", p, "invalid_request")
	p = defaultAuthz(client, "https://x.example/cb", challenge, "")
	p["code_challenge_method"] = "plain"
	redirectErr("plain", p, "invalid_request")
	p = defaultAuthz(client, "https://x.example/cb", challenge, "")
	p["response_type"] = "token"
	redirectErr("response_type", p, "unsupported_response_type")
	p = defaultAuthz(client, "https://x.example/cb", challenge, "")
	p["scope"] = "cadence admin"
	redirectErr("scope", p, "invalid_scope")
	for _, r := range []string{"https://other.example/mcp/projects/SDLC", oauthIssuer + "/v1/projects", oauthIssuer + "/mcp/projects/SDLC/x"} {
		redirectErr("resource "+r, defaultAuthz(client, "https://x.example/cb", challenge, r), "invalid_target")
	}
	// Без scope — оба, без resource — инстансовый без привязки.
	id := f.requestID(f.authorize(defaultAuthz(client, "https://x.example/cb", challenge, "")))
	var scope, resource string
	var key *string
	if err := f.store.DB().QueryRow(f.ctx, `SELECT scope, resource, project_key FROM oauth_authorization_requests WHERE id = $1`, id).
		Scan(&scope, &resource, &key); err != nil || scope != "cadence offline_access" || resource != oauthIssuer+"/mcp" || key != nil {
		t.Fatalf("AC-11 умолчания: %q %q %v %v", scope, resource, key, err)
	}
}

// ── согласие ────────────────────────────────────────────────────────────────

// AC-12, AC-13, AC-26.
func TestOAuthConsent(t *testing.T) {
	f := newOAuthFixture(t, oauthSetup{enabled: true})
	client := f.register("Codex", "http://127.0.0.1/callback")
	_, challenge := pkcePair("c")
	newReq := func(resource string) string {
		return f.requestID(f.authorize(defaultAuthz(client, "http://127.0.0.1:5000/callback", challenge, resource)))
	}

	id := newReq(oauthIssuer + "/mcp/projects/SDLC")
	if code, _ := f.jsonCall("GET", "/v1/oauth/requests/"+id, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("AC-12 без токена: %d", code)
	}
	code, out := f.jsonCall("GET", "/v1/oauth/requests/"+id, f.alice, nil)
	principal, _ := out["principal"].(map[string]any)
	if code != http.StatusOK || out["client_name"] != "Codex" || out["redirect_host"] != "127.0.0.1" || out["loopback"] != true ||
		out["project_key"] != "SDLC" || out["can_grant"] != true || principal["handle"] != "alice" {
		t.Fatalf("AC-12: %d %v", code, out)
	}
	for k := range out {
		if strings.Contains(k, "hash") || strings.Contains(k, "code") && k != "code_challenge" {
			t.Fatalf("AC-12: лишнее поле %s", k)
		}
	}
	if code, _ := f.jsonCall("GET", "/v1/oauth/requests/"+uuid.NewString(), f.alice, nil); code != http.StatusNotFound {
		t.Fatalf("AC-12 неизвестный: %d", code)
	}

	// AC-26: токен задачи — 403, выдачи нет.
	taskTok := f.taskToken()
	if code, out := f.jsonCall("POST", "/v1/oauth/requests/"+id, taskTok, map[string]any{"decision": "allow"}); code != http.StatusForbidden {
		t.Fatalf("AC-26: %d %v", code, out)
	}
	if n := f.count(`SELECT count(*) FROM oauth_grants`); n != 0 {
		t.Fatal("AC-26: выдача создана")
	}

	// AC-13: bob не участник — allow отказан с причиной, deny работает.
	code, out = f.jsonCall("GET", "/v1/oauth/requests/"+id, f.bob, nil)
	if code != http.StatusOK || out["can_grant"] != false || out["reason"] != workflow.ConsentNotMember {
		t.Fatalf("AC-13 чужой проект: %d %v", code, out)
	}
	if code, out := f.jsonCall("POST", "/v1/oauth/requests/"+id, f.bob, map[string]any{"decision": "allow"}); code != http.StatusForbidden {
		t.Fatalf("AC-13 allow не участника: %d %v", code, out)
	}
	code, out = f.jsonCall("POST", "/v1/oauth/requests/"+id, f.bob, map[string]any{"decision": "deny"})
	u, _ := url.Parse(fmt.Sprint(out["redirect_uri"]))
	if code != http.StatusOK || u.Query().Get("error") != "access_denied" || u.Query().Get("state") != "st-1" || u.Query().Get("iss") != oauthIssuer {
		t.Fatalf("AC-13 deny: %d %v", code, out)
	}
	if code, out := f.jsonCall("POST", "/v1/oauth/requests/"+id, f.alice, map[string]any{"decision": "allow"}); code != http.StatusConflict {
		t.Fatalf("AC-13 повтор решения: %d %v", code, out)
	}
	if code, out := f.jsonCall("GET", "/v1/oauth/requests/"+id, f.alice, nil); code != http.StatusConflict ||
		out["error"].(map[string]any)["details"].(map[string]any)["reason"] != "decided" {
		t.Fatalf("AC-12 решённый: %d %v", code, out)
	}

	// Allow: code, state, iss.
	id = newReq(oauthIssuer + "/mcp/projects/SDLC")
	code, out = f.jsonCall("POST", "/v1/oauth/requests/"+id, f.alice, map[string]any{"decision": "allow"})
	u, _ = url.Parse(fmt.Sprint(out["redirect_uri"]))
	if code != http.StatusOK || u.Host != "127.0.0.1:5000" || len(u.Query().Get("code")) < 43 || u.Query().Get("state") != "st-1" ||
		u.Query().Get("iss") != oauthIssuer {
		t.Fatalf("AC-13 allow: %d %v", code, out)
	}

	// Несуществующий проект — та же причина; администратор — любой
	// существующий проект.
	id = newReq(oauthIssuer + "/mcp/projects/NOPE")
	if _, out := f.jsonCall("GET", "/v1/oauth/requests/"+id, f.root, nil); out["can_grant"] != false {
		t.Fatalf("несуществующий проект: %v", out)
	}
	id = newReq(oauthIssuer + "/mcp/projects/SDLC")
	if _, out := f.jsonCall("GET", "/v1/oauth/requests/"+id, f.root, nil); out["can_grant"] != true {
		t.Fatalf("администратор: %v", out)
	}

	// Просроченный — 409.
	id = newReq("")
	f.now = f.now.Add(11 * time.Minute)
	if code, out := f.jsonCall("GET", "/v1/oauth/requests/"+id, f.alice, nil); code != http.StatusConflict ||
		out["error"].(map[string]any)["details"].(map[string]any)["reason"] != "expired" {
		t.Fatalf("AC-12 просроченный: %d %v", code, out)
	}
}

// taskToken — токен задачи держателя аренды SDLC-1 (агент pool).
func (f *oauthFixture) taskToken() string {
	f.t.Helper()
	code, out := f.jsonCall("POST", "/v1/auth/tokens", f.root, map[string]any{"name": "pool", "principal": "agent:pool", "project": "SDLC"})
	if code != http.StatusCreated {
		f.t.Fatalf("pool: %d %v", code, out)
	}
	pool := out["token"].(string)
	if code, out := f.jsonCall("POST", "/v1/tasks/SDLC-1/transitions/start", f.root, nil); code != http.StatusOK {
		f.t.Fatalf("старт: %d %v", code, out)
	}
	if code, out := f.jsonCall("POST", "/v1/tasks/SDLC-1/claim", pool, nil); code != http.StatusOK {
		f.t.Fatalf("захват: %d %v", code, out)
	}
	code, out = f.jsonCall("POST", "/v1/tasks/SDLC-1/task-token", pool, nil)
	if code != http.StatusCreated {
		f.t.Fatalf("токен задачи: %d %v", code, out)
	}
	return out["token"].(string)
}
