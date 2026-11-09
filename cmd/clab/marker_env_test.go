package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Окружение маркер-тестов CLAB-424: конфиг через XDG_CONFIG_HOME, без
// CLAB_CONFIG_DIR, SDLC_*; сервер — заглушки httptest по одной на хост с
// учётом запросов и заголовков Authorization.

type stubHost struct {
	srv    *httptest.Server
	token  string
	handle string
	key    string // ключ хоста в конфиге

	mu    sync.Mutex
	reqs  []string // METHOD путь?запрос
	auths []string
}

func newStubHost(t *testing.T, token, handle string) *stubHost {
	t.Helper()
	s := &stubHost{token: token, handle: handle}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	key, _, err := normalizeHost(s.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.key = key
	return s
}

func (s *stubHost) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Method+" "+r.URL.RequestURI())
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	authed := r.Header.Get("Authorization") == "Bearer "+s.token
	p := r.URL.Path
	switch {
	case p == "/v1/auth/cli" && r.Method == http.MethodPost:
		write(200, map[string]any{"user_code": "ABCD-1234", "poll_secret": "poll", "expires_at": time.Now().Add(time.Minute),
			"verify_path": "/login/cli", "interval_seconds": 1})
	case p == "/v1/auth/cli/poll":
		write(200, map[string]any{"status": "approved", "token": s.token, "record": map[string]any{"id": "tok-" + s.handle}})
	case !authed:
		write(401, map[string]any{"error": map[string]any{"code": "unauthenticated", "message": "токен не принят"}})
	case p == "/v1/auth/me":
		write(200, map[string]any{"principal": map[string]any{"kind": "user", "handle": s.handle}, "authenticated": true})
	case p == "/v1/instance":
		write(200, map[string]any{"version": "v0.0.0-stub"})
	case p == "/v1/projects":
		write(200, map[string]any{"projects": []map[string]string{{"key": "REMOTE"}}})
	case strings.HasPrefix(p, "/v1/auth/tokens/") && r.Method == http.MethodDelete:
		w.WriteHeader(204)
	case strings.HasPrefix(p, "/v1/projects/MISSING"):
		write(404, map[string]any{"error": map[string]any{"code": "not_found", "message": "проект не найден"}})
	case strings.HasSuffix(p, "/roles"):
		write(200, map[string]any{"roles": []any{}})
	case strings.HasPrefix(p, "/v1/projects/"):
		key := strings.Split(strings.TrimPrefix(p, "/v1/projects/"), "/")[0]
		write(200, map[string]any{"project": map[string]any{"key": key, "name": "Проект " + key}})
	default:
		write(404, map[string]any{"error": map[string]any{"code": "not_found", "message": p}})
	}
}

func (s *stubHost) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func (s *stubHost) sawAuth(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.auths {
		if a == "Bearer "+token {
			return true
		}
	}
	return false
}

func (s *stubHost) hc() HostConfig {
	return HostConfig{URL: s.srv.URL, Token: s.token, TokenID: "tok-" + s.handle, Handle: "user:" + s.handle}
}

// markerEnv — временное дерево и конфиг человека в нём; каталог процесса —
// корень дерева. Возвращает корень и путь config.json.
func markerEnv(t *testing.T, cfg Config) (root, configFile string) {
	t.Helper()
	root = markerTree(t)
	xdg := filepath.Join(root, "xdg")
	t.Setenv("CLAB_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("SDLC_API_URL", "")
	t.Setenv("SDLC_TOKEN", "")
	t.Setenv("SDLC_PROJECT", "")
	t.Setenv("CLAB_NO_BROWSER", "1")
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if cfg.Hosts != nil {
		if err := saveConfigAt(filepath.Join(xdg, "clab"), cfg); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(work)
	return work, filepath.Join(xdg, "clab", "config.json")
}

// twoHosts — конфиг с двумя входами: a — хост по умолчанию, b — второй.
func twoHosts(t *testing.T) (a, b *stubHost, cfg Config) {
	t.Helper()
	a = newStubHost(t, "sdlc_token_A", "alice")
	b = newStubHost(t, "sdlc_token_B", "bob")
	cfg = Config{DefaultHost: a.key, Hosts: map[string]HostConfig{a.key: a.hc(), b.key: b.hc()}}
	return a, b, cfg
}

func readCfg(t *testing.T, path string) Config {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// countMarkerReads подменяет чтение файлов маркера счётчиком.
func countMarkerReads(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := markerReadFile
	markerReadFile = func(path string) ([]byte, error) {
		n++
		return prev(path)
	}
	t.Cleanup(func() { markerReadFile = prev })
	return &n
}

func newMarkerClient(t *testing.T, g globals) *Client {
	t.Helper()
	c, err := NewClient(g)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}
