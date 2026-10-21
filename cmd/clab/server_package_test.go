package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Тесты clab server под обёрткой пакета и с бинарём пакета (CLAB-242).
// Обёртка — CADENCE_WRAPPER=1 в окружении (R1), бинарь пакета — e.self в
// <каталог>/bin/ (R2). Номера в комментариях — критерии приёмки CLAB-242.

var wrapper = map[string]string{wrapperEnv: "1"}

// withVars — переменные теста и обёртка поверх.
func withVars(a, b map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range a {
		m[k] = v
	}
	for k, v := range b {
		m[k] = v
	}
	return m
}

// packageSelf кладёт бинарь в <dir>/bin/ и делает его этим clab.
func packageSelf(t *testing.T, e *serverEnv, dir string) string {
	t.Helper()
	self := filepath.Join(dir, "bin", "clab-linux-amd64")
	mustWrite(t, self, "#!/bin/sh\n", 0o755)
	e.self = self
	return self
}

// adminCommand — команда clab server для администратора в выводе: clab
// или этот бинарь с подкомандой, которая меняет установку. status (у него
// нет скрипта), напоминание обновить clab людей и агентов и описательный
// текст («clab server refuses …») командами не считаются (AC-6).
func adminCommand(self string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[\s'])(clab|` + regexp.QuoteMeta(shellQuote(self)) + `) server (install|upgrade|backup|restore)\b`)
}

func noAdminCommands(t *testing.T, self, what string, outputs ...string) {
	t.Helper()
	re := adminCommand(self)
	for _, o := range outputs {
		for _, line := range strings.Split(o, "\n") {
			if re.MatchString(line) {
				t.Errorf("%s: a clab server command for the administrator under the wrapper: %q", what, line)
			}
		}
	}
}

// ── признак и бинарь пакета ─────────────────────────────────────────────────

func TestWrapperAndPackageBinary(t *testing.T) {
	e, _, _ := testEnv(t, "/opt/cadence", newFakeRunner(), nil)
	if e.wrapped() {
		t.Error("no wrapper without CADENCE_WRAPPER")
	}
	for v, want := range map[string]bool{"1": true, "0": false, "yes": false, "": false} {
		e.getenv = func(k string) string { return map[string]string{wrapperEnv: v}[k] }
		if e.wrapped() != want {
			t.Errorf("CADENCE_WRAPPER=%q: wrapped %v", v, !want)
		}
	}
	if got := e.script("/opt/cadence", "upgrade.sh"); got != "./upgrade.sh" {
		t.Errorf("from the directory: %q", got)
	}
	if got := e.script("/opt/my cadence", "upgrade.sh"); got != "'/opt/my cadence/upgrade.sh'" {
		t.Errorf("from elsewhere: %q", got)
	}
	if got := e.script("", "restore.sh"); got != "./restore.sh" {
		t.Errorf("no directory: %q", got)
	}

	// R2: бинарь в <каталог>/bin/, в том числе через ссылки на него и на
	// каталог; рядом или глубже — не бинарь пакета.
	dir := installed(t, "v0.7.0", nil)
	self := packageSelf(t, e, dir)
	if !e.packageBinary(dir) {
		t.Error("<dir>/bin/clab-linux-amd64 is the package binary")
	}
	link := filepath.Join(t.TempDir(), "clab")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	e.self = link
	dirLink := filepath.Join(t.TempDir(), "cadence-link")
	if err := os.Symlink(dir, dirLink); err != nil {
		t.Fatal(err)
	}
	if !e.packageBinary(dir) || !e.packageBinary(dirLink) {
		t.Error("links to the binary and to the directory must resolve")
	}
	for _, p := range []string{"/usr/local/bin/clab", filepath.Join(dir, "clab"), filepath.Join(dir, "bin", "x", "clab")} {
		e.self = p
		if e.packageBinary(dir) {
			t.Errorf("%s is not the package binary", p)
		}
	}
	e.self = ""
	if e.packageBinary(dir) {
		t.Error("no self, no package binary")
	}
	if restoreCommand("v0.6.0", true) != "./restore.sh" || restoreCommand("v0.12.0", true) != "./restore.sh" || restoreCommand("v0.5.2", true) != "./restore.sh" {
		t.Error("restoreCommand under the wrapper is always ./restore.sh")
	}
}

// ── install ─────────────────────────────────────────────────────────────────

// AC-1, AC-2, AC-6: итог install под обёрткой — скрипты пакета и cron через
// backup.sh; признака в .env нет.
func TestServerInstallWrapped(t *testing.T) {
	f := installSetup(t, withVars(nil, wrapper))
	d := f.dir("my cadence")
	if code := f.install("--mode", "proxy", "--http-addr", "127.0.0.1:18190", "--yes", "--dir", d); code != 0 {
		t.Fatalf("code %d\n%s\n%s", code, f.out, f.errw)
	}
	if _, ok := f.env(d)[wrapperEnv]; ok || strings.Contains(readFile(t, filepath.Join(d, ".env")), wrapperEnv) {
		t.Error("the wrapper flag must not reach .env")
	}
	qd, log := shellQuote(d), shellQuote(filepath.Join(d, "backups", "backup.log"))
	out := f.out.String()
	for _, s := range []string{
		"Manage the installation with the scripts in " + qd + ": ./upgrade.sh, ./backup.sh and ./restore.sh;",
		"its state: /usr/local/bin/clab server status --dir " + qd + ".",
		"0 3 * * * " + shellQuote(filepath.Join(d, "backup.sh")) + " >> " + log + " 2>&1",
		"0 5 * * 0 " + shellQuote(filepath.Join(d, "backup.sh")) + " verify >> " + log + " 2>&1",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	noAdminCommands(t, f.e.self, "install", out, f.errw.String())

	// Установка на месте бинарём пакета (UC-1): то же, status — его путём.
	g := installSetup(t, withVars(nil, wrapper))
	u := g.dir("unpacked")
	if err := extractTarGz(filepath.Join(g.releases, "download", "v0.7.0", packageName("v0.7.0")), u, nil); err != nil {
		t.Fatal(err)
	}
	self := packageSelf(t, g.e, u)
	g.e.cwd = u
	if code := g.install("--dir", u, "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("in place: %d\n%s", code, g.errw)
	}
	if !strings.Contains(g.out.String(), "its state: "+self+" server status --dir "+u+".") ||
		!strings.Contains(g.out.String(), "0 3 * * * "+filepath.Join(u, "backup.sh")+" >> ") {
		t.Errorf("in place output:\n%s", g.out)
	}
	noAdminCommands(t, self, "install in place", g.out.String(), g.errw.String())
}

// AC-3, AC-6: отказы install под обёрткой называют install.sh и
// upgrade.sh пакета; коды прежние.
func TestServerInstallWrappedRefusals(t *testing.T) {
	// .env уже есть.
	f := installSetup(t, withVars(nil, wrapper))
	d := installed(t, "v0.7.0", nil)
	if code := f.install("--dir", d, "--mode", "proxy", "--yes"); code != 1 {
		t.Fatalf(".env exists: code %d", code)
	}
	for _, s := range []string{"  - its state:  /usr/local/bin/clab server status --dir " + d + "\n", "  - upgrade:    " + filepath.Join(d, "upgrade.sh") + "\n"} {
		if !strings.Contains(f.errw.String()+"\n", s) {
			t.Errorf(".env exists lacks %q:\n%s", s, f.errw)
		}
	}
	noAdminCommands(t, f.e.self, ".env exists", f.errw.String())

	// Имя занято.
	g := installSetup(t, withVars(nil, wrapper))
	g.r.takenBy = "/opt/cadence"
	d = g.dir("cadence")
	if code := g.install("--dir", d, "--mode", "proxy", "--yes"); code != 1 ||
		!strings.Contains(g.errw.String(), "    "+filepath.Join(d, "install.sh")+" --name cadence-2 ...\n") {
		t.Errorf("name taken: %s", g.errw)
	}
	noAdminCommands(t, g.e.self, "name taken", g.errw.String())

	// tls: домен не резолвится — предложение proxy.
	h := installSetup(t, withVars(nil, wrapper))
	h.e.lookupHost = func(string) ([]string, error) { return nil, os.ErrNotExist }
	d = h.dir("a")
	if code := h.install("--dir", d, "--mode", "tls", "--domain", "c.example.com", "--yes"); code != 0 ||
		!strings.Contains(h.errw.String(), "or install in proxy mode behind your reverse proxy: "+filepath.Join(d, "install.sh")+" --mode proxy") {
		t.Errorf("tls warning: %d %s", code, h.errw)
	}
	noAdminCommands(t, h.e.self, "tls warning", h.out.String(), h.errw.String())

	// Внешняя база не готова.
	k := installSetup(t, withVars(nil, wrapper))
	k.r.dbcheckFails = true
	d = k.dir("cadence")
	if code := k.install("--mode", "proxy", "--yes", "--dir", d, "--external-db", "postgres://u:p@h/db?sslmode=require"); code != 1 ||
		!strings.Contains(k.errw.String(), "fix the database or the URL and run "+filepath.Join(d, "install.sh")+" again with the same flags") {
		t.Errorf("external db: %d %s", code, k.errw)
	}
	k.noEnv(d)
	noAdminCommands(t, k.e.self, "external db", k.errw.String())
}

// ── upgrade ─────────────────────────────────────────────────────────────────

// rollbackLine — регулярка check-upgrade.sh: строки отката, которые
// проверка выполняет.
var rollbackLine = regexp.MustCompile(`(?m)^  (tar -xpzf |\./restore\.sh --yes )`)

// AC-4, AC-6: откат, --force и даунгрейд под обёрткой — в форме скриптов.
func TestServerUpgradeWrappedHints(t *testing.T) {
	f := upgradeSetup(t, withVars(map[string]string{"CADENCE_READY_TIMEOUT": "0"}, wrapper))
	if code := f.upgrade("v0.6.1"); code == 0 {
		t.Fatal("must fail")
	}
	msg := f.errw.String()
	want := "  tar -xpzf backups/package-v0.6.0-20261018-030000.tar.gz   # package files of v0.6.0\n" +
		"  ./restore.sh --yes backups/cadence-v0.6.0-20261018-030000.sql.gz   # data as of the backup, starts v0.6.0"
	if !strings.Contains(msg, want) || strings.Contains(msg, "  cd ") || len(rollbackLine.FindAllString(msg, -1)) != 2 {
		t.Errorf("rollback:\n%s", msg)
	}
	noAdminCommands(t, f.e.self, "rollback", f.out.String(), msg)

	g := upgradeSetup(t, withVars(map[string]string{"CADENCE_READY_TIMEOUT": "0"}, wrapper))
	g.e.cwd = t.TempDir()
	if code := g.upgrade("--dir", g.dir, "v0.6.1"); code == 0 ||
		!strings.Contains(g.errw.String(), "  cd "+g.dir+"\n  tar -xpzf backups/") || !strings.Contains(g.errw.String(), "\n  ./restore.sh --yes backups/") {
		t.Errorf("rollback with cd: %s", g.errw)
	}

	// Правленый файл: --force — upgrade.sh пакета.
	h := upgradeSetup(t, wrapper)
	compose := filepath.Join(h.dir, "docker-compose.yml")
	mustWrite(t, compose, readFile(t, compose)+"# a local edit\n", 0o644)
	if code := h.upgrade("v0.6.1"); code != 1 || !strings.Contains(h.errw.String(), "or run ./upgrade.sh --force v0.6.1 to replace them") {
		t.Errorf("--force from the directory: %s", h.errw)
	}
	noAdminCommands(t, h.e.self, "--force", h.errw.String())
	h.errw.Reset()
	h.e.cwd = t.TempDir()
	if code := h.upgrade("--dir", h.dir, "v0.6.1"); code != 1 ||
		!strings.Contains(h.errw.String(), "or run "+filepath.Join(h.dir, "upgrade.sh")+" --force v0.6.1 to replace them") {
		t.Errorf("--force from elsewhere: %s", h.errw)
	}

	// Даунгрейд: возврат файлов и restore.sh.
	k := upgradeSetup(t, wrapper)
	if code := k.upgrade("v0.6.1"); code != 0 {
		t.Fatalf("upgrade: %s", k.errw)
	}
	k.errw.Reset()
	if code := k.upgrade("v0.6.0"); code != 1 ||
		!strings.Contains(k.errw.String(), "tar -xpzf backups/package-v0.6.0-20261018-030000.tar.gz && ./restore.sh backups/cadence-v0.6.0-<date>.sql.gz") {
		t.Errorf("downgrade: %s", k.errw)
	}
	noAdminCommands(t, k.e.self, "downgrade", k.out.String(), k.errw.String())
}

// AC-7: бинарь пакета, апгрейд на версию новее него — без ложных
// предупреждений о версии, с напоминанием для людей и агентов; с
// обёрткой и без.
func TestServerUpgradePackageBinary(t *testing.T) {
	for _, vars := range []map[string]string{nil, wrapper} {
		f := upgradeSetup(t, vars)
		f.e.cli = "v0.6.0"
		packageSelf(t, f.e, f.dir)
		if code := f.upgrade("v0.6.1"); code != 0 {
			t.Fatalf("wrapper %v: %s", vars != nil, f.errw)
		}
		if strings.Contains(f.errw.String(), "is older than the target") || strings.Contains(f.out.String(), "This clab is v0.6.0: update it too") ||
			!strings.Contains(f.out.String(), "  Update clab to v0.6.1 on the machines of people and agents:") {
			t.Errorf("wrapper %v:\n%s\n%s", vars != nil, f.out, f.errw)
		}
	}
}

// AC-8: обёртка зовёт внешний clab (CADENCE_CLAB) — сообщения о версии как
// раньше, подсказки — скрипты.
func TestServerUpgradeWrappedExternalClab(t *testing.T) {
	f := upgradeSetup(t, wrapper)
	f.e.cli = "v0.6.0"
	if code := f.upgrade("v0.6.1"); code != 0 {
		t.Fatalf("upgrade: %s", f.errw)
	}
	if !strings.Contains(f.errw.String(), "clab v0.6.0 is older than the target v0.6.1") ||
		!strings.Contains(f.out.String(), "This clab is v0.6.0: update it too") {
		t.Errorf("version messages:\n%s\n%s", f.out, f.errw)
	}
	g := upgradeSetup(t, withVars(map[string]string{"CADENCE_READY_TIMEOUT": "0"}, wrapper))
	g.e.cli = "v0.6.0"
	if code := g.upgrade("v0.6.1"); code == 0 || !strings.Contains(g.errw.String(), "  ./restore.sh --yes backups/cadence-v0.6.0-") ||
		!strings.Contains(g.errw.String(), "clab v0.6.0 is older than the target v0.6.1") {
		t.Errorf("rollback with an external clab: %s", g.errw)
	}
}

// ── backup и restore ─────────────────────────────────────────────────────────

// AC-5, AC-6: fetch, restore без файла и бэкап другой версии под обёрткой.
func TestServerBackupRestoreWrapped(t *testing.T) {
	ctx := context.Background()
	dir := installed(t, "v0.10.0", nil)
	withS3(t, dir)
	r := newFakeRunner()
	r.s3GetName = "cadence-v0.10.0-20261019-030000.sql.gz"
	e, out, errw := testEnv10(t, dir, r, wrapper)
	if code := runServer(ctx, e, []string{"backup", "fetch"}); code != 0 ||
		!strings.Contains(out.String(), "; restore it with: ./restore.sh --yes backups/cadence-v0.10.0-20261019-030000.sql.gz") {
		t.Errorf("fetch from the directory: %d %s %s", code, out, errw)
	}
	noAdminCommands(t, e.self, "fetch", out.String(), errw.String())
	r = newFakeRunner()
	r.s3GetName = "cadence-v0.10.0-20261019-030000.sql.gz"
	e, out, _ = testEnv10(t, t.TempDir(), r, wrapper)
	if code := runServer(ctx, e, []string{"backup", "fetch", "--dir", dir}); code != 0 ||
		!strings.Contains(out.String(), "; restore it with: "+filepath.Join(dir, "restore.sh")+" --yes backups/cadence-v0.10.0-20261019-030000.sql.gz") {
		t.Errorf("fetch from elsewhere: %d %s", code, out)
	}

	// restore без файла — код 2, как раньше; без обёртки — прежний текст.
	e, _, errw = testEnv10(t, t.TempDir(), newFakeRunner(), wrapper)
	if code := runServer(ctx, e, []string{"restore", "--dir", dir}); code != 2 ||
		!strings.Contains(errw.String(), "which backup? "+filepath.Join(dir, "restore.sh")+" backups/cadence-<version>-<date>.sql.gz") {
		t.Errorf("which backup, elsewhere: %d %s", code, errw)
	}
	noAdminCommands(t, e.self, "which backup", errw.String())
	e, _, errw = testEnv10(t, dir, newFakeRunner(), wrapper)
	if code := runServer(ctx, e, []string{"restore"}); code != 2 ||
		!strings.Contains(errw.String(), "which backup? ./restore.sh backups/cadence-<version>-<date>.sql.gz") {
		t.Errorf("which backup, in the directory: %d %s", code, errw)
	}
	e, _, errw = testEnv10(t, t.TempDir(), newFakeRunner(), wrapper)
	if code := runServer(ctx, e, []string{"restore", "--dir", "/nowhere"}); code != 2 ||
		!strings.Contains(errw.String(), "which backup? ./restore.sh backups/") {
		t.Errorf("which backup, no installation: %d %s", code, errw)
	}
	e, _, errw = testEnv10(t, dir, newFakeRunner(), nil)
	if code := runServer(ctx, e, []string{"restore"}); code != 2 ||
		!strings.Contains(errw.String(), "which backup? clab server restore backups/cadence-<version>-<date>.sql.gz") {
		t.Errorf("which backup without the wrapper: %d %s", code, errw)
	}

	// Бэкап другой версии: возврат файлов пакета, затем restore.sh.
	old := filepath.Join(dir, "backups", "cadence-v0.9.0-20261001-030000.sql.gz")
	gzipFile(old, "-- old\n")
	mustWrite(t, filepath.Join(dir, "backups", "package-v0.9.0-20261001-030000.tar.gz"), "x", 0o600)
	e, _, errw = testEnv10(t, dir, newFakeRunner(), wrapper)
	if code := runServer(ctx, e, []string{"restore", "--yes", "backups/cadence-v0.9.0-20261001-030000.sql.gz"}); code != 1 ||
		!strings.Contains(errw.String(), "    tar -xpzf backups/package-v0.9.0-20261001-030000.tar.gz && ./restore.sh backups/cadence-v0.9.0-20261001-030000.sql.gz") {
		t.Errorf("another version: %d %s", code, errw)
	}
	noAdminCommands(t, e.self, "another version", errw.String())
	// backup verify другой версии — подсказка возврата файлов, без clab.
	e, _, errw = testEnv10(t, dir, newFakeRunner(), wrapper)
	if code := runServer(ctx, e, []string{"backup", "verify", "backups/cadence-v0.9.0-20261001-030000.sql.gz"}); code != 1 ||
		!strings.Contains(errw.String(), "    tar -xpzf backups/package-v0.9.0-20261001-030000.tar.gz") {
		t.Errorf("verify another version: %d %s", code, errw)
	}
	noAdminCommands(t, e.self, "verify another version", errw.String())
}

// ── правило версий для бинаря пакета (R5) ───────────────────────────────────

// AC-11: бинарь пакета старше VERSION — файлы пакета разошлись: отказ
// называет бинарь и печатает возврат файлов пакета, без команд тапа; status
// показывает тот же текст. Внешний clab — прежний отказ.
func TestPackageBinaryOlderThanVersion(t *testing.T) {
	ctx := context.Background()
	dir := installed(t, "v0.10.0", nil)
	for _, vars := range []map[string]string{nil, wrapper} {
		for _, cmd := range [][]string{{"backup"}, {"restore", "--yes", "x.sql.gz"}, {"upgrade", "v0.10.1"}} {
			r := newFakeRunner()
			e, _, errw := testEnv(t, dir, r, vars)
			e.cli, e.brew = "v0.9.0", true
			self := packageSelf(t, e, dir)
			if cmd[0] == "restore" {
				gzipFile(filepath.Join(dir, "x.sql.gz"), "-- x\n")
			}
			msg := errw.String
			if code := runServer(ctx, e, cmd); code != 1 {
				t.Fatalf("%v: code %d %s", cmd, code, msg())
			}
			for _, s := range []string{"clab v0.9.0 at " + self + " is the clab of this package, but VERSION is v0.10.0",
				"the package files do not match VERSION", "Put the package files of v0.10.0 in place",
				"    curl -fsSLO https://github.com/cadence-lab-tech/self-hosted/releases/download/v0.10.0/cadence-self-hosted-v0.10.0.tar.gz"} {
				if !strings.Contains(msg(), s) {
					t.Errorf("%v: lacks %q:\n%s", cmd, s, msg())
				}
			}
			if strings.Contains(msg(), "brew upgrade clab") || strings.Contains(msg(), "install.sh") || strings.Contains(msg(), "CLAB_VERSION") {
				t.Errorf("%v: no commands of the tap for the package binary:\n%s", cmd, msg())
			}
			if r.called("docker") {
				t.Errorf("%v: the refusal comes before Docker", cmd)
			}
		}
	}
	e, out, _ := testEnv(t, dir, newFakeRunner(), nil)
	e.cli = "v0.9.0"
	packageSelf(t, e, dir)
	if code := runServer(ctx, e, []string{"status"}); code != 0 || !strings.Contains(out.String(), "the package files do not match VERSION") {
		t.Errorf("status: %d %s", code, out)
	}

	// Внешний clab той же версии — отказ с командами тапа, как раньше.
	e, _, errw := testEnv(t, dir, newFakeRunner(), wrapper)
	e.cli = "v0.9.0"
	if code := runServer(ctx, e, []string{"backup"}); code != 1 ||
		!strings.Contains(errw.String(), "clab v0.9.0 is older than the stack v0.10.0") || !strings.Contains(errw.String(), "CLAB_VERSION=v0.10.0") {
		t.Errorf("external clab: %d %s", code, errw)
	}
	// Бинарь пакета той же версии, что VERSION, или новее — работает.
	r := newFakeRunner()
	e, _, errw = testEnv(t, dir, r, nil)
	e.cli = "v0.10.0"
	packageSelf(t, e, dir)
	if code := runServer(ctx, e, []string{"backup"}); code != 0 {
		t.Errorf("same version: %d %s", code, errw)
	}

	// install на месте: бинарь пакета старше пакета — отказ R5, .env нет.
	f := installSetup(t, wrapper)
	u := f.dir("unpacked")
	if err := extractTarGz(filepath.Join(f.releases, "download", "v0.7.0", packageName("v0.7.0")), u, nil); err != nil {
		t.Fatal(err)
	}
	f.e.cli = "v0.6.1"
	self := packageSelf(t, f.e, u)
	if code := f.install("--dir", u, "--mode", "proxy", "--yes"); code != 1 ||
		!strings.Contains(f.errw.String(), "clab v0.6.1 at "+self+" is the clab of this package, but VERSION is v0.7.0") ||
		strings.Contains(f.errw.String(), "CLAB_VERSION") {
		t.Errorf("install in place: %d %s", code, f.errw)
	}
	f.noEnv(u)
	g := installSetup(t, wrapper)
	u = g.dir("unpacked")
	if err := extractTarGz(filepath.Join(g.releases, "download", "v0.7.0", packageName("v0.7.0")), u, nil); err != nil {
		t.Fatal(err)
	}
	g.e.cli = "v0.6.1"
	if code := g.install("--dir", u, "--mode", "proxy", "--yes"); code != 1 || !strings.Contains(g.errw.String(), "clab v0.6.1 is older than v0.7.0") {
		t.Errorf("install in place, external clab: %d %s", code, g.errw)
	}
}

// AC-9 в модульном виде: команды-скрипты из подсказок выполняются дословно
// в sh без clab в PATH (скрипты — заглушки echo ok). Заглушка шаблона
// <version>-<date> подставляется: это образец имени, а не команда.
func TestWrappedHintsRunWithoutClab(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := filepath.Join(t.TempDir(), "my cadence")
	dir = installedAt(t, dir)
	for _, s := range []string{"install.sh", "upgrade.sh", "backup.sh", "restore.sh"} {
		p := filepath.Join(dir, s)
		mustWrite(t, p, "#!/bin/sh\necho ok "+s+" \"$@\"\n", 0o755)
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	cwd := t.TempDir()
	run := func(cmd string) {
		t.Helper()
		c := exec.Command(sh, "-c", cmd)
		c.Dir, c.Env = cwd, []string{"PATH=/usr/bin:/bin"}
		got, err := c.CombinedOutput()
		if err != nil || !strings.HasPrefix(string(got), "ok ") {
			t.Errorf("%s: %v %s", cmd, err, got)
		}
	}
	e, _, errw := testEnv(t, cwd, newFakeRunner(), wrapper)
	runServer(ctx, e, []string{"restore", "--dir", dir})
	line := strings.TrimPrefix(strings.TrimSpace(errw.String()), "✗ which backup? ")
	run(strings.ReplaceAll(line, "<version>-<date>", "v0.7.0-20261018-030000"))

	f := installSetup(t, wrapper)
	f.e.cwd = cwd
	f.install("--dir", dir, "--mode", "proxy", "--yes")
	m := regexp.MustCompile(`- upgrade:\s+(.+)`).FindStringSubmatch(f.errw.String())
	if m == nil {
		t.Fatalf("no upgrade hint: %s", f.errw)
	}
	run(m[1])
}

// installedAt — installed в заданном каталоге (путь с пробелом).
func installedAt(t *testing.T, dir string) string {
	t.Helper()
	src := installed(t, "v0.7.0", nil)
	if err := os.Rename(src, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}
