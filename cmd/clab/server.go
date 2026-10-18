package main

// clab server — управление установкой self-hosted на этой машине (CLAB-125,
// решение 0045): upgrade, backup, restore и status с теми же гарантиями,
// что у скриптов пакета (upgrade.sh, backup.sh, restore.sh, lib.sh).
//
// Команда не ходит в API Cadence Lab и не требует входа: она работает с
// каталогом установки и Docker на этой машине. Docker — через CLI
// `docker compose`, а не через SDK движка: та же семантика, что у скриптов и
// у ручного `docker compose`, и статический бинарь не растёт.
//
// Вывод — по-английски (решение q4 CLAB-125): его читает администратор
// клиента рядом с документацией пакета, которая тоже английская. Префиксы
// строк те же, что у скриптов (lib.sh: say/info/ok/warn/die), чтобы логи cron
// и проверки ci/ читались одинаково при любом исполнителе.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// serverEnv — всё, что команда берёт у машины. Тесты подменяют исполнителя
// команд, часы, сон, окружение и потоки: Docker в верстаке нет, а логика
// скриптов должна проверяться без него.
type serverEnv struct {
	out, errw io.Writer
	in        io.Reader
	terminal  bool // stdin — терминал: можно спросить подтверждение
	getenv    func(string) string
	environ   []string
	cwd       string
	goos      string
	run       runner
	now       func() time.Time
	sleep     func(time.Duration)
	http      *http.Client
	uid, gid  int
	cli       string // версия этого clab
	brew      bool   // этот clab стоит из формулы тапа
}

func newServerEnv() (*serverEnv, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	terminal := false
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		// /dev/null — тоже символьное устройство, но не терминал: его дают
		// stdin'ом cron и systemd. Как [ -t 0 ] у restore.sh.
		null, err := os.Stat(os.DevNull)
		terminal = err != nil || !os.SameFile(fi, null)
	}
	return &serverEnv{
		out:      os.Stdout,
		errw:     os.Stderr,
		in:       os.Stdin,
		terminal: terminal,
		getenv:   os.Getenv,
		environ:  os.Environ(),
		cwd:      cwd,
		goos:     runtime.GOOS,
		run:      execRunner{},
		now:      time.Now,
		sleep:    time.Sleep,
		http:     &http.Client{Timeout: 10 * time.Minute},
		uid:      os.Getuid(),
		gid:      os.Getgid(),
		cli:      version,
		brew:     installedByBrew(),
	}, nil
}

func (e *serverEnv) say(format string, a ...any) { fmt.Fprintf(e.out, format+"\n", a...) }
func (e *serverEnv) info(format string, a ...any) {
	fmt.Fprintf(e.out, "▸ "+format+"\n", a...)
}
func (e *serverEnv) ok(format string, a ...any) { fmt.Fprintf(e.out, "✓ "+format+"\n", a...) }
func (e *serverEnv) warn(format string, a ...any) {
	fmt.Fprintf(e.errw, "! "+format+"\n", a...)
}

// kv — пары «подпись: значение» колонками, как kv у остальных команд, но в
// поток команды.
func (e *serverEnv) kv(pairs ...string) {
	w := tabwriter.NewWriter(e.out, 0, 4, 2, ' ', 0)
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(w, "%s\t%s\n", pairs[i], pairs[i+1])
	}
	w.Flush()
}

// usageError — ошибка в аргументах: код выхода 2, как у неизвестной команды.
type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

const serverUsage = `clab server — manage the self-hosted Cadence Lab installation on this machine

Usage: clab server <command> [--dir DIR] [options]

Commands:
  upgrade   upgrade the stack to the latest or a given release, with a backup first
  backup    back up the database and the blobs volume, rotate old backups
  restore   restore the database and the blobs volume from a backup
  status    the installation, its version, the version of clab and the services

The installation directory is --dir DIR, else $CADENCE_DIR, else the current
directory or the nearest one above it with VERSION and package.sha256 (or
VERSION and docker-compose.yml). No sign-in is needed: clab server works with
the files and Docker of this machine, not with the API.

Installing is ./install.sh or get-cadence.sh of the package for now.

clab server <command> --help — the options of a command.`

// serverCommand — подкоманда clab server.
type serverCommand struct {
	name string
	help string
	run  func(ctx context.Context, e *serverEnv, args []string) error
}

func serverCommands() []serverCommand {
	return []serverCommand{
		{"upgrade", upgradeHelp, serverUpgrade},
		{"backup", backupHelp, serverBackup},
		{"restore", restoreHelp, serverRestore},
		{"status", statusHelp, serverStatus},
	}
}

// cmdServer — точка входа из commands(). Клиента API у неё нет: main не
// строит его для локальных команд (localCommand).
func cmdServer(ctx context.Context, _ *Client, args []string) error {
	e, err := newServerEnv()
	if err != nil {
		return err
	}
	if code := runServer(ctx, e, args); code != 0 {
		os.Exit(code)
	}
	return nil
}

// runServer разбирает подкоманду и печатает ошибку так, как её печатали
// скрипты: «✗ текст» в stderr и код 1 (2 — ошибка в аргументах).
func runServer(ctx context.Context, e *serverEnv, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(e.out, serverUsage)
		return 0
	}
	// Windows: стек — Docker Compose на Linux (или macOS для оценки), а пути,
	// права файлов и docker run --volumes-from здесь другие. Внутри WSL2
	// работает Linux-сборка clab.
	if e.goos == "windows" {
		fmt.Fprintln(e.errw, "✗ clab server manages a Docker Compose installation on Linux or macOS and does not run on Windows.\n"+
			"  Run the Linux build of clab inside WSL2, where the installation lives: https://learn.microsoft.com/windows/wsl/install")
		return 1
	}
	for _, c := range serverCommands() {
		if c.name != args[0] {
			continue
		}
		if wantsHelp(args[1:]) {
			fmt.Fprintln(e.out, c.help)
			return 0
		}
		err := c.run(ctx, e, args[1:])
		if err == nil {
			return 0
		}
		fmt.Fprintf(e.errw, "✗ %s\n", err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	fmt.Fprintf(e.errw, "✗ unknown command %q of clab server\n\n%s\n", args[0], serverUsage)
	return 2
}

func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// parseServerArgs разбирает флаги в любом порядке с позиционными
// аргументами, как циклы case скриптов: «restore FILE --yes» и «restore --yes
// FILE» равны. Пакет flag останавливается на первом позиционном аргументе.
func parseServerArgs(args []string, bools map[string]*bool, strs map[string]*string) ([]string, error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		if b, ok := bools[name]; ok && !hasValue {
			*b = true
			continue
		}
		if s, ok := strs[name]; ok {
			if !hasValue {
				if i+1 >= len(args) {
					return nil, usageError{fmt.Sprintf("%s needs a value", name)}
				}
				i++
				value = args[i]
			}
			*s = value
			continue
		}
		return nil, usageError{fmt.Sprintf("unknown option: %s", a)}
	}
	return pos, nil
}

// ── каталог установки ───────────────────────────────────────────────────────

// installation — каталог установки и то, что из него прочитано.
type installation struct {
	e       *serverEnv
	dir     string
	version string            // из VERSION; главнее .env
	env     map[string]string // .env, как его видят скрипты после set -a; . ./.env
	hasEnv  bool
}

// isInstallation — в каталоге лежит установка: VERSION и эталон пакета, а у
// бывшего git-клона без эталона — VERSION и docker-compose.yml.
func isInstallation(dir string) bool {
	if !isFile(filepath.Join(dir, "VERSION")) {
		return false
	}
	return isFile(filepath.Join(dir, packageManifest)) || isFile(filepath.Join(dir, "docker-compose.yml"))
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// findInstallation — --dir, затем CADENCE_DIR, затем текущий каталог и выше
// по дереву. Глобального реестра установок нет: путь одной машины и одного
// пользователя (root в cron против человека в терминале) спутает больше, чем
// сэкономит.
func findInstallation(e *serverEnv, dirFlag string) (string, error) {
	explicit, from := dirFlag, "--dir"
	if explicit == "" {
		explicit, from = e.getenv("CADENCE_DIR"), "CADENCE_DIR"
	}
	if explicit != "" {
		dir := explicit
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(e.cwd, dir)
		}
		dir = filepath.Clean(dir)
		if !isInstallation(dir) {
			return "", fmt.Errorf("%s %s is not a Cadence Lab installation: no VERSION with package.sha256 or docker-compose.yml there", from, explicit)
		}
		return dir, nil
	}
	for dir := filepath.Clean(e.cwd); ; dir = filepath.Dir(dir) {
		if isInstallation(dir) {
			return dir, nil
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return "", fmt.Errorf("no Cadence Lab installation in %s or above it.\n"+
		"  Pass the installation directory: clab server <command> --dir /path/to/cadence (or set CADENCE_DIR)", e.cwd)
}

// openInstallation находит каталог и читает VERSION; .env — если он есть.
func openInstallation(e *serverEnv, dirFlag string) (*installation, error) {
	dir, err := findInstallation(e, dirFlag)
	if err != nil {
		return nil, err
	}
	inst := &installation{e: e, dir: dir}
	if err := inst.loadVersion(); err != nil {
		return nil, err
	}
	envPath := filepath.Join(dir, ".env")
	if isFile(envPath) {
		raw, err := os.ReadFile(envPath)
		if err != nil {
			return nil, err
		}
		env, err := parseDotEnv(string(raw), e.getenv)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envPath, err)
		}
		inst.env, inst.hasEnv = env, true
	}
	return inst, nil
}

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func validVersion(v string) bool { return versionPattern.MatchString(v) }

// loadVersion — lib.sh load_version: VERSION — единственный источник
// версии образов.
func (i *installation) loadVersion() error {
	raw, err := os.ReadFile(filepath.Join(i.dir, "VERSION"))
	if err != nil {
		return fmt.Errorf("no VERSION file in %s", i.dir)
	}
	v := strings.Join(strings.Fields(string(raw)), "")
	if !validVersion(v) {
		return fmt.Errorf("VERSION holds '%s', expected vX.Y.Z", v)
	}
	i.version = v
	return nil
}

// requireEnv — lib.sh load_env: без .env установки нет.
func (i *installation) requireEnv() error {
	if !i.hasEnv {
		return fmt.Errorf("no .env in %s: run ./install.sh first", i.dir)
	}
	return nil
}

// get — значение так, как его видит скрипт после set -a; . ./.env: .env
// перекрывает окружение процесса, остальное берётся из окружения.
func (i *installation) get(key string) string {
	if v, ok := i.env[key]; ok {
		return v
	}
	return i.e.getenv(key)
}

func (i *installation) getDefault(key, def string) string {
	if v := i.get(key); v != "" {
		return v
	}
	return def
}

// composeEnv — окружение дочерних docker и docker compose: окружение
// процесса, поверх него .env (как set -a; . ./.env), поверх — CADENCE_VERSION
// из VERSION. COMPOSE_PROJECT_NAME из окружения поэтому доходит до compose,
// как и у скриптов.
func (i *installation) composeEnv() []string {
	merged := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := merged[k]; !ok {
			order = append(order, k)
		}
		merged[k] = v
	}
	for _, kv := range i.e.environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	keys := make([]string, 0, len(i.env))
	for k := range i.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		set(k, i.env[k])
	}
	set("CADENCE_VERSION", i.version)
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// path — путь внутри установки; относительный BACKUP_DIR считается от неё,
// как у скриптов, которые делают cd в свой каталог.
func (i *installation) path(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(i.dir, p)
}

// rel — путь для напечатанных команд: относительно каталога установки, если
// он внутри неё, — команды отката выполняются из него.
func (i *installation) rel(p string) string {
	r, err := filepath.Rel(i.dir, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return p
	}
	return r
}

// cdHint — строка «cd каталог» перед напечатанными командами, если clab
// запущен не из каталога установки: команды отката относительны ему.
func (i *installation) cdHint() string {
	if filepath.Clean(i.e.cwd) == i.dir {
		return ""
	}
	return "  cd " + shellQuote(i.dir) + "\n"
}

// shellQuote — путь в напечатанной команде; простые пути без кавычек.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:@+=,-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// checkMode — lib.sh check_mode: CADENCE_MODE и COMPOSE_FILE согласованы.
func (i *installation) checkMode() error {
	mode := i.getDefault("CADENCE_MODE", "tls")
	if mode != "tls" && mode != "proxy" {
		return fmt.Errorf("CADENCE_MODE in .env is '%s', expected tls or proxy", mode)
	}
	if mode == "tls" && (i.get("SDLC_DOMAIN") == "" || i.get("ACME_EMAIL") == "") {
		return errors.New("CADENCE_MODE=tls needs SDLC_DOMAIN and ACME_EMAIL in .env")
	}
	cf := i.get("COMPOSE_FILE")
	if !strings.Contains(cf, "compose."+mode+".yml") {
		return fmt.Errorf("COMPOSE_FILE in .env ('%s') does not include compose.%s.yml for CADENCE_MODE=%s", cf, mode, mode)
	}
	return nil
}

// publicURL — lib.sh public_url.
func (i *installation) publicURL() string {
	if i.getDefault("CADENCE_MODE", "tls") == "proxy" {
		return "http://" + i.getDefault("CADENCE_HTTP_ADDR", "127.0.0.1:8080") + " (behind your proxy: its public address)"
	}
	return "https://" + i.get("SDLC_DOMAIN")
}

// kind — как установка появилась: из пакета релиза (есть эталон
// package.sha256) или git-клоном (до v0.6.0).
func (i *installation) kind() string {
	pkg := isFile(filepath.Join(i.dir, packageManifest))
	_, gitErr := os.Stat(filepath.Join(i.dir, ".git"))
	switch {
	case pkg && gitErr == nil:
		return "package (a former git clone: .git can be removed)"
	case pkg:
		return "package"
	case gitErr == nil:
		return "git clone"
	default:
		return "files without " + packageManifest
	}
}

// open — проверки, общие для backup, restore и upgrade: Docker, .env и
// правило версий.
func (i *installation) open(ctx context.Context) error {
	if err := i.compatible(); err != nil {
		return err
	}
	if err := requireDocker(ctx, i.e); err != nil {
		return err
	}
	return i.requireEnv()
}
