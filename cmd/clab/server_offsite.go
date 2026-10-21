package main

// Копия бэкапов вне машины и проверка восстановления (CLAB-132): выгрузка
// в бакет BACKUP_S3_*, скачивание оттуда (fetch) и verify — загрузка
// бэкапа в одноразовый PostgreSQL рядом со стеком. Правила и тексты — те же,
// что у backup.sh и lib.sh пакета (раздел «off-machine backups»): их читают
// cron клиентов и проверки ci/ в обоих исполнителях. S3 — подкоманда образа
// бэкенда `/api backup-s3`: у машины клиента нет ни aws, ни rclone.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/secrets"
)

// offsiteSince — первая версия, в образе которой есть /api backup-s3.
const offsiteSince = "v0.10.0"

// backupS3Vars — переменные бакета. В docker compose run они уходят
// именами (-e NAME), значения — окружением процесса docker: командную
// строку видит любой пользователь машины (ps). Ключи — из перечня
// секретов в обеих формах (решение 0052): путь в *_FILE — путь в
// контейнере, файл читает /api backup-s3 от имени того, кто запустил.
var backupS3Vars = append(append([]string{"BACKUP_S3_BUCKET", "BACKUP_S3_ENDPOINT", "BACKUP_S3_REGION", "BACKUP_S3_PREFIX"},
	secretsOf("backup-s3")...), "BACKUP_S3_PATH_STYLE", "BACKUP_S3_KEEP")

// secretsOf — имена секретов читателя reader в обеих формах.
func secretsOf(reader string) []string {
	var names []string
	for _, v := range secrets.List {
		if v.ReadBy(reader) {
			if n := v.ValueEnv(); n != "" {
				names = append(names, n)
			}
			names = append(names, v.FileEnv())
		}
	}
	return names
}

// offsiteConfigured — lib.sh backup_s3_configured.
func (i *installation) offsiteConfigured() bool { return i.get("BACKUP_S3_BUCKET") != "" }

// offsiteLocation — lib.sh backup_s3_location: s3://bucket/prefix/.
func (i *installation) offsiteLocation() string {
	prefix := strings.Trim(strings.TrimSpace(i.get("BACKUP_S3_PREFIX")), "/")
	if prefix != "" {
		prefix += "/"
	}
	return "s3://" + i.get("BACKUP_S3_BUCKET") + "/" + prefix
}

// offsiteSupported — образ этой установки умеет /api backup-s3.
func (i *installation) offsiteSupported() error {
	if versionLess(i.version, offsiteSince) {
		return fmt.Errorf("off-machine backups (BACKUP_S3_BUCKET) need Cadence Lab %s or newer; this installation is on %s. Upgrade it first",
			offsiteSince, i.version)
	}
	return nil
}

// backupS3 — lib.sh backup_s3: /api backup-s3 образа установки в
// одноразовом контейнере, каталог dir — как /backups, от имени того, кто
// запустил (бэкапы — 600 этого пользователя, а образ — nonroot).
func (i *installation) backupS3(dir string, args ...string) cmdSpec {
	a := []string{"run", "--rm", "--no-deps", "-T", "--user", fmt.Sprintf("%d:%d", i.e.uid, i.e.gid)}
	for _, v := range backupS3Vars {
		a = append(a, "-e", v)
	}
	a = append(a, "-v", dir+":/backups", "api", "backup-s3")
	return i.compose(append(a, args...)...)
}

// upload — lib.sh backup_upload: копия дампа (и архива blobs рядом) в
// бакет и хранение BACKUP_S3_KEEP. Локальный бэкап при сбое остаётся.
func (i *installation) upload(ctx context.Context, dump string) error {
	name := filepath.Base(dump)
	if err := i.offsiteSupported(); err != nil {
		return fmt.Errorf("%v; the local backup is kept: %s", err, dump)
	}
	i.e.info("uploading %s to %s", name, i.offsiteLocation())
	c := i.backupS3(filepath.Dir(dump), "put", "/backups/"+name)
	c.stdout, c.stderr = i.e.out, i.e.errw
	if err := i.e.run.run(ctx, c); err != nil {
		return fmt.Errorf("the off-machine copy of %s to %s failed (see above); the local backup is kept: %s",
			name, i.offsiteLocation(), dump)
	}
	return nil
}

// exitCode — код выхода внешней команды; -1, если она не запустилась.
func exitCode(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// fetch — backup.sh fetch: бэкап из бакета (по имени или самый новый) в
// каталог бэкапов, со сверкой сумм.
func (i *installation) fetch(ctx context.Context, name string) error {
	if !i.offsiteConfigured() {
		return errors.New("BACKUP_S3_BUCKET is not set in .env: there is no bucket to fetch from (docs/admin.md, \"Off-machine copy\")")
	}
	if err := i.offsiteSupported(); err != nil {
		return err
	}
	dir := i.path(i.backupsDir())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	args := []string{"get", "--dir", "/backups"}
	if name != "" {
		args = append(args, name)
	}
	c := i.backupS3(dir, args...)
	got, err := output(ctx, i.e.run, withStderr(c, i.e.errw))
	if err != nil {
		if exitCode(err) == 3 {
			return fmt.Errorf("nothing to fetch from %s", i.offsiteLocation())
		}
		return fmt.Errorf("fetching from %s failed (see above)", i.offsiteLocation())
	}
	dump := filepath.Join(dir, firstLine(got))
	restore := "clab server restore"
	if i.e.wrapped() {
		restore = i.e.script(i.dir, "restore.sh")
	}
	i.e.ok("%s; restore it with: %s --yes %s", i.rel(dump), restore, shellQuote(i.rel(dump)))
	return nil
}

func withStderr(c cmdSpec, w io.Writer) cmdSpec {
	c.stderr = w
	return c
}

// ── verify ──────────────────────────────────────────────────────────────────

// dbcheckReport — то, что verify читает из `/api dbcheck --json`.
type dbcheckReport struct {
	OK                 bool   `json:"ok"`
	Schema             string `json:"schema"`
	SchemaVersion      int64  `json:"schema_version"`
	ImageSchemaVersion int64  `json:"image_schema_version"`
}

// verifyCountsSQL — что verify считает в загруженной базе.
const verifyCountsSQL = "SELECT (SELECT count(*) FROM projects) || ' ' || (SELECT count(*) FROM tasks) || ' ' || (SELECT count(*) FROM artifacts)"

// verify — backup.sh verify: бэкап (FILE, иначе самый новый этой версии из
// бакета, иначе из каталога бэкапов) загружается в одноразовый PostgreSQL
// образа установки, схема сверяется /api dbcheck образа этой версии, строки
// считаются. Рабочая база и стек не трогаются; одноразовые контейнер, его
// том и сеть удаляются при любом исходе.
func (i *installation) verify(ctx context.Context, file string, ignoreVersion bool) error {
	e := i.e
	if err := i.checkDBMode(); err != nil {
		return err
	}
	dir := i.path(i.backupsDir())
	var dump, from string
	switch {
	case file != "":
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(e.cwd, path)
			if !isFile(path) && isFile(i.path(file)) {
				path = i.path(file)
			}
		}
		if !isFile(path) {
			return fmt.Errorf("no such file: %s", file)
		}
		if !strings.HasSuffix(path, ".sql.gz") {
			return fmt.Errorf("%s is not a database dump (*.sql.gz) made by backup", file)
		}
		dump, from = path, i.rel(path)
	case i.offsiteConfigured():
		if err := i.offsiteSupported(); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		tmp, err := os.MkdirTemp(dir, ".verify-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		got, err := output(ctx, e.run, withStderr(i.backupS3(tmp, "get", "--dir", "/backups", "--version", i.version), e.errw))
		switch {
		case err == nil:
			dump, from = filepath.Join(tmp, firstLine(got)), i.offsiteLocation()
		case exitCode(err) == 3:
			e.warn("no backup of %s in %s: verifying the newest local one", i.version, i.offsiteLocation())
		default:
			return fmt.Errorf("fetching the backup from %s failed (see above)", i.offsiteLocation())
		}
	}
	if dump == "" {
		matches, _ := filepath.Glob(filepath.Join(dir, "cadence-"+i.version+"-*.sql.gz"))
		sort.Strings(matches)
		if len(matches) == 0 {
			where := i.rel(dir) + "/"
			if i.offsiteConfigured() {
				where = i.offsiteLocation() + " or " + where
			}
			return fmt.Errorf("nothing to verify: no backup of %s in %s", i.version, where)
		}
		dump = matches[len(matches)-1]
		from = i.rel(dump)
	}
	name := filepath.Base(dump)

	backupVersion := ""
	if m := backupName.FindStringSubmatch(name); m != nil {
		backupVersion = m[1]
	}
	if backupVersion != i.version && !ignoreVersion {
		if backupVersion == "" {
			return fmt.Errorf("cannot tell the version of %s (expected cadence-vX.Y.Z-YYYYmmdd-HHMMSS.sql.gz).\n"+
				"  Pass --ignore-version if you are sure it was taken on %s.", name, i.version)
		}
		return fmt.Errorf("the backup was taken on %s, the package is on %s.\n"+
			"  Pass --ignore-version to check it with the images of %s, or put the package files of %s in place first:\n%s    %s",
			backupVersion, i.version, i.version, backupVersion, i.cdHint(), i.packageFilesHint(ctx, backupVersion))
	}

	fi, err := os.Stat(dump)
	if err != nil {
		return err
	}
	e.info("verifying %s (%s) from %s", name, humanSize(fi.Size()), from)

	service := "postgres"
	if i.externalDB() {
		service = "db-client"
	}
	pgImage, err := i.imageOf(ctx, service)
	if err != nil {
		return err
	}
	apiImage, err := i.imageOf(ctx, "api")
	if err != nil {
		return err
	}
	project, _, _ := i.name()
	suffix := make([]byte, 4)
	rand.Read(suffix)
	scratch := project + "-verify-" + hex.EncodeToString(suffix)
	label := "com.cadence.verify=" + project

	cleanup := func() {
		ctx := context.WithoutCancel(ctx)
		_, _ = output(ctx, e.run, i.docker("rm", "-f", "-v", scratch))
		_, _ = output(ctx, e.run, i.docker("network", "rm", scratch))
	}
	defer cleanup()

	e.info("loading it into a throwaway PostgreSQL (%s); the stack keeps running", pgImage)
	if _, err := output(ctx, e.run, withStderr(i.docker("network", "create", "--label", label, scratch), e.errw)); err != nil {
		return fmt.Errorf("docker network create %s failed (see above)", scratch)
	}
	if _, err := output(ctx, e.run, withStderr(i.docker("run", "-d", "--name", scratch, "--network", scratch, "--label", label,
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-e", "POSTGRES_DB=verify", pgImage), e.errw)); err != nil {
		return fmt.Errorf("starting the throwaway PostgreSQL failed (see above)")
	}
	psql := func(args ...string) cmdSpec {
		return i.docker(append([]string{"exec", scratch, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "verify"}, args...)...)
	}
	// По TCP: во время инициализации образ поднимает сервер только на сокете.
	ready := false
	for n := 0; n < 60 && !ready; n++ {
		if _, err := output(ctx, e.run, i.docker("exec", scratch, "pg_isready", "-q", "-h", "127.0.0.1", "-U", "postgres", "-d", "verify")); err == nil {
			ready = true
			break
		}
		e.sleep(time.Second)
	}
	if !ready {
		return errors.New("the throwaway PostgreSQL did not become ready")
	}
	if _, err := output(ctx, e.run, withStderr(psql("-c", "CREATE EXTENSION IF NOT EXISTS vector", "-c", "CREATE EXTENSION IF NOT EXISTS pgcrypto"), e.errw)); err != nil {
		return errors.New("creating the extensions in the throwaway PostgreSQL failed (see above)")
	}

	f, err := os.Open(dump)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s is not a valid gzip archive", name)
	}
	start := e.now()
	load := i.docker("exec", "-i", scratch, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "verify")
	load.stdin, load.stdout, load.stderr = gz, io.Discard, e.errw
	if err := e.run.run(ctx, load); err != nil {
		return fmt.Errorf("backup %s does not restore: loading it failed (the error of psql or gzip is above)", name)
	}
	e.ok("loaded in %ds", int(e.now().Sub(start).Round(time.Second)/time.Second))

	url := "postgres://postgres@" + scratch + ":5432/verify?sslmode=disable"
	check := i.docker("run", "--rm", "--network", scratch, "-e", "SDLC_DATABASE_URL", apiImage, "dbcheck", "--json")
	check.env = append(check.env, "SDLC_DATABASE_URL="+url)
	raw, err := output(ctx, e.run, check)
	var report dbcheckReport
	if jerr := json.Unmarshal([]byte(raw), &report); jerr != nil || err != nil || !report.OK || report.Schema != "cadence" {
		// Подробности — тем же dbcheck по-человечески.
		human := i.docker("run", "--rm", "--network", scratch, "-e", "SDLC_DATABASE_URL", apiImage, "dbcheck")
		human.env = check.env
		human.stdout, human.stderr = e.out, e.errw
		_ = e.run.run(ctx, human)
		return fmt.Errorf("backup %s does not restore: the images of %s do not accept its schema (see above)", name, i.version)
	}
	switch {
	case report.SchemaVersion == report.ImageSchemaVersion:
		e.ok("schema %d: the schema of the images of %s", report.SchemaVersion, i.version)
	case ignoreVersion:
		e.warn("schema %d: the images of %s apply up to %d (--ignore-version)", report.SchemaVersion, i.version, report.ImageSchemaVersion)
	default:
		return fmt.Errorf("backup %s does not match its version: schema %d, the images of %s apply up to %d", name, report.SchemaVersion, i.version, report.ImageSchemaVersion)
	}

	counts, err := output(ctx, e.run, withStderr(i.docker("exec", scratch, "psql", "-X", "-q", "-At", "-U", "postgres", "-d", "verify", "-c", verifyCountsSQL), e.errw))
	var projects, tasks, artifacts int
	if _, serr := fmt.Sscanf(strings.TrimSpace(counts), "%d %d %d", &projects, &tasks, &artifacts); err != nil || serr != nil {
		return fmt.Errorf("backup %s does not restore: counting its projects, tasks and artifacts failed (see above)", name)
	}

	blobs := strings.TrimSuffix(dump, ".sql.gz") + ".blobs.tar.gz"
	if isFile(blobs) {
		n, err := tarFiles(blobs)
		if err != nil {
			return fmt.Errorf("the blobs archive %s is damaged: %v", filepath.Base(blobs), err)
		}
		e.ok("blobs archive %s: %d files", filepath.Base(blobs), n)
	} else if i.get("SDLC_BLOB_S3_BUCKET") == "" {
		e.warn("no %s next to the dump: this backup has no blobs volume", filepath.Base(blobs))
	}
	e.ok("backup %s restores: schema %d, %d projects, %d tasks, %d artifacts", name, report.SchemaVersion, projects, tasks, artifacts)
	return nil
}

// imageOf — образ сервиса compose установки. Не `config --images SERVICE`:
// он добавляет сервисы, от которых SERVICE зависит, в случайном порядке.
func (i *installation) imageOf(ctx context.Context, service string) (string, error) {
	out, err := output(ctx, i.e.run, withStderr(i.compose("config", "--format", "json", service), i.e.errw))
	var cfg struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err != nil || json.Unmarshal([]byte(out), &cfg) != nil || cfg.Services[service].Image == "" {
		return "", fmt.Errorf("cannot tell the image of %s: docker compose config %s failed (see above)", service, service)
	}
	return cfg.Services[service].Image, nil
}

// tarFiles — число обычных файлов в архиве; архив читается до конца, как
// tar -tzf.
func tarFiles(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if h.Typeflag == tar.TypeReg {
			n++
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return n, err
		}
	}
}
