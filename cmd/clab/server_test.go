package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Тесты clab server идут без Docker: подставной исполнитель отвечает за
// docker и git так, как отвечал бы стек, а релизы — каталог file:// или
// httptest. Сценарии — те же, что ci/check-upgrade.sh и ci/check-stack.sh
// гоняют на настоящем стеке (CLAB-125, критерии 2-9).

// fakeRunner — docker и git без Docker. Каждый вызов записывается.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	stdin map[string]string   // вызов → то, что пришло на stdin
	envs  map[string][]string // вызов → его окружение (последний такой)

	postgresRunning bool
	apiContainer    string
	healthy         bool
	pullFails       bool
	dumpBytes       int
	gitAvailable    bool
	gitTags         map[string]bool
	takenBy         string // каталог установки, чьи контейнеры носят проверяемое имя (install)

	// Внешняя база (CLAB-127).
	composeVersion string // docker compose version --short
	dbcheckFails   bool   // /api dbcheck отвечает «не готова»
	dbClientFails  bool   // клиент бэкапа (db-client) не подключается

	// Копия вне машины и verify (CLAB-132).
	s3PutFails      bool   // /api backup-s3 put отвечает сбоем
	s3GetCode       int    // код выхода backup-s3 get; 0 — скачал s3GetName
	s3GetName       string // дамп, который «скачивает» get, в смонтированный каталог
	verifyLoadFails bool   // psql одноразовой базы не загружает дамп
	dbcheckJSON     string // ответ /api dbcheck --json одноразовой базе
	verifyCounts    string // проекты, задачи, артефакты

	// Подписи (CLAB-197): cosign в PATH, его версия и что он отвергает —
	// package (verify-blob) или репозиторий образа; ghcr.io не отвечает.
	cosign        bool
	cosignVersion string
	cosignBad     string
	ghcrDown      bool

	// Окружение сервиса api в docker compose config (CLAB-199): JSON-объект,
	// пусто — без environment.
	apiEnvironment string
}

// exitErr — код выхода внешней команды, как у *exec.ExitError.
type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

// mountOf — каталог хоста, смонтированный как /backups.
func mountOf(args []string) string {
	for i, a := range args {
		if a == "-v" && i+1 < len(args) && strings.HasSuffix(args[i+1], ":/backups") {
			return strings.TrimSuffix(args[i+1], ":/backups")
		}
	}
	return ""
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{postgresRunning: true, apiContainer: "api123", healthy: true, dumpBytes: 4096,
		composeVersion: "2.29.1", stdin: map[string]string{}, envs: map[string][]string{},
		dbcheckJSON:  `{"ok": true, "schema": "cadence", "schema_version": 51, "image_schema_version": 51}`,
		verifyCounts: "2 5 9"}
}

var errExit1 = errors.New("exit status 1")

func (f *fakeRunner) run(_ context.Context, c cmdSpec) error {
	line := c.String()
	f.mu.Lock()
	f.calls = append(f.calls, line)
	f.envs[line] = c.env
	f.mu.Unlock()
	if c.stdin != nil {
		body, _ := io.ReadAll(c.stdin)
		f.mu.Lock()
		f.stdin[line] = string(body)
		f.mu.Unlock()
	}
	write := func(s string) {
		if c.stdout != nil {
			io.WriteString(c.stdout, s)
		}
	}
	args := strings.Join(c.args, " ")
	switch c.name {
	case "cosign":
		return f.runCosign(c)
	case "git":
		if !f.gitAvailable {
			return errExit1
		}
		switch {
		case strings.HasPrefix(args, "rev-parse --git-dir"):
			write(".git\n")
			return nil
		case strings.HasPrefix(args, "rev-parse -q --verify refs/tags/"):
			tag := strings.TrimSuffix(strings.TrimPrefix(args, "rev-parse -q --verify refs/tags/"), "^{commit}")
			if f.gitTags[tag] {
				return nil
			}
			return errExit1
		case strings.HasPrefix(args, "diff --name-only"):
			return nil
		}
		return errExit1
	case "docker":
	default:
		return fmt.Errorf("unexpected command %s", line)
	}
	dump := func() {
		// Несжимаемое содержимое: проверка размера смотрит на сжатый дамп.
		var b strings.Builder
		for n := 0; b.Len() < f.dumpBytes; n++ {
			b.WriteString(sha(fmt.Sprint(n)))
		}
		write("-- PostgreSQL database dump\n" + b.String())
	}
	switch {
	case strings.HasSuffix(args, " config --images"):
		write(fakeComposeImages(c))
		return nil
	case strings.Contains(args, " api backup-s3 put "):
		if f.s3PutFails {
			io.WriteString(c.stderr, "✗ upload to s3://bk/ failed: PUT x: 403 Forbidden AccessDenied\n")
			return exitErr(1)
		}
		write("✓ uploaded\n")
		return nil
	case strings.Contains(args, " api backup-s3 get "):
		if f.s3GetCode != 0 {
			return exitErr(f.s3GetCode)
		}
		gzipFile(filepath.Join(mountOf(c.args), f.s3GetName), "-- dump from the bucket\n")
		write(f.s3GetName + "\n")
		return nil
	case strings.HasPrefix(args, "compose config --format json "):
		// Как настоящий compose: с сервисами, от которых этот зависит.
		services := `"postgres": {"image": "pgvector/pgvector:pg17"}, "db-client": {"image": "pgvector/pgvector:pg18"}`
		switch strings.TrimPrefix(args, "compose config --format json ") {
		case "api":
			env := ""
			if f.apiEnvironment != "" {
				env = `, "environment": ` + f.apiEnvironment
			}
			services = `"postgres": {"image": "pgvector/pgvector:pg17"}, "api": {"image": "ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.10.0"` + env + `}`
		}
		write(`{"name": "cadence", "services": {` + services + `}}`)
		return nil
	case strings.HasPrefix(args, "network create"), strings.HasPrefix(args, "run -d --name"),
		strings.HasPrefix(args, "rm -f -v"), strings.HasPrefix(args, "network rm"):
		return nil
	case strings.HasPrefix(args, "exec -i ") && strings.Contains(args, " psql "):
		if f.verifyLoadFails {
			return errExit1
		}
		return nil
	case strings.HasPrefix(args, "exec ") && strings.Contains(args, " pg_isready "):
		return nil
	case strings.HasPrefix(args, "exec ") && strings.Contains(args, " psql -X -q -At "):
		write(f.verifyCounts + "\n")
		return nil
	case strings.HasPrefix(args, "exec ") && strings.Contains(args, " psql "):
		return nil
	case strings.HasPrefix(args, "run --rm --network ") && strings.HasSuffix(args, " dbcheck --json"):
		write(f.dbcheckJSON + "\n")
		if !strings.Contains(f.dbcheckJSON, `"ok": true`) {
			return errExit1
		}
		return nil
	case strings.HasPrefix(args, "run --rm --network ") && strings.HasSuffix(args, " dbcheck"):
		write("✗ schema: ...\n")
		return errExit1
	case args == "compose version", args == "info":
		return nil
	case args == "compose version --short":
		write(f.composeVersion + "\n")
		return nil
	case strings.Contains(args, "run --rm --no-deps -T api dbcheck"):
		if f.dbcheckFails {
			write("✗ vector: ...\n")
			return errExit1
		}
		write("✓ the database is ready\n")
		return nil
	case strings.HasPrefix(args, "compose run --rm --no-deps -T -e PGPASSWORD db-client"):
		if f.dbClientFails {
			return errExit1
		}
		if strings.Contains(args, " pg_dump ") {
			dump()
		}
		return nil
	case args == "compose down -v --remove-orphans":
		return nil
	case args == "compose ps -q --status running postgres":
		if f.postgresRunning {
			write("pg123\n")
		}
		return nil
	case args == "compose ps -a -q api":
		if f.apiContainer != "" {
			write(f.apiContainer + "\n")
		}
		return nil
	case strings.HasPrefix(args, "ps -a -q --filter label=com.docker.compose.project="):
		if f.takenBy != "" {
			write("c1\n")
		}
		return nil
	case strings.HasPrefix(args, "volume ls -q --filter"):
		return nil
	case strings.HasPrefix(args, "inspect -f {{index .Config.Labels"):
		write(f.takenBy + "\n")
		return nil
	case strings.HasPrefix(args, "inspect -f"):
		if f.healthy {
			write("healthy\n")
		} else {
			write("starting\n")
		}
		return nil
	case strings.HasPrefix(args, "compose exec -T caddy wget"):
		if f.healthy {
			return nil
		}
		return errExit1
	case strings.HasPrefix(args, "compose exec -T postgres pg_dump"):
		dump()
		return nil
	case args == "compose pull --quiet":
		if f.pullFails {
			return errExit1
		}
		return nil
	case strings.HasPrefix(args, "run --rm --volumes-from"),
		strings.HasPrefix(args, "compose exec -T postgres"),
		strings.HasPrefix(args, "compose exec -T caddy caddy reload"),
		strings.HasPrefix(args, "compose stop"),
		strings.HasPrefix(args, "compose up"),
		strings.HasPrefix(args, "compose ps"),
		strings.HasPrefix(args, "compose logs"):
		return nil
	}
	return fmt.Errorf("unexpected docker call: %s", line)
}

// runCosign — cosign 3.x: version, verify-blob и verify; отказывает тому,
// что назвал cosignBad.
func (f *fakeRunner) runCosign(c cmdSpec) error {
	if len(c.args) == 0 {
		return errExit1
	}
	switch c.args[0] {
	case "version":
		v := f.cosignVersion
		if v == "" {
			v = "v3.1.3"
		}
		if c.stdout != nil {
			io.WriteString(c.stdout, "  ______   ______\n\nGitVersion:    "+v+"\nGitCommit:     0000000\n")
		}
		return nil
	case "verify-blob":
		if f.cosignBad == "package" {
			io.WriteString(c.stderr, "Error: none of the expected identities matched what was in the certificate\n")
			return exitErr(1)
		}
		return nil
	case "verify":
		if f.cosignBad != "" && strings.Contains(c.args[len(c.args)-1], "/"+f.cosignBad+"@") {
			io.WriteString(c.stderr, "Error: no matching signatures: none of the expected identities matched what was in the certificate\n")
			return exitErr(1)
		}
		if c.stdout != nil {
			io.WriteString(c.stdout, `[{"critical":{}}]`+"\n")
		}
		return nil
	}
	return fmt.Errorf("unexpected cosign call: %s", c)
}

// composeVar — подстановка переменных compose в тех формах, что у пакета:
// ${VAR:-по умолчанию}, ${VAR-по умолчанию}, ${VAR:?сообщение}.
var composeVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-|-|:\?)([^}]*)\}`)

// fakeComposeImages — docker compose -f … config --images: строки image:
// файлов после -f с переменными окружения команды.
func fakeComposeImages(c cmdSpec) string {
	env := map[string]string{}
	for _, kv := range c.env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	var files []string
	for i, a := range c.args {
		if a == "-f" && i+1 < len(c.args) {
			files = append(files, c.args[i+1])
		}
	}
	var b strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			img, ok := strings.CutPrefix(strings.TrimSpace(line), "image:")
			if !ok {
				continue
			}
			b.WriteString(composeVar.ReplaceAllStringFunc(strings.TrimSpace(img), func(m string) string {
				p := composeVar.FindStringSubmatch(m)
				v, set := env[p[1]]
				switch p[2] {
				case ":-":
					if v == "" {
						return p[3]
					}
				case "-":
					if !set {
						return p[3]
					}
				}
				return v
			}) + "\n")
		}
	}
	return b.String()
}

func (f *fakeRunner) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// testEnv — serverEnv теста: выводы в буферы, без терминала, docker «есть».
func testEnv(t *testing.T, cwd string, r *fakeRunner, vars map[string]string) (*serverEnv, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	origLook := lookPath
	lookPath = func(name string) (string, error) {
		if (name == "git" && !r.gitAvailable) || (name == "cosign" && !r.cosign) {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}
	t.Cleanup(func() { lookPath = origLook })
	var out, errw bytes.Buffer
	environ := []string{"PATH=/usr/bin"}
	for k, v := range vars {
		environ = append(environ, k+"="+v)
	}
	return &serverEnv{
		out:     &out,
		errw:    &errw,
		in:      strings.NewReader(""),
		getenv:  func(k string) string { return vars[k] },
		environ: environ,
		cwd:     cwd,
		goos:    "linux",
		run:     r,
		now:     func() time.Time { return time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC) },
		sleep:   func(time.Duration) {},
		http:    http.DefaultClient,
		uid:     1000,
		gid:     1000,
		cli:     "v0.7.0",
		self:    "/usr/local/bin/clab",

		arch:       "amd64",
		portBusy:   func(string, int) bool { return false },
		lookupHost: func(string) ([]string, error) { return []string{"203.0.113.7"}, nil },
		localAddrs: func() []string { return []string{"203.0.113.7"} },
		reachable:  func(context.Context, string) bool { return !r.ghcrDown },
	}, &out, &errw
}

// ── пакеты и релизы ─────────────────────────────────────────────────────────

// pkgFiles — файлы пакета версии: VERSION подставляется.
func pkgFiles(v string, extra map[string]string) map[string]string {
	files := map[string]string{
		"VERSION":            v + "\n",
		"docker-compose.yml": "name: cadence\nservices: {}\n",
		"compose.proxy.yml":  "services: {}\n",
		"compose.tls.yml":    "services: {}\n",
		"Caddyfile":          ":80\n",
		".env.example":       "CADENCE_MODE=tls\n",
		"upgrade.sh":         "#!/usr/bin/env bash\necho " + v + "\n",
		"lib.sh":             "# lib of " + v + "\n",
		"docs/admin.md":      "# Admin guide " + v + "\n",
	}
	for k, val := range extra {
		if val == "" {
			delete(files, k)
		} else {
			files[k] = val
		}
	}
	return files
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// manifestOf — package.sha256 файлов, как его пишет ci/package.sh.
func manifestOf(files map[string]string) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", sha(files[n]), n)
	}
	return b.String()
}

// buildPackage — тарбол пакета с эталоном внутри, как ci/package.sh.
func buildPackage(t *testing.T, files map[string]string) []byte {
	t.Helper()
	all := map[string]string{packageManifest: manifestOf(files)}
	for k, v := range files {
		all[k] = v
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		mode := int64(0o644)
		if strings.HasSuffix(n, ".sh") && n != "lib.sh" {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: mode, Size: int64(len(all[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, all[n])
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// publish кладёт релиз версии в каталог релизов; latest — и в latest/.
func publish(t *testing.T, releases, v string, files map[string]string, latest bool) {
	t.Helper()
	tarball := buildPackage(t, files)
	name := packageName(v)
	sums := fmt.Sprintf("%s  %s\n%s  get-cadence.sh\n", sha(string(tarball)), name, sha("#!/bin/sh\n"))
	dirs := []string{filepath.Join(releases, "download", v)}
	if latest {
		dirs = append(dirs, filepath.Join(releases, "latest", "download"))
	}
	for _, d := range dirs {
		mustWrite(t, filepath.Join(d, name), string(tarball), 0o644)
		mustWrite(t, filepath.Join(d, "checksums.txt"), sums, 0o644)
		mustWrite(t, filepath.Join(d, "get-cadence.sh"), "#!/bin/sh\n", 0o755)
	}
}

func mustWrite(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// installed — установка версии v из пакета: файлы, эталон, .env,
// compose.override.yml, backups/, secrets/.
func installed(t *testing.T, v string, extra map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cadence")
	files := pkgFiles(v, extra)
	for n, c := range files {
		mustWrite(t, filepath.Join(dir, n), c, 0o644)
	}
	mustWrite(t, filepath.Join(dir, packageManifest), manifestOf(files), 0o644)
	mustWrite(t, filepath.Join(dir, ".env"), "CADENCE_MODE=proxy\n"+
		"COMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.override.yml\n"+
		"CADENCE_HTTP_ADDR=127.0.0.1:18190\nPOSTGRES_PASSWORD=secret\nSDLC_ADMIN_TOKEN='sdlc_abc'\n", 0o600)
	mustWrite(t, filepath.Join(dir, "compose.override.yml"), "services: {}\n", 0o644)
	mustWrite(t, filepath.Join(dir, "secrets", "key.pem"), "not a key\n", 0o600)
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// tree — sha256 каждого файла установки, кроме бэкапов: что значит
// «не тронуто» в проверках ci/.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if fi.IsDir() {
			if rel == "backups" {
				return filepath.SkipDir
			}
			return nil
		}
		lines = append(lines, sha(readFile(t, p))+"  "+rel)
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// ── каркас ──────────────────────────────────────────────────────────────────

// Критерий 1: справка перечисляет подкоманды, по-английски, без клиента API.
func TestServerHelp(t *testing.T) {
	e, out, _ := testEnv(t, t.TempDir(), newFakeRunner(), nil)
	if code := runServer(context.Background(), e, []string{"--help"}); code != 0 {
		t.Fatalf("code %d", code)
	}
	for _, sub := range []string{"install", "upgrade", "backup", "restore", "status", "--dir"} {
		if !strings.Contains(out.String(), sub) {
			t.Errorf("help lacks %q:\n%s", sub, out)
		}
	}
	for _, sub := range serverCommands() {
		out.Reset()
		if code := runServer(context.Background(), e, []string{sub.name, "--help"}); code != 0 || !strings.Contains(out.String(), "--dir DIR") {
			t.Errorf("%s --help: code %d\n%s", sub.name, code, out)
		}
	}
	if !localCommand("server") || localCommand("tasks") {
		t.Error("server must be a local command and tasks must not")
	}
}

// Критерий 3: на Windows — отказ с подсказкой про WSL2.
func TestServerWindows(t *testing.T) {
	e, _, errw := testEnv(t, t.TempDir(), newFakeRunner(), nil)
	e.goos = "windows"
	if code := runServer(context.Background(), e, []string{"status"}); code == 0 {
		t.Fatal("status on windows must fail")
	}
	if !strings.Contains(errw.String(), "WSL2") {
		t.Errorf("no WSL2 hint: %s", errw)
	}
}

// stdin из /dev/null (cron, systemd) — символьное устройство, но не
// терминал: restore без --yes должен отказать, а не спрашивать.
func TestServerEnvDevNullIsNotTerminal(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer null.Close()
	stdin := os.Stdin
	os.Stdin = null
	defer func() { os.Stdin = stdin }()
	e, err := newServerEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.terminal {
		t.Error("stdin from /dev/null is taken for a terminal")
	}
}

func TestServerUnknownCommandAndOption(t *testing.T) {
	e, _, errw := testEnv(t, t.TempDir(), newFakeRunner(), nil)
	if code := runServer(context.Background(), e, []string{"uninstall"}); code != 2 {
		t.Errorf("unknown command: code %d", code)
	}
	dir := installed(t, "v0.6.1", nil)
	e.cwd = dir
	errw.Reset()
	if code := runServer(context.Background(), e, []string{"backup", "--bogus"}); code != 2 || !strings.Contains(errw.String(), "unknown option: --bogus") {
		t.Errorf("unknown option: code %d, %s", code, errw)
	}
}

func TestParseServerArgsAnyOrder(t *testing.T) {
	var yes, ignore bool
	var dir string
	pos, err := parseServerArgs([]string{"backups/x.sql.gz", "--yes", "--dir=/opt/c", "--ignore-version"},
		map[string]*bool{"--yes": &yes, "--ignore-version": &ignore}, map[string]*string{"--dir": &dir})
	if err != nil || !yes || !ignore || dir != "/opt/c" || len(pos) != 1 || pos[0] != "backups/x.sql.gz" {
		t.Fatalf("pos %v yes %v ignore %v dir %q err %v", pos, yes, ignore, dir, err)
	}
	if _, err := parseServerArgs([]string{"--dir"}, nil, map[string]*string{"--dir": &dir}); err == nil {
		t.Error("--dir without a value must fail")
	}
}

// Критерий 2: каталог — --dir, CADENCE_DIR, вверх от текущего; вне
// установки — ошибка с подсказкой --dir.
func TestFindInstallation(t *testing.T) {
	dir := installed(t, "v0.6.1", nil)
	sub := filepath.Join(dir, "docs", "deep")
	os.MkdirAll(sub, 0o755)
	outside := t.TempDir()

	e, _, _ := testEnv(t, sub, newFakeRunner(), nil)
	if got, err := findInstallation(e, ""); err != nil || got != dir {
		t.Errorf("from a subdirectory: %q %v", got, err)
	}
	e.cwd = outside
	if got, err := findInstallation(e, dir); err != nil || got != dir {
		t.Errorf("--dir: %q %v", got, err)
	}
	e.getenv = func(k string) string {
		if k == "CADENCE_DIR" {
			return dir
		}
		return ""
	}
	e.cwd = "/"
	if got, err := findInstallation(e, ""); err != nil || got != dir {
		t.Errorf("CADENCE_DIR from /: %q %v", got, err)
	}
	e.getenv = func(string) string { return "" }
	e.cwd = outside
	if _, err := findInstallation(e, ""); err == nil || !strings.Contains(err.Error(), "--dir") {
		t.Errorf("outside: %v", err)
	}
	if _, err := findInstallation(e, outside); err == nil {
		t.Error("--dir of a directory without an installation must fail")
	}

	// Бывший git-клон без эталона: VERSION и docker-compose.yml.
	clone := t.TempDir()
	mustWrite(t, filepath.Join(clone, "VERSION"), "v0.5.3\n", 0o644)
	mustWrite(t, filepath.Join(clone, "docker-compose.yml"), "services: {}\n", 0o644)
	os.Mkdir(filepath.Join(clone, ".git"), 0o755)
	e.cwd = clone
	inst, err := openInstallation(e, "")
	if err != nil || inst.kind() != "git clone" {
		t.Errorf("clone: %v %v", inst, err)
	}
}

// Критерии 2 и 8: status печатает каталог, VERSION, версию clab, способ
// установки и вердикт совместимости.
func TestServerStatus(t *testing.T) {
	dir := installed(t, "v0.6.1", nil)
	r := newFakeRunner()
	e, out, _ := testEnv(t, filepath.Join(dir, "docs"), r, nil)
	if code := runServer(context.Background(), e, []string{"status"}); code != 0 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{dir, "v0.6.1 (VERSION)", "v0.7.0", "package", "Compatible:", "yes", "proxy", "127.0.0.1:18190"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}

	out.Reset()
	e.cli = "v0.6.0"
	if code := runServer(context.Background(), e, []string{"status", "--dir", dir}); code != 0 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out.String(), "no - see below") || !strings.Contains(out.String(), "CLAB_VERSION=v0.6.1 sh") {
		t.Errorf("an older clab must be reported with the update command:\n%s", out)
	}

	jsonOut = true
	defer func() { jsonOut = false }()
	out.Reset()
	e.cli = "v0.7.0"
	if code := runServer(context.Background(), e, []string{"status"}); code != 0 || !strings.Contains(out.String(), `"version": "v0.6.1"`) {
		t.Errorf("--json: %d\n%s", code, out)
	}
}

// Критерий 8: правило версий на таблице.
func TestStackCompat(t *testing.T) {
	cases := []struct {
		cli, stack string
		ok         bool
		want       string
	}{
		{"v0.6.1", "v0.7.0", false, "older than the stack v0.7.0"}, // clab < стек
		{"v0.7.0", "v0.7.0", true, ""},                             // clab = стек
		{"v0.8.2", "v0.6.0", true, ""},                             // clab > стек
		{"v0.7.0", "v0.5.3", false, "./upgrade.sh v0.6.0"},         // стек до пакетов
		{"dev", "v0.9.0", true, ""},                                // сборка из ветки
		{"dev", "v0.5.3", false, "git clone"},
		{"v0.7.0-rc1", "v0.7.0", true, ""},
	}
	for _, c := range cases {
		err := stackCompat(c.cli, c.stack, false)
		if (err == nil) != c.ok || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("clab %s, stack %s: %v", c.cli, c.stack, err)
		}
	}
	if err := stackCompat("v0.6.1", "v0.7.0", true); err == nil || !strings.Contains(err.Error(), "brew upgrade clab") ||
		!strings.Contains(err.Error(), installCommand("v0.7.0")) {
		t.Errorf("brew hint: %v", err)
	}
	// Цель новее clab — предупреждение, не отказ.
	if w := targetNewerWarning("v0.7.0", "v0.8.0"); !strings.Contains(w, "older than the target v0.8.0") {
		t.Errorf("target newer: %q", w)
	}
	for _, c := range [][2]string{{"v0.7.0", "v0.7.0"}, {"v0.8.0", "v0.7.0"}, {"dev", "v0.9.0"}} {
		if w := targetNewerWarning(c[0], c[1]); w != "" {
			t.Errorf("clab %s, target %s: unexpected warning %q", c[0], c[1], w)
		}
	}
}

// Критерий 9: .env читается так, как его читает set -a; . ./.env.
func TestParseDotEnv(t *testing.T) {
	env := map[string]string{"HOME": "/root"}
	got, err := parseDotEnv(`# comment
CADENCE_MODE=proxy
export SDLC_DOMAIN=cadence.example.com
SDLC_BOOTSTRAP_TEAM_NAME='Acme & Co (ops)'
QUOTED="a \"b\" \$c \\ ${CADENCE_MODE}"
SECRETS=$HOME/secrets   # trailing comment
EMPTY=
MULTI='line one
line two'
  INDENTED=yes
JOINED=a'b c'"d"
DEF=${UNSET:-fallback}/${EMPTY:-x}/${EMPTY-y}/${CADENCE_MODE:-z}
`, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CADENCE_MODE":             "proxy",
		"SDLC_DOMAIN":              "cadence.example.com",
		"SDLC_BOOTSTRAP_TEAM_NAME": "Acme & Co (ops)",
		"QUOTED":                   `a "b" $c \ proxy`,
		"SECRETS":                  "/root/secrets",
		"EMPTY":                    "",
		"MULTI":                    "line one\nline two",
		"INDENTED":                 "yes",
		"JOINED":                   "ab cd",
		"DEF":                      "fallback/x//proxy",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for _, bad := range []string{"A=b c\n", "A='open\n", "A=`id`\n", "A=$(id)\n", "A=${B:?x}\n", "just text\n", `A="open` + "\n"} {
		if _, err := parseDotEnv(bad, nil); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// .env перекрывает окружение, VERSION — .env; имя установки — из .env, а
// COMPOSE_PROJECT_NAME окружения до compose не доходит (CLAB-164).
func TestComposeEnv(t *testing.T) {
	dir := installed(t, "v0.6.1", nil)
	mustWrite(t, filepath.Join(dir, ".env"), "COMPOSE_PROJECT_NAME=check-clab-164\nCADENCE_MODE=proxy\nCADENCE_VERSION=v0.1.0\nKEEP_DAYS=3\n", 0o600)
	e, _, _ := testEnv(t, dir, newFakeRunner(), map[string]string{"COMPOSE_PROJECT_NAME": "cadence", "KEEP_DAYS": "9", "CADENCE_MODE": "tls"})
	inst, err := openInstallation(e, "")
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(inst.composeEnv(), "\n")
	if strings.Count(env, "COMPOSE_PROJECT_NAME=") != 1 {
		t.Errorf("COMPOSE_PROJECT_NAME must be set once:\n%s", env)
	}
	for _, want := range []string{"COMPOSE_PROJECT_NAME=check-clab-164", "CADENCE_VERSION=v0.6.1", "CADENCE_MODE=proxy", "KEEP_DAYS=3"} {
		if !strings.Contains(env, want) {
			t.Errorf("compose env lacks %s:\n%s", want, env)
		}
	}
	if strings.Contains(env, "CADENCE_VERSION=v0.1.0") || inst.get("KEEP_DAYS") != "3" {
		t.Errorf("VERSION must win over .env and .env over the environment:\n%s", env)
	}
}

// ── релизы ──────────────────────────────────────────────────────────────────

// Критерий 7 и 9: загрузка и сумма — file:// и HTTP; подмена суммы — отказ.
func TestDownloadPackage(t *testing.T) {
	releases := t.TempDir()
	publish(t, releases, "v0.6.1", pkgFiles("v0.6.1", nil), true)
	srv := httptest.NewServer(http.FileServer(http.Dir(releases)))
	defer srv.Close()

	for _, base := range []string{"file://" + releases, srv.URL + "/"} {
		dir := installed(t, "v0.6.0", nil)
		e, _, _ := testEnv(t, dir, newFakeRunner(), map[string]string{"CADENCE_RELEASES_URL": base})
		inst, err := openInstallation(e, "")
		if err != nil {
			t.Fatal(err)
		}
		work := t.TempDir()
		if v, err := inst.latestVersion(context.Background(), work); err != nil || v != "v0.6.1" {
			t.Errorf("%s latest: %q %v", base, v, err)
		}
		tarball, err := inst.downloadPackage(context.Background(), "v0.6.1", work)
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if err := extractTarGz(tarball, filepath.Join(work, "x"), nil); err != nil || readFile(t, filepath.Join(work, "x", "VERSION")) != "v0.6.1\n" {
			t.Errorf("%s extract: %v", base, err)
		}
		if fi, _ := os.Stat(filepath.Join(work, "x", "upgrade.sh")); fi == nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: upgrade.sh must keep mode 755: %v", base, fi)
		}
		if _, err := inst.downloadPackage(context.Background(), "v9.9.9", t.TempDir()); !errors.Is(err, errNoRelease) {
			t.Errorf("%s missing release: %v", base, err)
		}
	}

	// Подмена тарбола в зеркале.
	tampered := filepath.Join(releases, "download", "v0.6.1", packageName("v0.6.1"))
	mustWrite(t, tampered, string(buildPackage(t, pkgFiles("v0.6.1", map[string]string{"Caddyfile": "evil\n"}))), 0o644)
	dir := installed(t, "v0.6.0", nil)
	e, _, _ := testEnv(t, dir, newFakeRunner(), map[string]string{"CADENCE_RELEASES_URL": "file://" + releases})
	inst, _ := openInstallation(e, "")
	if _, err := inst.downloadPackage(context.Background(), "v0.6.1", t.TempDir()); !errors.Is(err, errChecksum) {
		t.Errorf("tampered: %v", err)
	}
}

func TestExtractRejectsUnsafePaths(t *testing.T) {
	for _, name := range []string{"../evil", "/etc/evil"} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
		tw.Close()
		gz.Close()
		p := filepath.Join(t.TempDir(), "p.tar.gz")
		os.WriteFile(p, buf.Bytes(), 0o600)
		if err := extractTarGz(p, t.TempDir(), nil); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

// CLAB-158: пять веток подсказки «поставить файлы пакета версии».
func TestPackageFilesHint(t *testing.T) {
	dir := installed(t, "v0.6.1", nil)
	r := newFakeRunner()
	e, _, _ := testEnv(t, dir, r, map[string]string{"CADENCE_RELEASES_URL": "https://mirror.example.com/rel/"})
	inst, _ := openInstallation(e, "")
	ctx := context.Background()

	// 1. копия в backups/ — самая свежая.
	mustWrite(t, filepath.Join(dir, "backups", "package-v0.6.0-20261001-030000.tar.gz"), "x", 0o600)
	mustWrite(t, filepath.Join(dir, "backups", "package-v0.6.0-20261008-030000.tar.gz"), "x", 0o600)
	if got := inst.packageFilesHint(ctx, "v0.6.0"); got != "tar -xpzf backups/package-v0.6.0-20261008-030000.tar.gz" {
		t.Errorf("saved copy: %q", got)
	}
	// 2. версия с пакетом — релиз с зеркала.
	if got := inst.packageFilesHint(ctx, "v0.6.2"); got != "curl -fsSLO https://mirror.example.com/rel/download/v0.6.2/cadence-self-hosted-v0.6.2.tar.gz && tar -xpzf cadence-self-hosted-v0.6.2.tar.gz && rm cadence-self-hosted-v0.6.2.tar.gz" {
		t.Errorf("release package: %q", got)
	}
	// 5. до v0.6.0 без .git — архив тега с GitHub, не с зеркала; git не зовётся.
	if got := inst.packageFilesHint(ctx, "v0.5.3"); got != "curl -fsSL https://github.com/cadence-lab-tech/self-hosted/archive/refs/tags/v0.5.3.tar.gz | tar -xpzf - --strip-components=1" {
		t.Errorf("source archive: %q", got)
	}
	if r.called("git") {
		t.Error("git must not be called without .git")
	}
	// 3 и 4. git-клон: тег есть — checkout; нет — fetch и checkout.
	os.Mkdir(filepath.Join(dir, ".git"), 0o755)
	r.gitAvailable = true
	r.gitTags = map[string]bool{"v0.5.3": true}
	if got := inst.packageFilesHint(ctx, "v0.5.3"); got != "git -c advice.detachedHead=false checkout --force v0.5.3" {
		t.Errorf("tag in the clone: %q", got)
	}
	e.getenv = func(k string) string {
		return map[string]string{"CADENCE_GIT_REMOTE": "upstream"}[k]
	}
	if got := inst.packageFilesHint(ctx, "v0.5.2"); got != "git fetch --tags upstream && git -c advice.detachedHead=false checkout --force v0.5.2" {
		t.Errorf("no tag in the clone: %q", got)
	}
	if restoreCommand("v0.5.2", false) != "./restore.sh" || restoreCommand("v0.6.0", false) != "clab server restore" {
		t.Error("restoreCommand")
	}
}

// ── backup ──────────────────────────────────────────────────────────────────

// Критерий 4: дамп 600 с версией в имени, архив blobs, ротация; маленький
// дамп и остановленный стек — ошибка.
func TestServerBackup(t *testing.T) {
	dir := installed(t, "v0.6.1", nil)
	old := filepath.Join(dir, "backups", "cadence-v0.6.0-20260901-030000.sql.gz")
	oldBlobs := filepath.Join(dir, "backups", "cadence-v0.6.0-20260901-030000.blobs.tar.gz")
	recent := filepath.Join(dir, "backups", "cadence-v0.6.1-20261017-030000.sql.gz")
	other := filepath.Join(dir, "backups", "package-v0.6.0-20260901-030000.tar.gz")
	for _, p := range []string{old, oldBlobs, recent, other} {
		mustWrite(t, p, "x", 0o600)
	}
	now := time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)
	os.Chtimes(old, now.AddDate(0, 0, -20), now.AddDate(0, 0, -20))
	os.Chtimes(oldBlobs, now.AddDate(0, 0, -20), now.AddDate(0, 0, -20))
	os.Chtimes(other, now.AddDate(0, 0, -20), now.AddDate(0, 0, -20))
	os.Chtimes(recent, now.AddDate(0, 0, -1), now.AddDate(0, 0, -1))

	r := newFakeRunner()
	e, out, errw := testEnv(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	dump := filepath.Join(dir, "backups", "cadence-v0.6.1-20261018-030000.sql.gz")
	fi, err := os.Stat(dump)
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Size() < 100 {
		t.Fatalf("dump: %v %v", fi, err)
	}
	if err := gzipValid(dump); err != nil {
		t.Errorf("dump is not gzip: %v", err)
	}
	if !r.called("docker run --rm --volumes-from api123:ro -v " + filepath.Join(dir, "backups") + ":/backup alpine:3 sh -c tar -czf '/backup/cadence-v0.6.1-20261018-030000.blobs.tar.gz'") {
		t.Errorf("blobs volume not archived: %v", r.calls)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("a dump older than KEEP_DAYS must be removed")
	}
	if _, err := os.Stat(oldBlobs); err == nil {
		t.Error("a blobs archive older than KEEP_DAYS must be removed")
	}
	for _, p := range []string{recent, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must stay", p)
		}
	}
	if !strings.Contains(out.String(), "✓ 2026-10-18 03:00:00 "+dump) {
		t.Errorf("output: %s", out)
	}

	// S3 — без архива blobs.
	r = newFakeRunner()
	e, _, _ = testEnv(t, dir, r, map[string]string{"SDLC_BLOB_S3_BUCKET": "bucket"})
	e.now = func() time.Time { return now.Add(time.Hour) }
	if code := runServer(context.Background(), e, []string{"backup"}); code != 0 || r.called("docker run") {
		t.Errorf("S3: code %d, calls %v", code, r.calls)
	}

	// Маленький дамп.
	r = newFakeRunner()
	r.dumpBytes = 0
	e, _, errw = testEnv(t, dir, r, nil)
	e.now = func() time.Time { return now.Add(2 * time.Hour) }
	if code := runServer(context.Background(), e, []string{"backup"}); code == 0 || !strings.Contains(errw.String(), "suspiciously small") {
		t.Errorf("small dump: %d %s", code, errw)
	}

	// Стек остановлен.
	r = newFakeRunner()
	r.postgresRunning = false
	e, _, errw = testEnv(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code == 0 || !strings.Contains(errw.String(), "nothing to back up") {
		t.Errorf("stopped: %d %s", code, errw)
	}

	// BACKUP_DIR и KEEP_DAYS из .env.
	r = newFakeRunner()
	mustWrite(t, filepath.Join(dir, ".env"), readFile(t, filepath.Join(dir, ".env"))+"BACKUP_DIR=elsewhere\nKEEP_DAYS=0\n", 0o600)
	e, _, errw = testEnv(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code != 0 {
		t.Fatalf("BACKUP_DIR: %s", errw)
	}
	if _, err := os.Stat(filepath.Join(dir, "elsewhere", "cadence-v0.6.1-20261018-030000.sql.gz")); err != nil {
		t.Errorf("BACKUP_DIR: %v", err)
	}
}

func TestRotateBackupsMtimeDays(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ages := map[string]time.Duration{
		"cadence-v0.6.1-a.sql.gz": 14*24*time.Hour + time.Hour, // 14 суток: остаётся при KEEP_DAYS=14
		"cadence-v0.6.1-b.sql.gz": 15*24*time.Hour + time.Hour, // 15 суток: удаляется
	}
	for n, age := range ages {
		mustWrite(t, filepath.Join(dir, n), "x", 0o600)
		os.Chtimes(filepath.Join(dir, n), now.Add(-age), now.Add(-age))
	}
	if err := rotateBackups(dir, 14, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cadence-v0.6.1-a.sql.gz")); err != nil {
		t.Error("14 days old must stay (find -mtime +14)")
	}
	if _, err := os.Stat(filepath.Join(dir, "cadence-v0.6.1-b.sql.gz")); err == nil {
		t.Error("15 days old must go")
	}
}

// ── restore ─────────────────────────────────────────────────────────────────

func gzipped(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	io.WriteString(gz, s)
	gz.Close()
	return buf.String()
}

// Критерий 5: без терминала и --yes — отказ, база не тронута; другая
// версия — отказ с командой файлов пакета; --ignore-version; --yes — база
// заново, blobs, старт.
func TestServerRestore(t *testing.T) {
	dir := installed(t, "v0.6.1", nil)
	dump := filepath.Join(dir, "backups", "cadence-v0.6.1-20261008-030000.sql.gz")
	mustWrite(t, dump, gzipped(t, "CREATE TABLE x();\n"), 0o600)
	mustWrite(t, strings.TrimSuffix(dump, ".sql.gz")+".blobs.tar.gz", "x", 0o600)
	oldDump := filepath.Join(dir, "backups", "cadence-v0.6.0-20261001-030000.sql.gz")
	mustWrite(t, oldDump, gzipped(t, "old"), 0o600)
	mustWrite(t, filepath.Join(dir, "backups", "package-v0.6.0-20261001-030000.tar.gz"), "x", 0o600)
	ctx := context.Background()

	r := newFakeRunner()
	e, _, errw := testEnv(t, dir, r, nil)
	if code := runServer(ctx, e, []string{"restore", "backups/cadence-v0.6.1-20261008-030000.sql.gz"}); code == 0 ||
		!strings.Contains(errw.String(), "pass --yes") {
		t.Errorf("no terminal: %d %s", code, errw)
	}
	if r.called("docker compose stop") || r.called("docker compose exec -T postgres psql") {
		t.Error("the refusal must not touch the database")
	}

	errw.Reset()
	if code := runServer(ctx, e, []string{"restore", "--yes", "backups/cadence-v0.6.0-20261001-030000.sql.gz"}); code == 0 ||
		!strings.Contains(errw.String(), "tar -xpzf backups/package-v0.6.0-20261001-030000.tar.gz && clab server restore backups/cadence-v0.6.0-20261001-030000.sql.gz") {
		t.Errorf("another version: %d %s", code, errw)
	}

	// Подтверждение словом restore в терминале.
	r = newFakeRunner()
	e, _, errw = testEnv(t, dir, r, nil)
	e.terminal = true
	e.in = strings.NewReader("no\n")
	if code := runServer(ctx, e, []string{"restore", dump}); code == 0 || !strings.Contains(errw.String(), "cancelled") || r.called("docker compose stop") {
		t.Errorf("cancelled: %d %s", code, errw)
	}

	r = newFakeRunner()
	e, out, errw := testEnv(t, filepath.Join(dir, "docs"), r, nil)
	if code := runServer(ctx, e, []string{"restore", dump, "--yes"}); code != 0 {
		t.Fatalf("restore: %s", errw)
	}
	for _, call := range []string{
		"docker compose stop api worker",
		"docker compose up -d postgres",
		`docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -q -U sdlc -d postgres -c DROP DATABASE IF EXISTS "sdlc" WITH (FORCE) -c CREATE DATABASE "sdlc" OWNER "sdlc"`,
		"docker run --rm --volumes-from api123 -v " + filepath.Join(dir, "backups") + ":/backup:ro alpine:3",
		"docker compose up -d --remove-orphans",
	} {
		if !r.called(call) {
			t.Errorf("no call %q in %v", call, r.calls)
		}
	}
	if got := r.stdin["docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -q -U sdlc -d sdlc"]; got != "CREATE TABLE x();\n" {
		t.Errorf("the dump must reach psql unpacked: %q", got)
	}
	if !strings.Contains(out.String(), "restored cadence-v0.6.1-20261008-030000.sql.gz") {
		t.Errorf("output: %s", out)
	}

	// --ignore-version пропускает сверку.
	r = newFakeRunner()
	e, _, errw = testEnv(t, dir, r, nil)
	if code := runServer(ctx, e, []string{"restore", "--yes", "--ignore-version", oldDump}); code != 0 {
		t.Errorf("--ignore-version: %s", errw)
	}

	// Стек не поднялся.
	r = newFakeRunner()
	r.healthy = false
	e, _, errw = testEnv(t, dir, r, nil)
	if code := runServer(ctx, e, []string{"restore", "--yes", dump}); code == 0 || !strings.Contains(errw.String(), "did not become ready") {
		t.Errorf("not ready: %d %s", code, errw)
	}
}

// ── upgrade ─────────────────────────────────────────────────────────────────

type upgradeFixture struct {
	dir, releases string
	r             *fakeRunner
	e             *serverEnv
	out, errw     *bytes.Buffer
}

// upgradeSetup — установка v0.6.0 и релизы v0.6.0 и v0.6.1 (последний) в
// каталоге file://. В v0.6.1 нет obsolete.txt, который был в v0.6.0, и есть
// новый файл.
func upgradeSetup(t *testing.T, vars map[string]string) *upgradeFixture {
	t.Helper()
	oldExtra := map[string]string{"obsolete.txt": "shipped by v0.6.0 only\n"}
	newExtra := map[string]string{"bundles/starter.json": "{}\n", "Caddyfile": ":80\n# v0.6.1\n"}
	dir := installed(t, "v0.6.0", oldExtra)
	mustWrite(t, filepath.Join(dir, "notes.txt"), "notes of the administrator\n", 0o644)
	releases := t.TempDir()
	publish(t, releases, "v0.6.0", pkgFiles("v0.6.0", oldExtra), false)
	publish(t, releases, "v0.6.1", pkgFiles("v0.6.1", newExtra), true)
	all := map[string]string{"CADENCE_RELEASES_URL": "file://" + releases}
	for k, v := range vars {
		all[k] = v
	}
	r := newFakeRunner()
	e, out, errw := testEnv(t, dir, r, all)
	return &upgradeFixture{dir: dir, releases: releases, r: r, e: e, out: out, errw: errw}
}

func (f *upgradeFixture) upgrade(args ...string) int {
	return runServer(context.Background(), f.e, append([]string{"upgrade"}, args...))
}

func (f *upgradeFixture) version(t *testing.T) string {
	return strings.TrimSpace(readFile(t, filepath.Join(f.dir, "VERSION")))
}

// Критерий 6, успех: бэкап, копия пакета, новые файлы, VERSION, файлы
// администратора не тронуты, ушедший из пакета файл удалён, git не звался,
// напоминание обновить clab (CLAB-148).
func TestServerUpgrade(t *testing.T) {
	f := upgradeSetup(t, nil)
	kept := map[string]string{}
	for _, p := range []string{".env", "compose.override.yml", "notes.txt", "secrets/key.pem"} {
		kept[p] = readFile(t, filepath.Join(f.dir, p))
	}
	if code := f.upgrade(); code != 0 {
		t.Fatalf("upgrade: %s", f.errw)
	}
	if f.version(t) != "v0.6.1" {
		t.Errorf("VERSION %s", f.version(t))
	}
	if readFile(t, filepath.Join(f.dir, "Caddyfile")) != ":80\n# v0.6.1\n" || readFile(t, filepath.Join(f.dir, "bundles/starter.json")) != "{}\n" {
		t.Error("the files of v0.6.1 are not in place")
	}
	if fi, _ := os.Stat(filepath.Join(f.dir, "upgrade.sh")); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("upgrade.sh mode: %v", fi)
	}
	for p, c := range kept {
		if readFile(t, filepath.Join(f.dir, p)) != c {
			t.Errorf("%s changed", p)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "obsolete.txt")); err == nil {
		t.Error("obsolete.txt, no longer in the package, must be removed")
	}
	if !isFile(filepath.Join(f.dir, "backups", "cadence-v0.6.0-20261018-030000.sql.gz")) {
		t.Error("no backup of v0.6.0")
	}
	saved := filepath.Join(f.dir, "backups", "package-v0.6.0-20261018-030000.tar.gz")
	x := t.TempDir()
	if err := extractTarGz(saved, x, nil); err != nil || readFile(t, filepath.Join(x, "obsolete.txt")) != "shipped by v0.6.0 only\n" ||
		readFile(t, filepath.Join(x, "VERSION")) != "v0.6.0\n" {
		t.Errorf("package copy of v0.6.0: %v", err)
	}
	if !f.e.isManifestOf(f.dir) {
		t.Error("package.sha256 must be the one of v0.6.1")
	}
	if f.r.called("git") {
		t.Errorf("git was called: %v", f.r.calls)
	}
	out := f.out.String()
	for _, want := range []string{"upgraded v0.6.0 -> v0.6.1", "Update clab to v0.6.1 on the machines of people and agents",
		"brew upgrade clab", "CLAB_VERSION=v0.6.1 sh", "clab version"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Update clab") < strings.Index(out, "upgraded v0.6.0 -> v0.6.1") {
		t.Error("the reminder must follow 'upgraded'")
	}
	if !f.r.called("docker compose exec -T caddy caddy reload") {
		t.Error("caddy is not reloaded")
	}
	matches, _ := filepath.Glob(filepath.Join(f.dir, ".upgrade.*"))
	if len(matches) > 0 {
		t.Errorf("work directory left: %v", matches)
	}

	// Последний релиз — уже стоит.
	f.out.Reset()
	if code := f.upgrade(); code != 0 || !strings.Contains(f.out.String(), "already on v0.6.1; nothing to do") ||
		strings.Contains(f.out.String(), "Update clab") {
		t.Errorf("nothing to do: %d %s", code, f.out)
	}

	// Вниз — отказ с подсказкой из копии пакета, без git.
	f.errw.Reset()
	if code := f.upgrade("v0.6.0"); code == 0 ||
		!strings.Contains(f.errw.String(), "tar -xpzf backups/package-v0.6.0-20261018-030000.tar.gz && clab server restore backups/cadence-v0.6.0-<date>.sql.gz") {
		t.Errorf("downgrade: %d %s", code, f.errw)
	}
	if f.version(t) != "v0.6.1" || f.r.called("git") {
		t.Error("the downgrade refusal must change nothing and not call git")
	}
}

// isManifestOf — эталон в каталоге описывает его VERSION (для теста).
func (e *serverEnv) isManifestOf(dir string) bool {
	return (&installation{e: e, dir: dir}).manifestOfThisVersion()
}

// Правленый файл пакета — отказ до бэкапа со списком; --force — замена и
// копия правленого.
func TestServerUpgradeEditedFile(t *testing.T) {
	f := upgradeSetup(t, nil)
	compose := filepath.Join(f.dir, "docker-compose.yml")
	mustWrite(t, compose, readFile(t, compose)+"# a local edit\n", 0o644)
	before := tree(t, f.dir)
	if code := f.upgrade("v0.6.1"); code == 0 {
		t.Fatal("must refuse over an edited file")
	}
	if !strings.Contains(f.errw.String(), "    docker-compose.yml") || !strings.Contains(f.errw.String(), "--force v0.6.1") ||
		!strings.Contains(f.errw.String(), "Nothing has changed") {
		t.Errorf("refusal: %s", f.errw)
	}
	if f.r.called("docker compose exec -T postgres pg_dump") || tree(t, f.dir) != before {
		t.Error("the refusal must come before the backup and change nothing")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.dir, "backups")); len(entries) != 0 {
		t.Errorf("backups after the refusal: %v", entries)
	}

	if code := f.upgrade("--force", "v0.6.1"); code != 0 {
		t.Fatalf("--force: %s", f.errw)
	}
	if strings.Contains(readFile(t, compose), "# a local edit") {
		t.Error("--force must replace the edited file")
	}
	x := t.TempDir()
	extractTarGz(filepath.Join(f.dir, "backups", "package-v0.6.0-20261018-030000.tar.gz"), x, nil)
	if !strings.Contains(readFile(t, filepath.Join(x, "docker-compose.yml")), "# a local edit") {
		t.Error("the edited copy must be kept in the package copy")
	}
}

// Неудачный pull — файлы на месте, VERSION прежний.
func TestServerUpgradeFailedPull(t *testing.T) {
	f := upgradeSetup(t, nil)
	before := tree(t, f.dir)
	f.r.pullFails = true
	if code := f.upgrade("v0.6.1"); code == 0 {
		t.Fatal("must fail")
	}
	if !strings.Contains(f.errw.String(), "the package files are back on v0.6.0 and the running stack is untouched") {
		t.Errorf("message: %s", f.errw)
	}
	if after := tree(t, f.dir); after != before {
		t.Errorf("files changed:\n%s\n---\n%s", before, after)
	}
	if f.r.called("docker compose up") || strings.Contains(f.out.String(), "Update clab") {
		t.Error("a failed pull must not start anything or remind")
	}
}

// Неготовность — напечатанный откат: два шага без git, которые относятся к
// реальным копии пакета и бэкапу.
func TestServerUpgradeNotReady(t *testing.T) {
	f := upgradeSetup(t, map[string]string{"CADENCE_READY_TIMEOUT": "0"})
	if code := f.upgrade("v0.6.1"); code == 0 {
		t.Fatal("must fail")
	}
	msg := f.errw.String()
	want := "  tar -xpzf backups/package-v0.6.0-20261018-030000.tar.gz   # package files of v0.6.0\n" +
		"  clab server restore --yes backups/cadence-v0.6.0-20261018-030000.sql.gz   # data as of the backup, starts v0.6.0"
	if !strings.Contains(msg, want) || strings.Contains(msg, "git") || strings.Contains(msg, "  cd ") {
		t.Errorf("rollback:\n%s", msg)
	}
	if !f.r.called("docker compose logs --tail 40 api worker") {
		t.Error("logs are not shown")
	}
	// Шаги отката относятся к файлам, которые есть.
	for _, p := range []string{"backups/package-v0.6.0-20261018-030000.tar.gz", "backups/cadence-v0.6.0-20261018-030000.sql.gz"} {
		if !isFile(filepath.Join(f.dir, p)) {
			t.Errorf("%s is printed but missing", p)
		}
	}
	// Распаковка копии, как напечатано, возвращает файлы v0.6.0.
	if err := extractTarGz(filepath.Join(f.dir, "backups/package-v0.6.0-20261018-030000.tar.gz"), f.dir, nil); err != nil || f.version(t) != "v0.6.0" ||
		readFile(t, filepath.Join(f.dir, "obsolete.txt")) != "shipped by v0.6.0 only\n" {
		t.Errorf("rollback of files: %v %s", err, f.version(t))
	}

	// Не из каталога установки — откат начинается с cd.
	g := upgradeSetup(t, map[string]string{"CADENCE_READY_TIMEOUT": "0"})
	g.e.cwd = t.TempDir()
	if code := g.upgrade("--dir", g.dir, "v0.6.1"); code == 0 || !strings.Contains(g.errw.String(), "  cd "+g.dir+"\n  tar -xpzf backups/") {
		t.Errorf("cd hint: %s", g.errw)
	}
}

// Критерий 7: подменённая сумма в зеркале — отказ, каталог не изменён.
func TestServerUpgradeChecksumMismatch(t *testing.T) {
	f := upgradeSetup(t, nil)
	tarball := filepath.Join(f.releases, "download", "v0.6.1", packageName("v0.6.1"))
	mustWrite(t, tarball, string(buildPackage(t, pkgFiles("v0.6.1", map[string]string{"Caddyfile": "evil\n"}))), 0o644)
	before := tree(t, f.dir)
	if code := f.upgrade("v0.6.1"); code == 0 || !strings.Contains(f.errw.String(), "checksum") || !strings.Contains(f.errw.String(), "does not match") {
		t.Errorf("checksum: %s", f.errw)
	}
	if tree(t, f.dir) != before || f.r.called("docker compose exec -T postgres pg_dump") {
		t.Error("nothing may change")
	}
	f.errw.Reset()
	if code := f.upgrade("v0.9.9"); code == 0 || !strings.Contains(f.errw.String(), "there is no release v0.9.9") ||
		!strings.Contains(f.errw.String(), "(the latest is v0.6.1)") {
		t.Errorf("missing release: %s", f.errw)
	}
}

// q3: стек новее clab — отказ с командой обновить CLI; цель новее clab —
// апгрейд идёт с предупреждением.
func TestServerUpgradeVersionRule(t *testing.T) {
	f := upgradeSetup(t, nil)
	f.e.cli = "v0.5.9"
	if code := f.upgrade("v0.6.1"); code == 0 || !strings.Contains(f.errw.String(), "clab v0.5.9 is older than the stack v0.6.0") {
		t.Errorf("older clab: %s", f.errw)
	}
	if f.r.called("docker") {
		t.Error("an older clab must refuse before Docker")
	}

	f = upgradeSetup(t, nil)
	f.e.cli = "v0.6.0"
	if code := f.upgrade("v0.6.1"); code != 0 {
		t.Fatalf("target newer than clab: %s", f.errw)
	}
	if !strings.Contains(f.errw.String(), "clab v0.6.0 is older than the target v0.6.1") ||
		!strings.Contains(f.out.String(), "This clab is v0.6.0: update it too") {
		t.Errorf("warning: %s\n%s", f.errw, f.out)
	}
}

// Стек остановлен — отказ до бэкапа, с командой старта.
func TestServerUpgradeStackStopped(t *testing.T) {
	f := upgradeSetup(t, nil)
	f.r.postgresRunning = false
	before := tree(t, f.dir)
	if code := f.upgrade("v0.6.1"); code == 0 || !strings.Contains(f.errw.String(), "docker compose up -d") {
		t.Errorf("stopped: %s", f.errw)
	}
	if tree(t, f.dir) != before {
		t.Error("nothing may change")
	}
}

// Установка без эталона своей версии (бывший клон после отката) берёт
// эталон из пакета релиза.
func TestServerUpgradeReferenceFromRelease(t *testing.T) {
	f := upgradeSetup(t, nil)
	os.Remove(filepath.Join(f.dir, packageManifest))
	compose := filepath.Join(f.dir, "docker-compose.yml")
	mustWrite(t, compose, readFile(t, compose)+"# a local edit\n", 0o644)
	if code := f.upgrade("v0.6.1"); code == 0 || !strings.Contains(f.errw.String(), "    docker-compose.yml") {
		t.Errorf("reference from the release: %s", f.errw)
	}
}
