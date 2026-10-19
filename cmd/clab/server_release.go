package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Релизы пакета self-hosted (решение 0044, lib.sh «releases»). У каждого
// релиза vX.Y.Z три ассета с неизменными именами:
//
//	cadence-self-hosted-vX.Y.Z.tar.gz   пакет и package.sha256 — эталон файлов
//	checksums.txt                       sha256 двух других ассетов
//	get-cadence.sh                      однострочный установщик
//
// CADENCE_RELEASES_URL направляет на зеркало (или каталог file:///path) с той
// же раскладкой: download/vX.Y.Z/<ассет> и latest/download/<ассет>.

const (
	releasesDefault = "https://github.com/cadence-lab-tech/self-hosted/releases"
	packageManifest = "package.sha256"
	// Первая версия, выпущенная пакетом. Ранние (v0.5.x) ставились git-клоном
	// и пакета в релизе не имеют; clab server ими не управляет.
	firstPackageVersion = "v0.6.0"
	// Репозиторий пакета — для архива исходников тега до v0.6.0.
	cadenceRepoURL = "https://github.com/cadence-lab-tech/self-hosted"
)

var (
	errNoRelease = errors.New("no such release")
	errChecksum  = errors.New("checksum does not match")
)

// releasesFrom — адрес релизов: значение CADENCE_RELEASES_URL или GitHub.
// У установки оно берётся с учётом .env, у clab server install — только из
// окружения: .env ещё нет.
func releasesFrom(v string) string {
	if v == "" {
		v = releasesDefault
	}
	return strings.TrimRight(v, "/")
}

func (i *installation) releasesURL() string {
	return releasesFrom(i.get("CADENCE_RELEASES_URL"))
}

func packageName(v string) string { return "cadence-self-hosted-" + v + ".tar.gz" }

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetch скачивает адрес в файл; errNoRelease — такого файла нет. http(s) —
// с двумя повторами на сетевых сбоях и 5xx, как curl --retry 2; file:// —
// каталог-зеркало.
func (e *serverEnv) fetch(ctx context.Context, rawURL, dst string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Scheme == "file" {
		src, err := os.Open(u.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return errNoRelease
			}
			return err
		}
		defer src.Close()
		return writeFile(dst, src, 0o600)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: only https://, http:// and file:// addresses are supported", rawURL)
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			e.sleep(time.Duration(attempt) * time.Second)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		resp, err := e.http.Do(req)
		if err != nil {
			last = err
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			err := writeFile(dst, resp.Body, 0o600)
			resp.Body.Close()
			return err
		case resp.StatusCode >= 500:
			resp.Body.Close()
			last = fmt.Errorf("%s: HTTP %d", rawURL, resp.StatusCode)
			continue
		default:
			resp.Body.Close()
			return errNoRelease
		}
	}
	return last
}

func writeFile(dst string, src io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

var checksumsPackage = regexp.MustCompile(`^[0-9a-f]{64}  cadence-self-hosted-(v[0-9]+\.[0-9]+\.[0-9]+)\.tar\.gz$`)

// latestVersion — версия последнего релиза: имя пакета в его checksums.txt
// (GitHub отправляет latest/download/ на последний релиз, зеркало держит там
// копию).
func (i *installation) latestVersion(ctx context.Context, work string) (string, error) {
	return latestRelease(ctx, i.e, i.releasesURL(), work)
}

// latestRelease — то же без установки: по адресу релизов.
func latestRelease(ctx context.Context, e *serverEnv, releases, work string) (string, error) {
	dst := filepath.Join(work, "latest-checksums.txt")
	if err := e.fetch(ctx, releases+"/latest/download/checksums.txt", dst); err != nil {
		return "", err
	}
	defer os.Remove(dst)
	f, err := os.Open(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := checksumsPackage.FindStringSubmatch(strings.TrimRight(sc.Text(), "\r")); m != nil {
			return m[1], nil
		}
	}
	return "", errNoRelease
}

// downloadPackage скачивает пакет версии с его checksums.txt в каталог и
// сверяет сумму. errNoRelease — релиза или пакета в нём нет, errChecksum —
// сумма не сходится. Возвращает путь к тарболу.
func (i *installation) downloadPackage(ctx context.Context, v, dir string) (string, error) {
	return downloadRelease(ctx, i.e, i.releasesURL(), v, dir)
}

// downloadRelease — то же без установки: по адресу релизов.
func downloadRelease(ctx context.Context, e *serverEnv, releases, v, dir string) (string, error) {
	base := releases + "/download/" + v
	name := packageName(v)
	sums := filepath.Join(dir, "checksums.txt")
	if err := e.fetch(ctx, base+"/checksums.txt", sums); err != nil {
		return "", err
	}
	tarball := filepath.Join(dir, name)
	if err := e.fetch(ctx, base+"/"+name, tarball); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(sums)
	if err != nil {
		return "", err
	}
	want := ""
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			want = f[0]
			break
		}
	}
	got, err := sha256File(tarball)
	if err != nil {
		return "", err
	}
	if want == "" || want != got {
		return "", errChecksum
	}
	return tarball, nil
}

// extractTarGz распаковывает тарбол пакета в каталог с правами из архива
// (tar -xpzf). only — распаковать только эти пути; nil — все. Пакет — это
// обычные файлы и каталоги: ссылки, абсолютные пути и «..» — отказ, а не
// запись за пределы каталога.
func extractTarGz(archive, dst string, only map[string]bool) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(archive), err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(archive), err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("%s: unsafe path %q", filepath.Base(archive), h.Name)
		}
		if only != nil && !only[name] {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(h.Mode) & 0o777
			if err := writeFile(target, tr, mode); err != nil {
				return err
			}
			// Права ровно из архива, без umask: скрипты пакета 755.
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("%s: unexpected entry %q (type %c)", filepath.Base(archive), h.Name, h.Typeflag)
		}
	}
}

// writeTarGz кладёт файлы каталога в тарбол с их правами — копия файлов
// пакета для отката (её распаковывает обычный tar -xpzf).
func writeTarGz(archive, dir string, files []string) error {
	out, err := os.OpenFile(archive, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	fail := func(err error) error {
		tw.Close()
		gz.Close()
		out.Close()
		os.Remove(archive)
		return err
	}
	for _, name := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		fi, err := os.Stat(p)
		if err != nil {
			return fail(err)
		}
		h, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return fail(err)
		}
		h.Name = name
		h.Uname, h.Gname = "", ""
		if err := tw.WriteHeader(h); err != nil {
			return fail(err)
		}
		src, err := os.Open(p)
		if err != nil {
			return fail(err)
		}
		_, err = io.Copy(tw, src)
		src.Close()
		if err != nil {
			return fail(err)
		}
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	return out.Close()
}

// manifestPaths — пути эталона package.sha256.
func manifestPaths(manifest string) ([]string, error) {
	entries, err := readManifest(manifest)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.path)
	}
	return paths, nil
}

type manifestEntry struct{ sum, path string }

func readManifest(manifest string) ([]manifestEntry, error) {
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	var out []manifestEntry
	for _, line := range strings.Split(string(raw), "\n") {
		sum, p, ok := strings.Cut(strings.TrimRight(line, "\r"), "  ")
		if !ok || p == "" {
			continue
		}
		out = append(out, manifestEntry{sum, p})
	}
	return out, nil
}

// ── подсказка «поставить файлы пакета версии» (CLAB-158) ─────────────────

// gitClone — каталог установки — рабочий git-клон: .git есть, git есть и
// узнаёт его. Без .git git не зовётся вовсе: проверки ci/ следят, что
// установка из пакета git не трогает.
func (i *installation) gitClone(ctx context.Context) bool {
	if _, err := os.Stat(filepath.Join(i.dir, ".git")); err != nil {
		return false
	}
	if _, err := lookPath("git"); err != nil {
		return false
	}
	_, err := output(ctx, i.e.run, cmdSpec{dir: i.dir, env: i.composeEnv(), name: "git", args: []string{"rev-parse", "--git-dir"}})
	return err == nil
}

// backupsDir — каталог бэкапов так, как его пишут подсказки скриптов:
// BACKUP_DIR или backups относительно установки.
func (i *installation) backupsDir() string { return i.getDefault("BACKUP_DIR", "backups") }

// packageFilesHint — команда, которая ставит в каталог файлы пакета версии
// (lib.sh package_files_hint после CLAB-158), по пяти веткам:
//
//  1. копия, которую upgrade оставил в backups/ при уходе с версии;
//  2. версия с пакетом (v0.6.0+) — пакет её релиза (с зеркала, если задано);
//  3. версия до v0.6.0 в git-клоне, тег уже есть — git checkout тега;
//  4. то же, тега нет — git fetch --tags и checkout (CADENCE_GIT_REMOTE);
//  5. до v0.6.0 без git — архив исходников тега с GitHub: зеркала держат
//     только релизы, CADENCE_RELEASES_URL к нему не относится.
//
// --force у checkout: файлы поздних пакетов отличаются от тега.
func (i *installation) packageFilesHint(ctx context.Context, v string) string {
	backups := i.backupsDir()
	if saved, _ := filepath.Glob(filepath.Join(i.path(backups), "package-"+v+"-*.tar.gz")); len(saved) > 0 {
		sort.Strings(saved)
		return "tar -xpzf " + shellQuote(filepath.Join(backups, filepath.Base(saved[len(saved)-1])))
	}
	if !versionLess(v, firstPackageVersion) {
		name := packageName(v)
		return fmt.Sprintf("curl -fsSLO %s/download/%s/%s && tar -xpzf %s && rm %s", i.releasesURL(), v, name, name, name)
	}
	checkout := "git -c advice.detachedHead=false checkout --force " + v
	if i.gitClone(ctx) {
		_, err := output(ctx, i.e.run, cmdSpec{dir: i.dir, env: i.composeEnv(), name: "git",
			args: []string{"rev-parse", "-q", "--verify", "refs/tags/" + v + "^{commit}"}})
		if err == nil {
			return checkout
		}
		return fmt.Sprintf("git fetch --tags %s && %s", i.getDefault("CADENCE_GIT_REMOTE", "origin"), checkout)
	}
	return fmt.Sprintf("curl -fsSL %s/archive/refs/tags/%s.tar.gz | tar -xpzf - --strip-components=1", cadenceRepoURL, v)
}

// restoreCommand — чем восстанавливать бэкап версии v после того, как её
// файлы на месте: clab server для версий с пакетом, restore.sh — для более
// ранних, которыми clab server не управляет.
func restoreCommand(v string) string {
	if versionLess(v, firstPackageVersion) {
		return "./restore.sh"
	}
	return "clab server restore"
}
