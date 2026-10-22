package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const backupHelp = `clab server backup — back up the database and, when artifact content lives in
the blobs volume (SDLC_BLOB_S3_BUCKET is empty), that volume; rotate old backups;
copy the backup to an S3 bucket when BACKUP_S3_BUCKET is set. verify checks that
a backup restores, fetch downloads one from the bucket.

Usage: clab server backup [--dir DIR] [--no-upload]
       clab server backup verify [--dir DIR] [--ignore-version] [FILE]
       clab server backup fetch [--dir DIR] [NAME]

Options:
  --dir DIR          the installation directory (default: $CADENCE_DIR, else the
                     current directory or the nearest one above it)
  --no-upload        keep this backup on the machine only, even with BACKUP_S3_BUCKET
  --ignore-version   verify: check a backup of another version than VERSION

Files: backups/cadence-<version>-<YYYYmmdd-HHMMSS>.sql.gz (mode 600) and, next
to it, the same name with .blobs.tar.gz. The version in the name is what
restore checks. A dump smaller than 1 KB is a failure. The dump is the same in
both database modes: plain SQL without owners, privileges and extensions, so a
backup of the bundled database restores into an external one and back.

With an external database (CADENCE_DB=external) pg_dump runs in the backup
client db-client of compose.external-db.yml against SDLC_DATABASE_URL; the
postgres container is not needed.

Off-machine copy: with BACKUP_S3_BUCKET set, every backup goes to the bucket
too - the dump, the blobs archive and a .sha256 of both - and the bucket keeps
the newest BACKUP_S3_KEEP backups (backups before an upgrade count too).
Only objects named cadence-vX.Y.Z-<date>.* right under the prefix are ever
deleted. A failed upload exits with status 1 and keeps the local backup; the
backup of clab server upgrade only warns. The bucket is reached by /api
backup-s3 of the backend image (v0.10.0+), with the keys in its environment,
never on a command line.

verify loads a backup into a throwaway PostgreSQL next to the stack - the
running stack and its database are not touched - checks the schema with the
images of this version and prints the number of projects, tasks and
artifacts. It takes FILE, else the newest backup of this version in the
bucket, else the newest one in backups/. Exit status 0 - the backup
restores, 1 - it does not.

fetch downloads a backup from the bucket into backups/ - NAME
(cadence-vX.Y.Z-<date>), else the newest one - and checks its checksums;
restore it with clab server restore.

Environment (from the environment or .env):
  BACKUP_DIR                   where to write, default backups/ of the installation
  KEEP_DAYS                    how long to keep backups on this machine, default 14
  BACKUP_S3_BUCKET             the bucket of the off-machine copy; empty - none
  BACKUP_S3_ENDPOINT           e.g. https://s3.eu-central-1.amazonaws.com, default
                               the AWS endpoint of BACKUP_S3_REGION
  BACKUP_S3_REGION             default us-east-1
  BACKUP_S3_PREFIX             a "directory" in the bucket, e.g. cadence/
  BACKUP_S3_ACCESS_KEY_ID      the keys of the bucket
  BACKUP_S3_SECRET_ACCESS_KEY
  BACKUP_S3_PATH_STYLE         true (default) - endpoint/bucket/key; false - bucket.endpoint/key
  BACKUP_S3_KEEP               how many backups the bucket keeps, default 14

For cron:
  0 3 * * * clab server backup --dir /opt/cadence >> /opt/cadence/backups/backup.log 2>&1
  0 5 * * 0 clab server backup verify --dir /opt/cadence >> /opt/cadence/backups/backup.log 2>&1

Without BACKUP_S3_BUCKET backups stay on this machine: they protect against a
bad upgrade or a mistaken deletion, not against losing the machine.`

func serverBackup(ctx context.Context, e *serverEnv, args []string) error {
	var dir string
	var noUpload, ignoreVersion bool
	pos, err := parseServerArgs(args,
		map[string]*bool{"--no-upload": &noUpload, "--ignore-version": &ignoreVersion},
		map[string]*string{"--dir": &dir})
	if err != nil {
		return err
	}
	sub := ""
	if len(pos) > 0 {
		sub, pos = pos[0], pos[1:]
	}
	switch {
	case sub != "" && sub != "verify" && sub != "fetch":
		return usageError{fmt.Sprintf("unexpected argument: %s", sub)}
	case len(pos) > 1:
		return usageError{fmt.Sprintf("unexpected argument: %s", pos[1])}
	case sub != "verify" && ignoreVersion:
		return usageError{"--ignore-version is an option of verify"}
	case sub != "" && noUpload:
		return usageError{"--no-upload is an option of the backup itself"}
	}
	arg := ""
	if len(pos) == 1 {
		arg = pos[0]
	}
	inst, err := openInstallation(e, dir)
	if err != nil {
		return err
	}
	if err := inst.open(ctx); err != nil {
		return err
	}
	switch sub {
	case "verify":
		return inst.verify(ctx, arg, ignoreVersion)
	case "fetch":
		return inst.fetch(ctx, arg)
	}
	dump, err := inst.backup(ctx)
	if err != nil || noUpload || !inst.offsiteConfigured() {
		return err
	}
	return inst.upload(ctx, dump)
}

// minDumpBytes — дамп меньше этого — ошибка, которая иначе всплыла бы только
// при восстановлении.
const minDumpBytes = 1024

// backup — backup.sh целиком: дамп внутри контейнера базы, архив тома blobs,
// ротация. Возвращает путь к дампу; upgrade зовёт её в процессе, а не
// запуском скрипта.
func (i *installation) backup(ctx context.Context) (string, error) {
	keepDays, err := strconv.Atoi(i.getDefault("KEEP_DAYS", "14"))
	if err != nil || keepDays < 0 {
		return "", fmt.Errorf("KEEP_DAYS is '%s', expected a number of days", i.get("KEEP_DAYS"))
	}
	dir := i.path(i.getDefault("BACKUP_DIR", "backups"))
	// Бэкапы и .env — секреты: ничего из созданного не предназначено другим
	// пользователям машины (umask 077 у скриптов).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	if err := i.checkDBMode(); err != nil {
		return "", err
	}
	if i.externalDB() {
		// Файл адреса или пароля читается до дампа: путь вне secrets/ или
		// нечитаемый файл — отказ, а не пустой дамп.
		if _, _, err := i.dbConn(ctx); err != nil {
			return "", err
		}
	}
	if !i.externalDB() && !i.running(ctx, "postgres") {
		return "", errors.New("the postgres container is not running: nothing to back up")
	}

	stem := filepath.Join(dir, "cadence-"+i.version+"-"+i.e.now().Format("20060102-150405"))
	dump := stem + ".sql.gz"

	// pg_dump идёт ВНУТРИ контейнера: у машины нет клиента Postgres, а
	// клиент другой версии ломает дамп. Встроенная база — её же контейнер,
	// внешняя — клиент бэкапа с pg_dump старшей поддерживаемой версии.
	if i.externalDB() {
		i.e.info("dumping the external database %s to %s", i.externalTarget(), dump)
	} else {
		i.e.info("dumping the database to %s", dump)
	}
	if err := i.dumpTo(ctx, dump); err != nil {
		os.Remove(dump)
		if i.externalDB() {
			return "", fmt.Errorf("pg_dump of the external database failed (see above): %v", err)
		}
		return "", fmt.Errorf("pg_dump failed: %v", err)
	}
	fi, err := os.Stat(dump)
	if err != nil {
		return "", err
	}
	if fi.Size() < minDumpBytes {
		os.Remove(dump)
		return "", fmt.Errorf("the dump is suspiciously small (%d bytes): %s", fi.Size(), dump)
	}

	if i.get("SDLC_BLOB_S3_BUCKET") == "" {
		api := i.containerOf(ctx, "api")
		if api == "" {
			return "", errors.New("the api container does not exist: cannot reach the blobs volume")
		}
		name := filepath.Base(stem) + ".blobs.tar.gz"
		i.e.info("archiving the blobs volume to %s", stem+".blobs.tar.gz")
		// У образа сервиса нет shell: том берёт одноразовый контейнер. Он
		// работает от root, чтобы прочитать каждый файл, и отдаёт архив
		// вызывающему.
		c := i.docker("run", "--rm", "--volumes-from", api+":ro", "-v", dir+":/backup", "alpine:3",
			"sh", "-c", fmt.Sprintf("tar -czf '/backup/%s' -C /blobs . && chown %d:%d '/backup/%s' && chmod 600 '/backup/%s'",
				name, i.e.uid, i.e.gid, name, name))
		c.stdout, c.stderr = i.e.out, i.e.errw
		if err := i.e.run.run(ctx, c); err != nil {
			return "", fmt.Errorf("archiving the blobs volume failed: %v", err)
		}
	}

	if err := rotateBackups(dir, keepDays, i.e.now()); err != nil {
		return "", err
	}
	i.e.ok("%s %s (%s)", i.e.now().Format("2006-01-02 15:04:05"), dump, humanSize(fi.Size()))
	return dump, nil
}

// dumpTo — pg_dump в контейнере postgres или в клиенте бэкапа внешней базы,
// сжатый в файл с правами 600.
func (i *installation) dumpTo(ctx context.Context, dump string) error {
	var c cmdSpec
	if i.externalDB() {
		// Адрес до файла дампа: файл секрета, который не читается, —
		// отказ без пустого дампа.
		safe, password, err := i.dbConn(ctx)
		if err != nil {
			return err
		}
		c = i.dbClient(password, append([]string{"pg_dump", "-d", safe}, dumpOptions...)...)
	} else {
		c = i.compose(append([]string{"exec", "-T", "postgres", "pg_dump",
			"-U", i.getDefault("POSTGRES_USER", "sdlc"), "-d", i.getDefault("POSTGRES_DB", "sdlc")}, dumpOptions...)...)
	}
	f, err := os.OpenFile(dump, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	c.stdout, c.stderr = gz, i.e.errw
	runErr := i.e.run.run(ctx, c)
	gzErr := gz.Close()
	closeErr := f.Close()
	return errors.Join(runErr, gzErr, closeErr)
}

// rotateBackups удаляет дампы и архивы blobs старше keepDays — как
// find -mtime +N: возраст в целых сутках больше N.
func rotateBackups(dir string, keepDays int, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, en := range entries {
		name := en.Name()
		if !en.Type().IsRegular() || !strings.HasPrefix(name, "cadence-") ||
			!(strings.HasSuffix(name, ".sql.gz") || strings.HasSuffix(name, ".blobs.tar.gz")) {
			continue
		}
		fi, err := en.Info()
		if err != nil {
			continue
		}
		if int(now.Sub(fi.ModTime())/(24*time.Hour)) > keepDays {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// humanSize — размер как du -h: 512, 12K, 3.4M.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d", n)
	}
	value, suffix := float64(n), ""
	for _, s := range []string{"K", "M", "G", "T"} {
		value /= unit
		suffix = s
		if value < unit {
			break
		}
	}
	if value < 10 {
		return fmt.Sprintf("%.1f%s", value, suffix)
	}
	return fmt.Sprintf("%.0f%s", value, suffix)
}
