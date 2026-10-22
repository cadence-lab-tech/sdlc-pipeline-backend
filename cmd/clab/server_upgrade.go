package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const upgradeHelp = `clab server upgrade — upgrade the whole stack to a new version: backup,
package files of the new version (VERSION, compose, Caddyfile, scripts, docs),
new images, start, wait for readiness. git is not needed: the package comes
from the release.

Usage: clab server upgrade [--dir DIR] [--force] [vX.Y.Z]

  clab server upgrade                  # the latest release
  clab server upgrade v0.7.0           # a specific release
  clab server upgrade --force v0.7.0   # also replace package files edited here

Options:
  --dir DIR   the installation directory (default: $CADENCE_DIR, else the
              current directory or the nearest one above it)
  --force     replace package files edited here; the edited copies stay in
              backups/package-<old version>-<date>.tar.gz

The package of the release is downloaded with its checksums.txt and verified
before anything changes: the checksum, and with cosign 3.x in PATH the
signature of checksums.txt and of both images of the new version
(CADENCE_VERIFY in .env or the environment: auto, the default, warns once
without cosign, require refuses, off does not verify; docs/admin.md,
"Verifying a release"). Package files edited here stop the upgrade with their
list: keep settings in .env and compose changes in compose.override.yml.
.env, compose.override.yml, backups/, secrets/ and other files of the
directory are never touched.

With an external database (CADENCE_DB=external) the postgres container is
not needed: before the backup the images of the new version check the
database (/api dbcheck), and a database they cannot use stops the upgrade
while nothing has changed.

With BACKUP_S3_BUCKET set the backup is also copied to the bucket (clab
server backup --help); a failed copy is only a warning here - the rollback
uses the local backup.

Skipping versions is fine: migrations of every version in between are built
into the new image. Going down is refused - migrations only go forward; to
return to an older version restore its backup (clab server restore).

If the new version does not become ready, the command exits with an error and
prints the exact commands that return to the previous version and its backup.
After a successful upgrade it reminds to update clab, the CLI of people and
agents, to the new version: the server does not update it. An upgrade to a
version newer than this clab goes on with a warning.

  CADENCE_READY_TIMEOUT  seconds to wait for readiness, default 300
  CADENCE_RELEASES_URL   a mirror of the releases: download/vX.Y.Z/<asset> and
                         latest/download/<asset> (https://, http:// or file://)
  CADENCE_VERIFY         auto, require or off: the signatures of the release`

func serverUpgrade(ctx context.Context, e *serverEnv, args []string) error {
	var dir string
	var force bool
	pos, err := parseServerArgs(args, map[string]*bool{"--force": &force}, map[string]*string{"--dir": &dir})
	if err != nil {
		return err
	}
	target := ""
	switch len(pos) {
	case 0:
	case 1:
		target = pos[0]
		if !validVersion(target) {
			return usageError{fmt.Sprintf("'%s' is not a version: expected vX.Y.Z", target)}
		}
	default:
		return usageError{"one version at a time"}
	}
	inst, err := openInstallation(e, dir)
	if err != nil {
		return err
	}
	if err := inst.open(ctx); err != nil {
		return err
	}
	if err := inst.checkMode(); err != nil {
		return err
	}
	return inst.upgrade(ctx, target, force)
}

// upgrade — upgrade.sh по шагам: что ставить, правки файлов пакета, бэкап,
// файлы новой версии, образы и старт. До бэкапа ничего не меняется; после
// замены файлов неудачный pull возвращает их, неудачный старт печатает откат.
func (i *installation) upgrade(ctx context.Context, target string, force bool) error {
	e := i.e
	oldVersion := i.version
	backups := i.backupsDir()
	// Бинарь пакета этой установки апгрейд заменяет бинарём целевой версии:
	// предупреждать его о версии новее себя незачем (CLAB-242, R4). Внешний
	// clab — и под обёрткой (CADENCE_CLAB) — предупреждается как раньше.
	packageBinary := e.packageBinary(i.dir)
	// Команды для администратора: под обёрткой пакета — его скрипты.
	upgradeCmd, restoreCmd := "clab server upgrade", "clab server restore"
	if e.wrapped() {
		upgradeCmd, restoreCmd = e.script(i.dir, "upgrade.sh"), "./restore.sh"
	}
	// .env перекрывает окружение, как у всех переменных пакета (q7).
	v, err := newVerifier(ctx, e, i.get("CADENCE_VERIFY"), nothingChanged)
	if err != nil {
		return err
	}

	// Локальная правка compose, которую compose не читает, молча не
	// применяется; .env upgrade не правит, поэтому только говорит.
	for _, f := range []string{"compose.override.yml", "docker-compose.override.yml"} {
		if isFile(filepath.Join(i.dir, f)) && !strings.Contains(":"+i.get("COMPOSE_FILE")+":", ":"+f+":") {
			e.warn("%s exists, but COMPOSE_FILE in .env does not list it, so compose ignores it. To apply it: COMPOSE_FILE=%s:%s",
				f, i.get("COMPOSE_FILE"), f)
		}
	}

	// Рабочий каталог — в той же файловой системе, что установка: файлы
	// переезжают на место переименованием.
	work, err := os.MkdirTemp(i.dir, ".upgrade.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	// --- 1. что ставить, до любых изменений -----------------------------------
	if target == "" {
		e.info("looking up the latest release at %s", i.releasesURL())
		target, err = i.latestVersion(ctx, work)
		if err != nil {
			return fmt.Errorf("could not find the latest release at %s", i.releasesURL())
		}
	}
	if versionLess(target, oldVersion) {
		return fmt.Errorf("%s is older than the running %s: downgrades are not supported (migrations only go forward).\n"+
			"  To return to %s, put its package files back and restore one of its backups:\n%s    %s && %s backups/cadence-%s-<date>.sql.gz",
			target, oldVersion, target, i.cdHint(), i.packageFilesHint(ctx, target), restoreCommand(target, e.wrapped()), target)
	}
	if target == oldVersion {
		e.ok("already on %s; nothing to do", oldVersion)
		return nil
	}
	if w := targetNewerWarning(e.cli, target); w != "" && !packageBinary {
		e.warn("%s", w)
	}

	name := packageName(target)
	e.info("downloading %s", name)
	tarball, err := i.downloadPackage(ctx, target, work)
	switch {
	case errors.Is(err, errNoRelease):
		latest, _ := i.latestVersion(ctx, work)
		if latest != "" {
			latest = " (the latest is " + latest + ")"
		}
		return fmt.Errorf("there is no release %s with a package at %s%s", target, i.releasesURL(), latest)
	case errors.Is(err, errChecksum):
		return fmt.Errorf("the checksum of %s does not match checksums.txt of %s: the download is broken or altered; nothing has changed", name, target)
	case err != nil:
		return fmt.Errorf("downloading %s failed: %v; nothing has changed", name, err)
	}
	if err := v.verifyPackage(ctx, i.releasesURL(), target, work); err != nil {
		return err
	}
	newDir := filepath.Join(work, "new")
	if err := extractTarGz(tarball, newDir, nil); err != nil {
		return err
	}
	if !isFile(filepath.Join(newDir, packageManifest)) {
		return fmt.Errorf("%s has no %s: not a package of this repository", name, packageManifest)
	}
	raw, err := os.ReadFile(filepath.Join(newDir, "VERSION"))
	if err != nil {
		return fmt.Errorf("%s has no VERSION: not a package of this repository", name)
	}
	if v := strings.Join(strings.Fields(string(raw)), ""); v != target {
		return fmt.Errorf("VERSION in %s holds '%s', expected %s", name, v, target)
	}
	// Файлы нового пакета вместе с эталоном — отдельным списком: сами файлы
	// позже переезжают на место.
	newList, err := manifestPaths(filepath.Join(newDir, packageManifest))
	if err != nil {
		return err
	}
	newList = append(newList, packageManifest)
	inNew := setOf(newList)
	// Образы новой версии так, как их запустит compose, — до проверки
	// внешней базы ими же, до бэкапа и до замены файлов.
	if err := v.verifyImages(ctx, target, i.upgradeImages(newDir, target)); err != nil {
		return err
	}

	// --- 2. файлы пакета, правленые здесь ----------------------------------------
	// Эталон — манифест текущей версии: распакованный вместе с ней, либо — у
	// установки из git-клона — манифест её релиза, либо сам git.
	reference := ""
	if i.manifestOfThisVersion() {
		reference = filepath.Join(i.dir, packageManifest)
	} else {
		oldDir := filepath.Join(work, "old")
		if err := os.MkdirAll(oldDir, 0o700); err != nil {
			return err
		}
		if old, err := i.downloadPackage(ctx, oldVersion, oldDir); err == nil &&
			extractTarGz(old, oldDir, map[string]bool{packageManifest: true}) == nil &&
			isFile(filepath.Join(oldDir, packageManifest)) {
			reference = filepath.Join(oldDir, packageManifest)
		}
	}
	var edited []string
	switch {
	case reference != "":
		if edited, err = editedFiles(i.dir, reference); err != nil {
			return err
		}
	case i.gitClone(ctx):
		// Важны только файлы, которые заменит новый пакет.
		out, _ := output(ctx, e.run, cmdSpec{dir: i.dir, env: i.composeEnv(), name: "git", args: []string{"diff", "--name-only", "HEAD", "--"}})
		for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
			if f != "" && inNew[f] {
				edited = append(edited, f)
			}
		}
	default:
		e.warn("cannot tell whether package files were edited here: there is no %s of %s and no package of its release.", packageManifest, oldVersion)
		e.warn("They are replaced; the current ones are kept in %s/package-%s-<date>.tar.gz.", backups, oldVersion)
	}
	if len(edited) > 0 {
		list := "    " + strings.Join(edited, "\n    ")
		if !force {
			return fmt.Errorf("package files are edited here, and the upgrade would replace them:\n%s\n"+
				"  Keep settings in .env and compose changes in compose.override.yml (docs/admin.md, \"Local compose changes\"),\n"+
				"  and put these files back as they were - or run %s --force %s to replace them\n"+
				"  and keep the edited copies in %s/package-%s-<date>.tar.gz. Nothing has changed.",
				list, upgradeCmd, target, backups, oldVersion)
		}
		e.warn("replacing package files edited here (--force); the edited copies go to %s/package-%s-<date>.tar.gz:", backups, oldVersion)
		fmt.Fprintln(e.errw, list)
	}

	// --- 3. бэкап ---------------------------------------------------------------
	if i.externalDB() {
		// Внешнюю базу проверяют образы новой версии с файлами compose её
		// пакета до бэкапа: база, с которой новая версия не работает,
		// останавливает апгрейд, пока ничего не изменилось.
		e.info("checking the external database %s with the images of %s", i.externalTarget(), target)
		if !isFile(filepath.Join(newDir, externalDBFile)) {
			return fmt.Errorf("the package of %s has no %s: it cannot run on an external database. Nothing has changed.", target, externalDBFile)
		}
		// Оверлей секретов из COMPOSE_FILE — и здесь: без него токен
		// администратора и другие Docker secrets не дошли бы до api, а
		// SDLC_DATABASE_PASSWORD_FILE сбрасывает externalDBFile после него.
		files := []string{"--project-directory", i.dir, "-f", filepath.Join(newDir, "docker-compose.yml")}
		if strings.Contains(":"+i.get("COMPOSE_FILE")+":", ":"+secretsFile+":") && isFile(filepath.Join(newDir, secretsFile)) {
			files = append(files, "-f", filepath.Join(newDir, secretsFile))
		}
		c := i.compose(append(files, "-f", filepath.Join(newDir, externalDBFile), "run", "--rm", "--no-deps", "-T", "api", "dbcheck")...)
		c.env = append(c.env, "CADENCE_VERSION="+target)
		if err := i.dbCheck(ctx, c); err != nil {
			return fmt.Errorf("the external database is not ready for %s (see above). Nothing has changed.", target)
		}
	} else if !i.running(ctx, "postgres") {
		return errors.New("the stack is not running; start it before upgrading so that a backup can be taken:\n" +
			"  export CADENCE_VERSION=$(cat VERSION) && docker compose up -d")
	}
	e.info("backup of %s", oldVersion)
	backup, err := i.backup(ctx)
	if err != nil {
		return err
	}
	// Копия вне машины здесь — не условие апгрейда: откат опирается на
	// локальный файл (CLAB-132, q2).
	if i.offsiteConfigured() {
		if err := i.upload(ctx, backup); err != nil {
			e.warn("%v", err)
			e.warn("the upgrade goes on: its rollback uses the local backup")
		}
	}

	// Файлы пакета как они есть сейчас — старого манифеста и все, которые
	// перезапишет новый пакет, — для отката.
	stamp := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(backup), "cadence-"+oldVersion+"-"), ".sql.gz")
	saved := filepath.Join(i.path(backups), "package-"+oldVersion+"-"+stamp+".tar.gz")
	var refList []string
	if reference != "" {
		if refList, err = manifestPaths(reference); err != nil {
			return err
		}
	}
	var savedList []string
	for _, f := range uniqueSorted(append(append([]string{}, refList...), newList...)) {
		if isFile(filepath.Join(i.dir, filepath.FromSlash(f))) {
			savedList = append(savedList, f)
		}
	}
	if err := writeTarGz(saved, i.dir, savedList); err != nil {
		return fmt.Errorf("saving the package files of %s failed: %v", oldVersion, err)
	}
	e.info("package files of %s saved to %s", oldVersion, i.rel(saved))

	// --- 4. файлы пакета новой версии ----------------------------------------------
	e.info("replacing the package files with %s", target)
	// Файлы старого пакета, которых в новом нет.
	for _, f := range refList {
		if inNew[f] {
			continue
		}
		if err := os.Remove(filepath.Join(i.dir, filepath.FromSlash(f))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		e.info("removed %s (no longer in the package)", f)
	}
	if err := moveTree(newDir, i.dir); err != nil {
		return fmt.Errorf("moving the package files of %s into place failed: %v.\n  To put the files of %s back:\n%s  tar -xpzf %s",
			target, err, oldVersion, i.cdHint(), shellQuote(i.rel(saved)))
	}
	if err := i.loadVersion(); err != nil {
		return err
	}
	if i.version != target {
		return fmt.Errorf("VERSION is %s after the switch, expected %s", i.version, target)
	}

	// --- 5. образы и старт ------------------------------------------------------
	rollback := fmt.Sprintf("%s  tar -xpzf %s   # package files of %s\n  %s --yes %s   # data as of the backup, starts %s",
		i.cdHint(), shellQuote(i.rel(saved)), oldVersion, restoreCmd, shellQuote(i.rel(backup)), oldVersion)

	e.info("pulling images of %s", target)
	if err := i.pullImages(ctx); err != nil {
		// Кроме файлов, ещё ничего не изменилось: вернуть их.
		if rerr := i.putFilesBack(saved, work, newList, savedList); rerr != nil {
			return fmt.Errorf("could not pull the images of %s, and putting the package files back failed: %v.\n"+
				"  To put the files of %s back:\n%s  tar -xpzf %s", target, rerr, oldVersion, i.cdHint(), shellQuote(i.rel(saved)))
		}
		if err := i.loadVersion(); err != nil {
			return err
		}
		return fmt.Errorf("could not pull the images of %s (see above); the package files are back on %s and the running stack is untouched.", target, i.version)
	}
	e.info("starting %s", target)
	if err := i.composeRun(ctx, false, "up", "-d", "--remove-orphans"); err != nil {
		return fmt.Errorf("docker compose up failed for %s. To return to %s:\n%s", target, oldVersion, rollback)
	}
	// Caddyfile примонтирован файлом: up -d его правок не замечает.
	_, _ = output(ctx, e.run, i.compose("exec", "-T", "caddy", "caddy", "reload", "--config", "/etc/caddy/Caddyfile"))

	timeout, err := i.readyTimeout()
	if err != nil {
		return err
	}
	e.info("waiting for readiness (up to %ds)", timeout)
	if !i.waitReady(ctx, timeout) {
		i.showFailure(ctx, "api", "worker")
		return fmt.Errorf("%s did not become ready in %ds (logs above). The database may already be migrated.\n"+
			"  To return to %s and its backup:\n%s", target, timeout, oldVersion, rollback)
	}
	e.ok("upgraded %s -> %s", oldVersion, target)
	// clab людей и агентов остаётся на старой версии: сказать здесь, где
	// смотрит администратор.
	e.say("%s", clabReminder(target))
	if versionLess(e.cli, target) && !packageBinary {
		e.say("  This clab is %s: update it too, clab server does not manage a stack newer than itself:\n%s",
			e.cli, updateCLICommands(target, e.brew))
	}
	if _, err := os.Stat(filepath.Join(i.dir, ".git")); err == nil {
		e.say("  This directory is a git clone; upgrades no longer use git, and .git can be removed (docs/admin.md, \"Moving a git installation to release packages\").")
	}
	return nil
}

// manifestOfThisVersion — эталон описывает файлы версии из VERSION.
// Эталон другой версии (откат tar возвращает старые файлы, но оставляет
// новый эталон) — не эталон.
func (i *installation) manifestOfThisVersion() bool {
	entries, err := readManifest(filepath.Join(i.dir, packageManifest))
	if err != nil {
		return false
	}
	sum, err := sha256File(filepath.Join(i.dir, "VERSION"))
	if err != nil {
		return false
	}
	for _, en := range entries {
		if en.path == "VERSION" {
			return en.sum == sum
		}
	}
	return false
}

// editedFiles — файлы пакета, которые есть здесь с содержимым не по эталону.
func editedFiles(dir, manifest string) ([]string, error) {
	entries, err := readManifest(manifest)
	if err != nil {
		return nil, err
	}
	var edited []string
	for _, en := range entries {
		p := filepath.Join(dir, filepath.FromSlash(en.path))
		if !isFile(p) {
			continue
		}
		sum, err := sha256File(p)
		if err != nil {
			return nil, err
		}
		if sum != en.sum {
			edited = append(edited, en.path)
		}
	}
	return edited, nil
}

// moveTree переносит каждый файл из src на тот же путь в dst
// переименованием.
func moveTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.Rename(p, target)
	})
}

// putFilesBack — откат файлов после неудачного pull: файлы из копии на место,
// а пришедшие с новым пакетом и не бывшие здесь — прочь.
func (i *installation) putFilesBack(saved, work string, newList, savedList []string) error {
	back := filepath.Join(work, "back")
	if err := extractTarGz(saved, back, nil); err != nil {
		return err
	}
	had := setOf(savedList)
	for _, f := range newList {
		if had[f] {
			continue
		}
		if err := os.Remove(filepath.Join(i.dir, filepath.FromSlash(f))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return moveTree(back, i.dir)
}

func setOf(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}

func uniqueSorted(list []string) []string {
	sort.Strings(list)
	var out []string
	for _, s := range list {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
