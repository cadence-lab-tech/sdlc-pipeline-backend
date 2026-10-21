package main

// clab server install (CLAB-160, решение 0045 п. 1) — первая установка стека
// из пакета релиза одной командой: то, что делают get-cadence.sh и install.sh
// пакета, без curl, tar, sha256sum, openssl, ss и getent на машине.
//
// Результат — тот же каталог и тот же .env (ключи, кавычки, права), что даёт
// get-cadence.sh | sh той же версии: установку дальше обслуживают и скрипты
// пакета, и clab server. Поэтому .env пишется по правилу set_kv install.sh, а
// вопросы и умолчания — те же, что у него.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const installHelp = `clab server install — install Cadence Lab on this machine from a release
package: download the package and its checksums.txt, verify the checksum,
unpack it, write .env with generated secrets, start the stack and print the
administrator token. git, curl, tar and openssl are not needed.

Usage: clab server install [--version vX.Y.Z] [--dir DIR] [--name NAME]
                           [--mode tls|proxy] [--domain HOST] [--email ADDR]
                           [--http-addr [IP:]PORT] [--team KEY]
                           [--team-name NAME] [--external-db URL]
                           [--external-db-ca FILE] [--yes]

  clab server install --domain cadence.example.com --email ops@example.com
  clab server install --mode proxy --http-addr 127.0.0.1:8080 --yes
  clab server install --dir /opt/cadence-stage --name cadence-stage --mode proxy --http-addr 127.0.0.1:8081

Options:
  --version vX.Y.Z   the release to install; default the latest release this
                     clab can manage (not newer than clab itself)
  --dir DIR          the installation directory, default $CADENCE_DIR, else
                     ./cadence; it must not exist, be empty, or hold an
                     unpacked package without .env (installed in place, no
                     download)
  --name NAME        the name of this installation on the machine (the
                     Docker Compose project of its containers and volumes),
                     default $COMPOSE_PROJECT_NAME, else cadence; a second
                     installation on the machine needs a name of its own
  --mode MODE        tls: HTTPS on your domain with Let's Encrypt; proxy:
                     plain HTTP behind your reverse proxy (default tls)
  --domain HOST      tls mode: the domain that points at this machine
  --email ADDR       tls mode: e-mail for Let's Encrypt notices
                     (default admin@<domain>)
  --http-addr ADDR   proxy mode: where Caddy listens, default 127.0.0.1:8080
  --team KEY         the key of the first team (default none: create teams
                     later with clab team create)
  --team-name NAME   the display name of the first team (default its key)
  --external-db URL  keep the data in your own PostgreSQL 15-18 instead of the
                     postgres container: postgres://USER:PASSWORD@HOST:PORT/
                     DATABASE?sslmode=verify-full (docs/admin.md, "External
                     PostgreSQL"); the database is checked before .env is
                     written
  --external-db-ca FILE
                     the CA of the database server: copied to secrets/db-ca.pem
                     and named in the URL (sslrootcert), sslmode=verify-full
                     unless the URL sets another
  -y, --yes, --non-interactive
                     ask nothing: every answer comes from flags or the
                     environment (also without a terminal)

The answers may come from the environment as with install.sh: CADENCE_MODE,
SDLC_DOMAIN, ACME_EMAIL, CADENCE_HTTP_ADDR, SDLC_BOOTSTRAP_TEAM,
SDLC_BOOTSTRAP_TEAM_NAME, CADENCE_EXTERNAL_DB, CADENCE_EXTERNAL_DB_CA.
SDLC_ADMIN_TOKEN supplies the administrator token
(at least 16 characters); otherwise one is generated and shown once.

It runs once: a directory with .env is refused, and nothing is changed there
(new secrets would lock the database volume out). Every refusal comes before
.env is written. If the stack does not start after that, .env is kept and the
command prints how to start it.

With cosign 3.x in PATH it verifies the signature of checksums.txt of the
release (before anything is unpacked) and of both images of the version at
ghcr.io by the digests docker-compose.yml pins (before the answers, the check
of an external database and .env); installed in place, the images only.
CADENCE_VERIFY decides the rest: auto (default) warns once without cosign,
require refuses without it, off does not verify (docs/admin.md, "Verifying
a release").

  CADENCE_READY_TIMEOUT  seconds to wait for readiness, default 300
  CADENCE_RELEASES_URL   a mirror of the releases: download/vX.Y.Z/<asset> and
                         latest/download/<asset> (https://, http:// or file://)
  CADENCE_VERIFY         auto, require or off: the signatures of the release`

// installOptions — флаги clab server install.
type installOptions struct {
	version, dir, name            string
	mode, domain, email, httpAddr string
	team, teamName                string
	externalDB, externalDBCA      string
	yes                           bool
}

func serverInstall(ctx context.Context, e *serverEnv, args []string) error {
	var o installOptions
	pos, err := parseServerArgs(args,
		map[string]*bool{"--yes": &o.yes, "-y": &o.yes, "--non-interactive": &o.yes},
		map[string]*string{
			"--version": &o.version, "--dir": &o.dir, "--name": &o.name,
			"--mode": &o.mode, "--domain": &o.domain, "--email": &o.email, "--http-addr": &o.httpAddr,
			"--team": &o.team, "--team-name": &o.teamName,
			"--external-db": &o.externalDB, "--external-db-ca": &o.externalDBCA,
		})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{fmt.Sprintf("unexpected argument: %s (the version goes with --version)", pos[0])}
	}
	if o.version != "" && !validVersion(o.version) {
		return usageError{fmt.Sprintf("'%s' is not a version: expected vX.Y.Z", o.version)}
	}
	return install(ctx, e, o)
}

// install — шаги install.sh в порядке «сначала все отказы, потом запись»:
// имя, каталог, Docker и занятость имени, версия, загрузка и сумма, ответы и
// проверки машины, распаковка, .env, старт.
func install(ctx context.Context, e *serverEnv, o installOptions) error {
	// --- 1. имя и каталог: без Docker и сети ------------------------------------
	name, err := chooseInstallationName(o.name, e.getenv("COMPOSE_PROJECT_NAME"))
	if err != nil {
		return err
	}
	dir, inPlace, err := installDir(e, o.dir)
	if err != nil {
		return err
	}
	o.dir = dir
	// Таймаут ожидания проверяется сразу, а не после записи .env.
	probe := &installation{e: e, dir: dir}
	if _, err := probe.readyTimeout(); err != nil {
		return err
	}

	// --- 2. Docker и имя на этой машине ------------------------------------------
	e.info("checking the environment")
	// Режим проверки подписей — до Docker и сети: require без cosign
	// отказывает раньше всего (lib.sh verify_init в install.sh).
	v, err := newVerifier(ctx, e, e.getenv("CADENCE_VERIFY"), nothingInstalled)
	if err != nil {
		return err
	}
	if err := requireDocker(ctx, e); err != nil {
		return err
	}
	taken, err := nameInUse(ctx, e.run, name)
	if err != nil {
		return err
	}
	if taken != "" {
		retry := "clab server install --dir <a new directory>"
		if e.wrapped() {
			retry = e.script(dir, "install.sh")
		}
		return nameTaken(name, taken, retry)
	}
	switch e.arch {
	case "amd64", "arm64", "":
	default:
		e.warn("this machine is %s; the images are built for linux/amd64 and linux/arm64 only, so pulling them here will most likely fail", e.arch)
	}

	// --- 3. версия и пакет ---------------------------------------------------------
	work, err := os.MkdirTemp("", "clab-install.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	var version, tarball, pkgDir string
	if inPlace {
		if version, err = inPlaceVersion(dir, o.version); err != nil {
			return err
		}
		if err := (&installation{e: e, dir: dir, version: version}).installCompatible(ctx); err != nil {
			return err
		}
		e.info("installing %s in place: %s holds its unpacked package", version, dir)
		pkgDir = dir
	} else {
		releases := releasesFrom(e.getenv("CADENCE_RELEASES_URL"))
		if version, err = installVersion(ctx, e, releases, o.version, work); err != nil {
			return err
		}
		if tarball, err = fetchInstallPackage(ctx, e, v, releases, version, work); err != nil {
			return err
		}
		pkgDir = filepath.Join(work, "peek")
	}
	// Подписи образов версии — до вопросов, до предпроверки внешней базы
	// (она запускает образ backend) и до .env. Установка на месте
	// проверяет только их: скачанного checksums.txt у неё нет.
	if err := v.verifyImages(ctx, version, installImages(e, pkgDir, version)); err != nil {
		return err
	}

	// --- 4. ответы и проверки машины ---------------------------------------------
	a, err := installAnswers(e, o)
	if err != nil {
		return err
	}
	pairs := a.pairs(name)
	for _, p := range pairs {
		if strings.ContainsAny(p.value, "'\n") {
			return fmt.Errorf("the value of %s must not contain a single quote or a line break", p.key)
		}
	}
	if a.external {
		if err := requireComposeForExternalDB(ctx, e); err != nil {
			return err
		}
	}

	// --- 5. файлы: распаковка, .env, override, каталоги ---------------------------
	if !inPlace {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := extractTarGz(tarball, dir, nil); err != nil {
			return err
		}
		e.info("unpacked Cadence Lab %s into %s", version, dir)
	}
	if a.external {
		if err := externalPrecheck(ctx, e, dir, version, name, a); err != nil {
			return err
		}
	}
	e.info("writing .env")
	if err := writeInstallFiles(dir, pairs); err != nil {
		return err
	}
	e.ok(".env written (mode 600)")

	// --- 6. старт -----------------------------------------------------------------
	inst, err := openInstallation(e, dir)
	if err != nil {
		return err
	}
	if err := inst.checkName(); err != nil {
		return err
	}
	if err := inst.checkMode(); err != nil {
		return err
	}
	start := fmt.Sprintf("  cd %s && export CADENCE_VERSION=$(cat VERSION) && docker compose up -d", shellQuote(dir))
	e.info("pulling images of %s", version)
	if err := inst.composeRun(ctx, false, "pull", "--quiet"); err != nil {
		return fmt.Errorf("could not pull the images of %s: check that the registry (ghcr.io, or CADENCE_REGISTRY in .env) is reachable from this host.\n"+
			"  .env is kept; then start the stack:\n%s", version, start)
	}
	e.info("starting the stack")
	if err := inst.composeRun(ctx, false, "up", "-d", "--remove-orphans"); err != nil {
		return fmt.Errorf("docker compose up failed (see above). .env is kept: fix the cause and run\n%s", start)
	}
	timeout, err := inst.readyTimeout()
	if err != nil {
		return err
	}
	e.info("waiting for the service to become ready (up to %ds)", timeout)
	if !inst.waitReady(ctx, timeout) {
		inst.showFailure(ctx, "api", "caddy")
		return fmt.Errorf("the stack did not become ready in %ds; the logs are above. .env is kept: fix the cause and run\n%s", timeout, start)
	}

	e.ok("Cadence Lab %s is up", version)
	e.say("")
	// Одна таблица: колонка значений начинается с 24-го символа, под ней —
	// пометка о токене, как у install.sh.
	token := "the one supplied in SDLC_ADMIN_TOKEN"
	if a.tokenGenerated {
		token = a.token
	}
	rows := []string{"  Installation:", fmt.Sprintf("%s (%s)", dir, name), "  Address:", inst.publicURL()}
	if inst.externalDB() {
		rows = append(rows, "  Database:", "external "+dbTarget(inst.get("SDLC_DATABASE_URL")))
	}
	e.kv(append(rows, "  Administrator token:", token)...)
	if a.tokenGenerated {
		e.say("                        (shown once; it is also in .env - keep both private)")
	}
	e.say("")
	e.say("Next: sign in with the token and follow docs/admin.md (teams, GitHub App, OIDC, CLI):")
	e.say("  clab auth login --host %s --with-token", signInURL(inst))
	backupLog := shellQuote(filepath.Join(dir, "backups", "backup.log"))
	if e.wrapped() {
		// Под обёрткой — скрипты пакета, как у install.sh: cron зовёт
		// backup.sh по абсолютному пути, clab в PATH ему не нужен. У
		// состояния скрипта нет — его показывает этот clab.
		backupSh := shellQuote(filepath.Join(dir, "backup.sh"))
		e.say("Manage the installation with the scripts in %s: ./upgrade.sh, ./backup.sh and ./restore.sh;", shellQuote(dir))
		e.say("its state: %s server status --dir %s.", shellQuote(e.self), shellQuote(dir))
		e.say("Set up nightly backups: 0 3 * * * %s >> %s 2>&1", backupSh, backupLog)
		e.say("and a weekly restore check: 0 5 * * 0 %s verify >> %s 2>&1", backupSh, backupLog)
	} else {
		e.say("Manage the installation with clab server upgrade, backup, restore and status --dir %s.", shellQuote(dir))
		e.say("Set up nightly backups: 0 3 * * * %s server backup --dir %s >> %s 2>&1",
			shellQuote(e.self), shellQuote(dir), backupLog)
		e.say("and a weekly restore check: 0 5 * * 0 %s server backup verify --dir %s >> %s 2>&1",
			shellQuote(e.self), shellQuote(dir), backupLog)
	}
	e.say("Copy the backups off this machine: BACKUP_S3_* in .env (docs/admin.md, \"Off-machine copy\").")
	return nil
}

// signInURL — адрес для clab auth login: в режиме proxy — адрес Caddy на
// этой машине, в tls — домен.
func signInURL(i *installation) string {
	if i.getDefault("CADENCE_MODE", "tls") == "proxy" {
		return "http://" + i.getDefault("CADENCE_HTTP_ADDR", "127.0.0.1:8080")
	}
	return "https://" + i.get("SDLC_DOMAIN")
}

// nameTaken — отказ install.sh на занятом имени, с подсказкой для clab;
// retry — команда установки перед --name: clab server install с новым
// каталогом или install.sh пакета под обёрткой (CLAB-242).
func nameTaken(name, taken, retry string) error {
	var where string
	switch taken {
	case "volumes":
		where = fmt.Sprintf("Docker keeps volumes of the compose project '%s' (an installation removed without its data)", name)
	case "containers":
		where = fmt.Sprintf("Docker has containers of the compose project '%s'", name)
	default:
		where = "the installation in " + taken + " uses it"
	}
	return fmt.Errorf("the installation name '%s' is taken on this machine: %s.\n"+
		"  Every installation on one machine needs a name of its own; pick another one with --name:\n"+
		"    %s --name %s-2 ...\n"+
		"  The name is the Docker Compose project of the installation, not a project inside Cadence Lab. Nothing was changed.",
		name, where, retry, name)
}

// ── каталог ──────────────────────────────────────────────────────────────────

// installDir — каталог новой установки: --dir, иначе CADENCE_DIR, иначе
// ./cadence (как get-cadence.sh). Вверх по дереву, как остальные подкоманды,
// install не ищет: он создаёт каталог, а не находит его. inPlace — в
// каталоге распакованный пакет без .env (решение q3): ставится он сам.
func installDir(e *serverEnv, flag string) (dir string, inPlace bool, err error) {
	dir = flag
	if dir == "" {
		dir = e.getenv("CADENCE_DIR")
	}
	if dir == "" {
		dir = "cadence"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(e.cwd, dir)
	}
	dir = filepath.Clean(dir)

	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dir, false, nil
	case err != nil:
		return "", false, err
	case !fi.IsDir():
		return "", false, fmt.Errorf("%s exists and is not a directory: pick a new directory with --dir", dir)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".env")); err == nil {
		status, upgrade := "clab server status --dir "+shellQuote(dir), "clab server upgrade --dir "+shellQuote(dir)
		if e.wrapped() {
			status, upgrade = shellQuote(e.self)+" server status --dir "+shellQuote(dir), e.script(dir, "upgrade.sh")
		}
		return "", false, fmt.Errorf("%s/.env exists: Cadence Lab is already installed there, and install never regenerates secrets\n"+
			"  (a new POSTGRES_PASSWORD would no longer open the existing database volume). Nothing was changed.\n"+
			"  - its state:  %s\n"+
			"  - upgrade:    %s",
			dir, status, upgrade)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false, err
	}
	if len(entries) == 0 {
		return dir, false, nil
	}
	if isFile(filepath.Join(dir, "VERSION")) && isFile(filepath.Join(dir, packageManifest)) {
		return dir, true, nil
	}
	return "", false, fmt.Errorf("%s is not empty and holds no unpacked package (VERSION and %s): pick a new directory with --dir", dir, packageManifest)
}

// inPlaceVersion — версия распакованного пакета; --version, если задан,
// должен с ней совпасть: другую версию на месте не поставить.
func inPlaceVersion(dir, want string) (string, error) {
	probe := &installation{dir: dir}
	if err := probe.loadVersion(); err != nil {
		return "", err
	}
	if !isFile(filepath.Join(dir, ".env.example")) {
		return "", fmt.Errorf("%s has no .env.example: not an unpacked package of Cadence Lab", dir)
	}
	if want != "" && want != probe.version {
		return "", fmt.Errorf("%s holds the package of %s, not %s: install %s there, or %s into a new directory with --dir",
			dir, probe.version, want, probe.version, want)
	}
	return probe.version, nil
}

// ── версия и пакет ─────────────────────────────────────────────────────────

// installVersion — какую версию ставить (решение q2 CLAB-160): заданную
// --version, если clab её умеет; без неё — последний релиз, но не новее
// этого clab.
func installVersion(ctx context.Context, e *serverEnv, releases, want, work string) (string, error) {
	if want != "" {
		if err := installCompat(e.cli, want, e.brew); err != nil {
			return "", err
		}
		return want, nil
	}
	e.info("looking up the latest release at %s", releases)
	latest, err := latestRelease(ctx, e, releases, work)
	if err != nil {
		return "", fmt.Errorf("could not find the latest release at %s", releases)
	}
	if versionLess(e.cli, latest) {
		e.info("installing %s, the latest release clab %s can manage (the latest is %s; update clab to install it)", e.cli, e.cli, latest)
		return e.cli, nil
	}
	if err := installCompat(e.cli, latest, e.brew); err != nil {
		return "", err
	}
	return latest, nil
}

// fetchInstallPackage скачивает пакет версии во временный каталог, сверяет
// сумму и подпись checksums.txt (до распаковки) и заглядывает внутрь:
// эталон есть, VERSION — та самая; docker-compose.yml — для проверки
// образов. Каталог установки до этого не создаётся.
func fetchInstallPackage(ctx context.Context, e *serverEnv, verify *verifier, releases, v, work string) (string, error) {
	name := packageName(v)
	e.info("downloading %s", name)
	tarball, err := downloadRelease(ctx, e, releases, v, work)
	switch {
	case errors.Is(err, errNoRelease):
		latest := ""
		if l, _ := latestRelease(ctx, e, releases, work); l != "" {
			latest = " (the latest is " + l + ")"
		}
		return "", fmt.Errorf("there is no release %s with a package at %s%s", v, releases, latest)
	case errors.Is(err, errChecksum):
		return "", fmt.Errorf("the checksum of %s does not match checksums.txt of %s: the download is broken or altered; nothing was installed", name, v)
	case err != nil:
		return "", fmt.Errorf("downloading %s failed: %v; nothing was installed", name, err)
	}
	if err := verify.verifyPackage(ctx, releases, v, work); err != nil {
		return "", err
	}
	peek := filepath.Join(work, "peek")
	if err := extractTarGz(tarball, peek, map[string]bool{"VERSION": true, packageManifest: true, ".env.example": true, "docker-compose.yml": true}); err != nil {
		return "", err
	}
	if !isFile(filepath.Join(peek, packageManifest)) || !isFile(filepath.Join(peek, ".env.example")) {
		return "", fmt.Errorf("%s has no %s or .env.example: not a package of Cadence Lab", name, packageManifest)
	}
	probe := &installation{dir: peek}
	if err := probe.loadVersion(); err != nil {
		return "", fmt.Errorf("%s: %v", name, err)
	}
	if probe.version != v {
		return "", fmt.Errorf("VERSION in %s holds '%s', expected %s", name, probe.version, v)
	}
	return tarball, nil
}

// ── ответы ──────────────────────────────────────────────────────────────────

type installAnswersT struct {
	mode, domain, email, httpAddr string
	team, teamName                string
	composeFile                   string
	external                      bool
	dbURL, dbCA                   string // dbCA — абсолютный путь к файлу CA
	token                         string
	tokenGenerated                bool
	postgresPassword, webhook     string
}

// installAnswers — ответы install.sh: флаг, иначе переменная окружения,
// иначе вопрос в терминале с умолчанием (без терминала или с --yes — само
// умолчание), и проверки машины, которые от них зависят.
func installAnswers(e *serverEnv, o installOptions) (*installAnswersT, error) {
	pick := func(flag, env string) string {
		if flag != "" {
			return flag
		}
		return e.getenv(env)
	}
	q := asker{e: e, interactive: e.terminal && !o.yes}
	a := &installAnswersT{
		mode:     pick(o.mode, "CADENCE_MODE"),
		domain:   pick(o.domain, "SDLC_DOMAIN"),
		email:    pick(o.email, "ACME_EMAIL"),
		httpAddr: pick(o.httpAddr, "CADENCE_HTTP_ADDR"),
		team:     pick(o.team, "SDLC_BOOTSTRAP_TEAM"),
		teamName: pick(o.teamName, "SDLC_BOOTSTRAP_TEAM_NAME"),
	}
	var err error
	if a.mode, err = q.ask("Mode: tls (HTTPS on your domain) or proxy (HTTP behind your reverse proxy)", a.mode, "tls"); err != nil {
		return nil, err
	}
	switch a.mode {
	case "tls":
		if a.domain, err = q.ask("Domain name that points at this machine", a.domain, ""); err != nil {
			return nil, err
		}
		if a.domain == "" {
			return nil, errors.New("tls mode needs a domain: --domain cadence.example.com (or SDLC_DOMAIN), or use --mode proxy")
		}
		if a.email, err = q.ask("E-mail for Let's Encrypt notices", a.email, "admin@"+a.domain); err != nil {
			return nil, err
		}
		for _, port := range []int{80, 443} {
			if e.portBusy("", port) {
				return nil, fmt.Errorf("port %d is already in use on this machine; tls mode needs ports 80 and 443 (or use --mode proxy behind the existing web server)", port)
			}
		}
		checkDomain(e, a.domain, o.dir)
		a.composeFile = "docker-compose.yml:compose.tls.yml:compose.override.yml"
	case "proxy":
		if a.httpAddr, err = q.ask("Address Caddy listens on for your proxy ([ip:]port)", a.httpAddr, "127.0.0.1:8080"); err != nil {
			return nil, err
		}
		host, port, err := parseHTTPAddr(a.httpAddr)
		if err != nil {
			return nil, err
		}
		if e.portBusy(host, port) {
			return nil, fmt.Errorf("port %d is already in use on this machine; pick another with --http-addr 127.0.0.1:<port>", port)
		}
		a.composeFile = "docker-compose.yml:compose.proxy.yml:compose.override.yml"
	default:
		return nil, fmt.Errorf("mode must be tls or proxy, not '%s'", a.mode)
	}
	if a.team, err = q.ask("Key of the first team (empty: create teams later with clab)", a.team, ""); err != nil {
		return nil, err
	}
	if a.team != "" && a.teamName == "" {
		if a.teamName, err = q.ask("Display name of team "+a.team, a.teamName, a.team); err != nil {
			return nil, err
		}
	}

	if err := a.externalDB(e, pick(o.externalDB, "CADENCE_EXTERNAL_DB"), pick(o.externalDBCA, "CADENCE_EXTERNAL_DB_CA")); err != nil {
		return nil, err
	}

	if a.token = e.getenv("SDLC_ADMIN_TOKEN"); a.token != "" {
		if len(a.token) < 16 {
			return nil, errors.New("SDLC_ADMIN_TOKEN must be at least 16 characters")
		}
	} else {
		if a.token, err = randomHex(24); err != nil {
			return nil, err
		}
		a.token, a.tokenGenerated = "sdlc_"+a.token, true
	}
	if !a.external {
		if a.postgresPassword, err = randomHex(24); err != nil {
			return nil, err
		}
	}
	if a.webhook, err = randomHex(24); err != nil {
		return nil, err
	}
	return a, nil
}

// pairs — строки .env в порядке install.sh (порядок важен для ключей,
// которых нет в .env.example: они дописываются в конец по очереди).
func (a *installAnswersT) pairs(name string) []envPair {
	p := []envPair{
		{"COMPOSE_PROJECT_NAME", name},
		{"CADENCE_MODE", a.mode},
		{"COMPOSE_FILE", a.composeFile},
	}
	if a.mode == "tls" {
		p = append(p, envPair{"SDLC_DOMAIN", a.domain}, envPair{"ACME_EMAIL", a.email})
	} else {
		p = append(p, envPair{"SDLC_DOMAIN", ""}, envPair{"ACME_EMAIL", ""}, envPair{"CADENCE_HTTP_ADDR", a.httpAddr})
	}
	if a.external {
		p = append(p, envPair{"CADENCE_DB", "external"}, envPair{"SDLC_DATABASE_URL", a.dbURL})
	} else {
		p = append(p, envPair{"CADENCE_DB", "bundled"})
	}
	return append(p,
		envPair{"POSTGRES_PASSWORD", a.postgresPassword},
		envPair{"SDLC_ADMIN_TOKEN", a.token},
		envPair{"SDLC_GITHUB_WEBHOOK_SECRET", a.webhook},
		envPair{"SDLC_BOOTSTRAP_TEAM", a.team},
		envPair{"SDLC_BOOTSTRAP_TEAM_NAME", a.teamName},
	)
}

// asker — вопросы install.sh: «вопрос [текущее]: », пустая строка оставляет
// текущее значение (или умолчание). Без терминала — сразу оно.
type asker struct {
	e           *serverEnv
	interactive bool
	r           *bufio.Reader
}

func (q *asker) ask(question, current, def string) (string, error) {
	if current == "" {
		current = def
	}
	if !q.interactive {
		return current, nil
	}
	if q.r == nil {
		q.r = bufio.NewReader(q.e.in)
	}
	fmt.Fprintf(q.e.out, "%s [%s]: ", question, current)
	line, err := q.r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if line = strings.TrimRight(line, "\r\n"); line == "" {
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(q.e.out)
		}
		return current, nil
	}
	return line, nil
}

// parseHTTPAddr — [ip:]port из --http-addr; хост для проверки порта: пустой
// и 0.0.0.0 — 127.0.0.1, как у install.sh.
func parseHTTPAddr(addr string) (string, int, error) {
	host, port := "", addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host, port = addr[:i], addr[i+1:]
	}
	n, err := strconv.Atoi(port)
	if err != nil || port == "" || strings.Trim(port, "0123456789") != "" || n < 1 || n > 65535 {
		return "", 0, fmt.Errorf("--http-addr '%s' is not [ip:]port", addr)
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return host, n, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("could not generate a random secret: %v", err)
	}
	return hex.EncodeToString(b), nil
}

// ── проверки машины ─────────────────────────────────────────────────────────

// portBusy — на порту уже кто-то слушает: к нему подключаются, или его
// нельзя занять (EADDRINUSE). Привилегии не нужны: EACCES на порту ниже
// 1024 у обычного пользователя — не «занят» (ss у install.sh не биндится,
// а Docker публикует порт от root). host пуст — все адреса машины.
func portBusy(dial func(addr string) error, listen func(addr string) error, host string, port int) bool {
	dialHost := host
	if dialHost == "" {
		dialHost = "127.0.0.1"
	}
	if dial(net.JoinHostPort(dialHost, strconv.Itoa(port))) == nil {
		return true
	}
	err := listen(net.JoinHostPort(host, strconv.Itoa(port)))
	return err != nil && errors.Is(err, syscall.EADDRINUSE)
}

func realPortBusy(host string, port int) bool {
	return portBusy(
		func(addr string) error {
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				c.Close()
			}
			return err
		},
		func(addr string) error {
			l, err := net.Listen("tcp", addr)
			if err == nil {
				l.Close()
			}
			return err
		},
		host, port)
}

// checkDomain — предупреждения install.sh про DNS: сертификат выдаётся,
// только если домен указывает на эту машину. Это не отказ: за NAT адреса
// и должны различаться.
//
// dir — каталог установки: под обёрткой пакета подсказка называет его
// install.sh (CLAB-242).
func checkDomain(e *serverEnv, domain, dir string) {
	resolved, err := e.lookupHost(domain)
	if err != nil || len(resolved) == 0 {
		install := "clab server install"
		if e.wrapped() {
			install = e.script(dir, "install.sh")
		}
		e.warn("%s does not resolve. Create a DNS record pointing at this machine before the first start,", domain)
		e.warn("or install in proxy mode behind your reverse proxy: %s --mode proxy", install)
		return
	}
	local := e.localAddrs()
	for _, ip := range resolved {
		for _, l := range local {
			if ip == l {
				return
			}
		}
	}
	known := strings.Join(local, " ")
	if known == "" {
		known = "unknown"
	}
	e.warn("%s resolves to %s which is not an address of this machine (%s).", domain, strings.Join(resolved, " "), known)
	e.warn("Fine behind NAT with ports 80/443 forwarded here; otherwise Let's Encrypt will fail - consider --mode proxy.")
}

func realLocalAddrs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() {
			out = append(out, n.IP.String())
		}
	}
	return out
}

// clabPath — путь этого clab для строки cron: cron не знает PATH человека.
// Brew ставит бинарь в Cellar/<версия>, который меняется с обновлением, —
// тогда берётся стабильная ссылка из PATH, если она ведёт на этот же бинарь.
func clabPath() string {
	self, err := executablePath()
	if err != nil {
		return "clab"
	}
	if p, err := exec.LookPath("clab"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved == self {
				return abs
			}
		}
	}
	return self
}

// ── файлы ───────────────────────────────────────────────────────────────────

type envPair struct{ key, value string }

// renderEnv — set_kv install.sh над шаблоном: строка, которая начинается с
// KEY=, заменяется на KEY=значение, иначе пара дописывается в конец; значение
// с символами вне [A-Za-z0-9_.,:/@+=-] — в одинарных кавычках, которые
// одинаково читают compose, shell и parseDotEnv. Как awk, результат
// заканчивается переводом строки. Значения с ' и переводом строки
// проверяются до записи.
func renderEnv(template string, pairs []envPair) string {
	var lines []string
	if template != "" {
		lines = strings.Split(strings.TrimSuffix(template, "\n"), "\n")
	}
	for _, p := range pairs {
		v := p.value
		if strings.Trim(v, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.,:/@+=-") != "" {
			v = "'" + v + "'"
		}
		line, done := p.key+"="+v, false
		for i, l := range lines {
			if strings.HasPrefix(l, p.key+"=") {
				lines[i], done = line, true
			}
		}
		if !done {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// overrideTemplate — compose.override.yml, который пишет install.sh.
const overrideTemplate = `# Local changes of the compose configuration (docs/admin.md, "Local compose
# changes"). COMPOSE_FILE in .env lists this file; upgrade.sh never touches it.
#
# services:
#   api:
#     extra_hosts: ["host.docker.internal:host-gateway"]
services: {}
`

// writeInstallFiles — .env из .env.example (временный файл 0600 и
// переименование: обрыв не оставит половину .env), compose.override.yml,
// если его нет, backups/ 0700 и secrets/ 0711 (сервис с uid 65532 проходит
// в secrets/ за ключом GitHub App, листать его не нужно).
func writeInstallFiles(dir string, pairs []envPair) error {
	tmpl, err := os.ReadFile(filepath.Join(dir, ".env.example"))
	if err != nil {
		return fmt.Errorf("no .env.example in %s: not a package of Cadence Lab", dir)
	}
	tmp := filepath.Join(dir, ".env.tmp")
	if err := os.WriteFile(tmp, []byte(renderEnv(string(tmpl), pairs)), 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, ".env")); err != nil {
		os.Remove(tmp)
		return err
	}
	override := filepath.Join(dir, "compose.override.yml")
	if _, err := os.Lstat(override); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(override, []byte(overrideTemplate), 0o644); err != nil {
			return err
		}
		if err := os.Chmod(override, 0o644); err != nil {
			return err
		}
	}
	for d, mode := range map[string]os.FileMode{"backups": 0o700, "secrets": 0o711} {
		p := filepath.Join(dir, d)
		if err := os.MkdirAll(p, mode); err != nil {
			return err
		}
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
	}
	return nil
}

// ── внешняя база ────────────────────────────────────────────────────────────

// externalDB — --external-db и --external-db-ca, как их разбирает install.sh:
// адрес postgres://, CA — PEM-файл, который ляжет в secrets/db-ca.pem и
// попадёт в адрес как sslrootcert с sslmode=verify-full, если адрес не
// задаёт свой. Ничего не пишет: файлы — после распаковки, перед .env.
func (a *installAnswersT) externalDB(e *serverEnv, rawURL, ca string) error {
	if ca != "" && rawURL == "" {
		return errors.New("--external-db-ca needs --external-db URL")
	}
	if rawURL == "" {
		return nil
	}
	if !strings.HasPrefix(rawURL, "postgres://") && !strings.HasPrefix(rawURL, "postgresql://") {
		return errors.New("--external-db must be a URL: postgres://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=verify-full (the key=value form is not supported)")
	}
	if strings.ContainsAny(rawURL, "'\n") {
		return errors.New("--external-db must not contain a single quote or a line break: percent-encode it (%27)")
	}
	if ca != "" {
		if !filepath.IsAbs(ca) {
			ca = filepath.Join(e.cwd, ca)
		}
		body, err := os.ReadFile(ca)
		if err != nil {
			return fmt.Errorf("--external-db-ca: no readable file %s", ca)
		}
		if !strings.Contains(string(body), "BEGIN CERTIFICATE") {
			return fmt.Errorf("--external-db-ca: %s is not a PEM certificate", ca)
		}
		if strings.Contains(rawURL, "?sslrootcert=") || strings.Contains(rawURL, "&sslrootcert=") {
			return errors.New("the CA is given twice: --external-db-ca and sslrootcert in --external-db; keep one")
		}
		rawURL = urlWithParam(rawURL, "sslrootcert", "/secrets/"+externalDBCAFile)
		if !strings.Contains(rawURL, "?sslmode=") && !strings.Contains(rawURL, "&sslmode=") {
			rawURL = urlWithParam(rawURL, "sslmode", "verify-full")
		}
	}
	a.external, a.dbURL, a.dbCA = true, rawURL, ca
	a.composeFile = "docker-compose.yml:compose." + a.mode + ".yml:" + externalDBFile + ":compose.override.yml"
	return nil
}

// urlWithParam — url_with_param install.sh: KEY=VALUE в конец запроса.
func urlWithParam(raw, key, value string) string {
	if strings.Contains(raw, "?") {
		return raw + "&" + key + "=" + value
	}
	return raw + "?" + key + "=" + value
}

// externalPrecheck — external_precheck install.sh: CA в secrets/, затем база
// глазами сервиса (/api dbcheck --install образа версии) и клиента бэкапа
// (psql db-client). .env ещё нет, поэтому настройки — окружением; сеть и
// тома, которые compose заводит под запуск, после него убираются.
func externalPrecheck(ctx context.Context, e *serverEnv, dir, version, name string, a *installAnswersT) error {
	secrets := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secrets, 0o711); err != nil {
		return err
	}
	if err := os.Chmod(secrets, 0o711); err != nil {
		return err
	}
	if a.dbCA != "" {
		body, err := os.ReadFile(a.dbCA)
		if err != nil {
			return err
		}
		// Сервис работает от uid 65532: CA должен читаться им.
		target := filepath.Join(secrets, externalDBCAFile)
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
		if err := os.Chmod(target, 0o644); err != nil {
			return err
		}
	}
	probe := &installation{e: e, dir: dir, version: version, hasEnv: true, env: map[string]string{
		"COMPOSE_PROJECT_NAME": name,
		"COMPOSE_FILE":         "docker-compose.yml:" + externalDBFile,
		"CADENCE_DB":           "external",
		"SDLC_DATABASE_URL":    a.dbURL,
		"POSTGRES_PASSWORD":    "",
	}}
	// Запуск завёл сеть и тома api под именем этой установки, свободным
	// мгновение назад: отказ не должен оставить их и сделать имя занятым.
	defer func() { _, _ = output(ctx, e.run, probe.compose("down", "-v", "--remove-orphans")) }()

	e.info("checking the external database %s", dbTarget(a.dbURL))
	again := "clab server install --dir " + shellQuote(dir)
	if e.wrapped() {
		again = e.script(dir, "install.sh")
	}
	notReady := fmt.Errorf("the external database is not ready (see above). .env was not written:\n"+
		"  fix the database or the URL and run %s again with the same flags (docs/admin.md, \"External PostgreSQL\")", again)
	if err := probe.dbCheck(ctx, probe.compose("run", "--rm", "--no-deps", "-T", "api", "dbcheck", "--install")); err != nil {
		return notReady
	}
	e.info("checking the backup client (pg_dump and psql of db-client) with the same URL")
	if err := probe.dbClientCheck(ctx); err != nil {
		e.warn("the backup client (libpq) cannot connect with SDLC_DATABASE_URL, though the service can: see its error above")
		return notReady
	}
	e.ok("the backup client connects")
	return nil
}
