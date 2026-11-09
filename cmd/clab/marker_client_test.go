package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CLAB-424: выбор проекта и хоста клиентом с маркером каталога, clab whoami
// и отказ от запасного ./.clab.

type whoamiOut struct {
	API        string `json:"api"`
	Host       string `json:"host"`
	HostSource string `json:"host_source"`
	Project    *struct {
		Key           string `json:"key"`
		ProjectSource string `json:"project_source"`
		MarkerPath    string `json:"marker_path"`
	} `json:"project"`
}

func whoamiJSON(t *testing.T, c *Client, args ...string) (whoamiOut, error) {
	t.Helper()
	withJSON(t, true)
	out, _, err := capture(t, func() error { return cmdWhoami(context.Background(), c, args) })
	var w whoamiOut
	if err == nil {
		if jerr := json.Unmarshal([]byte(out), &w); jerr != nil {
			t.Fatalf("whoami --json не JSON: %q", out)
		}
	}
	return w, err
}

// gitRepo — репозиторий с origin в каталоге dir; без git тест пропускается.
func gitRepo(t *testing.T, dir, origin string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git нет")
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v %s", args, err, out)
		}
	}
}

func hasRequest(reqs []string, prefix string) bool {
	for _, r := range reqs {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

// AC-1: маркер в корне, вызов из .worktrees/X/backend со своим CLAUDE.md и
// своим origin — проект из маркера, поиска по git remote нет.
func TestMarkerProjectFromNestedWorktree(t *testing.T) {
	_, b, cfg := twoHosts(t)
	root, _ := markerEnv(t, cfg)
	marker := writeMarkerTestFile(t, filepath.Join(root, "CLAUDE.md"), markerText("CLAB", b.srv.URL))
	deep := filepath.Join(root, ".worktrees", "X", "backend")
	writeMarkerTestFile(t, filepath.Join(deep, "CLAUDE.md"), "# backend без маркера\n")
	gitRepo(t, deep, "git@github.com:acme/api.git")
	t.Chdir(deep)

	w, err := whoamiJSON(t, newMarkerClient(t, globals{}))
	if err != nil {
		t.Fatal(err)
	}
	if w.Project == nil || w.Project.Key != "CLAB" || w.Project.ProjectSource != projectFromMarker || w.Project.MarkerPath != marker {
		t.Fatalf("проект: %+v", w.Project)
	}
	reqs := b.requests()
	if !hasRequest(reqs, "GET /v1/projects/CLAB") || hasRequest(reqs, "GET /v1/projects?repo=") {
		t.Fatalf("запросы: %v", reqs)
	}
}

// AC-3, AC-4: --project → SDLC_PROJECT → маркер → git remote → ошибка с
// советом clab setup.
func TestMarkerProjectOrder(t *testing.T) {
	a := newStubHost(t, "sdlc_token_A", "alice")
	cfg := Config{DefaultHost: a.key, Hosts: map[string]HostConfig{a.key: a.hc()}}
	root, _ := markerEnv(t, cfg)
	gitRepo(t, root, "git@github.com:acme/api.git")
	writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), markerText("CLAB", ""))

	check := func(name, want, source string, args ...string) {
		t.Helper()
		w, err := whoamiJSON(t, newMarkerClient(t, globals{}), args...)
		if err != nil || w.Project == nil || w.Project.Key != want || w.Project.ProjectSource != source {
			t.Fatalf("%s: %+v %v", name, w.Project, err)
		}
	}
	t.Setenv("SDLC_PROJECT", "ENV")
	check("флаг сильнее окружения", "ZED", projectFromFlag, "--project", "ZED")
	check("окружение сильнее маркера", "ENV", projectFromEnv)
	t.Setenv("SDLC_PROJECT", "")
	before := len(a.requests())
	check("маркер сильнее git remote", "CLAB", projectFromMarker)
	if hasRequest(a.requests()[before:], "GET /v1/projects?repo=") {
		t.Fatal("с маркером спросили проект по git remote")
	}
	if err := os.Remove(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	check("без маркера — git remote", "REMOTE", projectFromRemote)

	// Ни флага, ни окружения, ни маркера, ни origin — ошибка с советом.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	before = len(a.requests())
	c := newMarkerClient(t, globals{})
	_, _, err := capture(t, func() error { return cmdQueue(context.Background(), c, nil) })
	if err == nil || !strings.Contains(err.Error(), "clab setup") || !strings.Contains(err.Error(), "не указан проект: --project, SDLC_PROJECT") {
		t.Fatalf("без проекта: %v", err)
	}
	if n := len(a.requests()) - before; n != 0 {
		t.Fatalf("без проекта запросов: %d", n)
	}
}

// AC-5, AC-20: --host → SDLC_API_URL → host маркера → default_host; по
// маркеру запрос уходит на адрес записи с её токеном.
func TestMarkerHostOrder(t *testing.T) {
	a, b, cfg := twoHosts(t)
	root, _ := markerEnv(t, cfg)
	marker := writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), markerText("P", b.srv.URL))

	cases := []struct {
		name   string
		g      globals
		env    string
		host   *stubHost
		source string
	}{
		{"флаг", globals{host: a.srv.URL}, b.srv.URL, a, hostFromFlag},
		{"окружение", globals{}, a.srv.URL, a, hostFromEnv},
		{"маркер", globals{}, "", b, hostFromMarker},
	}
	for _, tc := range cases {
		t.Setenv("SDLC_API_URL", tc.env)
		w, err := whoamiJSON(t, newMarkerClient(t, tc.g))
		if err != nil || w.Host != tc.host.srv.URL || w.HostSource != tc.source || w.API != tc.host.srv.URL+"/v1" {
			t.Fatalf("%s: %+v %v", tc.name, w, err)
		}
		if w.Project == nil || w.Project.Key != "P" {
			t.Fatalf("%s: проект %+v", tc.name, w.Project)
		}
	}
	t.Setenv("SDLC_API_URL", "")
	if !b.sawAuth(b.token) || b.sawAuth(a.token) || a.sawAuth(b.token) {
		t.Fatal("токен ушёл не на свой хост")
	}

	// Текст называет хост и источник.
	withJSON(t, false)
	out, _, err := capture(t, func() error { return cmdWhoami(context.Background(), newMarkerClient(t, globals{}), nil) })
	if err != nil || !strings.Contains(out, "Хост:") || !strings.Contains(out, b.srv.URL+" (маркер "+marker+")") ||
		!strings.Contains(out, "Источник:") || !strings.Contains(out, "маркер "+marker) {
		t.Fatalf("текст whoami:\n%s %v", out, err)
	}

	// Маркер без host — хост по умолчанию.
	writeMarkerTestFile(t, marker, markerText("P", ""))
	w, err := whoamiJSON(t, newMarkerClient(t, globals{}))
	if err != nil || w.Host != a.srv.URL || w.HostSource != hostFromDefault {
		t.Fatalf("маркер без хоста: %+v %v", w, err)
	}
}

// AC-6, AC-26: хост маркера без записи — ошибка с советом входа, ни одного
// запроса, default_host не подставлен; --host или SDLC_API_URL снимают её.
func TestMarkerHostWithoutLogin(t *testing.T) {
	a := newStubHost(t, "sdlc_token_A", "alice")
	cfg := Config{DefaultHost: a.key, Hosts: map[string]HostConfig{a.key: a.hc()}}
	root, _ := markerEnv(t, cfg)
	writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), markerText("X", "https://evil.example"))

	c := newMarkerClient(t, globals{})
	rt := &countingTransport{}
	c.http.Transport = rt
	for name, run := range map[string]func() error{
		"whoami": func() error { return cmdWhoami(context.Background(), c, nil) },
		"queue":  func() error { return cmdQueue(context.Background(), c, nil) },
		"show":   func() error { return cmdShow(context.Background(), c, []string{"X-1"}) },
	} {
		_, _, err := capture(t, run)
		if err == nil || !strings.Contains(err.Error(), "clab auth login --host https://evil.example") ||
			!strings.Contains(err.Error(), filepath.Join(root, "AGENTS.md")) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if rt.calls != 0 || len(a.requests()) != 0 {
		t.Fatalf("запросов: %d, на хост по умолчанию %d", rt.calls, len(a.requests()))
	}

	if _, err := whoamiJSON(t, newMarkerClient(t, globals{host: a.srv.URL})); err != nil {
		t.Fatalf("--host: %v", err)
	}
	t.Setenv("SDLC_API_URL", a.srv.URL)
	if _, err := whoamiJSON(t, newMarkerClient(t, globals{})); err != nil {
		t.Fatalf("SDLC_API_URL: %v", err)
	}
	if a.sawAuth("") {
		t.Fatal("запрос без токена")
	}
}

// AC-7: с CLAB_CONFIG_DIR маркер не влияет ни на проект, ни на хост, и файлы
// AGENTS.md и CLAUDE.md не открываются.
func TestMarkerIgnoredWithConfigDir(t *testing.T) {
	a, b, cfg := twoHosts(t)
	root, _ := markerEnv(t, cfg)
	writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), markerText("CLAB", b.srv.URL))
	writeMarkerTestFile(t, filepath.Join(root, "CLAUDE.md"), markerText("CLAB", b.srv.URL))
	task := filepath.Join(root, "task-config")
	if err := saveConfigAt(task, Config{DefaultHost: a.key, Hosts: map[string]HostConfig{a.key: a.hc()}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAB_CONFIG_DIR", task)
	reads := countMarkerReads(t)

	c := newMarkerClient(t, globals{})
	w, err := whoamiJSON(t, c)
	if err != nil || w.Host != a.srv.URL || w.HostSource != hostFromDefault || w.Project != nil {
		t.Fatalf("whoami с CLAB_CONFIG_DIR: %+v %v", w, err)
	}
	withJSON(t, false)
	out, _, err := capture(t, func() error { return cmdAuth(context.Background(), c, []string{"status"}) })
	if err != nil || strings.Contains(out, "Каталог") || strings.Contains(out, "маркер") {
		t.Fatalf("auth status с CLAB_CONFIG_DIR:\n%s %v", out, err)
	}
	if *reads != 0 {
		t.Fatalf("файлы маркера открыты %d раз", *reads)
	}
	if len(b.requests()) != 0 {
		t.Fatalf("запросы на хост маркера: %v", b.requests())
	}
}

// AC-8: битый маркер не мешает clab version и командам с --project; без
// проекта — ошибка маркера с путём и без запросов к серверу проекта.
func TestMarkerBrokenOnlyWhenNeeded(t *testing.T) {
	a := newStubHost(t, "sdlc_token_A", "alice")
	cfg := Config{DefaultHost: a.key, Hosts: map[string]HostConfig{a.key: a.hc()}}
	root, _ := markerEnv(t, cfg)
	path := writeMarkerTestFile(t, filepath.Join(root, "AGENTS.md"), "<!-- cadence-lab: project=ACME token=sdlc_leak -->\n")

	withJSON(t, false)
	out, _, err := capture(t, func() error { return cmdVersion(context.Background(), newMarkerClient(t, globals{}), nil) })
	if err != nil || !strings.Contains(out, "сервер "+a.srv.URL) {
		t.Fatalf("version с битым маркером: %q %v", out, err)
	}
	if w, err := whoamiJSON(t, newMarkerClient(t, globals{}), "--project", "ACME"); err != nil || w.Project.Key != "ACME" {
		t.Fatalf("--project с битым маркером: %+v %v", w, err)
	}
	_, err = whoamiJSON(t, newMarkerClient(t, globals{}))
	if err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "sdlc_leak") {
		t.Fatalf("без проекта: %v", err)
	}
}

// AC-10: проект из маркера, которого нет на сервере, — ошибка называет файл.
func TestWhoamiMarkerProjectNotFound(t *testing.T) {
	a := newStubHost(t, "sdlc_token_A", "alice")
	cfg := Config{DefaultHost: a.key, Hosts: map[string]HostConfig{a.key: a.hc()}}
	root, _ := markerEnv(t, cfg)
	path := writeMarkerTestFile(t, filepath.Join(root, "CLAUDE.md"), markerText("MISSING", ""))
	_, err := whoamiJSON(t, newMarkerClient(t, globals{}))
	var apiErr *APIError
	if err == nil || !strings.Contains(err.Error(), "проект MISSING взят из маркера "+path) || !errors.As(err, &apiErr) {
		t.Fatalf("проект не найден: %v", err)
	}
	// exit() печатает только Code и Message APIError: путь — в сообщении.
	if apiErr.Code != "not_found" || !strings.Contains(apiErr.Message, "взят из маркера "+path) {
		t.Fatalf("путь маркера не в сообщении APIError: %#v", apiErr)
	}
}

// AC-17: без CLAB_CONFIG_DIR, XDG_CONFIG_HOME и домашнего каталога конфигу
// жить негде: вход и токен задачи отказывают, ./.clab не появляется, а
// команды с SDLC_API_URL и SDLC_TOKEN работают.
func TestConfigDirWithoutHome(t *testing.T) {
	a := newStubHost(t, "sdlc_token_A", "alice")
	root, _ := markerEnv(t, Config{})
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")

	if _, err := configDir(); err == nil || err.Error() != "нет домашнего каталога: задайте CLAB_CONFIG_DIR" {
		t.Fatalf("configDir: %v", err)
	}
	if err := saveConfig(Config{Hosts: map[string]HostConfig{}}); !errors.Is(err, errNoConfigDir) {
		t.Fatalf("saveConfig: %v", err)
	}
	if cfg, err := loadConfig(); err != nil || len(cfg.Hosts) != 0 {
		t.Fatalf("loadConfig: %+v %v", cfg, err)
	}

	c := newMarkerClient(t, globals{host: a.srv.URL})
	_, _, err := capture(t, func() error { return cmdAuth(context.Background(), c, []string{"login", "--no-browser"}) })
	if !errors.Is(err, errNoConfigDir) {
		t.Fatalf("auth login: %v", err)
	}
	withStdin(t, a.token+"\n")
	_, _, err = capture(t, func() error { return cmdAuth(context.Background(), c, []string{"login", "--with-token"}) })
	if !errors.Is(err, errNoConfigDir) {
		t.Fatalf("auth login --with-token: %v", err)
	}
	t.Setenv("SDLC_TOKEN", a.token)
	t.Setenv("SDLC_API_URL", a.srv.URL)
	c = newMarkerClient(t, globals{})
	for _, args := range [][]string{{"task", "X-1"}, {"task", "X-1", "--config-dir", filepath.Join(root, "exec")}} {
		_, _, err = capture(t, func() error { return cmdToken(context.Background(), c, args) })
		if !errors.Is(err, errNoConfigDir) {
			t.Fatalf("token %v: %v", args, err)
		}
	}
	if hasRequest(a.requests(), "POST") {
		t.Fatalf("до отказа ушли запросы: %v", a.requests())
	}
	if w, err := whoamiJSON(t, c, "--project", "ACME"); err != nil || w.HostSource != hostFromEnv {
		t.Fatalf("SDLC_API_URL + SDLC_TOKEN: %+v %v", w, err)
	}
	for _, p := range []string{filepath.Join(root, ".clab"), filepath.Join(root, "exec")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s создан", p)
		}
	}

	// Запасного пути нет и в исходниках.
	files, _ := filepath.Glob(filepath.Join(sourceDir(t), "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `".clab"`) {
			t.Fatalf("%s: запасной каталог \".clab\"", f)
		}
	}
}

// sourceDir — каталог пакета: тесты идут из него, но маркер-тесты меняют
// текущий каталог.
var packageDir, _ = os.Getwd()

func sourceDir(t *testing.T) string {
	t.Helper()
	if packageDir == "" {
		t.Skip("каталог пакета неизвестен")
	}
	return packageDir
}
