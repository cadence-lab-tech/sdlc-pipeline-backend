package httpapi

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/oidc"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Сведения об инстансе (CLAB-87). Базы не нужно: ответ собирается из
// настроек процесса, и ручка обязана отвечать, даже пока база недоступна.

// Известные значения секретов: тест ищет их в теле ответа.
const (
	secretGitHubClient = "gh-client-secret-5f1c"
	secretAdminToken   = "sdlc_admin-token-77aa"
	secretDBPassword   = "db-password-913e"
	secretAppKeyFile   = "/run/secrets/app-key-1b2c.pem"
)

// oidcStub — провайдер, который отвечает дискавери и ключами или падает с 500.
type oidcStub struct {
	srv  *httptest.Server
	down atomic.Bool
}

func newOIDCStub(t *testing.T) *oidcStub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	stub := &oidcStub{}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stub.down.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":                 stub.srv.URL,
				"authorization_endpoint": stub.srv.URL + "/authorize",
				"token_endpoint":         stub.srv.URL + "/token",
				"jwks_uri":               stub.srv.URL + "/jwks",
			})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kid": "k1", "kty": "RSA", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

type instanceSetup struct {
	mode     string
	tenancy  string
	creation string
	github   bool
	oidc     *oidcStub
	version  string
}

func instanceServer(t *testing.T, in instanceSetup) http.Handler {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	cfg := config.Config{
		Env: "test", AuthMode: in.mode, Tenancy: in.tenancy, ProjectCreation: in.creation,
		AdminToken:            secretAdminToken,
		GitHubAppClientID:     "Iv1.client",
		GitHubAppClientSecret: secretGitHubClient,
		GitHubAppKeyFile:      secretAppKeyFile,
		DatabaseURL:           "postgres://clab:" + secretDBPassword + "@db:5432/clab",
	}
	// Так же, как собирает cmd/api.
	auth := workflow.NewAuth(nil, log, cfg.AuthMode).
		OpenProjectCreation(cfg.ProjectCreation == "open").
		Tenancy(cfg.Tenancy)
	if in.oidc != nil {
		auth.UseOIDC(oidc.New(oidc.Config{Issuer: in.oidc.srv.URL, ClientID: "clab-web"}), 0)
	}
	if in.github {
		auth.UseGitHub(github.NewOAuth("", "", cfg.GitHubAppClientID, cfg.GitHubAppClientSecret), 0).
			GitHubApp("cadence-lab")
	}
	return NewServer(cfg, log, nil, nil, nil, auth, nil).WithVersion(in.version).Handler()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

type instanceView struct {
	Version         string `json:"version"`
	Tenancy         string `json:"tenancy"`
	ProjectCreation string `json:"project_creation"`
	Login           struct {
		Methods []string                   `json:"methods"`
		GitHub  map[string]json.RawMessage `json:"github"`
		OIDC    map[string]json.RawMessage `json:"oidc"`
	} `json:"login"`
}

func decodeInstance(t *testing.T, rec *httptest.ResponseRecorder) instanceView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/instance: %d %s", rec.Code, rec.Body)
	}
	var v instanceView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("тело не JSON: %s", rec.Body)
	}
	return v
}

// Критерии 1, 3, 4: в режиме токенов ручка открыта, отдаёт версию сборки,
// профиль, режим заведения проектов и настроенные способы входа — с теми же
// полями, что у старых ручек.
func TestInstanceOpenInTokenMode(t *testing.T) {
	provider := newOIDCStub(t)
	h := instanceServer(t, instanceSetup{
		mode: "token", tenancy: "single", creation: "teams",
		github: true, oidc: provider, version: "v0.5.0",
	})

	rec := get(t, h, "/v1/instance")
	v := decodeInstance(t, rec)
	if v.Version != "v0.5.0" || v.Tenancy != "single" || v.ProjectCreation != "teams" {
		t.Fatalf("сведения: %+v", v)
	}
	if strings.Join(v.Login.Methods, ",") != "token,github,oidc" {
		t.Fatalf("способы входа: %v", v.Login.Methods)
	}
	if string(v.Login.OIDC["available"]) != "true" || string(v.Login.GitHub["available"]) != "true" {
		t.Fatalf("доступность: github %s, oidc %s", v.Login.GitHub["available"], v.Login.OIDC["available"])
	}
	if got := rec.Header().Get(HeaderVersion); got != "v0.5.0" {
		t.Fatalf("заголовок версии: %q", got)
	}

	// Поля способа совпадают с ответом старой ручки один в один.
	for method, path := range map[string]string{"github": "/v1/auth/github", "oidc": "/v1/auth/oidc"} {
		old := get(t, h, path)
		if old.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, old.Code, old.Body)
		}
		var want map[string]json.RawMessage
		if err := json.Unmarshal(old.Body.Bytes(), &want); err != nil {
			t.Fatal(err)
		}
		got := v.Login.GitHub
		if method == "oidc" {
			got = v.Login.OIDC
		}
		for k, val := range want {
			if string(got[k]) != string(val) {
				t.Errorf("%s.%s: %s, у %s — %s", method, k, got[k], path, val)
			}
		}
	}

	// Старые ручки и пробы тоже несут заголовок версии.
	if got := get(t, h, "/healthz").Header().Get(HeaderVersion); got != "v0.5.0" {
		t.Fatalf("заголовок версии у /healthz: %q", got)
	}
}

// Критерии 1, 4: без версии сборки — dev; профиль и режим по умолчанию;
// ненастроенных способов нет ни в списке, ни полями.
func TestInstanceDefaults(t *testing.T) {
	h := instanceServer(t, instanceSetup{mode: "header", creation: "open"})
	rec := get(t, h, "/v1/instance")
	v := decodeInstance(t, rec)
	if v.Version != "dev" || v.Tenancy != "shared" || v.ProjectCreation != "open" {
		t.Fatalf("сведения: %+v", v)
	}
	if len(v.Login.Methods) != 0 || v.Login.GitHub != nil || v.Login.OIDC != nil {
		t.Fatalf("ненастроенные способы: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"methods":[]`) {
		t.Fatalf("список способов должен быть пустым массивом, а не null: %s", rec.Body)
	}
	if got := rec.Header().Get(HeaderVersion); got != "dev" {
		t.Fatalf("заголовок версии: %q", got)
	}
}

// Критерий 5: провайдер отвечает 500 — ручка всё равно 200, oidc в списке
// и помечен недоступным, остальное на месте. Старая ручка ведёт себя как
// раньше — ошибкой (критерий 7).
func TestInstanceOIDCDown(t *testing.T) {
	provider := newOIDCStub(t)
	provider.down.Store(true)
	h := instanceServer(t, instanceSetup{
		mode: "token", tenancy: "shared", creation: "open",
		github: true, oidc: provider, version: "v0.5.0",
	})

	v := decodeInstance(t, get(t, h, "/v1/instance"))
	if v.Version != "v0.5.0" || v.Tenancy != "shared" || v.ProjectCreation != "open" {
		t.Fatalf("сведения: %+v", v)
	}
	if strings.Join(v.Login.Methods, ",") != "token,github,oidc" {
		t.Fatalf("способы входа: %v", v.Login.Methods)
	}
	if string(v.Login.OIDC["available"]) != "false" || len(v.Login.OIDC["error"]) == 0 {
		t.Fatalf("oidc должен быть недоступен: %v", v.Login.OIDC)
	}
	if string(v.Login.OIDC["issuer"]) != `"`+provider.srv.URL+`"` {
		t.Fatalf("issuer недоступного провайдера: %s", v.Login.OIDC["issuer"])
	}
	if string(v.Login.GitHub["available"]) != "true" {
		t.Fatalf("github: %v", v.Login.GitHub)
	}

	if old := get(t, h, "/v1/auth/oidc"); old.Code != http.StatusInternalServerError {
		t.Fatalf("/v1/auth/oidc при лежащем провайдере: %d", old.Code)
	}
}

// Критерий 6: ни значений секретов, ни ключей, похожих на секрет.
func TestInstanceHasNoSecrets(t *testing.T) {
	provider := newOIDCStub(t)
	h := instanceServer(t, instanceSetup{
		mode: "token", creation: "open", github: true, oidc: provider, version: "v0.5.0",
	})
	body := get(t, h, "/v1/instance").Body.String()
	for _, secret := range []string{secretGitHubClient, secretAdminToken, secretDBPassword, secretAppKeyFile} {
		if strings.Contains(body, secret) {
			t.Fatalf("в ответе секрет %q: %s", secret, body)
		}
	}
	var tree any
	if err := json.Unmarshal([]byte(body), &tree); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(n any) {
		switch n := n.(type) {
		case map[string]any:
			for k, v := range n {
				lk := strings.ToLower(k)
				for _, bad := range []string{"secret", "password", "private_key"} {
					if strings.Contains(lk, bad) {
						t.Errorf("ключ %q в ответе", k)
					}
				}
				walk(v)
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(tree)
}
