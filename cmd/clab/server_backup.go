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
the blobs volume (SDLC_BLOB_S3_BUCKET is empty), that volume; rotate old backups.

Usage: clab server backup [--dir DIR]

Options:
  --dir DIR   the installation directory (default: $CADENCE_DIR, else the
              current directory or the nearest one above it)

Files: backups/cadence-<version>-<YYYYmmdd-HHMMSS>.sql.gz (mode 600) and, next
to it, the same name with .blobs.tar.gz. The version in the name is what
restore checks. A dump smaller than 1 KB is a failure.

Environment (from the environment or .env):
  BACKUP_DIR  where to write, default backups/ of the installation
  KEEP_DAYS   how long to keep backups, default 14

For cron:
  0 3 * * * clab server backup --dir /opt/cadence >> /opt/cadence/backups/backup.log 2>&1

Backups stay on this machine: they protect against a bad upgrade or a
mistaken deletion, not against losing the machine. Copy them elsewhere.`

func serverBackup(ctx context.Context, e *serverEnv, args []string) error {
	var dir string
	pos, err := parseServerArgs(args, nil, map[string]*string{"--dir": &dir})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{fmt.Sprintf("unexpected argument: %s", pos[0])}
	}
	inst, err := openInstallation(e, dir)
	if err != nil {
		return err
	}
	if err := inst.open(ctx); err != nil {
		return err
	}
	_, err = inst.backup(ctx)
	return err
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

	if !i.running(ctx, "postgres") {
		return "", errors.New("the postgres container is not running: nothing to back up")
	}

	stem := filepath.Join(dir, "cadence-"+i.version+"-"+i.e.now().Format("20060102-150405"))
	dump := stem + ".sql.gz"

	// pg_dump идёт ВНУТРИ контейнера базы: у машины нет клиента Postgres, а
	// клиент другой версии ломает дамп.
	i.e.info("dumping the database to %s", dump)
	if err := i.dumpTo(ctx, dump); err != nil {
		os.Remove(dump)
		return "", fmt.Errorf("pg_dump failed: %v", err)
	}
	fi, err := os.Stat(dump)
	if err != nil {
		return "", err
	}
	if fi.Size() < minDumpBytes {
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

// dumpTo — pg_dump в контейнере postgres, сжатый в файл с правами 600.
func (i *installation) dumpTo(ctx context.Context, dump string) error {
	f, err := os.OpenFile(dump, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	c := i.compose("exec", "-T", "postgres", "pg_dump",
		"-U", i.getDefault("POSTGRES_USER", "sdlc"), "-d", i.getDefault("POSTGRES_DB", "sdlc"),
		"--clean", "--if-exists", "--no-owner")
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
