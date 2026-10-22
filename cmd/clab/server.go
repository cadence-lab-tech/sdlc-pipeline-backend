package main

// clab server — управление установкой self-hosted на этой машине (CLAB-125,
// решение 0045): install (CLAB-160), upgrade, backup, restore и status с теми
// же гарантиями, что у скриптов пакета (get-cadence.sh, install.sh,
// upgrade.sh, backup.sh, restore.sh, lib.sh).
//
// Команда не требует входа: она работает с каталогом установки и Docker на
// этой машине. В API ходит только шаг стартового проекта (install
// --with-starter и starter, CLAB-199) — к API своей установки через её Caddy
// с токеном администратора установки, не с конфигом clab. Docker — через CLI
// `docker compose`, а не через SDK движка: та же семантика, что у скриптов и
// у ручного `docker compose`, и статический бинарь не растёт.
//
// Вывод — по-английски (решение q4 CLAB-125): его читает администратор
// клиента рядом с документацией пакета, которая тоже английская. Префиксы
// строк те же, что у скриптов (lib.sh: say/info/ok/warn/die), чтобы логи cron
// и проверки ci/ читались одинаково при любом исполнителе.

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
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
	self      string // путь этого clab для напечатанных команд (cron)

	// Проверки машины для clab server install: в тестах подменяются, чтобы
	// не зависеть от портов и DNS верстака.
	arch       string // runtime.GOARCH
	portBusy   func(host string, port int) bool
	lookupHost func(host string) ([]string, error)
	localAddrs func() []string

	// Отвечает ли адрес по HTTP (ghcr.io для проверки подписей): в тестах
	// подменяется, сети в верстаке нет.
	reachable func(ctx context.Context, rawURL string) bool

	// API своей установки (CLAB-199): куда соединяться в режиме tls вместо
	// 127.0.0.1:443 и каким корням доверять (nil — системным). Тесты
	// подставляют httptest.NewTLSServer; у настоящего clab оба пусты.
	apiTLSDial string
	apiRoots   *x509.CertPool
	// Чтение файла секрета на этой машине (CLAB-252); nil — os.ReadFile.
	// Тесты подставляют отказ по правам: верстак работает от root, и chmod
	// его не даёт.
	readFile func(string) ([]byte, error)
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
		self:     clabPath(),

		arch:       runtime.GOARCH,
		portBusy:   realPortBusy,
		lookupHost: net.LookupHost,
		localAddrs: realLocalAddrs,
		reachable:  realReachable,
	}, nil
}

// wrapperEnv — признак обёртки пакета (CLAB-242, R1). Скрипт пакета
// (install.sh, upgrade.sh, backup.sh, restore.sh на обёртках CLAB-198)
// запускает clab server с CADENCE_WRAPPER=1 в окружении exec:
//
//	CADENCE_WRAPPER=1 exec "$clab" server upgrade --dir "$dir" "$@"
//
// С ним напечатанные команды для администратора — скрипты пакета, а не
// clab server: своего clab в PATH у него может не быть. Признак —
// договорённость обёртки и бинаря, а не настройка: в .env он не пишется и
// из .env не читается, только из окружения процесса. Другое значение или
// его отсутствие — прежние подсказки clab server.
const wrapperEnv = "CADENCE_WRAPPER"

// wrapped — clab server запущен обёрткой пакета.
func (e *serverEnv) wrapped() bool { return e.getenv(wrapperEnv) == "1" }

// script — однострочная команда скрипта пакета name из каталога установки
// dir для администратора: ./name, если clab запущен из этого каталога,
// иначе путь к скрипту — так команда выполняется дословно из любого
// каталога. В блоке после cdHint скрипт пишется ./name; без каталога — тоже.
func (e *serverEnv) script(dir, name string) string {
	if dir == "" || filepath.Clean(e.cwd) == dir {
		return "./" + name
	}
	return shellQuote(filepath.Join(dir, name))
}

// packageBinary — этот clab — бинарь пакета установки dir: лежит в
// <dir>/bin/ (CLAB-242, R2). Его заменяет upgrade вместе с остальными
// файлами пакета, поэтому сообщения о версии для него другие: обновлять его
// из тапа не нужно. От признака обёртки не зависит: обёртка может звать
// внешний clab (CADENCE_CLAB), а администратор — бинарь пакета напрямую.
// Пути сравниваются без ссылок: clab в PATH бывает ссылкой на бинарь
// пакета, а каталог — путём через ссылку (/var на macOS).
func (e *serverEnv) packageBinary(dir string) bool {
	if e.self == "" || dir == "" {
		return false
	}
	return filepath.Dir(realPath(e.self)) == filepath.Join(realPath(dir), "bin")
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
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
  install   install Cadence Lab from a release package into a new directory
  upgrade   upgrade the stack to the latest or a given release, with a backup first
  backup    back up the database and the blobs volume, rotate old backups, copy
            them to an S3 bucket; backup verify checks that a backup restores
  restore   restore the database and the blobs volume from a backup
  status    the installation, its version, the version of clab and the services
  starter   set up the starter project: a project in a team with the roles and
            workflows of bundles/starter.json, its workflows active

The installation directory is --dir DIR, else $CADENCE_DIR, else the current
directory or the nearest one above it with VERSION and package.sha256 (or
VERSION and docker-compose.yml); install creates it, default ./cadence. No
sign-in is needed: clab server works with the files and Docker of this
machine. Only the starter project (starter and install --with-starter) goes
to the API, and only to the API of this installation, through its Caddy on
this machine with the administrator token of the installation.

clab server <command> --help — the options of a command.`

// serverCommand — подкоманда clab server.
type serverCommand struct {
	name string
	help string
	run  func(ctx context.Context, e *serverEnv, args []string) error
}

func serverCommands() []serverCommand {
	return []serverCommand{
		{"install", installHelp, serverInstall},
		{"upgrade", upgradeHelp, serverUpgrade},
		{"backup", backupHelp, serverBackup},
		{"restore", restoreHelp, serverRestore},
		{"status", statusHelp, serverStatus},
		{"starter", starterHelp, serverStarter},
	}
}

// cmdServer — точка входа из commands(). Клиента API из конфига у неё нет:
// main не строит его для локальных команд (localCommand). К API своей
// установки ходит только шаг стартового проекта — своим клиентом
// (server_api.go) с адресом и токеном из установки.
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
// из VERSION и COMPOSE_PROJECT_NAME — действующее имя установки (CLAB-164).
// Имя ставится всегда, и cadence по умолчанию тоже: иначе переменная
// окружения увела бы compose к другой установке на той же машине.
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
	name, _, _ := i.name()
	set("COMPOSE_PROJECT_NAME", name)
	// PWD — каталог установки, как у скриптов после cd: compose пишет его в
	// метку working_dir контейнеров, а по ней «имя занято» называет
	// установку. С PWD вызывающего Go-шный getcwd compose отдал бы путь без
	// симлинков (/private/var вместо /var на macOS), и та же установка
	// называлась бы по-разному у clab и у скриптов.
	set("PWD", i.dir)
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
	return i.checkDBMode()
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

// open — проверки, общие для backup, restore и upgrade: правило версий,
// .env и имя установки, затем Docker. Имя — до Docker, как load_env у
// скриптов: COMPOSE_PROJECT_NAME другой установки останавливает команду,
// прежде чем она что-то спросит у Docker.
func (i *installation) open(ctx context.Context) error {
	if err := i.compatible(ctx); err != nil {
		return err
	}
	if err := i.requireEnv(); err != nil {
		return err
	}
	if err := i.checkName(); err != nil {
		return err
	}
	return requireDocker(ctx, i.e)
}
