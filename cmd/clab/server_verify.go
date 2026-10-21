package main

// Проверка подписей релиза у клиента (CLAB-197, решения 0046 и 0050):
// clab server install и upgrade проверяют подпись checksums.txt пакета и
// подписи обоих образов внешним cosign 3.x — по тем же правилам и с теми же
// сообщениями, что lib.sh («signatures») и get-cadence.sh пакета. Режим —
// CADENCE_VERIFY: auto (по умолчанию) — проверять, если cosign есть, и один
// раз предупредить, если нет; require — без cosign, подписи, pin или
// доступа к ghcr.io отказ; off — не проверять (закрытый контур). Подпись,
// которая не сошлась, останавливает и auto, и require.
//
// Встроенной проверки (sigstore-go) нет: внешний cosign — тот же
// инструмент, которым клиент проверяет релиз руками по admin.md, и
// статический бинарь не растёт.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	verifyFirstSigned     = "v0.9.0"
	verifyIssuer          = "https://token.actions.githubusercontent.com"
	verifySource          = "https://github.com/cadence-lab-tech"
	verifyRegistry        = "ghcr.io/cadence-lab-tech"
	verifyPackageIdentity = verifySource + "/self-hosted/.github/workflows/release.yml@refs/heads/main"
	verifyBundle          = "checksums.txt.sigstore.json"
	verifyCosignURL       = "https://docs.sigstore.dev/cosign/system_config/installation/"
	// Адрес, по которому видно, отвечает ли ghcr.io вообще: 401 без токена —
	// тоже ответ.
	verifyRegistryProbe = "https://ghcr.io/v2/"

	nothingInstalled = "Nothing was installed."
	nothingChanged   = "Nothing has changed."
)

// verifyImageRepos — образы релиза, чьи подписи проверяются.
var verifyImageRepos = []string{"sdlc-pipeline-backend", "sdlc-pipeline-frontend"}

var cosignGitVersion = regexp.MustCompile(`(?m)^GitVersion:\s*(v?[0-9][0-9.]*)`)

// verifyIdentity — identity подписи образа repo версии v: release.yml его
// репозитория на теге.
func verifyIdentity(repo, v string) string {
	return verifySource + "/" + repo + "/.github/workflows/release.yml@refs/tags/" + v
}

// verifier — режим проверки одного запуска install или upgrade.
type verifier struct {
	e       *serverEnv
	mode    string // auto, require, off
	cosign  bool   // нашёлся cosign 3.x
	why     string // …или почему его нет
	noted   bool   // предупреждение о режиме напечатано
	nothing string // чем кончается отказ: что не сделано
}

// newVerifier — lib.sh verify_init: значение CADENCE_VERIFY и cosign, до
// любых действий. Незнакомое значение — отказ, require без cosign — тоже.
func newVerifier(ctx context.Context, e *serverEnv, value, nothing string) (*verifier, error) {
	v := &verifier{e: e, mode: value, nothing: nothing}
	if v.mode == "" {
		v.mode = "auto"
	}
	switch v.mode {
	case "auto", "require":
	case "off":
		e.info("signature verification is off (CADENCE_VERIFY=off)")
		v.noted = true
		return v, nil
	default:
		return nil, fmt.Errorf("CADENCE_VERIFY is '%s', expected auto, require or off. %s", v.mode, nothing)
	}
	if _, err := lookPath("cosign"); err != nil {
		v.why = "cosign 3.x is not installed"
	} else {
		var out bytes.Buffer
		_ = e.run.run(ctx, cmdSpec{name: "cosign", args: []string{"version"}, stdout: &out, stderr: &out})
		ver := ""
		if m := cosignGitVersion.FindStringSubmatch(out.String()); m != nil {
			ver = m[1]
		}
		major := strings.TrimPrefix(ver, "v")
		if i := strings.IndexByte(major, '.'); i >= 0 {
			major = major[:i]
		}
		switch {
		case ver == "" || major == "":
			v.why = "cosign of an unknown version is not 3.x"
		case len(major) == 1 && major[0] < '3':
			v.why = "cosign " + ver + " is older than 3.x"
		default:
			v.cosign = true
		}
	}
	if v.mode == "require" && !v.cosign {
		return nil, fmt.Errorf("CADENCE_VERIFY=require, but %s: install cosign 3.x (%s), or set CADENCE_VERIFY=auto. %s", v.why, verifyCosignURL, nothing)
	}
	return v, nil
}

// note — предупреждение о режиме, один раз за запуск.
func (v *verifier) note(format string, a ...any) {
	if v.noted {
		return
	}
	v.noted = true
	v.e.warn(format, a...)
}

// skip — lib.sh verify_skip: проверять нечего — режим off, версия старше
// подписей или (auto) нет cosign; почему — говорится один раз. require
// отказывает неподписанной версии.
func (v *verifier) skip(version string) (bool, error) {
	if v.mode == "off" {
		return true, nil
	}
	if versionLess(version, verifyFirstSigned) {
		if v.mode != "auto" {
			return true, fmt.Errorf("%s is not signed (signatures start with %s), and CADENCE_VERIFY=require refuses it. %s", version, verifyFirstSigned, v.nothing)
		}
		v.note("%s is not signed (signatures start with %s): its package and images are not verified", version, verifyFirstSigned)
		return true, nil
	}
	if !v.cosign {
		v.note("%s: the signatures of the release are not verified (CADENCE_VERIFY=auto). Install cosign 3.x to verify them: docs/admin.md, \"Verifying a release\"", v.why)
		return true, nil
	}
	return false, nil
}

// runCosign — cosign с аргументами; stderr — для отказа.
func (v *verifier) runCosign(ctx context.Context, args ...string) (string, error) {
	var errb bytes.Buffer
	err := v.e.run.run(ctx, cmdSpec{name: "cosign", args: args, stdout: io.Discard, stderr: &errb})
	return errb.String(), err
}

// tail — последние строки ответа внешней команды перед отказом, как
// tail -n 5 у скриптов.
func (v *verifier) tail(s string) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	if text := strings.Join(lines, "\n"); strings.TrimSpace(text) != "" {
		fmt.Fprintln(v.e.errw, text)
	}
}

// verifyPackage — lib.sh verify_package: подпись dir/checksums.txt версии
// бандлом того же релиза, скачанным рядом, — до распаковки пакета. Релиз
// v0.9.0+ без бандла — отказ: зеркало, потерявшее подпись, — не повод её
// не проверять.
func (v *verifier) verifyPackage(ctx context.Context, releases, version, dir string) error {
	if skip, err := v.skip(version); skip || err != nil {
		return err
	}
	bundle := filepath.Join(dir, verifyBundle)
	if err := v.e.fetch(ctx, releases+"/download/"+version+"/"+verifyBundle, bundle); err != nil {
		if errors.Is(err, errNoRelease) {
			return fmt.Errorf("the release %s has no %s at %s: the signature of its checksums.txt cannot be verified.\n"+
				"  Copy %s of the release to the mirror, or set CADENCE_VERIFY=off. %s", version, verifyBundle, releases, verifyBundle, v.nothing)
		}
		return fmt.Errorf("downloading %s of %s failed: %v. %s", verifyBundle, version, err, v.nothing)
	}
	out, err := v.runCosign(ctx, "verify-blob", "--bundle", bundle, "--certificate-identity", verifyPackageIdentity,
		"--certificate-oidc-issuer", verifyIssuer, filepath.Join(dir, "checksums.txt"))
	if err != nil {
		v.tail(out)
		return fmt.Errorf("the signature of checksums.txt of %s did not verify for %s: the release is altered or not ours.\n"+
			"  Do not use it and report it privately (SECURITY.md). %s", version, verifyPackageIdentity, v.nothing)
	}
	v.e.ok("the signature of checksums.txt of %s verified: signed by %s", version, verifyPackageIdentity)
	return nil
}

// composeImages — образы итоговой конфигурации compose: stdout c (docker
// compose … config --images) построчно. Ошибка compose — его stderr к
// человеку.
func composeImages(ctx context.Context, e *serverEnv, c cmdSpec) ([]string, error) {
	var out, errb bytes.Buffer
	c.stdout, c.stderr = &out, &errb
	if err := e.run.run(ctx, c); err != nil {
		(&verifier{e: e}).tail(errb.String())
		return nil, err
	}
	var images []string
	for _, line := range strings.Split(out.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			images = append(images, line)
		}
	}
	return images, nil
}

// imageRefs — образы repo среди images, без повторов: в compose его может
// запускать не один сервис (api и worker).
func imageRefs(images []string, repo string) []string {
	re := regexp.MustCompile(`(^|/)` + regexp.QuoteMeta(repo) + `([:@]|$)`)
	seen := map[string]bool{}
	var refs []string
	for _, img := range images {
		if re.MatchString(img) && !seen[img] {
			seen[img] = true
			refs = append(refs, img)
		}
	}
	sort.Strings(refs)
	return refs
}

// verifyImages — lib.sh verify_images: подписи образов sdlc-pipeline-backend
// и sdlc-pipeline-frontend, которые итоговая конфигурация compose (c — её
// docker compose … config --images) запускает в версии version. Каждый
// проверяется в ghcr.io по своему digest, откуда бы он ни тянулся: зеркало
// хранит digest.
func (v *verifier) verifyImages(ctx context.Context, version string, c cmdSpec) error {
	if skip, err := v.skip(version); skip || err != nil {
		return err
	}
	images, err := composeImages(ctx, v.e, c)
	if err != nil {
		return fmt.Errorf("could not read the images of %s from the compose configuration (see above). %s", version, v.nothing)
	}
	v.e.info("verifying the signatures of the images of %s", version)
	for _, repo := range verifyImageRepos {
		pin := "CADENCE_" + strings.ToUpper(strings.TrimPrefix(repo, "sdlc-pipeline-")) + "_DIGEST"
		refs := imageRefs(images, repo)
		if len(refs) == 0 {
			if v.mode != "auto" {
				return fmt.Errorf("the compose configuration runs no image of %s (compose.override.yml replaces it?), and CADENCE_VERIFY=require verifies only the images of the release. %s", repo, v.nothing)
			}
			v.e.warn("the compose configuration runs no image of %s (compose.override.yml replaces it?): not pinned, not verified", repo)
			continue
		}
		for _, ref := range refs {
			at := strings.LastIndex(ref, "@sha256:")
			if at < 0 {
				if v.mode != "auto" {
					return fmt.Errorf("%s is not pinned to a digest (an empty %s, or compose.override.yml), and CADENCE_VERIFY=require verifies images by digest only. %s", ref, pin, v.nothing)
				}
				v.e.warn("%s is not pinned to a digest (an empty %s, or compose.override.yml): not pinned, not verified", ref, pin)
				continue
			}
			target := verifyRegistry + "/" + repo + "@" + ref[at+1:]
			identity := verifyIdentity(repo, version)
			out, err := v.runCosign(ctx, "verify", "--certificate-identity", identity, "--certificate-oidc-issuer", verifyIssuer, target)
			if err == nil {
				v.e.ok("%s is signed by %s", target, identity)
				continue
			}
			if !v.e.reachable(ctx, verifyRegistryProbe) {
				if v.mode != "auto" {
					return fmt.Errorf("the images cannot be verified: ghcr.io is unreachable from this host, and CADENCE_VERIFY=require needs it (the signatures are checked there by digest). %s", v.nothing)
				}
				v.e.warn("images not verified: ghcr.io is unreachable from this host (a registry mirror in a closed network?); verify them where it is reachable: docs/admin.md, \"Verifying a release\"")
				return nil
			}
			v.tail(out)
			return fmt.Errorf("the signature of %s (%s %s) did not verify for %s: the image is altered or not ours.\n"+
				"  Do not run it and report it privately (SECURITY.md). %s", target, repo, version, identity, v.nothing)
		}
	}
	return nil
}

// composeImagesSpec — docker compose -f … config --images для версии
// version: файлы files, CADENCE_VERSION версии поверх окружения env.
func composeImagesSpec(dir string, env []string, version string, files ...string) cmdSpec {
	args := []string{"compose"}
	if dir != "" {
		args = append(args, "--project-directory", dir)
	}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	return cmdSpec{dir: dir, env: append(append([]string{}, env...), "CADENCE_VERSION="+version),
		name: "docker", args: append(args, "config", "--images")}
}

// installImages — образы, которые поставит install: docker-compose.yml
// пакета и compose.override.yml, если он уже лежит в распакованном пакете.
// Окружение — процесса: .env ещё нет.
func installImages(e *serverEnv, pkgDir, version string) cmdSpec {
	files := []string{filepath.Join(pkgDir, "docker-compose.yml")}
	if override := filepath.Join(pkgDir, "compose.override.yml"); isFile(override) {
		files = append(files, override)
	}
	return composeImagesSpec("", e.environ, version, files...)
}

// upgradeImages — образы новой версии так, как их запустит compose после
// апгрейда: файлы COMPOSE_FILE из нового пакета, а локальные (override) —
// из установки, с .env.
func (i *installation) upgradeImages(newDir, target string) cmdSpec {
	cf := i.get("COMPOSE_FILE")
	if cf == "" {
		cf = "docker-compose.yml"
	}
	var files []string
	for _, f := range strings.Split(cf, ":") {
		switch {
		case f == "":
		case isFile(filepath.Join(newDir, f)):
			files = append(files, filepath.Join(newDir, f))
		case isFile(i.path(f)):
			files = append(files, i.path(f))
		}
	}
	return composeImagesSpec(i.dir, i.composeEnv(), target, files...)
}

// realReachable — адрес отвечает по HTTP хоть чем-то.
func realReachable(ctx context.Context, rawURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
