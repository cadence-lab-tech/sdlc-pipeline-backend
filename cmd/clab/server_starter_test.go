package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Тесты стартового проекта (CLAB-199): API установки — подставной сервер
// httptest с правилами импорта и активации сервера (unchanged у той же
// роли и того же цикла, черновик у нового цикла, активация уводит на покой
// активный цикл тех же типов), Docker — подставной исполнитель. Номера —
// критерии приёмки CLAB-199.

const testStarterBundle = `{"version":1,
 "roles":[{"key":"analyst","name":"Analyst","instructions":"analyse"},{"key":"developer","name":"Developer","instructions":"build"}],
 "workflows":[{"name":"Starter","task_types":["task"],"statuses":[{"key":"new"}]},
              {"name":"Starter epics","task_types":["epic"],"statuses":[{"key":"open"}]}]}`

// Связка других имён (AC-16): шаг не знает ни ролей, ни циклов.
const otherNamesBundle = `{"version":1,
 "roles":[{"key":"writer","name":"Writer","instructions":"write"}],
 "workflows":[{"name":"Flow","task_types":["task"],"statuses":[{"key":"a"}]},
              {"name":"Flow epics","task_types":["epic"],"statuses":[{"key":"b"}]}]}`

const testAdminToken = "sdlc_starter_admin_token_0123456789"

type fakeWorkflow struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Version   int      `json:"version"`
	State     string   `json:"state"`
	TaskTypes []string `json:"task_types"`
	project   string
	body      string
}

// fakeAPI — ручки установки, которыми ходит шаг.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	version     string
	token       string
	projects    map[string]string            // ключ → команда
	roles       map[string]map[string]string // проект → роль → инструкция
	workflows   []*fakeWorkflow
	nextID      int
	imports     int // не dry_run
	dryImports  int
	activations int
	instanceHit int
	failImport  int // код ответа на импорт не dry_run
	redirect    bool
	auth        []string
	sni         []string
	created     []map[string]string
}

func newFakeAPI(t *testing.T, version string, tlsServer bool) *fakeAPI {
	t.Helper()
	a := &fakeAPI{t: t, version: version, token: testAdminToken,
		projects: map[string]string{}, roles: map[string]map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/instance", a.instance)
	mux.HandleFunc("GET /v1/projects/{key}", a.getProject)
	mux.HandleFunc("POST /v1/projects", a.createProject)
	mux.HandleFunc("POST /v1/projects/{key}/import", a.importBundle)
	mux.HandleFunc("GET /v1/projects/{key}/workflows", a.listWorkflows)
	mux.HandleFunc("POST /v1/workflows/{id}/activate", a.activate)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.auth = append(a.auth, r.Header.Get("Authorization"))
		if r.TLS != nil {
			a.sni = append(a.sni, r.TLS.ServerName)
		}
		redirect := a.redirect
		a.mu.Unlock()
		if redirect {
			http.Redirect(w, r, "http://203.0.113.9/v1/instance", http.StatusFound)
			return
		}
		if h := r.Header.Get("Authorization"); h == "Bearer " || (a.token != "" && h != "Bearer "+a.token) || (a.token == "" && !strings.HasPrefix(h, "Bearer sdlc_")) {
			apiErr(w, http.StatusUnauthorized, "unauthorized", "нужен токен")
			return
		}
		mux.ServeHTTP(w, r)
	})
	a.srv = httptest.NewUnstartedServer(h)
	// Отказы рукопожатия — часть проверок, а не шум в выводе тестов.
	a.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	if tlsServer {
		a.srv.StartTLS()
	} else {
		a.srv.Start()
	}
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAPI) addr() string { return a.srv.Listener.Addr().String() }

func apiErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func apiJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *fakeAPI) instance(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	a.instanceHit++
	a.mu.Unlock()
	apiJSON(w, 200, map[string]any{"version": a.version, "tenancy": "single"})
}

func (a *fakeAPI) getProject(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	team, ok := a.projects[r.PathValue("key")]
	if !ok {
		apiErr(w, 404, "not_found", "проект не найден")
		return
	}
	apiJSON(w, 200, map[string]any{"project": map[string]any{"key": r.PathValue("key"), "owner_team_key": team}})
}

func (a *fakeAPI) createProject(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	json.NewDecoder(r.Body).Decode(&req)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.created = append(a.created, req)
	if req["team"] == "" {
		apiErr(w, 400, "invalid_input", "нужна команда")
		return
	}
	a.projects[req["key"]] = req["team"]
	apiJSON(w, 201, map[string]any{"key": req["key"], "name": req["name"]})
}

func (a *fakeAPI) importBundle(w http.ResponseWriter, r *http.Request) {
	key, dry := r.PathValue("key"), r.URL.Query().Get("dry_run") == "true"
	var b struct {
		Roles []struct {
			Key          string `json:"key"`
			Instructions string `json:"instructions"`
		} `json:"roles"`
		Workflows []json.RawMessage `json:"workflows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		apiErr(w, 400, "invalid_input", "связка не разбирается")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if dry {
		a.dryImports++
	} else {
		a.imports++
		if a.failImport != 0 {
			// Сервер, который цитирует токен: шаг обязан его скрыть.
			apiErr(w, a.failImport, "internal", "сбой импорта "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			return
		}
	}
	if _, ok := a.projects[key]; !ok {
		apiErr(w, 404, "not_found", "проект не найден")
		return
	}
	if a.roles[key] == nil {
		a.roles[key] = map[string]string{}
	}
	type entry struct {
		Key     string `json:"key"`
		Outcome string `json:"outcome"`
		ID      string `json:"id"`
	}
	rep := map[string]any{"dry_run": dry}
	var roles, flows []entry
	for _, role := range b.Roles {
		have, ok := a.roles[key][role.Key]
		switch {
		case !ok:
			roles = append(roles, entry{role.Key, "created", "r-" + role.Key})
		case have == role.Instructions:
			roles = append(roles, entry{role.Key, "unchanged", "r-" + role.Key})
			continue
		default:
			roles = append(roles, entry{role.Key, "new_version", "r-" + role.Key})
		}
		if !dry {
			a.roles[key][role.Key] = role.Instructions
		}
	}
	for _, raw := range b.Workflows {
		var wf struct {
			Name      string   `json:"name"`
			TaskTypes []string `json:"task_types"`
		}
		json.Unmarshal(raw, &wf)
		var match *fakeWorkflow
		for _, have := range a.workflows {
			if have.project == key && (have.State == "active" || have.State == "draft") && have.Name == wf.Name &&
				strings.Join(have.TaskTypes, ",") == strings.Join(wf.TaskTypes, ",") && have.body == string(raw) {
				match = have
				break
			}
		}
		if match != nil {
			flows = append(flows, entry{wf.Name, "unchanged", match.ID})
			continue
		}
		a.nextID++
		id := fmt.Sprintf("wf-%d", a.nextID)
		flows = append(flows, entry{wf.Name, "draft_created", id})
		if !dry {
			a.workflows = append(a.workflows, &fakeWorkflow{ID: id, Name: wf.Name, Version: a.nextID, State: "draft",
				TaskTypes: wf.TaskTypes, project: key, body: string(raw)})
		}
	}
	rep["roles"], rep["workflows"] = roles, flows
	apiJSON(w, 200, rep)
}

func (a *fakeAPI) listWorkflows(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []*fakeWorkflow{}
	for _, wf := range a.workflows {
		if wf.project == r.PathValue("key") {
			out = append(out, wf)
		}
	}
	apiJSON(w, 200, map[string]any{"workflows": out})
}

func (a *fakeAPI) activate(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activations++
	for _, wf := range a.workflows {
		if wf.ID != r.PathValue("id") {
			continue
		}
		for _, other := range a.workflows {
			if other != wf && other.project == wf.project && other.State == "active" && overlaps(other.TaskTypes, wf.TaskTypes) {
				other.State = "retired"
			}
		}
		wf.State = "active"
		apiJSON(w, 200, map[string]any{"workflow": wf})
		return
	}
	apiErr(w, 404, "not_found", "цикла нет")
}

// state — сводка проекта: роли с инструкциями и циклы с состоянием.
func (a *fakeAPI) state(key string) (roles int, active, drafts []string, versions int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, wf := range a.workflows {
		if wf.project != key {
			continue
		}
		versions++
		switch wf.State {
		case "active":
			active = append(active, wf.Name+":"+strings.Join(wf.TaskTypes, ","))
		case "draft":
			drafts = append(drafts, wf.Name)
		}
	}
	return len(a.roles[key]), active, drafts, versions
}

// ── установка с API ─────────────────────────────────────────────────────────

// starterInstall — install с пакетом, где есть связка, и API на адресе
// --http-addr.
func starterInstall(t *testing.T, vars map[string]string, bundle string) (*installFixture, *fakeAPI) {
	t.Helper()
	api := newFakeAPI(t, "v0.7.0", false)
	api.token = vars["SDLC_ADMIN_TOKEN"] // пусто — токен, который install сгенерирует
	f := installSetup(t, vars)
	extra := map[string]string{".env.example": testEnvExample}
	if bundle != "" {
		extra["bundles/starter.json"] = bundle
	}
	publish(t, f.releases, "v0.7.0", pkgFiles("v0.7.0", extra), true)
	return f, api
}

// starterInstalled — установка v0.7.0 со связкой и .env, который смотрит на
// API; env — строки .env поверх режима proxy.
func starterInstalled(t *testing.T, api *fakeAPI, bundle, env string) string {
	t.Helper()
	dir := installed(t, "v0.7.0", map[string]string{"bundles/starter.json": bundle})
	if env == "" {
		env = "CADENCE_MODE=proxy\nCOMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.override.yml\n" +
			"CADENCE_HTTP_ADDR=" + api.addr() + "\nSDLC_ADMIN_TOKEN=" + testAdminToken + "\nSDLC_BOOTSTRAP_TEAM=acme\n"
	}
	mustWrite(t, filepath.Join(dir, ".env"), "POSTGRES_PASSWORD=secret\n"+env, 0o600)
	return dir
}

func starter(e *serverEnv, args ...string) int {
	return runServer(context.Background(), e, append([]string{"starter"}, args...))
}

// noTokenInCalls — токен администратора не уходит в аргументы docker (AC-15).
func noTokenInCalls(t *testing.T, r *fakeRunner, token string) {
	t.Helper()
	for _, c := range r.calls {
		if strings.Contains(c, token) {
			t.Errorf("the token is in the arguments of %q", c)
		}
	}
}

// AC-1: справка.
func TestStarterHelp(t *testing.T) {
	e, out, _ := testEnv(t, t.TempDir(), newFakeRunner(), nil)
	runServer(context.Background(), e, []string{"--help"})
	if !strings.Contains(out.String(), "starter ") || strings.Contains(out.String(), "not with the API") {
		t.Errorf("clab server --help:\n%s", out)
	}
	out.Reset()
	if code := runServer(context.Background(), e, []string{"install", "--help"}); code != 0 {
		t.Fatalf("code %d", code)
	}
	for _, flag := range []string{"--with-starter", "--project KEY", "--project-name NAME"} {
		if !strings.Contains(out.String(), flag) {
			t.Errorf("install --help lacks %s", flag)
		}
	}
	out.Reset()
	if code := starter(e, "--help"); code != 0 || !strings.Contains(out.String(), "--project KEY") || !strings.Contains(out.String(), "--team KEY") {
		t.Errorf("starter --help: %d\n%s", code, out)
	}
}

// AC-6, AC-7, AC-15: установка одной командой.
func TestServerInstallWithStarter(t *testing.T) {
	f, api := starterInstall(t, nil, testStarterBundle)
	cfg := t.TempDir()
	t.Setenv("CLAB_CONFIG_DIR", cfg)
	d := f.dir("cadence")
	code := f.install("--dir", d, "--mode", "proxy", "--http-addr", api.addr(), "--team", "acme",
		"--with-starter", "--project", "ACME", "--yes")
	if code != 0 {
		t.Fatalf("code %d\n%s\n%s", code, f.out, f.errw)
	}
	token := f.env(d)["SDLC_ADMIN_TOKEN"]
	for _, h := range api.auth {
		if h != "Bearer "+token {
			t.Fatalf("the API got %q, want the token of the installation", h)
		}
	}
	if team := api.projects["ACME"]; team != "acme" || api.created[0]["name"] != "ACME" {
		t.Errorf("project: %v %v", api.projects, api.created)
	}
	roles, active, drafts, _ := api.state("ACME")
	if roles != 2 || strings.Join(active, " ") != "Starter:task Starter epics:epic" || len(drafts) != 0 {
		t.Errorf("roles %d, active %v, drafts %v", roles, active, drafts)
	}
	out := f.out.String()
	host := "http://" + api.addr()
	for _, s := range []string{"project ACME created in team acme", "analyst created", "Starter created, activated",
		"Starter epics created, activated", host + "/p/ACME", "clab auth login --host " + host + " --with-token",
		"clab invite create --team acme --role lead", "clab invite create --project ACME --level agent --roles analyst,developer",
		"clab team grant acme --project ACME --level agent", `clab create --project ACME --title "First task"`} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if strings.Count(out, token) != 1 || strings.Contains(f.errw.String(), token) {
		t.Error("the generated token must be printed exactly once, by install")
	}
	if strings.Count(out, "clab auth login") != 1 {
		t.Error("the sign-in line is printed once")
	}
	noTokenInCalls(t, f.r, token)
	if left, _ := os.ReadDir(cfg); len(left) > 0 {
		t.Errorf("CLAB_CONFIG_DIR got %v", left)
	}
	f.tmpEmpty()

	// AC-2: .env с флагом и без — те же ключи, без новых строк.
	g, _ := starterInstall(t, nil, testStarterBundle)
	d2 := g.dir("cadence")
	if code := g.install("--dir", d2, "--mode", "proxy", "--http-addr", api.addr(), "--team", "acme", "--yes"); code != 0 {
		t.Fatalf("plain: %d %s", code, g.errw)
	}
	keys := func(m map[string]string) string {
		var k []string
		for key := range m {
			k = append(k, key)
		}
		sort.Strings(k)
		return strings.Join(k, " ")
	}
	if keys(f.env(d)) != keys(g.env(d2)) {
		t.Errorf(".env keys differ:\n%s\n%s", keys(f.env(d)), keys(g.env(d2)))
	}
	if strings.Contains(g.out.String(), "project") || strings.Contains(g.out.String(), "invite") {
		t.Errorf("without --with-starter nothing new is printed:\n%s", g.out)
	}

	// Заданный токен не печатается и с шагом.
	h, api2 := starterInstall(t, map[string]string{"SDLC_ADMIN_TOKEN": testAdminToken}, testStarterBundle)
	if code := h.install("--dir", h.dir("c"), "--mode", "proxy", "--http-addr", api2.addr(), "--team", "acme",
		"--with-starter", "--project", "ACME", "--project-name", "Acme Corp", "--yes"); code != 0 {
		t.Fatalf("supplied token: %d %s", code, h.errw)
	}
	if strings.Contains(h.out.String()+h.errw.String(), testAdminToken) || api2.created[0]["name"] != "Acme Corp" {
		t.Errorf("supplied token printed or name lost: %v\n%s", api2.created, h.out)
	}
}

// AC-3, AC-4, AC-5: отказы до записи .env.
func TestServerInstallWithStarterRefusals(t *testing.T) {
	cases := []struct {
		name   string
		vars   map[string]string
		bundle string
		args   []string
		code   int
		msg    string
	}{
		{"no team", nil, testStarterBundle, []string{"--with-starter", "--project", "ACME"}, 1, "--team KEY"},
		{"team in env", map[string]string{"SDLC_BOOTSTRAP_TEAM": "acme"}, "", []string{"--with-starter", "--project", "ACME"}, 1, "has no bundles/starter.json"},
		{"lower key", nil, testStarterBundle, []string{"--team", "acme", "--with-starter", "--project", "plat"}, 1, "'plat' is not a project key"},
		{"short key", nil, testStarterBundle, []string{"--team", "acme", "--with-starter", "--project", "P"}, 1, "'P' is not a project key"},
		{"long key", nil, testStarterBundle, []string{"--team", "acme", "--with-starter", "--project", "ABCDEFGHIJKLMNOPQ"}, 1, "is not a project key"},
		{"no key", nil, testStarterBundle, []string{"--team", "acme", "--with-starter"}, 1, "--project KEY"},
		{"project alone", nil, testStarterBundle, []string{"--team", "acme", "--project", "ACME"}, 2, "go with --with-starter"},
		{"project name alone", nil, testStarterBundle, []string{"--project-name", "Acme"}, 2, "go with --with-starter"},
		{"no bundle", nil, "", []string{"--team", "acme", "--with-starter", "--project", "ACME"}, 1, "the package of v0.7.0 has no bundles/starter.json"},
		{"bad bundle", nil, "{not json", []string{"--team", "acme", "--with-starter", "--project", "ACME"}, 1, "is not a JSON bundle"},
		{"no workflows", nil, `{"version":1,"roles":[]}`, []string{"--team", "acme", "--with-starter", "--project", "ACME"}, 1, "has no workflows"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, api := starterInstall(t, c.vars, c.bundle)
			d := f.dir("cadence")
			args := append([]string{"--dir", d, "--mode", "proxy", "--http-addr", api.addr(), "--yes"}, c.args...)
			if code := f.install(args...); code != c.code || !strings.Contains(f.errw.String(), c.msg) {
				t.Errorf("code %d, want %d; %s", code, c.code, f.errw)
			}
			f.noEnv(d)
			if _, err := os.Stat(d); err == nil {
				t.Errorf("%s must not be created", d)
			}
			if f.r.called("docker compose up") || len(api.auth) > 0 {
				t.Error("nothing is started and the API is not asked")
			}
		})
	}
}

// AC-4: в терминале ключ проекта спрашивается.
func TestInstallAnswersStarterProject(t *testing.T) {
	f := installSetup(t, nil)
	f.e.terminal = true
	f.e.in = strings.NewReader("proxy\n127.0.0.1:18191\nacme\n\nACME\n")
	a, err := installAnswers(f.e, installOptions{withStarter: true})
	if err != nil || a.project != "ACME" || a.projectName != "ACME" || a.team != "acme" {
		t.Errorf("terminal: %+v %v", a, err)
	}
	if !strings.Contains(f.out.String(), "Key of the starter project (2-16 characters A-Z, 0-9 and _) []: ") {
		t.Errorf("no question:\n%s", f.out)
	}
	f.e.in = strings.NewReader("proxy\n127.0.0.1:18191\nacme\n\n\n")
	f.out.Reset()
	if _, err := installAnswers(f.e, installOptions{withStarter: true}); err == nil || !strings.Contains(err.Error(), "--project KEY") {
		t.Errorf("an empty answer must be refused: %v", err)
	}
	// Без --with-starter вопроса нет.
	f.e.in = strings.NewReader("proxy\n127.0.0.1:18191\nacme\n\n")
	f.out.Reset()
	if _, err := installAnswers(f.e, installOptions{}); err != nil || strings.Contains(f.out.String(), "starter project") {
		t.Errorf("without --with-starter: %v\n%s", err, f.out)
	}
}

// AC-11: сбой после старта — стек и .env остаются, команда повтора.
func TestServerInstallWithStarterFailure(t *testing.T) {
	f, api := starterInstall(t, nil, testStarterBundle)
	api.failImport = 500
	d := f.dir("cadence")
	code := f.install("--dir", d, "--mode", "proxy", "--http-addr", api.addr(), "--team", "acme",
		"--with-starter", "--project", "ACME", "--project-name", "Acme Corp", "--yes")
	if code != 1 || !isFile(filepath.Join(d, ".env")) {
		t.Fatalf("code %d", code)
	}
	errw := f.errw.String()
	for _, s := range []string{"HTTP 500", "The stack is running and .env is kept",
		"clab server starter --dir " + d + " --team acme --project ACME --project-name 'Acme Corp'"} {
		if !strings.Contains(errw, s) {
			t.Errorf("stderr lacks %q:\n%s", s, errw)
		}
	}
	if strings.Contains(errw, f.env(d)["SDLC_ADMIN_TOKEN"]) || !strings.Contains(errw, "[token]") {
		t.Errorf("a token quoted by the server must be redacted:\n%s", errw)
	}
	if f.r.called("docker compose down") {
		t.Error("the stack must stay")
	}

	// Повтор напечатанной командой доводит шаг.
	api.failImport = 0
	f.out.Reset()
	f.errw.Reset()
	if code := starter(f.e, "--dir", d, "--team", "acme", "--project", "ACME", "--project-name", "Acme Corp"); code != 0 {
		t.Fatalf("retry: %d %s", code, f.errw)
	}
	if _, active, _, _ := api.state("ACME"); len(active) != 2 {
		t.Errorf("retry: active %v", active)
	}

	// Под обёрткой пакета — путь clab пакета; API недоступен.
	g, api2 := starterInstall(t, map[string]string{wrapperEnv: "1", "CADENCE_READY_TIMEOUT": "3"}, testStarterBundle)
	addr := api2.addr()
	api2.srv.Close()
	d = g.dir("cadence")
	if code := g.install("--dir", d, "--mode", "proxy", "--http-addr", addr, "--team", "acme", "--with-starter", "--project", "ACME", "--yes"); code != 1 {
		t.Fatalf("down: %d", code)
	}
	if !strings.Contains(g.errw.String(), "did not answer in 3s") ||
		!strings.Contains(g.errw.String(), "/usr/local/bin/clab server starter --dir "+d+" --team acme --project ACME") {
		t.Errorf("wrapped retry:\n%s", g.errw)
	}
}

// AC-8: повтор на установленной машине ничего не меняет.
func TestServerStarterRepeat(t *testing.T) {
	api := newFakeAPI(t, "v0.7.0", false)
	dir := starterInstalled(t, api, testStarterBundle, "")
	r := newFakeRunner()
	e, out, errw := testEnv(t, t.TempDir(), r, nil)
	if code := starter(e, "--dir", dir, "--project", "ACME"); code != 0 {
		t.Fatalf("code %d %s", code, errw)
	}
	if api.projects["ACME"] != "acme" || !strings.Contains(out.String(), "clab auth login --host http://"+api.addr()+" --with-token") {
		t.Errorf("team from .env or sign-in line:\n%s", out)
	}
	roles, active, _, versions := api.state("ACME")
	out.Reset()
	if code := starter(e, "--dir", dir, "--project", "ACME"); code != 0 {
		t.Fatalf("repeat: %d %s", code, errw)
	}
	for _, s := range []string{"project ACME already exists (team acme)", "analyst unchanged", "developer unchanged",
		"Starter unchanged, already active", "Starter epics unchanged, already active"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("repeat lacks %q:\n%s", s, out)
		}
	}
	roles2, active2, _, versions2 := api.state("ACME")
	if roles != roles2 || versions != versions2 || strings.Join(active, " ") != strings.Join(active2, " ") || api.activations != 2 {
		t.Errorf("repeat changed the project: %d/%d roles, %d/%d versions, %d activations", roles, roles2, versions, versions2, api.activations)
	}
	noTokenInCalls(t, r, testAdminToken)
	if strings.Contains(out.String()+errw.String(), testAdminToken) {
		t.Error("starter must not print the token")
	}
}

// AC-9: частичное состояние доводится.
func TestServerStarterPartial(t *testing.T) {
	api := newFakeAPI(t, "v0.7.0", false)
	api.projects["ACME"] = "acme" // пустой проект после сбоя между созданием и импортом
	dir := starterInstalled(t, api, testStarterBundle, "")
	e, out, errw := testEnv(t, t.TempDir(), newFakeRunner(), nil)
	if code := starter(e, "--dir", dir, "--project", "ACME"); code != 0 {
		t.Fatalf("empty project: %d %s", code, errw)
	}
	if len(api.created) != 0 || !strings.Contains(out.String(), "already exists") || !strings.Contains(out.String(), "Starter created, activated") {
		t.Errorf("empty project:\n%s", out)
	}

	// Связка импортирована, циклы остались черновиками.
	api2 := newFakeAPI(t, "v0.7.0", false)
	api2.projects["ACME"] = "acme"
	dir2 := starterInstalled(t, api2, testStarterBundle, "")
	a, _ := installAPIFor(&installation{e: e, env: map[string]string{"CADENCE_MODE": "proxy", "CADENCE_HTTP_ADDR": api2.addr()}}, testAdminToken)
	b, _ := parseStarterBundle([]byte(testStarterBundle), "test")
	if _, err := a.importBundle(context.Background(), "ACME", b.raw, false); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := starter(e, "--dir", dir2, "--project", "ACME"); code != 0 {
		t.Fatalf("drafts: %d %s", code, errw)
	}
	if _, active, drafts, _ := api2.state("ACME"); len(active) != 2 || len(drafts) != 0 ||
		!strings.Contains(out.String(), "Starter unchanged, activated") {
		t.Errorf("drafts: active %v drafts %v\n%s", active, drafts, out)
	}
}

// AC-10: чужие правила не затираются.
func TestServerStarterOwnRules(t *testing.T) {
	check := func(t *testing.T, api *fakeAPI, dir string) {
		t.Helper()
		e, _, errw := testEnv(t, t.TempDir(), newFakeRunner(), nil)
		roles, active, drafts, versions := api.state("ACME")
		if code := starter(e, "--dir", dir, "--project", "ACME"); code != 1 {
			t.Fatalf("code %d", code)
		}
		if api.imports != 0 || api.activations != 0 {
			t.Errorf("%d imports and %d activations, want none", api.imports, api.activations)
		}
		r2, a2, d2, v2 := api.state("ACME")
		if roles != r2 || versions != v2 || fmt.Sprint(active, drafts) != fmt.Sprint(a2, d2) {
			t.Error("the project changed")
		}
		if !strings.Contains(errw.String(), "already has rules of its own") ||
			!strings.Contains(errw.String(), "clab import --project ACME --file bundles/starter.json --dry-run") {
			t.Errorf("message:\n%s", errw)
		}
	}
	t.Run("role", func(t *testing.T) {
		api := newFakeAPI(t, "v0.7.0", false)
		api.projects["ACME"] = "acme"
		api.roles["ACME"] = map[string]string{"analyst": "our own instruction"}
		check(t, api, starterInstalled(t, api, testStarterBundle, ""))
	})
	t.Run("workflow", func(t *testing.T) {
		api := newFakeAPI(t, "v0.7.0", false)
		api.projects["ACME"] = "acme"
		api.workflows = []*fakeWorkflow{{ID: "own", Name: "Main", Version: 3, State: "active", TaskTypes: []string{"task"}, project: "ACME"}}
		check(t, api, starterInstalled(t, api, testStarterBundle, ""))
	})
	t.Run("draft of the starter beside an own active workflow", func(t *testing.T) {
		api := newFakeAPI(t, "v0.7.0", false)
		api.projects["ACME"] = "acme"
		a, _ := installAPIFor(&installation{e: &serverEnv{}, env: map[string]string{"CADENCE_MODE": "proxy", "CADENCE_HTTP_ADDR": api.addr()}}, testAdminToken)
		if _, err := a.importBundle(context.Background(), "ACME", []byte(testStarterBundle), false); err != nil {
			t.Fatal(err)
		}
		api.workflows = append(api.workflows, &fakeWorkflow{ID: "own", Name: "Main", Version: 9, State: "active", TaskTypes: []string{"task"}, project: "ACME"})
		api.imports = 0
		check(t, api, starterInstalled(t, api, testStarterBundle, ""))
	})
}

// AC-12: режим tls — loopback, SNI домена, сертификат проверяется.
func TestServerStarterTLS(t *testing.T) {
	tlsEnv := func(domain string) string {
		return "CADENCE_MODE=tls\nCOMPOSE_FILE=docker-compose.yml:compose.tls.yml:compose.override.yml\n" +
			"SDLC_DOMAIN=" + domain + "\nACME_EMAIL=ops@example.com\nSDLC_ADMIN_TOKEN=" + testAdminToken +
			"\nSDLC_BOOTSTRAP_TEAM=acme\nCADENCE_READY_TIMEOUT=6\n"
	}
	api := newFakeAPI(t, "v0.7.0", true)
	dir := starterInstalled(t, api, testStarterBundle, tlsEnv("example.com"))
	e, out, errw := testEnv(t, t.TempDir(), newFakeRunner(), nil)
	e.apiTLSDial = api.addr()
	e.apiRoots = x509.NewCertPool()
	e.apiRoots.AddCert(api.srv.Certificate())
	if code := starter(e, "--dir", dir, "--project", "ACME"); code != 0 {
		t.Fatalf("code %d %s", code, errw)
	}
	for _, sni := range api.sni {
		if sni != "example.com" {
			t.Errorf("SNI %q, want the domain", sni)
		}
	}
	if len(api.sni) == 0 || !strings.Contains(out.String(), "https://example.com/p/ACME") {
		t.Errorf("tls:\n%s", out)
	}

	// Недоверенный сертификат и чужое имя — ожидание до таймаута, затем отказ.
	for name, setup := range map[string]func() (string, *x509.CertPool){
		"untrusted":    func() (string, *x509.CertPool) { return "example.com", x509.NewCertPool() },
		"another name": func() (string, *x509.CertPool) { return "cadence.test", e.apiRoots },
	} {
		domain, roots := setup()
		api := newFakeAPI(t, "v0.7.0", true)
		dir := starterInstalled(t, api, testStarterBundle, tlsEnv(domain))
		e, _, errw := testEnv(t, t.TempDir(), newFakeRunner(), nil)
		e.apiTLSDial, e.apiRoots = api.addr(), roots
		sleeps := 0
		e.sleep = func(time.Duration) { sleeps++ }
		if code := starter(e, "--dir", dir, "--project", "ACME"); code != 1 {
			t.Fatalf("%s: code %d", name, code)
		}
		if api.instanceHit != 0 || sleeps != 2 || !strings.Contains(errw.String(), "did not answer in 6s") ||
			!strings.Contains(errw.String(), "Let's Encrypt") || !strings.Contains(errw.String(), "certificate") {
			t.Errorf("%s: %d hits, %d sleeps\n%s", name, api.instanceHit, sleeps, errw)
		}
	}
	for _, f := range []string{"server_api.go", "server_starter.go"} {
		if strings.Contains(readFile(t, f), "InsecureSkipVerify") {
			t.Errorf("%s turns certificate checks off", f)
		}
	}
}

// AC-13: адрес в режиме proxy.
func TestInstallAPIAddress(t *testing.T) {
	e, _, _ := testEnv(t, t.TempDir(), newFakeRunner(), nil)
	for addr, want := range map[string]string{
		"127.0.0.1:8081": "http://127.0.0.1:8081",
		"0.0.0.0:8080":   "http://127.0.0.1:8080",
		"8080":           "http://127.0.0.1:8080",
		"":               "http://127.0.0.1:8080",
		"[::1]:8082":     "http://[::1]:8082",
	} {
		a, err := installAPIFor(&installation{e: e, env: map[string]string{"CADENCE_MODE": "proxy", "CADENCE_HTTP_ADDR": addr}}, "t")
		if err != nil || a.base != want {
			t.Errorf("%q: %v %v, want %s", addr, a, err, want)
		}
	}
	a, err := installAPIFor(&installation{e: e, env: map[string]string{"SDLC_DOMAIN": "cadence.example.com"}}, "t")
	if err != nil || a.base != "https://cadence.example.com" {
		t.Errorf("tls: %v %v", a, err)
	}
	// Прокси окружения не используется: токен идёт только на loopback.
	if tr := a.http.Transport.(*http.Transport); tr.Proxy != nil {
		t.Error("the transport must not use a proxy")
	}
}

// AC-14: подкоманда на установленной машине.
func TestServerStarterInstallation(t *testing.T) {
	api := newFakeAPI(t, "v0.7.0", false)
	e, _, errw := testEnv(t, t.TempDir(), newFakeRunner(), nil)

	noEnv := installed(t, "v0.7.0", map[string]string{"bundles/starter.json": testStarterBundle})
	os.Remove(filepath.Join(noEnv, ".env"))
	if code := starter(e, "--dir", noEnv, "--project", "ACME"); code != 1 || !strings.Contains(errw.String(), "no .env") {
		t.Errorf("no .env: %d %s", code, errw)
	}

	errw.Reset()
	noTeam := starterInstalled(t, api, testStarterBundle, "CADENCE_MODE=proxy\nCOMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.override.yml\n"+
		"CADENCE_HTTP_ADDR="+api.addr()+"\nSDLC_ADMIN_TOKEN="+testAdminToken+"\nSDLC_BOOTSTRAP_TEAM=\n")
	if code := starter(e, "--dir", noTeam, "--project", "ACME"); code != 1 || !strings.Contains(errw.String(), "--team KEY") {
		t.Errorf("no team: %d %s", code, errw)
	}
	errw.Reset()
	if code := starter(e, "--dir", noTeam, "--team", "acme"); code != 1 || !strings.Contains(errw.String(), "--project KEY") {
		t.Errorf("no project: %d %s", code, errw)
	}

	errw.Reset()
	noToken := starterInstalled(t, api, testStarterBundle, "CADENCE_MODE=proxy\nCOMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.override.yml\n"+
		"CADENCE_HTTP_ADDR="+api.addr()+"\nSDLC_ADMIN_TOKEN=\nSDLC_BOOTSTRAP_TEAM=acme\n")
	if code := starter(e, "--dir", noToken, "--project", "ACME"); code != 1 || !strings.Contains(errw.String(), "docs/admin.md") || len(api.auth) > 0 {
		t.Errorf("no token: %d %s", code, errw)
	}

	// Токен только файлом в secrets/.
	errw.Reset()
	inFile := starterInstalled(t, api, testStarterBundle, "CADENCE_MODE=proxy\nCOMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.override.yml\n"+
		"CADENCE_HTTP_ADDR="+api.addr()+"\nSDLC_ADMIN_TOKEN_FILE=/run/secrets/admin_token\nSDLC_BOOTSTRAP_TEAM=acme\n")
	mustWrite(t, filepath.Join(inFile, "secrets", "admin_token"), testAdminToken+"\n", 0o600)
	if code := starter(e, "--dir", inFile, "--project", "ACME"); code != 0 {
		t.Errorf("token file: %d %s", code, errw)
	}

	// compose.secrets.yml: в .env токена нет, SDLC_ADMIN_TOKEN_FILE задан
	// сервису api в compose-файле.
	r2 := newFakeRunner()
	r2.apiEnvironment = `{"SDLC_ADMIN_TOKEN": "", "SDLC_ADMIN_TOKEN_FILE": "/run/secrets/admin_token"}`
	e2, out2, errw2 := testEnv(t, t.TempDir(), r2, nil)
	inCompose := starterInstalled(t, api, testStarterBundle, "CADENCE_MODE=proxy\nCOMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.secrets.yml:compose.override.yml\n"+
		"CADENCE_HTTP_ADDR="+api.addr()+"\nSDLC_ADMIN_TOKEN=\nSDLC_BOOTSTRAP_TEAM=acme\n")
	mustWrite(t, filepath.Join(inCompose, "secrets", "admin_token"), testAdminToken+"\n", 0o400)
	if code := starter(e2, "--dir", inCompose, "--project", "ACME"); code != 0 || strings.Contains(out2.String()+errw2.String(), testAdminToken) {
		t.Errorf("token in compose.secrets.yml: %d %s", code, errw2)
	}
	noTokenInCalls(t, r2, testAdminToken)

	// Чужой токен — отказ без ожидания.
	errw.Reset()
	wrong := starterInstalled(t, api, testStarterBundle, "CADENCE_MODE=proxy\nCOMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.override.yml\n"+
		"CADENCE_HTTP_ADDR="+api.addr()+"\nSDLC_ADMIN_TOKEN=sdlc_not_the_token_of_it_0000\nSDLC_BOOTSTRAP_TEAM=acme\n")
	if code := starter(e, "--dir", wrong, "--project", "ACME"); code != 1 || !strings.Contains(errw.String(), "refused the administrator token") ||
		strings.Contains(errw.String(), "sdlc_not_the_token_of_it_0000") {
		t.Errorf("wrong token: %d %s", code, errw)
	}

	// Ответ другой версии — не API этой установки.
	other := newFakeAPI(t, "v0.6.1", false)
	errw.Reset()
	dir := starterInstalled(t, other, testStarterBundle, "")
	if code := starter(e, "--dir", dir, "--project", "ACME"); code != 1 || !strings.Contains(errw.String(), "is version v0.6.1") || other.dryImports != 0 {
		t.Errorf("version: %d %s", code, errw)
	}

	// Переадресация не уводит запрос с токеном.
	moved := newFakeAPI(t, "v0.7.0", false)
	moved.redirect = true
	errw.Reset()
	dir = starterInstalled(t, moved, testStarterBundle, "")
	if code := starter(e, "--dir", dir, "--project", "ACME"); code != 1 || !strings.Contains(errw.String(), "HTTP 302") {
		t.Errorf("redirect: %d %s", code, errw)
	}
}

// AC-16: другие имена ролей и циклов.
func TestServerStarterOtherNames(t *testing.T) {
	api := newFakeAPI(t, "v0.7.0", false)
	dir := starterInstalled(t, api, otherNamesBundle, "")
	e, out, errw := testEnv(t, t.TempDir(), newFakeRunner(), nil)
	if code := starter(e, "--dir", dir, "--project", "DOCS"); code != 0 {
		t.Fatalf("code %d %s", code, errw)
	}
	if _, active, _, _ := api.state("DOCS"); strings.Join(active, " ") != "Flow:task Flow epics:epic" ||
		!strings.Contains(out.String(), "--roles writer") {
		t.Errorf("active %v\n%s", active, out)
	}
	for _, f := range []string{"server_api.go", "server_starter.go"} {
		src := readFile(t, f)
		for _, lit := range []string{`"Starter`, `"analyst`, `"developer`} {
			if strings.Contains(src, lit) {
				t.Errorf("%s knows %s", f, lit)
			}
		}
	}
}
