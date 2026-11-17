package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// AC-3: функция метаданных ресурса принимает путь MCP — инстансовый и
// проекта, resource — адрес побайтно.
func TestResourceFor(t *testing.T) {
	for _, path := range []string{"/mcp", "/mcp/projects/ACME", "/mcp/projects/A%20B"} {
		m := ResourceFor("https://a.example", path)
		if m.Resource != "https://a.example"+path || !slices.Equal(m.AuthorizationServers, []string{"https://a.example"}) ||
			!slices.Equal(m.BearerMethodsSupported, []string{"header"}) || !slices.Equal(m.ScopesSupported, SupportedScopes) {
			t.Fatalf("%s: %+v", path, m)
		}
		if got := ResourceMetadataURL("https://a.example", path); got != "https://a.example/.well-known/oauth-protected-resource"+path {
			t.Fatalf("адрес метаданных %s: %s", path, got)
		}
	}
}

// AC-4: registration_endpoint — только при DCR, признак CIMD — по выключателю.
func TestServerFor(t *testing.T) {
	m := ServerFor("https://a.example", true, true)
	if m.Issuer != "https://a.example" || m.AuthorizationEndpoint != "https://a.example/v1/oauth/authorize" ||
		m.TokenEndpoint != "https://a.example/v1/oauth/token" || m.RevocationEndpoint != "https://a.example/v1/oauth/revoke" ||
		m.RegistrationEndpoint != "https://a.example/v1/oauth/register" || !m.ClientIDMetadataDocumentSupported ||
		!slices.Equal(m.CodeChallengeMethodsSupported, []string{"S256"}) ||
		!slices.Equal(m.TokenEndpointAuthMethodsSupported, []string{"none"}) || !m.AuthorizationResponseISSParameter {
		t.Fatalf("метаданные: %+v", m)
	}
	m = ServerFor("https://a.example", false, false)
	if m.RegistrationEndpoint != "" || m.ClientIDMetadataDocumentSupported {
		t.Fatalf("выключенные способы: %+v", m)
	}
}

func TestNormalizeScope(t *testing.T) {
	for in, want := range map[string]string{
		"": "cadence offline_access", "cadence": "cadence", "offline_access": "cadence offline_access",
		"cadence offline_access": "cadence offline_access",
	} {
		if got, ok := NormalizeScope(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	if _, ok := NormalizeScope("cadence admin"); ok {
		t.Fatal("чужой скоуп принят")
	}
}

func TestPKCE(t *testing.T) {
	verifier := strings.Repeat("a", 43)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if !ValidChallenge(challenge) || !VerifyS256(verifier, challenge) {
		t.Fatal("верная пара не сошлась")
	}
	if VerifyS256(strings.Repeat("b", 43), challenge) || VerifyS256("short", challenge) || ValidChallenge("plain") {
		t.Fatal("неверная пара сошлась")
	}
}

func TestCleanName(t *testing.T) {
	if got := CleanName(" Claude\x00‮\n "); got != "Claude" {
		t.Fatalf("управляющие символы: %q", got)
	}
	if got := CleanName(strings.Repeat("я", 150)); len([]rune(got)) != MaxClientName {
		t.Fatalf("длина: %d", len([]rune(got)))
	}
}

// AC-5: публичный адрес — только схема и хост.
func TestParsePublicURL(t *testing.T) {
	for in, want := range map[string]string{"": "", "https://a.example": "https://a.example", "https://a.example/": "https://a.example",
		"http://localhost:8080": "http://localhost:8080"} {
		if got, err := ParsePublicURL(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"a.example", "https://a.example/path", "ftp://a.example", "https://u:p@a.example", "https://a.example?x=1", "https://"} {
		if _, err := ParsePublicURL(bad); err == nil {
			t.Errorf("%q принят", bad)
		}
	}
}

func TestRequestBase(t *testing.T) {
	r := httptest.NewRequest("GET", "http://cadence.example/x", nil)
	if got := RequestBase(r); got != "http://cadence.example" {
		t.Fatal(got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := RequestBase(r); got != "https://cadence.example" {
		t.Fatal(got)
	}
	r.Header.Set("X-Forwarded-Proto", "gopher")
	if got := RequestBase(r); got != "http://cadence.example" {
		t.Fatal("иное значение заголовка не проигнорировано:", got)
	}
	if !InsecureBase("http://cadence.example") || InsecureBase("http://127.0.0.1:8080") || InsecureBase("https://cadence.example") {
		t.Fatal("признак небезопасного адреса")
	}
}

// AC-10: побайтно; loopback по http — без порта, остальное совпадает.
func TestMatchRedirect(t *testing.T) {
	reg := []string{"https://claude.ai/api/mcp/auth_callback", "http://127.0.0.1/callback", "http://localhost:1/cb?x=1", "http://[::1]:5/p"}
	ok := []string{
		"https://claude.ai/api/mcp/auth_callback", "http://127.0.0.1:54321/callback", "http://127.0.0.1/callback",
		"http://localhost:9999/cb?x=1", "http://[::1]:7777/p",
	}
	bad := []string{
		"https://claude.ai:8443/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback/", "http://127.0.0.1:1/other",
		"https://127.0.0.1:1/callback", "http://localhost:9999/cb?x=2", "http://localhost:9999/cb", "http://127.0.0.2:1/callback",
		"http://localhost:1/callback", "http://127.0.0.1:1/callback#f",
	}
	for _, g := range ok {
		if !MatchRedirect(reg, g) {
			t.Errorf("не принят %s", g)
		}
	}
	for _, g := range bad {
		if MatchRedirect(reg, g) {
			t.Errorf("принят %s", g)
		}
	}
}

func TestCheckRedirectURI(t *testing.T) {
	for _, good := range []string{"https://x.example/cb", "http://127.0.0.1/cb", "http://localhost:3000/cb", "http://[::1]/cb"} {
		if err := CheckRedirectURI(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"http://example.com/cb", "https://x.example/cb#f", "/cb", "javascript:alert(1)", "https://u@x.example/cb", ""} {
		if err := CheckRedirectURI(bad); err == nil {
			t.Errorf("принят %s", bad)
		}
	}
}
