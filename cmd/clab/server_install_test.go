package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// Тесты clab server install (CLAB-160) идут без Docker и сети: релизы —
// каталог file:// (или httptest, где важно число запросов), Docker —
// подставной исполнитель, порты и DNS — подставные проверки serverEnv.
// Номера в комментариях — критерии приёмки CLAB-160.

// testEnvExample — .env.example пакета в миниатюре: те же ключи, что пишет
// install.sh, комментарии и закомментированный ключ, который set_kv не
// трогает.
const testEnvExample = `# Cadence Lab configuration.
COMPOSE_PROJECT_NAME=cadence
CADENCE_MODE=tls
COMPOSE_FILE=docker-compose.yml:compose.tls.yml:compose.override.yml
SDLC_DOMAIN=cadence.example.com
ACME_EMAIL=admin@example.com
CADENCE_HTTP_ADDR=127.0.0.1:8080
POSTGRES_USER=sdlc
POSTGRES_PASSWORD=
SDLC_ADMIN_HANDLE=admin
SDLC_ADMIN_TOKEN=
SDLC_TENANCY=single
SDLC_BOOTSTRAP_TEAM=
SDLC_BOOTSTRAP_TEAM_NAME=
SDLC_GITHUB_APP_KEY_FILE=
# SDLC_GITHUB_APP_KEY_FILE=/secrets/github-app.pem
SDLC_GITHUB_WEBHOOK_SECRET=
# CADENCE_REGISTRY=ghcr.io/cadence-lab-tech
`

type installFixture struct {
	t        *testing.T
	root     string // cwd команды
	releases string
	tmp      string // TMPDIR: временные файлы install
	r        *fakeRunner
	e        *serverEnv
	out      *bytes.Buffer
	errw     *bytes.Buffer
}

// installSetup — релизы v0.6.1 и v0.7.0 (последний) в каталоге file://,
// clab v0.7.0, без терминала.
func installSetup(t *testing.T, vars map[string]string) *installFixture {
	t.Helper()
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	tpl := map[string]string{".env.example": testEnvExample}
	publish(t, releases, "v0.6.1", pkgFiles("v0.6.1", tpl), false)
	publish(t, releases, "v0.7.0", pkgFiles("v0.7.0", tpl), true)
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	if vars == nil {
		vars = map[string]string{}
	}
	if _, ok := vars["CADENCE_RELEASES_URL"]; !ok {
		vars["CADENCE_RELEASES_URL"] = "file://" + releases
	}
	r := newFakeRunner()
	e, out, errw := testEnv(t, root, r, vars)
	return &installFixture{t: t, root: root, releases: releases, tmp: tmp, r: r, e: e, out: out, errw: errw}
}

func (f *installFixture) install(args ...string) int {
	return runServer(context.Background(), f.e, append([]string{"install"}, args...))
}

func (f *installFixture) dir(name string) string { return filepath.Join(f.root, name) }

// env — .env установки так, как его читают скрипты.
func (f *installFixture) env(dir string) map[string]string {
	f.t.Helper()
	m, err := parseDotEnv(readFile(f.t, filepath.Join(dir, ".env")), func(string) string { return "" })
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *installFixture) noEnv(dir string) {
	f.t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		f.t.Errorf("%s/.env must not be written", dir)
	}
}

// tmpEmpty — временные файлы install убраны.
func (f *installFixture) tmpEmpty() {
	f.t.Helper()
	if left, _ := os.ReadDir(f.tmp); len(left) > 0 {
		f.t.Errorf("temporary files left: %v", left)
	}
}

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// 1, 2: справка и Windows.
func TestServerInstallHelpAndWindows(t *testing.T) {
	f := installSetup(t, nil)
	if code := f.install("--help"); code != 0 {
		t.Fatalf("code %d", code)
	}
	for _, flag := range []string{"--version", "--dir", "--mode", "--domain", "--email", "--http-addr", "--team", "--team-name", "--name", "--yes", "-y", "--non-interactive"} {
		if !regexp.MustCompile(`(^|[ ,])` + regexp.QuoteMeta(flag) + `([ ,=]|$)`).MatchString(strings.ReplaceAll(f.out.String(), "\n", " \n")) {
			t.Errorf("install --help lacks %s", flag)
		}
	}
	f.out.Reset()
	runServer(context.Background(), f.e, []string{"--help"})
	if strings.Contains(f.out.String(), "for now") || !strings.Contains(f.out.String(), "install ") {
		t.Errorf("clab server --help:\n%s", f.out)
	}
	f.e.goos = "windows"
	if code := f.install("--mode", "proxy", "--yes"); code != 1 || !strings.Contains(f.errw.String(), "WSL2") {
		t.Errorf("windows: %d %s", code, f.errw)
	}
}

// 3, 4, 8, 16: установка из релиза в режиме proxy.
func TestServerInstall(t *testing.T) {
	f := installSetup(t, nil)
	d := f.dir("cadence")
	if code := f.install("--mode", "proxy", "--http-addr", "127.0.0.1:18190", "--yes", "--dir", d); code != 0 {
		t.Fatalf("code %d\n%s\n%s", code, f.out, f.errw)
	}
	if v := strings.TrimSpace(readFile(t, filepath.Join(d, "VERSION"))); v != "v0.7.0" {
		t.Errorf("VERSION %s, want the latest v0.7.0", v)
	}
	if _, err := os.Stat(filepath.Join(d, ".git")); err == nil || f.r.called("git") {
		t.Error("the installation must not be a git clone, and git must not be called")
	}
	if !isFile(filepath.Join(d, packageManifest)) || modeOf(t, filepath.Join(d, "upgrade.sh")) != 0o755 {
		t.Error("the package must be unpacked with its manifest and modes")
	}

	if m := modeOf(t, filepath.Join(d, ".env")); m != 0o600 {
		t.Errorf(".env mode %o", m)
	}
	env := f.env(d)
	hex48 := regexp.MustCompile(`^[0-9a-f]{48}$`)
	if !hex48.MatchString(env["POSTGRES_PASSWORD"]) || !hex48.MatchString(env["SDLC_GITHUB_WEBHOOK_SECRET"]) ||
		!regexp.MustCompile(`^sdlc_[0-9a-f]{48}$`).MatchString(env["SDLC_ADMIN_TOKEN"]) {
		t.Errorf("secrets: %q %q %q", env["POSTGRES_PASSWORD"], env["SDLC_GITHUB_WEBHOOK_SECRET"], env["SDLC_ADMIN_TOKEN"])
	}
	if env["POSTGRES_PASSWORD"] == env["SDLC_GITHUB_WEBHOOK_SECRET"] {
		t.Error("secrets must differ")
	}
	want := map[string]string{
		"COMPOSE_PROJECT_NAME": "cadence",
		"CADENCE_MODE":         "proxy",
		"COMPOSE_FILE":         "docker-compose.yml:compose.proxy.yml:compose.override.yml",
		"CADENCE_HTTP_ADDR":    "127.0.0.1:18190",
		"SDLC_DOMAIN":          "",
		"ACME_EMAIL":           "",
		"POSTGRES_USER":        "sdlc",
		"SDLC_TENANCY":         "single",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s=%q, want %q", k, env[k], v)
		}
	}
	raw := readFile(t, filepath.Join(d, ".env"))
	for _, line := range []string{"# Cadence Lab configuration.", "# SDLC_GITHUB_APP_KEY_FILE=/secrets/github-app.pem", "# CADENCE_REGISTRY=ghcr.io/cadence-lab-tech"} {
		if !strings.Contains(raw, line+"\n") {
			t.Errorf(".env lost the template line %q", line)
		}
	}
	if readFile(t, filepath.Join(d, "compose.override.yml")) != overrideTemplate || modeOf(t, filepath.Join(d, "compose.override.yml")) != 0o644 {
		t.Error("compose.override.yml must be written with mode 644")
	}
	if modeOf(t, filepath.Join(d, "backups")) != 0o700 || modeOf(t, filepath.Join(d, "secrets")) != 0o711 {
		t.Error("backups/ must be 700 and secrets/ 711")
	}
	for _, call := range []string{"docker compose pull --quiet", "docker compose up -d --remove-orphans"} {
		if !f.r.called(call) {
			t.Errorf("%s was not called: %v", call, f.r.calls)
		}
		if got := envValue(f.r.envs[call], "COMPOSE_PROJECT_NAME"); got != "cadence" {
			t.Errorf("%s ran as %q", call, got)
		}
		if got := envValue(f.r.envs[call], "CADENCE_VERSION"); got != "v0.7.0" {
			t.Errorf("%s ran with CADENCE_VERSION=%q", call, got)
		}
		if got := envValue(f.r.envs[call], "PWD"); got != d {
			t.Errorf("%s ran with PWD=%q, want the installation directory", call, got)
		}
	}

	out := f.out.String()
	for _, s := range []string{"Cadence Lab v0.7.0 is up", "http://127.0.0.1:18190", env["SDLC_ADMIN_TOKEN"], "shown once",
		"clab auth login --host http://127.0.0.1:18190 --with-token",
		"0 3 * * * /usr/local/bin/clab server backup --dir " + d + " >> " + filepath.Join(d, "backups", "backup.log") + " 2>&1"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	if strings.Count(out, env["SDLC_ADMIN_TOKEN"]) != 1 {
		t.Error("the generated token must be printed exactly once")
	}
	f.tmpEmpty()

	// 8: после установки clab server status видит её имя и версию.
	f.out.Reset()
	if code := runServer(context.Background(), f.e, []string{"status", "--dir", d}); code != 0 ||
		!regexp.MustCompile(`Name:\s+cadence \(\.env\)`).MatchString(f.out.String()) || !strings.Contains(f.out.String(), "v0.7.0") {
		t.Errorf("status after install: %d\n%s", code, f.out)
	}
}

// 9: какая версия ставится (решение q2).
func TestServerInstallVersionRule(t *testing.T) {
	cases := []struct {
		cli     string
		args    []string
		code    int
		version string // ожидаемый VERSION; пусто — установки нет
		msg     string
	}{
		{"v0.7.0", nil, 0, "v0.7.0", ""},
		{"v0.8.0", nil, 0, "v0.7.0", ""},
		{"v0.6.1", nil, 0, "v0.6.1", "installing v0.6.1, the latest release clab v0.6.1 can manage (the latest is v0.7.0"},
		{"dev", nil, 0, "v0.7.0", ""},
		{"v0.7.0", []string{"--version", "v0.6.1"}, 0, "v0.6.1", ""},
		{"v0.7.0", []string{"--version", "v0.5.3"}, 1, "", "v0.5.3 comes from before release packages"},
		{"v0.7.0", []string{"--version", "v0.5.3"}, 1, "", "git clone --branch v0.5.3 " + cadenceRepoURL + ".git cadence && cd cadence && ./install.sh"},
		{"v0.6.1", []string{"--version", "v0.7.0"}, 1, "", "clab v0.6.1 is older than v0.7.0"},
		{"dev", []string{"--version", "v9.9.9"}, 1, "", "there is no release v9.9.9 with a package at file://"},
		{"v0.7.0", []string{"--version", "0.7"}, 2, "", "'0.7' is not a version"},
	}
	for _, c := range cases {
		f := installSetup(t, nil)
		f.e.cli = c.cli
		d := f.dir("cadence")
		code := f.install(append([]string{"--mode", "proxy", "--yes", "--dir", d}, c.args...)...)
		all := f.out.String() + f.errw.String()
		if code != c.code || (c.msg != "" && !strings.Contains(all, c.msg)) {
			t.Errorf("clab %s %v: code %d, want %d with %q\n%s", c.cli, c.args, code, c.code, c.msg, all)
			continue
		}
		if c.version == "" {
			if _, err := os.Stat(d); err == nil {
				t.Errorf("clab %s %v: %s must not be created", c.cli, c.args, d)
			}
			continue
		}
		if v := strings.TrimSpace(readFile(t, filepath.Join(d, "VERSION"))); v != c.version {
			t.Errorf("clab %s %v: VERSION %s, want %s", c.cli, c.args, v, c.version)
		}
	}
	// Отказ «новее clab» несёт команду обновить clab.
	f := installSetup(t, nil)
	f.e.cli = "v0.6.1"
	f.install("--version", "v0.7.0", "--mode", "proxy", "--yes")
	if !strings.Contains(f.errw.String(), installCommand("v0.7.0")) {
		t.Errorf("no update command:\n%s", f.errw)
	}
}

// 10: подменённый тарбол — отказ, каталога нет, временные файлы убраны.
func TestServerInstallChecksumMismatch(t *testing.T) {
	f := installSetup(t, nil)
	mustWrite(t, filepath.Join(f.releases, "download", "v0.7.0", packageName("v0.7.0")), "tampered", 0o644)
	d := f.dir("cadence")
	if code := f.install("--version", "v0.7.0", "--mode", "proxy", "--yes", "--dir", d); code != 1 ||
		!strings.Contains(f.errw.String(), "checksum of cadence-self-hosted-v0.7.0.tar.gz does not match") ||
		!strings.Contains(f.errw.String(), "nothing was installed") {
		t.Fatalf("code %d\n%s", code, f.errw)
	}
	if _, err := os.Stat(d); err == nil {
		t.Error("the directory must not be created")
	}
	f.tmpEmpty()
	if f.r.called("docker compose pull") || f.r.called("docker compose up") {
		t.Error("compose must not pull or start")
	}
}

// 11: каталог с .env — отказ, ничего не тронуто.
func TestServerInstallExistingEnv(t *testing.T) {
	f := installSetup(t, nil)
	d := installed(t, "v0.7.0", nil)
	before := tree(t, d)
	if code := f.install("--dir", d, "--mode", "proxy", "--yes"); code != 1 {
		t.Fatalf("code %d", code)
	}
	for _, s := range []string{".env exists", "clab server upgrade --dir " + d, "clab server status --dir " + d, "Nothing was changed"} {
		if !strings.Contains(f.errw.String(), s) {
			t.Errorf("refusal lacks %q:\n%s", s, f.errw)
		}
	}
	if tree(t, d) != before || len(f.r.calls) > 0 {
		t.Errorf("the installation must be untouched, docker calls: %v", f.r.calls)
	}
}

// 12: каталог не пуст и не пакет; путь — файл. Отказ до Docker и сети.
func TestServerInstallDirNotUsable(t *testing.T) {
	f := installSetup(t, map[string]string{"CADENCE_RELEASES_URL": "http://127.0.0.1:1/nowhere"})
	busy := f.dir("busy")
	mustWrite(t, filepath.Join(busy, "notes.txt"), "mine\n", 0o644)
	if code := f.install("--dir", busy, "--mode", "proxy", "--yes"); code != 1 ||
		!strings.Contains(f.errw.String(), "is not empty and holds no unpacked package") {
		t.Errorf("not empty: %d %s", code, f.errw)
	}
	file := f.dir("file")
	mustWrite(t, file, "x", 0o644)
	f.errw.Reset()
	if code := f.install("--dir", file, "--mode", "proxy", "--yes"); code != 1 || !strings.Contains(f.errw.String(), "is not a directory") {
		t.Errorf("file: %d %s", code, f.errw)
	}
	if len(f.r.calls) > 0 {
		t.Errorf("refusals must come before Docker: %v", f.r.calls)
	}
	// Пустой каталог годится.
	empty := f.dir("empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	f.e.getenv = func(k string) string {
		if k == "CADENCE_RELEASES_URL" {
			return "file://" + f.releases
		}
		return ""
	}
	if code := f.install("--dir", empty, "--mode", "proxy", "--yes"); code != 0 {
		t.Errorf("empty dir: %d %s", code, f.errw)
	}
}

// 13: установка на месте (решение q3) — без загрузки; --version ≠ VERSION
// — отказ; без --dir и CADENCE_DIR — ./cadence.
func TestServerInstallInPlace(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	f := installSetup(t, map[string]string{"CADENCE_RELEASES_URL": srv.URL})
	d := f.dir("unpacked")
	tarball := filepath.Join(f.releases, "download", "v0.6.1", packageName("v0.6.1"))
	if err := extractTarGz(tarball, d, nil); err != nil {
		t.Fatal(err)
	}
	if code := f.install("--dir", d, "--version", "v0.7.0", "--mode", "proxy", "--yes"); code != 1 ||
		!strings.Contains(f.errw.String(), "holds the package of v0.6.1, not v0.7.0") {
		t.Errorf("--version mismatch: %d %s", code, f.errw)
	}
	f.noEnv(d)
	if code := f.install("--dir", d, "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("in place: %d\n%s", code, f.errw)
	}
	if requests.Load() != 0 {
		t.Errorf("in place must not download: %d requests", requests.Load())
	}
	if f.env(d)["CADENCE_MODE"] != "proxy" || strings.TrimSpace(readFile(t, filepath.Join(d, "VERSION"))) != "v0.6.1" {
		t.Error("in place: .env or VERSION wrong")
	}
	if !strings.Contains(f.out.String(), "installing v0.6.1 in place") {
		t.Errorf("output:\n%s", f.out)
	}

	// Каталог по умолчанию — ./cadence от текущего.
	g := installSetup(t, nil)
	if code := g.install("--mode", "proxy", "--yes"); code != 0 || !isFile(filepath.Join(g.root, "cadence", ".env")) {
		t.Errorf("default dir: %d %s", code, g.errw)
	}
	h := installSetup(t, nil)
	h.e.getenv = func(k string) string {
		switch k {
		case "CADENCE_DIR":
			return "elsewhere"
		case "CADENCE_RELEASES_URL":
			return "file://" + h.releases
		}
		return ""
	}
	if code := h.install("--mode", "proxy", "--yes"); code != 0 || !isFile(filepath.Join(h.root, "elsewhere", ".env")) {
		t.Errorf("CADENCE_DIR: %d %s", code, h.errw)
	}
}

// 14, 15, 16: имя установки.
func TestServerInstallName(t *testing.T) {
	f := installSetup(t, map[string]string{"COMPOSE_PROJECT_NAME": "b"})
	if code := f.install("--name", "a", "--mode", "proxy", "--yes"); code != 1 ||
		!strings.Contains(f.errw.String(), "--name is 'a', but COMPOSE_PROJECT_NAME in the environment is 'b'") {
		t.Errorf("mismatch: %d %s", code, f.errw)
	}
	g := installSetup(t, nil)
	if code := g.install("--name", "Bad_Name", "--mode", "proxy", "--yes"); code != 1 ||
		!strings.Contains(g.errw.String(), installationNameRule) {
		t.Errorf("bad name: %d %s", code, g.errw)
	}
	if len(f.r.calls)+len(g.r.calls) > 0 {
		t.Errorf("name refusals must come before Docker: %v %v", f.r.calls, g.r.calls)
	}

	h := installSetup(t, nil)
	h.r.takenBy = "/opt/cadence"
	d := h.dir("cadence")
	if code := h.install("--dir", d, "--mode", "proxy", "--yes"); code != 1 {
		t.Fatalf("taken: %d", code)
	}
	for _, s := range []string{"the installation name 'cadence' is taken on this machine: the installation in /opt/cadence uses it",
		"clab server install --dir <a new directory> --name cadence-2"} {
		if !strings.Contains(h.errw.String(), s) {
			t.Errorf("taken refusal lacks %q:\n%s", s, h.errw)
		}
	}
	h.noEnv(d)
	if h.r.called("docker compose pull") || h.r.called("docker compose up") {
		t.Error("a refused install must not pull or start")
	}

	k := installSetup(t, map[string]string{"COMPOSE_PROJECT_NAME": "stage"})
	d = k.dir("stage")
	if code := k.install("--dir", d, "--mode", "proxy", "--yes"); code != 0 || k.env(d)["COMPOSE_PROJECT_NAME"] != "stage" {
		t.Errorf("name from the environment: %d %s", code, k.errw)
	}
}

// 17: ответы — без терминала ни одного вопроса, в терминале — вопросы
// install.sh с его умолчаниями.
func TestInstallAnswers(t *testing.T) {
	f := installSetup(t, map[string]string{"SDLC_BOOTSTRAP_TEAM": "ops"})
	a, err := installAnswers(f.e, installOptions{mode: "proxy"})
	if err != nil || a.httpAddr != "127.0.0.1:8080" || a.team != "ops" || a.teamName != "ops" || f.out.Len() > 0 {
		t.Errorf("no terminal: %+v %v, output %q", a, err, f.out)
	}

	f = installSetup(t, nil)
	f.e.terminal = true
	f.e.in = strings.NewReader("proxy\n127.0.0.1:18191\nteam1\n\n")
	a, err = installAnswers(f.e, installOptions{})
	if err != nil || a.mode != "proxy" || a.httpAddr != "127.0.0.1:18191" || a.team != "team1" || a.teamName != "team1" {
		t.Errorf("terminal proxy: %+v %v", a, err)
	}
	for _, q := range []string{"Mode: tls (HTTPS on your domain) or proxy (HTTP behind your reverse proxy) [tls]: ",
		"Address Caddy listens on for your proxy ([ip:]port) [127.0.0.1:8080]: ",
		"Key of the first team (empty: create teams later with clab) []: ", "Display name of team team1 [team1]: "} {
		if !strings.Contains(f.out.String(), q) {
			t.Errorf("no question %q:\n%s", q, f.out)
		}
	}

	f = installSetup(t, nil)
	f.e.terminal = true
	f.e.in = strings.NewReader("\ncadence.example.com\n\n\n")
	a, err = installAnswers(f.e, installOptions{})
	if err != nil || a.mode != "tls" || a.domain != "cadence.example.com" || a.email != "admin@cadence.example.com" || a.team != "" ||
		a.composeFile != "docker-compose.yml:compose.tls.yml:compose.override.yml" {
		t.Errorf("terminal tls: %+v %v", a, err)
	}

	// --yes в терминале — не спрашивать.
	f = installSetup(t, nil)
	f.e.terminal = true
	f.e.in = strings.NewReader("proxy\n")
	if a, err = installAnswers(f.e, installOptions{yes: true, domain: "c.example.com"}); err != nil || a.mode != "tls" || f.out.Len() > 0 {
		t.Errorf("--yes in a terminal asked: %+v %v %q", a, err, f.out)
	}
}

// 6: токен — заданный не печатается, короткий — отказ до .env.
func TestServerInstallToken(t *testing.T) {
	f := installSetup(t, map[string]string{"SDLC_ADMIN_TOKEN": "sdlc_supplied_token_123"})
	d := f.dir("cadence")
	if code := f.install("--dir", d, "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("code %d %s", code, f.errw)
	}
	if strings.Contains(f.out.String(), "sdlc_supplied_token_123") || !strings.Contains(f.out.String(), "the one supplied in SDLC_ADMIN_TOKEN") ||
		f.env(d)["SDLC_ADMIN_TOKEN"] != "sdlc_supplied_token_123" {
		t.Errorf("supplied token:\n%s", f.out)
	}
	g := installSetup(t, map[string]string{"SDLC_ADMIN_TOKEN": "short"})
	d = g.dir("cadence")
	if code := g.install("--dir", d, "--mode", "proxy", "--yes"); code != 1 || !strings.Contains(g.errw.String(), "at least 16 characters") {
		t.Errorf("short token: %d %s", code, g.errw)
	}
	g.noEnv(d)
}

// 5, 18: отказы до записи .env — значение с ', tls без домена, занятые
// порты, неверный адрес и режим.
func TestServerInstallRefusalsBeforeEnv(t *testing.T) {
	cases := []struct {
		args []string
		busy int
		msg  string
	}{
		{[]string{"--mode", "proxy", "--team", "ops", "--team-name", "O'Brien"}, 0, "the value of SDLC_BOOTSTRAP_TEAM_NAME must not contain a single quote or a line break"},
		{[]string{"--mode", "tls"}, 0, "tls mode needs a domain"},
		{[]string{"--mode", "tls", "--domain", "c.example.com"}, 443, "port 443 is already in use"},
		{[]string{"--mode", "proxy", "--http-addr", "127.0.0.1:18190"}, 18190, "port 18190 is already in use on this machine; pick another with --http-addr"},
		{[]string{"--mode", "proxy", "--http-addr", "localhost"}, 0, "--http-addr 'localhost' is not [ip:]port"},
		{[]string{"--mode", "http"}, 0, "mode must be tls or proxy, not 'http'"},
	}
	for _, c := range cases {
		f := installSetup(t, nil)
		f.e.portBusy = func(_ string, port int) bool { return port == c.busy }
		d := f.dir("cadence")
		if code := f.install(append([]string{"--dir", d, "--yes"}, c.args...)...); code != 1 || !strings.Contains(f.errw.String(), c.msg) {
			t.Errorf("%v: code %d, want 1 with %q\n%s", c.args, code, c.msg, f.errw)
		}
		f.noEnv(d)
		if _, err := os.Stat(d); err == nil {
			t.Errorf("%v: %s must not be created", c.args, d)
		}
		f.tmpEmpty()
	}
	// Хост проверки порта: пустой и 0.0.0.0 — 127.0.0.1.
	for addr, want := range map[string]string{"8080": "127.0.0.1", "0.0.0.0:8080": "127.0.0.1", "10.0.0.5:8080": "10.0.0.5"} {
		if host, port, err := parseHTTPAddr(addr); err != nil || host != want || port != 8080 {
			t.Errorf("parseHTTPAddr(%q) = %q %d %v", addr, host, port, err)
		}
	}
}

// 18: EACCES на порту ниже 1024 — не «занят»; EADDRINUSE и ответ на
// подключение — занят.
func TestPortBusy(t *testing.T) {
	refused := func(string) error { return syscall.ECONNREFUSED }
	listenErr := func(errno syscall.Errno) func(string) error {
		return func(string) error { return &net.OpError{Op: "listen", Err: os.NewSyscallError("bind", errno)} }
	}
	if portBusy(refused, listenErr(syscall.EACCES), "", 80) {
		t.Error("EACCES must not count as busy")
	}
	if !portBusy(refused, listenErr(syscall.EADDRINUSE), "", 80) {
		t.Error("EADDRINUSE must count as busy")
	}
	if !portBusy(func(string) error { return nil }, listenErr(syscall.EACCES), "", 443) {
		t.Error("an answering port must count as busy")
	}
	var dialed, listened string
	portBusy(func(a string) error { dialed = a; return errors.New("x") }, func(a string) error { listened = a; return nil }, "", 80)
	if dialed != "127.0.0.1:80" || listened != ":80" {
		t.Errorf("dialed %q listened %q", dialed, listened)
	}
	// Настоящий порт: занятый слушателем — занят.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	if !realPortBusy("127.0.0.1", l.Addr().(*net.TCPAddr).Port) {
		t.Error("a listening port must be busy")
	}
}

// 19: DNS — предупреждения, не отказ.
func TestServerInstallDomainWarnings(t *testing.T) {
	f := installSetup(t, nil)
	f.e.lookupHost = func(string) ([]string, error) { return nil, errors.New("no such host") }
	if code := f.install("--dir", f.dir("a"), "--mode", "tls", "--domain", "c.example.com", "--yes"); code != 0 ||
		!strings.Contains(f.errw.String(), "! c.example.com does not resolve") {
		t.Errorf("unresolved: %d %s", code, f.errw)
	}
	if env := f.env(f.dir("a")); env["SDLC_DOMAIN"] != "c.example.com" || env["ACME_EMAIL"] != "admin@c.example.com" {
		t.Errorf("tls .env: %v", env)
	}
	g := installSetup(t, nil)
	g.e.lookupHost = func(string) ([]string, error) { return []string{"198.51.100.1"}, nil }
	if code := g.install("--dir", g.dir("a"), "--mode", "tls", "--domain", "c.example.com", "--email", "ops@example.com", "--yes"); code != 0 ||
		!strings.Contains(g.errw.String(), "resolves to 198.51.100.1 which is not an address of this machine (203.0.113.7)") {
		t.Errorf("elsewhere: %d %s", code, g.errw)
	}
	// Архитектура без образов — предупреждение.
	h := installSetup(t, nil)
	h.e.arch = "riscv64"
	if code := h.install("--dir", h.dir("a"), "--mode", "proxy", "--yes"); code != 0 || !strings.Contains(h.errw.String(), "this machine is riscv64") {
		t.Errorf("arch: %d %s", code, h.errw)
	}
}

// 20: сбой после .env — .env остаётся, команда старта с cd.
func TestServerInstallFailureAfterEnv(t *testing.T) {
	f := installSetup(t, nil)
	f.r.pullFails = true
	d := f.dir("cadence")
	if code := f.install("--dir", d, "--mode", "proxy", "--yes"); code != 1 {
		t.Fatalf("pull: code %d", code)
	}
	start := "cd " + d + " && export CADENCE_VERSION=$(cat VERSION) && docker compose up -d"
	if !isFile(filepath.Join(d, ".env")) || !strings.Contains(f.errw.String(), start) || !strings.Contains(f.errw.String(), ".env is kept") {
		t.Errorf("pull failure:\n%s", f.errw)
	}
	if f.r.called("docker compose up") {
		t.Error("no start after a failed pull")
	}

	g := installSetup(t, map[string]string{"CADENCE_READY_TIMEOUT": "0"})
	g.r.healthy = false
	d = g.dir("cadence")
	start = "cd " + d + " && export CADENCE_VERSION=$(cat VERSION) && docker compose up -d"
	if code := g.install("--dir", d, "--mode", "proxy", "--yes"); code != 1 {
		t.Fatalf("not ready: code %d", code)
	}
	if !g.r.called("docker compose logs --tail 40 api caddy") || !strings.Contains(g.errw.String(), "did not become ready in 0s") ||
		!strings.Contains(g.errw.String(), start) || !isFile(filepath.Join(d, ".env")) {
		t.Errorf("not ready: %v\n%s", g.r.calls, g.errw)
	}
	// Неверный таймаут — до записи.
	k := installSetup(t, map[string]string{"CADENCE_READY_TIMEOUT": "soon"})
	if code := k.install("--dir", k.dir("c"), "--mode", "proxy", "--yes"); code != 1 || len(k.r.calls) > 0 {
		t.Errorf("bad timeout: %d %v", code, k.r.calls)
	}
}

// ── паритет .env с set_kv install.sh (критерий 5) ─────────────────────────

const parityTemplate = `# comment KEY=x
COMPOSE_PROJECT_NAME=cadence
CADENCE_MODE=tls
SDLC_DOMAIN=cadence.example.com
# SDLC_GITHUB_APP_KEY_FILE=/secrets/github-app.pem
SDLC_GITHUB_APP_KEY_FILE=
SDLC_BOOTSTRAP_TEAM_NAME=`

var parityPairs = []envPair{
	{"COMPOSE_PROJECT_NAME", "stage"},
	{"CADENCE_MODE", "proxy"},
	{"SDLC_DOMAIN", ""},
	{"SDLC_BOOTSTRAP_TEAM_NAME", `Acme & Co $HOME #1 "ops"`},
	{"NEW_KEY", "x/y:z@a+b=c,d-e_f.g"},
	{"SECOND", "two words"},
}

const parityWant = `# comment KEY=x
COMPOSE_PROJECT_NAME=stage
CADENCE_MODE=proxy
SDLC_DOMAIN=
# SDLC_GITHUB_APP_KEY_FILE=/secrets/github-app.pem
SDLC_GITHUB_APP_KEY_FILE=
SDLC_BOOTSTRAP_TEAM_NAME='Acme & Co $HOME #1 "ops"'
NEW_KEY=x/y:z@a+b=c,d-e_f.g
SECOND='two words'
`

// setKV — set_kv из install.sh дословно: эталон паритета.
const setKV = `set -euo pipefail
die() { printf '%s\n' "$*" >&2; exit 1; }
set_kv() {
  case "$2" in
  *"'"* | *$'\n'*) die "the value of $1 must not contain a single quote or a line break" ;;
  esac
  KEY="$1" VALUE="$2" awk '
    BEGIN {
      k = ENVIRON["KEY"]; v = ENVIRON["VALUE"]; done = 0
      if (v ~ /[^A-Za-z0-9_.,:\/@+=-]/) v = "\047" v "\047"
    }
    index($0, k "=") == 1 { print k "=" v; done = 1; next }
    { print }
    END { if (!done) print k "=" v }
  ' .env.tmp >.env.tmp.next && mv .env.tmp.next .env.tmp
}
`

func TestRenderEnvParity(t *testing.T) {
	for _, tpl := range []string{parityTemplate, parityTemplate + "\n"} {
		if got := renderEnv(tpl, parityPairs); got != parityWant {
			t.Errorf("renderEnv:\n%s\nwant:\n%s", got, parityWant)
		}
	}
	if got := renderEnv("", []envPair{{"A", "b"}}); got != "A=b\n" {
		t.Errorf("empty template: %q", got)
	}

	// parseDotEnv читает то, что записано, буквально.
	parsed, err := parseDotEnv(parityWant, func(k string) string { return map[string]string{"HOME": "/root"}[k] })
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parityPairs {
		if parsed[p.key] != p.value {
			t.Errorf("parseDotEnv %s=%q, want %q", p.key, parsed[p.key], p.value)
		}
	}

	bash, errB := exec.LookPath("bash")
	_, errA := exec.LookPath("awk")
	if errB != nil || errA != nil {
		t.Skip("bash or awk is missing: the golden file above stands for set_kv")
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".env.tmp"), parityTemplate, 0o600)
	script := setKV
	args := []string{"-c", "", "set_kv"}
	for i, p := range parityPairs {
		script += `set_kv "${` + strconv.Itoa(2*i+1) + `}" "${` + strconv.Itoa(2*i+2) + `}"` + "\n"
		args = append(args, p.key, p.value)
	}
	args[1] = script
	cmd := exec.Command(bash, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set_kv: %v %s", err, out)
	}
	if ref := readFile(t, filepath.Join(dir, ".env.tmp")); ref != renderEnv(parityTemplate, parityPairs) {
		t.Errorf("set_kv of install.sh:\n%s\nrenderEnv:\n%s", ref, renderEnv(parityTemplate, parityPairs))
	}
	// И shell читает .env так же, как parseDotEnv.
	mustWrite(t, filepath.Join(dir, ".env"), parityWant, 0o600)
	for _, p := range parityPairs {
		cmd := exec.Command(bash, "-c", `set -a; . ./.env; printf '%s' "${`+p.key+`}"`)
		cmd.Dir = dir
		cmd.Env = []string{"HOME=/root", "PATH=" + os.Getenv("PATH")}
		out, err := cmd.Output()
		if err != nil || string(out) != p.value {
			t.Errorf("bash reads %s=%q (%v), want %q", p.key, out, err, p.value)
		}
	}
	// set_kv отказывает на ' — clab тоже, до записи (TestServerInstallRefusalsBeforeEnv).
	cmd = exec.Command(bash, "-c", setKV+`set_kv A "it's"`)
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Error("set_kv must refuse a single quote")
	}
}
