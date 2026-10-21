package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const restoreHelp = `clab server restore — restore the database (and the blobs volume, when the
backup has it) from a backup made by clab server backup or backup.sh, then
start the stack and wait for it.

Usage: clab server restore [--dir DIR] [--yes] [--ignore-version] FILE

  clab server restore backups/cadence-v0.6.1-20261008-030000.sql.gz
  clab server restore --yes backups/cadence-v0.6.1-20261008-030000.sql.gz   # no prompt

Options:
  --dir DIR          the installation directory (default: $CADENCE_DIR, else the
                     current directory or the nearest one above it)
  -y, --yes          do not ask for confirmation (required without a terminal)
  --ignore-version   restore a backup of another version than VERSION

EVERYTHING written after the backup is lost: the database is dropped and
recreated from the dump. With an external database (CADENCE_DB=external)
nothing is dropped but the objects of Cadence Lab in the schema public: they
go and the dump is loaded in one transaction, by the role of
SDLC_DATABASE_URL - no superuser, no DROP DATABASE; a failed load leaves the
database as it was.

The backup must match the version in VERSION: migrations only go forward,
and a dump of one schema under images of another version is not a state
anybody tested. When they differ, put the package files of the version of
the backup in place first - the command is printed: the copy upgrade kept in
backups/package-vX.Y.Z-<date>.tar.gz, or the package of the release - or
pass --ignore-version if you know what you are doing.

  CADENCE_READY_TIMEOUT  seconds to wait for readiness, default 300`

var backupName = regexp.MustCompile(`^cadence-(v[0-9]+\.[0-9]+\.[0-9]+)-[0-9]{8}-[0-9]{6}\.sql\.gz$`)

func serverRestore(ctx context.Context, e *serverEnv, args []string) error {
	var dir string
	var yes, ignoreVersion bool
	pos, err := parseServerArgs(args,
		map[string]*bool{"-y": &yes, "--yes": &yes, "--ignore-version": &ignoreVersion},
		map[string]*string{"--dir": &dir})
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 0:
		cmd := "clab server restore"
		if e.wrapped() {
			// Каталог — только для пути к скрипту; не нашёлся — ./restore.sh.
			where, _ := findInstallation(e, dir)
			cmd = e.script(where, "restore.sh")
		}
		return usageError{"which backup? " + cmd + " backups/cadence-<version>-<date>.sql.gz"}
	case len(pos) > 1:
		return usageError{"one backup file at a time"}
	}
	file := pos[0]
	inst, err := openInstallation(e, dir)
	if err != nil {
		return err
	}
	// Файл — относительно текущего каталога, как у любой команды; скрипт
	// делал cd в каталог установки, поэтому и его путь проверяется там.
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.cwd, path)
		if !isFile(path) && isFile(inst.path(file)) {
			path = inst.path(file)
		}
	}
	if !isFile(path) {
		return fmt.Errorf("no such file: %s", file)
	}
	if !strings.HasSuffix(path, ".sql.gz") {
		return fmt.Errorf("%s is not a database dump (*.sql.gz) made by backup", file)
	}
	if err := gzipValid(path); err != nil {
		return fmt.Errorf("%s is not a valid gzip archive", file)
	}

	if err := inst.open(ctx); err != nil {
		return err
	}
	if err := inst.checkMode(); err != nil {
		return err
	}

	// Версия — часть имени, которое даёт файлу backup.
	name := filepath.Base(path)
	backupVersion := ""
	if m := backupName.FindStringSubmatch(name); m != nil {
		backupVersion = m[1]
	}
	if backupVersion != inst.version && !ignoreVersion {
		if backupVersion == "" {
			return fmt.Errorf("cannot tell the version of %s (expected cadence-vX.Y.Z-YYYYmmdd-HHMMSS.sql.gz).\n"+
				"  Pass --ignore-version if you are sure it was taken on %s.", name, inst.version)
		}
		return fmt.Errorf("the backup was taken on %s, the package is on %s.\n"+
			"  Put the package files of %s in place first, then restore:\n%s    %s && %s %s",
			backupVersion, inst.version, backupVersion, inst.cdHint(),
			inst.packageFilesHint(ctx, backupVersion), restoreCommand(backupVersion, e.wrapped()), shellQuote(inst.rel(path)))
	}

	blobs := strings.TrimSuffix(path, ".sql.gz") + ".blobs.tar.gz"
	if !isFile(blobs) {
		blobs = ""
		if inst.get("SDLC_BLOB_S3_BUCKET") == "" {
			e.warn("no %s.blobs.tar.gz next to the dump: the blobs volume is left as it is", strings.TrimSuffix(name, ".sql.gz"))
		}
	}

	if !yes {
		if !e.terminal {
			return errors.New("restore replaces the database; run it in a terminal or pass --yes")
		}
		what := "the database"
		if blobs != "" {
			what += " and the blobs volume"
		}
		e.say("This REPLACES %s with %s.", what, name)
		e.say("Everything written after the backup is lost.")
		fmt.Fprint(e.out, "Type 'restore' to continue: ")
		answer, _ := bufio.NewReader(e.in).ReadString('\n')
		if strings.TrimSpace(answer) != "restore" {
			return errors.New("cancelled")
		}
	}
	return inst.restore(ctx, path, blobs)
}

// gzipValid — gzip -t: архив читается до конца.
func gzipValid(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	_, err = io.Copy(io.Discard, gz)
	return err
}

// restore — restore.sh после проверок: база заново из дампа, том blobs из
// архива, старт и ожидание готовности.
func (i *installation) restore(ctx context.Context, dump, blobs string) error {
	e := i.e
	if i.externalDB() {
		// До остановки: недоступная база оставляет стек работать.
		e.info("checking the external database %s", i.externalTarget())
		if err := i.dbClientCheck(ctx); err != nil {
			return errors.New("the backup client cannot connect to the external database (see above); nothing was changed")
		}
	}
	e.info("stopping api and worker")
	if err := i.composeRun(ctx, true, "stop", "api", "worker"); err != nil {
		return fmt.Errorf("docker compose stop api worker failed: %v", err)
	}
	if i.externalDB() {
		if err := i.loadExternal(ctx, dump); err != nil {
			return err
		}
	} else if err := i.loadBundled(ctx, dump); err != nil {
		return err
	}
	return i.restoreBlobsAndStart(ctx, dump, blobs)
}

// loadExternal — дамп во внешнюю базу одной транзакцией: сначала
// dbResetSQL, затем сам дамп.
func (i *installation) loadExternal(ctx context.Context, dump string) error {
	e := i.e
	e.info("loading %s into %s (one transaction)", filepath.Base(dump), i.externalTarget())
	safe, password, err := i.dbConn()
	if err != nil {
		return err
	}
	f, err := os.Open(dump)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	c := i.dbClient(password, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction", "-d", safe)
	c.stdin = io.MultiReader(strings.NewReader(dbResetSQL), filterTransactionTimeout(gz))
	c.stdout, c.stderr = io.Discard, e.errw
	if err := e.run.run(ctx, c); err != nil {
		return fmt.Errorf("loading %s failed (see above); the transaction is rolled back and the database is as it was.\n"+
			"  Start the stack again: export CADENCE_VERSION=$(cat VERSION) && docker compose up -d", filepath.Base(dump))
	}
	return nil
}

// loadBundled — restore.sh для контейнера postgres: база заново, расширения,
// дамп.
func (i *installation) loadBundled(ctx context.Context, dump string) error {
	e := i.e
	db := i.getDefault("POSTGRES_DB", "sdlc")
	user := i.getDefault("POSTGRES_USER", "sdlc")
	e.info("starting postgres")
	if err := i.composeRun(ctx, true, "up", "-d", "postgres"); err != nil {
		return fmt.Errorf("docker compose up -d postgres failed: %v", err)
	}
	ready := func() bool {
		_, err := output(ctx, e.run, i.compose("exec", "-T", "postgres", "pg_isready", "-U", user, "-d", "postgres"))
		return err == nil
	}
	isReady := false
	for n := 0; n < 60; n++ {
		if isReady = ready(); isReady {
			break
		}
		e.sleep(2 * time.Second)
	}
	if !isReady && !ready() {
		return errors.New("postgres did not become ready")
	}

	e.info("recreating database %s", db)
	c := i.compose("exec", "-T", "postgres", "psql", "-v", "ON_ERROR_STOP=1", "-q", "-U", user, "-d", "postgres",
		"-c", fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, db),
		"-c", fmt.Sprintf(`CREATE DATABASE "%s" OWNER "%s"`, db, user))
	c.stdout, c.stderr = io.Discard, e.errw
	if err := e.run.run(ctx, c); err != nil {
		return fmt.Errorf("recreating database %s failed: %v", db, err)
	}
	// Дампы без расширений (dumpOptions): они создаются до загрузки. Дамп
	// прежних версий их несёт и пересоздаёт сам.
	c = i.compose("exec", "-T", "postgres", "psql", "-v", "ON_ERROR_STOP=1", "-q", "-U", user, "-d", db,
		"-c", "CREATE EXTENSION IF NOT EXISTS vector", "-c", "CREATE EXTENSION IF NOT EXISTS pgcrypto")
	c.stdout, c.stderr = io.Discard, e.errw
	if err := e.run.run(ctx, c); err != nil {
		return fmt.Errorf("creating the extensions in %s failed: %v", db, err)
	}

	e.info("loading %s", filepath.Base(dump))
	f, err := os.Open(dump)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	c = i.compose("exec", "-T", "postgres", "psql", "-v", "ON_ERROR_STOP=1", "-q", "-U", user, "-d", db)
	c.stdin, c.stdout, c.stderr = gz, io.Discard, e.errw
	if err := e.run.run(ctx, c); err != nil {
		return fmt.Errorf("loading %s failed: %v", filepath.Base(dump), err)
	}
	return nil
}

// restoreBlobsAndStart — том blobs из архива, старт и ожидание готовности.
func (i *installation) restoreBlobsAndStart(ctx context.Context, dump, blobs string) error {
	e := i.e
	if blobs != "" {
		api := i.containerOf(ctx, "api")
		if api == "" {
			return errors.New("the api container does not exist: cannot reach the blobs volume")
		}
		e.info("restoring the blobs volume from %s", filepath.Base(blobs))
		// Сервис работает как nonroot (uid 65532) и должен писать в том.
		c := i.docker("run", "--rm", "--volumes-from", api, "-v", filepath.Dir(blobs)+":/backup:ro", "alpine:3",
			"sh", "-c", fmt.Sprintf("find /blobs -mindepth 1 -delete && tar -xzf '/backup/%s' -C /blobs && chown -R 65532:65532 /blobs",
				filepath.Base(blobs)))
		c.stdout, c.stderr = e.out, e.errw
		if err := e.run.run(ctx, c); err != nil {
			return fmt.Errorf("restoring the blobs volume failed: %v", err)
		}
	}

	e.info("starting the stack")
	if err := i.composeRun(ctx, false, "up", "-d", "--remove-orphans"); err != nil {
		return fmt.Errorf("docker compose up failed: %v", err)
	}
	timeout, err := i.readyTimeout()
	if err != nil {
		return err
	}
	if !i.waitReady(ctx, timeout) {
		i.showFailure(ctx, "api")
		return errors.New("the stack did not become ready after the restore; the logs are above")
	}
	e.ok("restored %s; Cadence Lab %s is up", filepath.Base(dump), i.version)
	return nil
}
