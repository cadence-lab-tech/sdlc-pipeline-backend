package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Токен задачи и окружение агента (CLAB-213, решение 0049). Сервер — httptest
// вместо API, агент — скрипт-заглушка, который сохраняет, что получил.

const (
	runnerToken = "clab_runner_SECRET"
	taskTokenV  = "clab_task_SECRET"
	taskTokenID = "7d3c1a2e-0000-4000-8000-000000000001"
)

type apiCall struct {
	Method, Path, Auth, Actor, Body string
}

// fakeAPI — сервер раннера: очередь из одной задачи, захват, выпуск токена
// задачи с настраиваемым ответом, отпуск, стоимость, отзыв. Токен задачи он
// не принимает нигде: после перехода он мёртв, а раннер обязан обходиться
// своим.
type fakeAPI struct {
	srv    *httptest.Server
	issue  func(w http.ResponseWriter) // ответ на POST …/task-token
	revoke func(w http.ResponseWriter) // ответ на DELETE /auth/tokens/:id
	// claim — ответ захвата; nil — прогон роли developer без уровня и
	// без execution, как у сервера прежней версии.
	claim map[string]any

	mu    sync.Mutex
	calls []apiCall
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{}
	f.issue = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"token":            taskTokenV,
			"record":           map[string]any{"id": taskTokenID, "kind": "task"},
			"task_key":         "CLAB-1",
			"lease_expires_at": time.Now().Add(time.Hour),
		})
	}
	// Токен погас на переходе, но запись есть: отзыв ставит revoked_at.
	f.revoke = func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	call := apiCall{Method: req.Method, Path: req.URL.Path, Auth: req.Header.Get("Authorization"),
		Actor: req.Header.Get("X-SDLC-Actor"), Body: string(body)}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	if call.Auth == "Bearer "+taskTokenV {
		domainError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	switch {
	case req.Method == http.MethodGet && call.Path == "/v1/projects/CLAB/queue":
		json.NewEncoder(w).Encode(map[string]any{"queue": []any{map[string]any{
			"task": map[string]any{"key": "CLAB-1"}, "run": map[string]any{"id": "run-1"},
			"role_key": "developer", "claimable": true,
		}}})
	case call.Path == "/v1/tasks/CLAB-1/claim":
		reply := f.claim
		if reply == nil {
			reply = map[string]any{"stage_run_id": "run-1", "role": map[string]any{"key": "developer"}}
		}
		json.NewEncoder(w).Encode(reply)
	case call.Path == "/v1/tasks/CLAB-1/task-token":
		f.issue(w)
	case call.Path == "/v1/tasks/CLAB-1/release", call.Path == "/v1/tasks/CLAB-1/heartbeat",
		call.Path == "/v1/tasks/CLAB-1/runs/run-1/usage":
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}"))
	case req.Method == http.MethodDelete && call.Path == "/v1/auth/tokens/"+taskTokenID:
		f.revoke(w)
	default:
		http.NotFound(w, req) // как gin на неизвестный маршрут: текст, без кода
	}
}

func domainError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": code}})
}

func (f *fakeAPI) seen() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiCall{}, f.calls...)
}

func (f *fakeAPI) find(method, path string) []apiCall {
	var out []apiCall
	for _, c := range f.seen() {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// stubAgent — агент-заглушка: копирует mcp.json и конфиг clab, пишет своё
// окружение и выходит с заданным кодом. Пишет в каталог out.
func stubAgent(t *testing.T, script string) (command, out string) {
	t.Helper()
	dir := t.TempDir()
	out = filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent.sh")
	body := "#!/bin/sh\nOUT=" + out + "\n" +
		`echo "$1" > "$OUT/mcp_path"` + "\n" +
		`cp "$1" "$OUT/mcp.json"` + "\n" +
		`ls -A "$CLAB_CONFIG_DIR" > "$OUT/clab_ls"` + "\n" +
		`cp "$CLAB_CONFIG_DIR/config.json" "$OUT/config.json"` + "\n" +
		`env > "$OUT/env"` + "\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return "/bin/sh " + path + " {mcp_config} {prompt}", out
}

const agentOK = `echo '{"total_cost_usd": 0.5, "model": "claude-sonnet-5"}'`

// parentEnv — окружение раннера, из которого агенту положено получить только
// разрешённое.
var parentEnv = []string{
	"PATH=/usr/bin:/bin", "HOME=/home/dev", "ANTHROPIC_API_KEY=sk-ant",
	"SDLC_TOKEN=" + runnerToken, "SDLC_ACTOR=agent:dev", "SDLC_RUNNER_ROLES=developer",
	"CLAB_CONFIG_DIR=/home/dev/.config/clab", "GH_TOKEN=ghp_secret", "FOO_SECRET=foo",
}

func testRunner(t *testing.T, api *fakeAPI, command string, logs *bytes.Buffer) *runner {
	t.Helper()
	cfg := Config{
		APIURL: api.srv.URL + "/v1", Token: runnerToken, Actor: "agent:runner", Project: "CLAB",
		Slots: 1, Interval: time.Hour, Timeout: 20 * time.Second, Command: command, Once: true,
	}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := newRunner(cfg, log)
	r.environ = func() []string { return parentEnv }
	return r
}

func readOut(t *testing.T, out, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(out, name))
	if err != nil {
		t.Fatalf("агент не оставил %s: %v", name, err)
	}
	return string(raw)
}

func envOf(t *testing.T, out string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(readOut(t, out, "env"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env
}

func mcpHeaders(t *testing.T, out string) map[string]string {
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readOut(t, out, "mcp.json")), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("в mcp.json агента ровно один сервер: %+v", cfg.MCPServers)
	}
	return cfg.MCPServers["sdlc"].Headers
}

func clabConfigOf(t *testing.T, out string) clabConfigFile {
	var cfg clabConfigFile
	if err := json.Unmarshal([]byte(readOut(t, out, "config.json")), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// clabConfigFile — формат конфига clab (cmd/clab/config.go: Config, HostConfig).
type clabConfigFile struct {
	DefaultHost string `json:"default_host"`
	Hosts       map[string]struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"hosts"`
}

// Обычный проход: claim → task-token → агент с токеном задачи → стоимость
// токеном раннера → отзыв. Критерии 2, 3, 5, 6, 8, 10, 11, 15.
func TestAgentGetsTaskToken(t *testing.T) {
	api := newFakeAPI(t)
	command, out := stubAgent(t, agentOK)
	var logs bytes.Buffer
	r := testRunner(t, api, command, &logs)

	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Последовательность запросов и чьим токеном.
	var seq []string
	for _, c := range api.seen() {
		if c.Auth != "Bearer "+runnerToken {
			t.Fatalf("раннер ходит только своим токеном, а %s %s пришёл с %q", c.Method, c.Path, c.Auth)
		}
		seq = append(seq, c.Method+" "+c.Path)
	}
	want := []string{
		"GET /v1/projects/CLAB/queue",
		"POST /v1/tasks/CLAB-1/claim",
		"POST /v1/tasks/CLAB-1/task-token",
		"POST /v1/tasks/CLAB-1/runs/run-1/usage",
		"DELETE /v1/auth/tokens/" + taskTokenID,
	}
	if !slices.Equal(seq, want) {
		t.Fatalf("запросы раннера:\n%s\nожидались:\n%s", strings.Join(seq, "\n"), strings.Join(want, "\n"))
	}

	// mcp.json — токен задачи, без токена раннера и актора.
	headers := mcpHeaders(t, out)
	if headers["Authorization"] != "Bearer "+taskTokenV || len(headers) != 1 {
		t.Fatalf("заголовки mcp.json агента: %v", headers)
	}
	if raw := readOut(t, out, "mcp.json"); strings.Contains(raw, runnerToken) || strings.Contains(raw, "X-SDLC-Actor") {
		t.Fatalf("в mcp.json агента токен раннера или актор: %s", raw)
	}

	// Окружение — по списку.
	env := envOf(t, out)
	for k, v := range map[string]string{
		"PATH": "/usr/bin:/bin", "HOME": "/home/dev", "ANTHROPIC_API_KEY": "sk-ant",
		"SDLC_API_URL": api.srv.URL + "/v1", "SDLC_PROJECT": "CLAB", "SDLC_TOKEN": taskTokenV,
	} {
		if env[k] != v {
			t.Errorf("агент получил %s=%q, а не %q", k, env[k], v)
		}
	}
	for _, k := range []string{"SDLC_ACTOR", "SDLC_RUNNER_ROLES", "GH_TOKEN", "FOO_SECRET"} {
		if _, ok := env[k]; ok {
			t.Errorf("агенту не положено %s", k)
		}
	}
	if strings.Contains(readOut(t, out, "env"), runnerToken) {
		t.Error("токен раннера в окружении агента")
	}

	// CLAB_CONFIG_DIR — временный каталог с одним config.json токеном задачи.
	clabDir := env["CLAB_CONFIG_DIR"]
	mcpPath := strings.TrimSpace(readOut(t, out, "mcp_path"))
	if clabDir == "/home/dev/.config/clab" || filepath.Dir(clabDir) != filepath.Dir(mcpPath) {
		t.Fatalf("CLAB_CONFIG_DIR агента %q не рядом с mcp.json %q", clabDir, mcpPath)
	}
	if ls := strings.TrimSpace(readOut(t, out, "clab_ls")); ls != "config.json" {
		t.Fatalf("в каталоге конфига clab агента лишнее: %q", ls)
	}
	cfg := clabConfigOf(t, out)
	host := strings.TrimPrefix(api.srv.URL, "http://")
	if cfg.DefaultHost != host || cfg.Hosts[host].URL != api.srv.URL || cfg.Hosts[host].Token != taskTokenV || len(cfg.Hosts) != 1 {
		t.Fatalf("конфиг clab агента: %+v", cfg)
	}

	// Всё временное убрано.
	if _, err := os.Stat(filepath.Dir(mcpPath)); !os.IsNotExist(err) {
		t.Fatalf("временный каталог агента остался: %v", err)
	}

	// Токен задачи нигде в журнале раннера.
	if strings.Contains(logs.String(), taskTokenV) || strings.Contains(logs.String(), runnerToken) {
		t.Fatalf("токен в журнале раннера:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "credentials=task") {
		t.Fatalf("журнал не говорит, чем ходит агент:\n%s", logs.String())
	}
}

// Права и рамки каталога агента: 0700 и 0600 (критерий 8).
func TestAgentHomePermissions(t *testing.T) {
	r := newRunner(Config{APIURL: "https://cadence.example.com/v1", Project: "CLAB"}, slog.Default())
	home, err := r.agentHome(agentCreds{token: taskTokenV, tokenID: taskTokenID})
	if err != nil {
		t.Fatal(err)
	}
	defer home.cleanup()
	for path, want := range map[string]os.FileMode{
		home.root: 0o700, home.clabDir: 0o700, home.mcpConfig: 0o600,
		filepath.Join(home.clabDir, "config.json"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: права %o, а не %o", path, got, want)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(home.clabDir, "config.json"))
	var cfg clabConfigFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultHost != "cadence.example.com" || cfg.Hosts["cadence.example.com"].URL != "https://cadence.example.com" {
		t.Fatalf("хост конфига clab — без /v1: %+v", cfg)
	}
	home.cleanup()
	if _, err := os.Stat(home.root); !os.IsNotExist(err) {
		t.Fatal("cleanup не убрал каталог")
	}
}

// Агент упал или вышел по таймауту: задача отпущена, токен отозван, каталог
// убран (критерий 11).
func TestAgentFailureRevokes(t *testing.T) {
	for name, tc := range map[string]struct {
		script  string
		timeout time.Duration
	}{
		"ошибка":  {script: "exit 3", timeout: 20 * time.Second},
		"таймаут": {script: "exec sleep 30", timeout: 500 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			command, out := stubAgent(t, tc.script)
			var logs bytes.Buffer
			r := testRunner(t, api, command, &logs)
			r.cfg.Timeout = tc.timeout

			if err := r.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(api.find(http.MethodPost, "/v1/tasks/CLAB-1/release")) != 1 {
				t.Fatalf("задача не отпущена: %+v", api.seen())
			}
			if len(api.find(http.MethodDelete, "/v1/auth/tokens/"+taskTokenID)) != 1 {
				t.Fatalf("токен задачи не отозван: %+v", api.seen())
			}
			if len(api.find(http.MethodPost, "/v1/tasks/CLAB-1/runs/run-1/usage")) != 0 {
				t.Fatal("упавший агент не отчитывается о стоимости")
			}
			mcpPath := strings.TrimSpace(readOut(t, out, "mcp_path"))
			if _, err := os.Stat(filepath.Dir(mcpPath)); !os.IsNotExist(err) {
				t.Fatalf("временный каталог агента остался: %v", err)
			}
		})
	}
}

// Отзыв уже мёртвого токена — штатный исход, не предупреждение (критерий 10).
func TestRevokeDeadTokenIsQuiet(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized} {
		api := newFakeAPI(t)
		api.revoke = func(w http.ResponseWriter) {
			domainError(w, status, map[int]string{404: "not_found", 401: "unauthenticated"}[status])
		}
		command, _ := stubAgent(t, agentOK)
		var logs bytes.Buffer
		r := testRunner(t, api, command, &logs)
		if err := r.run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "level=ERROR") {
			t.Fatalf("%d на отзыв не повод шуметь:\n%s", status, logs.String())
		}
	}
}

// Сервер прежней версии: ручки нет — агент работает токеном раннера с
// предупреждением, окружение всё равно чистится (критерий 12).
func TestOldServerFallsBackToRunnerToken(t *testing.T) {
	api := newFakeAPI(t)
	api.issue = func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound); w.Write([]byte("404 page not found")) }
	command, out := stubAgent(t, agentOK)
	var logs bytes.Buffer
	r := testRunner(t, api, command, &logs)

	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := mcpHeaders(t, out); h["Authorization"] != "Bearer "+runnerToken {
		t.Fatalf("со старым сервером агент получает токен раннера: %v", h)
	}
	env := envOf(t, out)
	if env["SDLC_TOKEN"] != runnerToken {
		t.Fatalf("SDLC_TOKEN агента: %q", env["SDLC_TOKEN"])
	}
	host := strings.TrimPrefix(api.srv.URL, "http://")
	if cfg := clabConfigOf(t, out); cfg.Hosts[host].Token != runnerToken {
		t.Fatalf("конфиг clab агента: %+v", cfg)
	}
	for _, k := range []string{"SDLC_ACTOR", "SDLC_RUNNER_ROLES", "GH_TOKEN", "FOO_SECRET"} {
		if _, ok := env[k]; ok {
			t.Errorf("окружение чистится и со старым сервером, а %s прошло", k)
		}
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "токен задачи") {
		t.Fatalf("нет предупреждения о старом сервере:\n%s", logs.String())
	}
	if len(api.find(http.MethodDelete, "/v1/auth/tokens/"+taskTokenID)) != 0 {
		t.Fatal("отзывать нечего: токен задачи не выпускался")
	}
	if len(api.find(http.MethodPost, "/v1/tasks/CLAB-1/runs/run-1/usage")) != 1 {
		t.Fatal("отчёт о стоимости со старым сервером не отправлен")
	}
}

// Режим заголовка, старый сервер: агенту — актор раннера, как раньше.
func TestOldServerHeaderModeGivesActor(t *testing.T) {
	api := newFakeAPI(t)
	api.issue = func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }
	command, out := stubAgent(t, agentOK)
	var logs bytes.Buffer
	r := testRunner(t, api, command, &logs)
	r.cfg.Token = ""
	r.api = newClient(r.cfg)

	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := mcpHeaders(t, out); h["X-SDLC-Actor"] != "agent:runner" || h["Authorization"] != "" {
		t.Fatalf("mcp.json в режиме заголовка: %v", h)
	}
	env := envOf(t, out)
	if env["SDLC_ACTOR"] != "agent:runner" || env["SDLC_TOKEN"] != "" {
		t.Fatalf("окружение агента в режиме заголовка: actor=%q token=%q", env["SDLC_ACTOR"], env["SDLC_TOKEN"])
	}
}

// Режим заголовка, новый сервер: выпуск — с актором, агенту — токен задачи.
func TestHeaderModeIssuesWithActor(t *testing.T) {
	api := newFakeAPI(t)
	command, out := stubAgent(t, agentOK)
	var logs bytes.Buffer
	r := testRunner(t, api, command, &logs)
	r.cfg.Token = ""
	r.api = newClient(r.cfg)

	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	issue := api.find(http.MethodPost, "/v1/tasks/CLAB-1/task-token")
	if len(issue) != 1 || issue[0].Actor != "agent:runner" || issue[0].Auth != "" {
		t.Fatalf("выпуск в режиме заголовка: %+v", issue)
	}
	if h := mcpHeaders(t, out); h["Authorization"] != "Bearer "+taskTokenV || len(h) != 1 {
		t.Fatalf("mcp.json: %v", h)
	}
	if _, ok := envOf(t, out)["SDLC_ACTOR"]; ok {
		t.Fatal("с токеном задачи актор агенту не нужен")
	}
}

// Любой другой отказ выпуска — агент не запускается, задача отпущена с
// кодом в заметке, токен раннера агенту не уходит (критерий 13).
func TestIssueRefusalReleases(t *testing.T) {
	for name, issue := range map[string]func(w http.ResponseWriter){
		"lease_expired":       func(w http.ResponseWriter) { domainError(w, http.StatusConflict, "lease_expired") },
		"lease_held_by_other": func(w http.ResponseWriter) { domainError(w, http.StatusConflict, "lease_held_by_other") },
		"forbidden":           func(w http.ResponseWriter) { domainError(w, http.StatusForbidden, "forbidden") },
		"not_found":           func(w http.ResponseWriter) { domainError(w, http.StatusNotFound, "not_found") },
		"internal":            func(w http.ResponseWriter) { domainError(w, http.StatusInternalServerError, "internal") },
		"Internal Server Error": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"без идентификатора": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"token":"` + taskTokenV + `"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.issue = issue
			command, out := stubAgent(t, agentOK)
			var logs bytes.Buffer
			r := testRunner(t, api, command, &logs)

			if err := r.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(out, "env")); !os.IsNotExist(err) {
				t.Fatal("агент запущен, хотя токен задачи не выпущен")
			}
			rel := api.find(http.MethodPost, "/v1/tasks/CLAB-1/release")
			if len(rel) != 1 {
				t.Fatalf("задача не отпущена: %+v", api.seen())
			}
			var body struct{ Note string }
			json.Unmarshal([]byte(rel[0].Body), &body)
			code := name
			if name == "без идентификатора" {
				code = "без значения или идентификатора"
			}
			if !strings.Contains(body.Note, "токен задачи") || !strings.Contains(body.Note, code) {
				t.Fatalf("заметка отпуска: %q", body.Note)
			}
			if strings.Contains(body.Note, taskTokenV) || strings.Contains(logs.String(), taskTokenV) {
				t.Fatal("токен задачи в заметке или журнале")
			}
		})
	}
}

// Сухой прогон токен не выпускает (критерий 14).
func TestDryRunIssuesNothing(t *testing.T) {
	api := newFakeAPI(t)
	command, out := stubAgent(t, agentOK)
	var logs bytes.Buffer
	r := testRunner(t, api, command, &logs)
	r.cfg.DryRun = true

	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.find(http.MethodPost, "/v1/tasks/CLAB-1/task-token")) != 0 {
		t.Fatal("сухой прогон выпустил токен задачи")
	}
	if len(api.find(http.MethodPost, "/v1/tasks/CLAB-1/release")) != 1 {
		t.Fatal("сухой прогон отпускает задачу")
	}
	if _, err := os.Stat(filepath.Join(out, "env")); !os.IsNotExist(err) {
		t.Fatal("сухой прогон запустил агента")
	}
	if !strings.Contains(logs.String(), "токен задачи") || strings.Contains(logs.String(), runnerToken) {
		t.Fatalf("журнал сухого прогона:\n%s", logs.String())
	}
}

// Окружение агента по списку (критерии 6, 7).
func TestAgentEnvAllowlist(t *testing.T) {
	set := map[string]string{"SDLC_API_URL": "https://c.example.com/v1", "SDLC_PROJECT": "CLAB",
		"SDLC_TOKEN": taskTokenV, "CLAB_CONFIG_DIR": "/tmp/clab-runner-1/clab"}
	parent := append(append([]string{}, parentEnv...),
		"LC_ALL=C.UTF-8", "XDG_RUNTIME_DIR=/run/user/1", "https_proxy=http://proxy:3128",
		"SSH_AUTH_SOCK=/tmp/ssh", "CLAUDE_CODE_USE_BEDROCK=1", "AWS_REGION=eu-west-1", "CLAB_X=1")

	got := map[string]string{}
	for _, kv := range agentEnv(parent, nil, set) {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	want := map[string]string{
		"PATH": "/usr/bin:/bin", "HOME": "/home/dev", "ANTHROPIC_API_KEY": "sk-ant",
		"LC_ALL": "C.UTF-8", "XDG_RUNTIME_DIR": "/run/user/1", "https_proxy": "http://proxy:3128",
		"SSH_AUTH_SOCK": "/tmp/ssh", "CLAUDE_CODE_USE_BEDROCK": "1",
		"SDLC_API_URL": "https://c.example.com/v1", "SDLC_PROJECT": "CLAB",
		"SDLC_TOKEN": taskTokenV, "CLAB_CONFIG_DIR": "/tmp/clab-runner-1/clab",
	}
	if len(got) != len(want) {
		t.Fatalf("окружение агента:\n%v\nожидалось:\n%v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%q, ожидалось %q", k, got[k], v)
		}
	}

	// SDLC_RUNNER_ENV расширяет список, но SDLC_* и CLAB_* не пускает.
	allow, refused := parseAgentEnv(" GH_TOKEN, FOO_* ,SDLC_ACTOR,CLAB_X,sdlc_lower, ")
	if !slices.Equal(allow, []string{"GH_TOKEN", "FOO_*"}) || !slices.Equal(refused, []string{"SDLC_ACTOR", "CLAB_X", "sdlc_lower"}) {
		t.Fatalf("разбор SDLC_RUNNER_ENV: allow=%v refused=%v", allow, refused)
	}
	got = map[string]string{}
	for _, kv := range agentEnv(parent, append(allow, "SDLC_ACTOR", "*"), set) {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["GH_TOKEN"] != "ghp_secret" || got["FOO_SECRET"] != "foo" {
		t.Fatalf("SDLC_RUNNER_ENV не расширил окружение: %v", got)
	}
	for _, k := range []string{"SDLC_ACTOR", "SDLC_RUNNER_ROLES", "CLAB_X"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s не проходит никогда, даже по шаблону", k)
		}
	}
	if got["SDLC_TOKEN"] != taskTokenV || got["CLAB_CONFIG_DIR"] != "/tmp/clab-runner-1/clab" {
		t.Fatalf("заданное раннером перекрывает окружение: %v", got)
	}
}

// SDLC_RUNNER_ENV читается конфигом, запрещённые имена откладываются для
// предупреждения при старте (критерий 7).
func TestLoadConfigAgentEnv(t *testing.T) {
	t.Setenv("SDLC_API_URL", "https://c.example.com/v1")
	t.Setenv("SDLC_PROJECT", "CLAB")
	t.Setenv("SDLC_RUNNER_ENV", "GH_TOKEN,AWS_*,SDLC_ACTOR,CLAB_X")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.AgentEnv, []string{"GH_TOKEN", "AWS_*"}) || !slices.Equal(cfg.AgentEnvRefused, []string{"SDLC_ACTOR", "CLAB_X"}) {
		t.Fatalf("AgentEnv=%v AgentEnvRefused=%v", cfg.AgentEnv, cfg.AgentEnvRefused)
	}
}

// Команда по умолчанию изолирует MCP агента (критерий 4).
func TestDefaultCommandStrictMCP(t *testing.T) {
	r := newRunner(Config{Project: "CLAB", Command: defaultCommand}, slog.Default())
	item := queueItem{RoleKey: "developer"}
	item.Task.Key = "CLAB-1"
	argv := strings.Join(r.argv(item, claimResult{}, "/tmp/x/mcp.json", ""), " ")
	if !strings.Contains(argv, "--mcp-config /tmp/x/mcp.json --strict-mcp-config") {
		t.Fatalf("команда по умолчанию без --strict-mcp-config: %s", argv)
	}
}
