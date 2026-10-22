package main

// clab server images (CLAB-202, решение 0058) — образы версии для закрытого
// контура: list печатает перечень, save собирает архив версии на машине с
// сетью, load загружает его на машине без сети так, что pin compose по
// digest находится.
//
// Копирует образы внешний regctl, а не Go в самом clab (решение человека,
// 0058): `regctl image copy --referrers` переносит индекс со всеми
// платформами, аттестациями BuildKit, подписью cosign и OpenVEX в OCI-layout
// и обратно, а cosign save/load 3.1.3 referrers теряет (спайк CLAB-202).
// Подписи проверяет тот же внешний cosign, что у install и upgrade (0054);
// без сети — по trusted_root.json, который save кладёт в архив.
//
// Загрузка без своего реестра идёт через временный реестр на 127.0.0.1:
// docker load не сохраняет digest, а docker pull по digest из реестра и
// docker tag в имя compose дают ссылку «тег@pin», которую compose находит
// локально, — на хранилище containerd. Классическое хранилище overlay2
// такой ссылки не создаёт: там load отказывает (q11), а не оставляет образ,
// за которым compose пойдёт в сеть.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// helperImage — образ, который clab server запускает сам, а не compose:
	// одноразовый контейнер тома blobs у backup и restore. Он входит в
	// перечень образов версии.
	helperImage = "alpine:3"

	regctlMinVersion = "v0.9.2" // проверено спайком CLAB-202
	regctlURL        = "https://github.com/regclient/regclient/releases"

	// Образ временного реестра load — distribution, закреплённый по digest
	// индекса: в архив он попадает tar'ом для docker load по платформе, под
	// именем airgapRegistryName.
	airgapRegistryImage = "registry:3@sha256:01cd312e67026f06f5d89c3e133a4a9adc7cb9eeb9b1194e0b2954dfd770efd2"
	airgapRegistryName  = "clab-images-registry:3"
	airgapLabel         = "cadence.images-load"

	archiveManifest  = "cadence-images.json"
	archiveFormat    = 1
	trustedRootAsset = "trusted_root.json"
	// Адрес Sigstore TUF: отвечает — корень берётся из сети, как в 0054.
	verifyTUFProbe = "https://tuf-repo-cdn.sigstore.dev/"

	nothingSaved  = "Nothing was saved."
	nothingLoaded = "Nothing was loaded."
)

// airgapPlatforms — платформы образа временного реестра в архиве: те же,
// под которые собраны наши образы.
var airgapPlatforms = []string{"linux/amd64", "linux/arm64"}

const imagesHelp = `clab server images — the images of a version for a closed network: list
them, save them with the release into one archive on a machine with
internet access, load the archive on a machine without it.

Usage: clab server images list [--dir DIR | --version vX.Y.Z]
       clab server images save --version vX.Y.Z [--output FILE]
       clab server images load FILE [--registry HOST[/PATH]]

  clab server images list --version v0.13.0
  clab server images save --version v0.13.0 --output cadence-v0.13.0-airgap.tar
  clab server images load cadence-v0.13.0-airgap.tar
  clab server images load cadence-v0.13.0-airgap.tar --registry registry.corp.example/cadence

save and load need regctl ` + regctlMinVersion + ` or later and cosign 3.x besides Docker
(docs/admin.md, "Air-gapped installation").

clab server images <list|save|load> --help — the options of a command.`

const imagesListHelp = `clab server images list — the images a version runs: every image of the
compose files of its package in every profile, and ` + helperImage + `, which
clab server runs itself for backup and restore. One line per image: ours with
the tag and the digest docker-compose.yml pins, the others by tag.

Usage: clab server images list [--dir DIR | --version vX.Y.Z]

Options:
  --dir DIR          the installation directory (default: $CADENCE_DIR, else
                     the current directory or the nearest one above it): the
                     images of its version, with CADENCE_REGISTRY of its .env
  --version vX.Y.Z   the images of a release: its package is downloaded from
                     CADENCE_RELEASES_URL (default GitHub) and the signature of
                     its checksums.txt verified (CADENCE_VERIFY)

Copy them into a registry mirror with crane copy, skopeo copy --all or regctl
image copy --referrers - the whole index, so that the digests stay - or carry
them in an archive: clab server images save.`

const imagesSaveHelp = `clab server images save — one archive with everything a closed machine needs
for a version: every image of clab server images list as an OCI layout with
all its platforms (ours with their signatures, attestations and OpenVEX), the
release of the package (download/vX.Y.Z/ with the package, checksums.txt, its
signature, get-cadence.sh) with trusted_root.json of Sigstore to verify them
offline, and the image of the temporary registry clab server images load runs.

Usage: clab server images save --version vX.Y.Z [--output FILE]

Options:
  --version vX.Y.Z   the release to save
  --output FILE      the archive, default ./cadence-vX.Y.Z-airgap.tar; it must
                     not exist

It needs access to GitHub (or CADENCE_RELEASES_URL), ghcr.io, Docker Hub and
Sigstore, Docker with compose v2, regctl ` + regctlMinVersion + ` or later and cosign 3.x.
The signature of checksums.txt and of both images of the version are verified
before anything is written (CADENCE_VERIFY=off saves without them, and
without trusted_root.json); the archive is written next to FILE and renamed,
so a failure leaves nothing. It prints the SHA-256 of the archive: compare it
on the closed machine before loading.

  CADENCE_RELEASES_URL   a mirror of the releases (https://, http://, file://)
  CADENCE_VERIFY         auto or require: verify (cosign 3.x is needed); off`

const imagesLoadHelp = `clab server images load — load an archive of clab server images save on this
machine, without network access: verify it, then put the images where the
compose files of the version find them.

Usage: clab server images load FILE [--registry HOST[/PATH]]

Options:
  --registry HOST[/PATH]  also copy our two images into this registry (with
                          the same digests, signatures and OpenVEX), for
                          CADENCE_REGISTRY or image.registry of Helm; the
                          credentials are those of docker login

It verifies the signature of checksums.txt of the release and of both our
images offline, with trusted_root.json of the archive, in a temporary
registry on 127.0.0.1 that it runs from the archive and removes in any
outcome. A signature that does not verify stops it before anything reaches
Docker or your registry. Without --registry the images are pulled from the
temporary registry into Docker and tagged as the compose files name them:
the pin by digest is then found locally with the containerd image store of
Docker; the classic overlay2 storage cannot keep it, and load refuses there.

The release goes to download/vX.Y.Z/ next to FILE: the mirror of the
releases for clab server install, get-cadence.sh and upgrade.sh
(CADENCE_RELEASES_URL=file://...). load changes no .env. It needs Docker,
regctl ` + regctlMinVersion + ` or later and, to verify, cosign 3.x (CADENCE_VERIFY: auto
warns without it, require refuses, off does not verify).`

// commandHelp — справка подкоманды clab server; у images своя справка у
// каждой из list, save и load.
func commandHelp(c serverCommand, args []string) string {
	if c.name != "images" {
		return c.help
	}
	for _, a := range args {
		switch a {
		case "list":
			return imagesListHelp
		case "save":
			return imagesSaveHelp
		case "load":
			return imagesLoadHelp
		}
	}
	return imagesHelp
}

func serverImages(ctx context.Context, e *serverEnv, args []string) error {
	if len(args) == 0 {
		return usageError{"clab server images needs a command: list, save or load (clab server images --help)"}
	}
	switch args[0] {
	case "list":
		return imagesList(ctx, e, args[1:])
	case "save":
		return imagesSave(ctx, e, args[1:])
	case "load":
		return imagesLoad(ctx, e, args[1:])
	}
	return usageError{fmt.Sprintf("unknown command %q of clab server images: list, save or load", args[0])}
}

// ── перечень ───────────────────────────────────────────────────────────────

// imageRef — ссылка образа compose по частям.
type imageRef struct {
	ref    string // как в compose
	domain string // ghcr.io, docker.io, ...
	path   string // путь в реестре: cadence-lab-tech/sdlc-pipeline-backend, library/alpine
	tag    string
	digest string // sha256:… или пусто
}

func parseImageRef(ref string) imageRef {
	r := imageRef{ref: ref}
	name := ref
	if at := strings.Index(name, "@"); at >= 0 {
		name, r.digest = name[:at], name[at+1:]
	}
	if c := strings.LastIndex(name, ":"); c > strings.LastIndex(name, "/") {
		name, r.tag = name[:c], name[c+1:]
	}
	if r.tag == "" {
		r.tag = "latest"
	}
	first, rest, ok := strings.Cut(name, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.domain, r.path = first, rest
	} else {
		r.domain, r.path = "docker.io", name
	}
	if r.domain == "docker.io" && !strings.Contains(r.path, "/") {
		r.path = "library/" + r.path
	}
	return r
}

// named — ссылка без digest: имя, под которым образ кладётся в Docker.
func (r imageRef) named() string {
	n, _, _ := strings.Cut(r.ref, "@")
	return n
}

// repo — последний элемент пути: sdlc-pipeline-backend.
func (r imageRef) repo() string { return r.path[strings.LastIndex(r.path, "/")+1:] }

// ours — образ релиза, чья подпись проверяется.
func (r imageRef) ours() bool {
	for _, repo := range verifyImageRepos {
		if r.repo() == repo {
			return true
		}
	}
	return false
}

// layout — каталог OCI-layout образа в архиве.
func (r imageRef) layout() string {
	return "images/" + strings.NewReplacer("/", "_", ":", "_").Replace(r.domain+"/"+r.path) + "_" + r.tag
}

// packageComposeFiles — файлы compose пакета в каталоге: docker-compose.yml
// и compose.*.yml, кроме своего compose.override.yml установки — он не
// часть пакета.
func packageComposeFiles(dir string) []string {
	files := []string{filepath.Join(dir, "docker-compose.yml")}
	more, _ := filepath.Glob(filepath.Join(dir, "compose.*.yml"))
	sort.Strings(more)
	for _, f := range more {
		if filepath.Base(f) != "compose.override.yml" {
			files = append(files, f)
		}
	}
	return files
}

// versionImagesSpec — docker compose config --images по всем файлам пакета
// и во всех профилях (db-client в compose.external-db.yml — профиль tools).
func versionImagesSpec(dir string, env []string, version string) cmdSpec {
	c := composeImagesSpec(dir, env, version, packageComposeFiles(dir)...)
	args := append([]string{}, c.args[:len(c.args)-2]...)
	c.args = append(args, "--profile", "*", "config", "--images")
	return c
}

// versionImages — перечень образов версии: compose пакета в каталоге dir и
// helperImage, без повторов, по порядку.
func versionImages(ctx context.Context, e *serverEnv, dir string, env []string, version string) ([]string, error) {
	images, err := composeImages(ctx, e, versionImagesSpec(dir, env, version))
	if err != nil {
		return nil, fmt.Errorf("could not read the images of %s from the compose files of its package (see above)", version)
	}
	return uniqueSorted(append(images, helperImage)), nil
}

// canonicalEnv — окружение для перечня архива: без зеркала реестра и без
// снятых pin. Архив несёт образы под теми именами, что у compose пакета по
// умолчанию, — их и ищет установка без CADENCE_REGISTRY.
func canonicalEnv(environ []string) []string {
	drop := map[string]bool{"CADENCE_REGISTRY": true, "CADENCE_BACKEND_DIGEST": true, "CADENCE_FRONTEND_DIGEST": true,
		"COMPOSE_FILE": true, "COMPOSE_PROFILES": true, "CADENCE_VERSION": true}
	var out []string
	for _, kv := range environ {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			out = append(out, kv)
		}
	}
	return out
}

func imagesList(ctx context.Context, e *serverEnv, args []string) error {
	var dir, version string
	pos, err := parseServerArgs(args, nil, map[string]*string{"--dir": &dir, "--version": &version})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{fmt.Sprintf("unexpected argument: %s", pos[0])}
	}
	if dir != "" && version != "" {
		return usageError{"--dir and --version go one at a time"}
	}
	if version != "" && !validVersion(version) {
		return usageError{fmt.Sprintf("'%s' is not a version: expected vX.Y.Z", version)}
	}
	// Перечень — в stdout по строке, остальное — в stderr: его читают
	// скриптом (xargs, цикл копирования в зеркало).
	quiet := *e
	quiet.out = e.errw
	var images []string
	if version != "" {
		v, err := newVerifier(ctx, &quiet, e.getenv("CADENCE_VERIFY"), "")
		if err != nil {
			return err
		}
		if err := requireDocker(ctx, e); err != nil {
			return err
		}
		work, err := os.MkdirTemp("", "clab-images.")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
		releases := releasesFrom(e.getenv("CADENCE_RELEASES_URL"))
		pkgDir, err := fetchRelease(ctx, &quiet, v, releases, version, filepath.Join(work, "download"), filepath.Join(work, "pkg"), "")
		if err != nil {
			return err
		}
		if images, err = versionImages(ctx, e, pkgDir, e.environ, version); err != nil {
			return err
		}
	} else {
		inst, err := openInstallation(e, dir)
		if err != nil {
			return err
		}
		if err := requireDocker(ctx, e); err != nil {
			return err
		}
		if images, err = versionImages(ctx, e, inst.dir, inst.composeEnv(), inst.version); err != nil {
			return err
		}
	}
	for _, img := range images {
		e.say("%s", img)
	}
	return nil
}

// fetchRelease скачивает релиз версии в каталог rel (пакет, checksums.txt,
// сверка суммы), проверяет подпись checksums.txt (verifyPackage кладёт бандл
// туда же) и распаковывает пакет в pkg. Возвращает pkg.
func fetchRelease(ctx context.Context, e *serverEnv, v *verifier, releases, version, rel, pkg, nothing string) (string, error) {
	if err := os.MkdirAll(rel, 0o755); err != nil {
		return "", err
	}
	name := packageName(version)
	e.info("downloading %s", name)
	tarball, err := downloadRelease(ctx, e, releases, version, rel)
	suffix := ""
	if nothing != "" {
		suffix = " " + nothing
	}
	switch {
	case errors.Is(err, errNoRelease):
		return "", fmt.Errorf("there is no release %s with a package at %s.%s", version, releases, suffix)
	case errors.Is(err, errChecksum):
		return "", fmt.Errorf("the checksum of %s does not match checksums.txt of %s: the download is broken or altered.%s", name, version, suffix)
	case err != nil:
		return "", fmt.Errorf("downloading %s failed: %v.%s", name, err, suffix)
	}
	if err := v.verifyPackage(ctx, releases, version, rel); err != nil {
		return "", err
	}
	if err := extractTarGz(tarball, pkg, nil); err != nil {
		return "", err
	}
	probe := &installation{dir: pkg}
	if err := probe.loadVersion(); err != nil {
		return "", fmt.Errorf("%s: %v", name, err)
	}
	if probe.version != version {
		return "", fmt.Errorf("VERSION in %s holds '%s', expected %s", name, probe.version, version)
	}
	return pkg, nil
}

// ── regctl ─────────────────────────────────────────────────────────────────

var regctlVersionPattern = regexp.MustCompile(`v?[0-9]+\.[0-9]+\.[0-9]+`)

// findRegctl — regctl в PATH и его версия, до любых действий: без него ни
// save, ни load ничего не делают.
func findRegctl(ctx context.Context, e *serverEnv, nothing string) (string, error) {
	where := fmt.Sprintf("install regctl %s or later (%s; docs/admin.md, \"Air-gapped installation\"). %s", regctlMinVersion, regctlURL, nothing)
	if _, err := lookPath("regctl"); err != nil {
		return "", fmt.Errorf("regctl is not installed: clab server images copies the images with it; %s", where)
	}
	out, _ := output(ctx, e.run, cmdSpec{name: "regctl", args: []string{"version", "--format", "{{.VCSTag}}"}})
	ver := regctlVersionPattern.FindString(out)
	if ver == "" {
		return "", fmt.Errorf("regctl of an unknown version (%q) is not %s or later; %s", strings.TrimSpace(out), regctlMinVersion, where)
	}
	if !strings.HasPrefix(ver, "v") {
		ver = "v" + ver
	}
	if versionLess(ver, regctlMinVersion) {
		return "", fmt.Errorf("regctl %s is older than %s; %s", ver, regctlMinVersion, where)
	}
	return ver, nil
}

// regctl — вызов regctl с окружением env; при ошибке — хвост его вывода к
// человеку.
func regctl(ctx context.Context, e *serverEnv, env []string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	err := e.run.run(ctx, cmdSpec{name: "regctl", args: args, env: env, stdout: &out, stderr: &errb})
	if err != nil {
		(&verifier{e: e}).tail(errb.String() + out.String())
	}
	return out.String(), err
}

// cosignVersion — строка версии cosign для манифеста архива.
func cosignVersion(ctx context.Context, e *serverEnv) string {
	var out bytes.Buffer
	_ = e.run.run(ctx, cmdSpec{name: "cosign", args: []string{"version"}, stdout: &out, stderr: &out})
	if m := cosignGitVersion.FindStringSubmatch(out.String()); m != nil {
		return m[1]
	}
	return ""
}

// ── архив ──────────────────────────────────────────────────────────────────

// imagesArchive — манифест архива (cadence-images.json).
type imagesArchive struct {
	Format      int             `json:"format"`
	Version     string          `json:"version"`
	Created     string          `json:"created"`
	Clab        string          `json:"clab"`
	Regctl      string          `json:"regctl"`
	Cosign      string          `json:"cosign,omitempty"`
	TrustedRoot string          `json:"trusted_root,omitempty"` // откуда взят
	Images      []archiveImage  `json:"images"`
	Registry    archiveRegistry `json:"registry"`
}

type archiveImage struct {
	Ref    string `json:"ref"` // как в compose пакета
	Layout string `json:"layout"`
	Tag    string `json:"tag"`
	Digest string `json:"digest"` // на момент сохранения
}

type archiveRegistry struct {
	Image string            `json:"image"`
	Name  string            `json:"name"`
	Files map[string]string `json:"files"` // платформа → tar для docker load
}

func imagesSave(ctx context.Context, e *serverEnv, args []string) error {
	var version, output string
	pos, err := parseServerArgs(args, nil, map[string]*string{"--version": &version, "--output": &output})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{fmt.Sprintf("unexpected argument: %s (the archive goes with --output)", pos[0])}
	}
	if version == "" {
		return usageError{"clab server images save needs --version vX.Y.Z"}
	}
	if !validVersion(version) {
		return usageError{fmt.Sprintf("'%s' is not a version: expected vX.Y.Z", version)}
	}
	if output == "" {
		output = "cadence-" + version + "-airgap.tar"
	}
	if !filepath.IsAbs(output) {
		output = filepath.Join(e.cwd, output)
	}
	output = filepath.Clean(output)
	if _, err := os.Lstat(output); err == nil {
		return fmt.Errorf("%s exists: it is not overwritten; pick another --output. %s", output, nothingSaved)
	}

	// --- 1. инструменты и режим: до сети и Docker ------------------------------
	regVer, err := findRegctl(ctx, e, nothingSaved)
	if err != nil {
		return err
	}
	v, err := newVerifier(ctx, e, e.getenv("CADENCE_VERIFY"), nothingSaved)
	if err != nil {
		return err
	}
	if v.mode != "off" && !v.cosign {
		return fmt.Errorf("%s: save verifies the release and puts trusted_root.json of Sigstore into the archive with cosign 3.x (%s), or set CADENCE_VERIFY=off to save without them. %s",
			v.why, verifyCosignURL, nothingSaved)
	}
	if err := requireDocker(ctx, e); err != nil {
		return err
	}

	// --- 2. релиз: пакет, подпись, ассеты ----------------------------------------
	work, err := os.MkdirTemp(filepath.Dir(output), ".clab-images.")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %v. %s", output, err, nothingSaved)
	}
	defer os.RemoveAll(work)
	tree := filepath.Join(work, "archive")
	releases := releasesFrom(e.getenv("CADENCE_RELEASES_URL"))
	rel := filepath.Join(tree, "download", version)
	pkgDir, err := fetchRelease(ctx, e, v, releases, version, rel, filepath.Join(work, "pkg"), nothingSaved)
	if err != nil {
		return err
	}
	// Остальные ассеты релиза — в раскладке зеркала: get-cadence.sh всегда,
	// бандл подписи — если verifyPackage его не взял (off, версия до подписей).
	if err := e.fetch(ctx, releases+"/download/"+version+"/get-cadence.sh", filepath.Join(rel, "get-cadence.sh")); err != nil {
		return fmt.Errorf("the release %s has no get-cadence.sh at %s. %s", version, releases, nothingSaved)
	}
	if !isFile(filepath.Join(rel, verifyBundle)) {
		if err := e.fetch(ctx, releases+"/download/"+version+"/"+verifyBundle, filepath.Join(rel, verifyBundle)); err != nil && !errors.Is(err, errNoRelease) {
			return fmt.Errorf("downloading %s of %s failed: %v. %s", verifyBundle, version, err, nothingSaved)
		}
	}

	// --- 3. перечень и подписи образов --------------------------------------------
	env := canonicalEnv(e.environ)
	images, err := versionImages(ctx, e, pkgDir, env, version)
	if err != nil {
		return fmt.Errorf("%v. %s", err, nothingSaved)
	}
	if err := v.verifyImages(ctx, version, versionImagesSpec(pkgDir, env, version)); err != nil {
		return err
	}
	manifest := imagesArchive{Format: archiveFormat, Version: version, Created: e.now().UTC().Format(time.RFC3339),
		Clab: e.cli, Regctl: regVer,
		Registry: archiveRegistry{Image: airgapRegistryImage, Name: airgapRegistryName, Files: map[string]string{}}}
	if v.mode != "off" {
		manifest.Cosign = cosignVersion(ctx, e)
		root, err := sigstoreRoot(ctx, e, work)
		if err != nil {
			return err
		}
		if err := copyFile(root, filepath.Join(rel, trustedRootAsset), 0o644); err != nil {
			return err
		}
		manifest.TrustedRoot = "https://tuf-repo-cdn.sigstore.dev (cosign initialize)"
		e.ok("trusted_root.json of Sigstore taken through TUF for offline verification")
	}

	// --- 4. образы в OCI-layout -------------------------------------------------
	for _, img := range images {
		r := parseImageRef(img)
		dst := "ocidir://" + filepath.Join(tree, filepath.FromSlash(r.layout())) + ":" + r.tag
		e.info("copying %s", img)
		if _, err := regctl(ctx, e, nil, "image", "copy", "--referrers", "--digest-tags", img, dst); err != nil {
			return fmt.Errorf("regctl could not copy %s (see above): check that its registry is reachable from this host. %s", img, nothingSaved)
		}
		digest := r.digest
		if digest == "" {
			out, err := regctl(ctx, e, nil, "image", "digest", dst)
			if err != nil {
				return fmt.Errorf("regctl could not read the digest of %s in the archive (see above). %s", img, nothingSaved)
			}
			digest = strings.TrimSpace(out)
		}
		manifest.Images = append(manifest.Images, archiveImage{Ref: img, Layout: r.layout(), Tag: r.tag, Digest: digest})
	}
	e.info("copying the image of the temporary registry for load")
	for _, p := range airgapPlatforms {
		file := "registry/registry-" + strings.ReplaceAll(p, "/", "-") + ".tar"
		if err := os.MkdirAll(filepath.Join(tree, "registry"), 0o755); err != nil {
			return err
		}
		if _, err := regctl(ctx, e, nil, "image", "export", "--platform", p, "--name", airgapRegistryName,
			airgapRegistryImage, filepath.Join(tree, filepath.FromSlash(file))); err != nil {
			return fmt.Errorf("regctl could not export %s for %s (see above). %s", airgapRegistryImage, p, nothingSaved)
		}
		manifest.Registry.Files[p] = file
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tree, archiveManifest), append(raw, '\n'), 0o644); err != nil {
		return err
	}

	// --- 5. один tar: временный файл рядом и переименование ------------------------
	e.info("writing %s", output)
	sum, size, err := writeArchive(tree, output)
	if err != nil {
		return fmt.Errorf("writing %s failed: %v. %s", output, err, nothingSaved)
	}
	e.ok("saved the images and the release of %s", version)
	e.say("")
	e.kv("  Archive:", output, "  Size:", humanSize(size), "  SHA-256:", sum)
	e.say("")
	e.say("Images (digests as saved):")
	for _, img := range manifest.Images {
		e.say("  %s  %s", img.Ref, img.Digest)
	}
	e.say("")
	e.say("Carry the archive to the closed machine, compare its SHA-256 there with the one above")
	e.say("(sha256sum %s), then load it:", shellQuote(filepath.Base(output)))
	e.say("  clab server images load %s   # or --registry HOST[/PATH] for your registry", shellQuote(filepath.Base(output)))
	return nil
}

// sigstoreRoot — trusted_root.json Sigstore через TUF во временном
// каталоге (TUF_ROOT): кэш TUF администратора (~/.sigstore) не меняется.
func sigstoreRoot(ctx context.Context, e *serverEnv, work string) (string, error) {
	tuf := filepath.Join(work, "tuf")
	var errb bytes.Buffer
	c := cmdSpec{name: "cosign", args: []string{"initialize"}, env: append(append([]string{}, e.environ...), "TUF_ROOT="+tuf),
		stdout: io.Discard, stderr: &errb}
	root := filepath.Join(tuf, "tuf-repo-cdn.sigstore.dev", "targets", trustedRootAsset)
	if err := e.run.run(ctx, c); err != nil || !isFile(root) {
		(&verifier{e: e}).tail(errb.String())
		return "", fmt.Errorf("cosign initialize could not fetch trusted_root.json of Sigstore (see above): check that tuf-repo-cdn.sigstore.dev is reachable from this host. %s", nothingSaved)
	}
	return root, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return writeFile(dst, in, mode)
}

// writeArchive — каталог tree одним несжатым tar в output: во временный
// файл в том же каталоге и переименование, чтобы обрыв не оставил
// половину архива под настоящим именем. Возвращает sha256 и размер.
func writeArchive(tree, output string) (string, int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(output), "."+filepath.Base(output)+".*.partial")
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(tmp, h))
	fail := func(err error) (string, int64, error) {
		tw.Close()
		tmp.Close()
		os.Remove(tmp.Name())
		return "", 0, err
	}
	err = filepath.WalkDir(tree, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == tree {
			return err
		}
		rel, err := filepath.Rel(tree, p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return fmt.Errorf("%s: not a regular file", rel)
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			hdr.Name += "/"
			hdr.Mode = 0o755
		} else {
			hdr.Mode = 0o644
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return fail(err)
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	fi, err := os.Stat(tmp.Name())
	if err != nil {
		return fail(err)
	}
	if _, err := os.Lstat(output); err == nil {
		os.Remove(tmp.Name())
		return "", 0, fmt.Errorf("%s appeared while the archive was written", output)
	}
	if err := os.Rename(tmp.Name(), output); err != nil {
		os.Remove(tmp.Name())
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), fi.Size(), nil
}

// ── load ───────────────────────────────────────────────────────────────────

var registryPattern = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)

func imagesLoad(ctx context.Context, e *serverEnv, args []string) error {
	var registry string
	pos, err := parseServerArgs(args, nil, map[string]*string{"--registry": &registry})
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{"clab server images load needs one archive: clab server images load FILE [--registry HOST[/PATH]]"}
	}
	registry = strings.TrimRight(registry, "/")
	if registry != "" && !registryPattern.MatchString(registry) {
		return usageError{fmt.Sprintf("--registry '%s' is not HOST[:PORT][/PATH] (no scheme, no tag)", registry)}
	}
	file := pos[0]
	if !filepath.IsAbs(file) {
		file = filepath.Join(e.cwd, file)
	}
	file = filepath.Clean(file)
	if !isFile(file) {
		return fmt.Errorf("%s is not a file. %s", file, nothingLoaded)
	}

	// --- 1. инструменты и режим: до любых действий ---------------------------------
	if _, err := findRegctl(ctx, e, nothingLoaded); err != nil {
		return err
	}
	v, err := newVerifier(ctx, e, e.getenv("CADENCE_VERIFY"), nothingLoaded)
	if err != nil {
		return err
	}
	if err := requireDocker(ctx, e); err != nil {
		return err
	}

	// --- 2. архив: распаковка, версия, пакет -------------------------------------
	work, err := os.MkdirTemp("", "clab-images-load.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tree := filepath.Join(work, "archive")
	e.info("unpacking %s", filepath.Base(file))
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	err = extractTar(f, file, tree, nil)
	f.Close()
	if err != nil {
		return fmt.Errorf("%v: not an archive of clab server images save. %s", err, nothingLoaded)
	}
	var manifest imagesArchive
	raw, err := os.ReadFile(filepath.Join(tree, archiveManifest))
	if err == nil {
		err = json.Unmarshal(raw, &manifest)
	}
	if err != nil || manifest.Format != archiveFormat || !validVersion(manifest.Version) {
		return fmt.Errorf("%s has no readable %s of format %d: not an archive of clab server images save. %s", file, archiveManifest, archiveFormat, nothingLoaded)
	}
	version := manifest.Version
	if versionLess(e.cli, version) {
		pkg := "download/" + version + "/" + packageName(version)
		return fmt.Errorf("the archive holds %s, newer than this clab %s: it may not know how to load it.\n"+
			"  Use the clab of the package in the archive:\n"+
			"    tar -xf %s %s && tar -xzf %s bin/clab-<os>-<arch>\n"+
			"    bin/clab-<os>-<arch> server images load %s. %s",
			version, e.cli, shellQuote(file), pkg, pkg, shellQuote(file), nothingLoaded)
	}
	rel := filepath.Join(tree, "download", version)
	root := filepath.Join(rel, trustedRootAsset)
	if !isFile(root) {
		root = ""
	}
	// Без корня проверить без сети нечем: require отказывает до действий,
	// auto говорит один раз.
	skip, err := v.skip(version)
	if err != nil {
		return err
	}
	if !skip && root == "" {
		if v.mode == "require" {
			return fmt.Errorf("the archive has no %s (saved with CADENCE_VERIFY=off?): its signatures cannot be verified offline, and CADENCE_VERIFY=require refuses it. %s", trustedRootAsset, nothingLoaded)
		}
		v.note("the archive has no %s (saved with CADENCE_VERIFY=off?): its signatures are not verified", trustedRootAsset)
		skip = true
	}
	check := filepath.Join(work, "check")
	if err := os.MkdirAll(check, 0o755); err != nil {
		return err
	}
	tarball, err := downloadRelease(ctx, e, "file://"+filepath.Join(tree), version, check)
	if err != nil {
		return fmt.Errorf("the package of %s in the archive is missing or does not match its checksums.txt: the archive is broken or altered. %s", version, nothingLoaded)
	}
	if !skip {
		if err := v.verifyChecksumsOffline(ctx, version, rel, root); err != nil {
			return err
		}
	}
	pkgDir := filepath.Join(work, "pkg")
	if err := extractTarGz(tarball, pkgDir, nil); err != nil {
		return err
	}
	// Перечень — из подписанного пакета: pin наших образов берутся из его
	// docker-compose.yml, а не из манифеста архива.
	images, err := versionImages(ctx, e, pkgDir, canonicalEnv(e.environ), version)
	if err != nil {
		return fmt.Errorf("%v. %s", err, nothingLoaded)
	}
	byRef := map[string]archiveImage{}
	for _, img := range manifest.Images {
		byRef[img.Ref] = img
	}
	type loadImage struct {
		r      imageRef
		layout string
		digest string
	}
	var plan []loadImage
	for _, img := range images {
		a, ok := byRef[img]
		r := parseImageRef(img)
		if !ok || (r.digest != "" && a.Digest != r.digest) {
			return fmt.Errorf("the archive does not hold %s, which the package of %s runs: the archive is broken or altered. %s", img, version, nothingLoaded)
		}
		plan = append(plan, loadImage{r: r, layout: filepath.Join(tree, filepath.FromSlash(a.Layout)) + ":" + a.Tag, digest: a.Digest})
	}
	// Наши — первыми: на них видно, держит ли хранилище Docker pin.
	sort.SliceStable(plan, func(i, j int) bool { return plan[i].r.ours() && !plan[j].r.ours() })

	// Зеркало релизов рядом с архивом — до образов: без него установка не
	// пойдёт, а отказ сейчас ничего не оставляет в Docker.
	mirror := filepath.Dir(file)
	if err := writeMirror(rel, filepath.Join(mirror, "download", version)); err != nil {
		return fmt.Errorf("cannot put the release of %s next to the archive (%s): %v. Copy the archive to a writable directory. %s",
			version, filepath.Join(mirror, "download", version), err, nothingLoaded)
	}

	// --- 3. временный реестр -----------------------------------------------------
	reg, err := startAirgapRegistry(ctx, e, tree, manifest.Registry)
	if err != nil {
		return err
	}
	defer reg.stop()
	// http до временного реестра — флагом --host на каждый вызов, а не
	// regctl registry set: конфиг regctl администратора не пишется, а его
	// настройки (TLS своего реестра) и docker login работают для --registry.
	host := "--host=reg=" + reg.addr + ",tls=disabled"
	for _, img := range plan {
		e.info("copying %s into the temporary registry", img.r.named())
		if _, err := regctl(ctx, e, nil, "image", "copy", host, "--referrers", "ocidir://"+img.layout, reg.addr+"/"+img.r.path+":"+img.r.tag); err != nil {
			return fmt.Errorf("regctl could not copy %s from the archive into the temporary registry (see above). %s", img.r.named(), nothingLoaded)
		}
	}
	if !skip {
		for _, img := range plan {
			if !img.r.ours() {
				continue
			}
			if err := v.verifyImageOffline(ctx, version, img.r.repo(), reg.addr+"/"+img.r.path+"@"+img.digest, root, "the archive"); err != nil {
				return err
			}
		}
	}

	// --- 4. в свой реестр ----------------------------------------------------------
	var pushed []string
	for _, img := range plan {
		if registry == "" || !img.r.ours() {
			continue
		}
		dst := registry + "/" + img.r.repo() + ":" + img.r.tag
		e.info("copying %s to %s", img.r.named(), dst)
		if _, err := regctl(ctx, e, nil, "image", "copy", host, "--referrers", "--digest-tags", reg.addr+"/"+img.r.path+"@"+img.digest, dst); err != nil {
			done := ""
			if len(pushed) > 0 {
				done = " Already copied: " + strings.Join(pushed, ", ") + "."
			}
			return fmt.Errorf("regctl could not copy %s to %s (see above): check that the registry is reachable and docker login to it is done.%s Nothing was loaded into Docker.", img.r.named(), registry, done)
		}
		pushed = append(pushed, dst+"@"+img.digest)
		e.ok("%s@%s in %s", dst, img.digest, registry)
	}

	// --- 5. в Docker этой машины -----------------------------------------------------
	var added []string
	undo := func() {
		bg := context.WithoutCancel(ctx)
		for _, ref := range added {
			_ = e.run.run(bg, cmdSpec{name: "docker", args: []string{"rmi", ref}, stdout: io.Discard, stderr: io.Discard})
		}
	}
	for _, img := range plan {
		if registry != "" && img.r.ours() {
			continue
		}
		named := img.r.named()
		// Ссылка, которую ищет compose: наш — с pin, сторонний — по тегу.
		want := img.r.ref
		// Тот же образ уже здесь — повтор load ничего не добавляет. У
		// стороннего сверяется содержимое архива, а не тег: под тем же
		// тегом может лежать другая сборка.
		exact := want
		if img.r.digest == "" {
			exact = strings.TrimSuffix(named, ":"+img.r.tag) + "@" + img.digest
		}
		if dockerImagePresent(ctx, e, exact) && dockerImagePresent(ctx, e, want) {
			e.ok("%s is already on this machine", want)
			continue
		}
		src := reg.addr + "/" + img.r.path + "@" + img.digest
		hadNamed := dockerImagePresent(ctx, e, named)
		if err := e.run.run(ctx, cmdSpec{name: "docker", args: []string{"pull", "-q", src}, stdout: io.Discard, stderr: e.errw}); err != nil {
			undo()
			return fmt.Errorf("docker could not pull %s from the temporary registry (see above). %s", named, nothingLoaded)
		}
		err := e.run.run(ctx, cmdSpec{name: "docker", args: []string{"tag", src, named}, stdout: io.Discard, stderr: e.errw})
		// Ссылка временного реестра — только своя, под ней ничего не жило.
		_ = e.run.run(context.WithoutCancel(ctx), cmdSpec{name: "docker", args: []string{"rmi", src}, stdout: io.Discard, stderr: io.Discard})
		if err != nil {
			undo()
			return fmt.Errorf("docker could not tag %s (see above). %s", named, nothingLoaded)
		}
		if !hadNamed {
			added = append(added, named)
		}
		if !dockerImagePresent(ctx, e, want) {
			undo()
			return fmt.Errorf("after docker pull and docker tag, %s is not found by its digest: the image storage of this Docker does not keep the digest of a pulled-and-tagged image\n"+
				"  (the classic overlay2 storage; docker info shows no driver-type io.containerd.snapshotter.v1), and compose would pull it from the network.\n"+
				"  Turn on the containerd image store (https://docs.docker.com/engine/storage/containerd/) and load again,\n"+
				"  or load into a registry of this network: clab server images load %s --registry HOST[/PATH]. %s",
				want, shellQuote(file), nothingLoaded)
		}
		e.ok("%s", want)
	}

	// --- 6. итог ---------------------------------------------------------------------
	e.say("")
	e.ok("loaded the images of %s; the release is in %s", version, filepath.Join(mirror, "download", version))
	releasesURL := "file://" + mirror
	prefix := "CADENCE_RELEASES_URL=" + shellQuote(releasesURL)
	if registry != "" {
		prefix = "CADENCE_REGISTRY=" + registry + " " + prefix
		e.say("Our images are in %s: set CADENCE_REGISTRY=%s (Helm: --set image.registry=%s);", registry, registry, registry)
		e.say("the other images are in Docker of this machine.")
	}
	e.say("Next, install %s:", version)
	e.say("  %s %s server install --version %s [--dir DIR] [--mode ...]", prefix, shellQuote(e.self), version)
	e.say("or upgrade an installation to it, in its directory:")
	e.say("  %s ./upgrade.sh %s", prefix, version)
	return nil
}

// writeMirror — ассеты релиза из архива в каталог зеркала релизов.
func writeMirror(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, en := range entries {
		if !en.Type().IsRegular() {
			continue
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(en.Name(), ".sh") {
			mode = 0o755
		}
		if err := copyFile(filepath.Join(src, en.Name()), filepath.Join(dst, en.Name()), mode); err != nil {
			return err
		}
	}
	return nil
}

func dockerImagePresent(ctx context.Context, e *serverEnv, ref string) bool {
	return dockerImageID(ctx, e, ref) != ""
}

// dockerImageID — ID образа в Docker этой машины; пусто — его нет.
func dockerImageID(ctx context.Context, e *serverEnv, ref string) string {
	out, err := output(ctx, e.run, cmdSpec{name: "docker", args: []string{"image", "inspect", "--format", "{{.Id}}", ref}})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// airgapRegistry — временный реестр load: контейнер на 127.0.0.1.
type airgapRegistry struct {
	e         *serverEnv
	ctx       context.Context
	name      string
	addr      string // 127.0.0.1:PORT
	dropImage bool   // образ загружен этим load: убрать вместе с контейнером
}

// startAirgapRegistry грузит образ реестра своей платформы из архива и
// запускает его на свободном порту 127.0.0.1. Контейнер и его том
// убирает stop — в любом исходе, в том числе после SIGINT (контекст
// команды отменён, уборка — без него).
func startAirgapRegistry(ctx context.Context, e *serverEnv, tree string, spec archiveRegistry) (*airgapRegistry, error) {
	out, err := output(ctx, e.run, cmdSpec{name: "docker", args: []string{"version", "--format", "{{.Server.Arch}}"}})
	arch := strings.TrimSpace(out)
	file, ok := spec.Files["linux/"+arch]
	if err != nil || !ok || spec.Name == "" {
		return nil, fmt.Errorf("the archive has no image of the temporary registry for linux/%s (Docker of this machine). %s", arch, nothingLoaded)
	}
	reg := &airgapRegistry{e: e, ctx: context.WithoutCancel(ctx), name: "clab-images-load-" + randomSuffix()}
	reg.dropImage = !dockerImagePresent(ctx, e, spec.Name)
	if err := e.run.run(ctx, cmdSpec{name: "docker", args: []string{"load", "-q", "-i", filepath.Join(tree, filepath.FromSlash(file))},
		stdout: io.Discard, stderr: e.errw}); err != nil {
		reg.stop()
		return nil, fmt.Errorf("docker could not load the image of the temporary registry from the archive (see above). %s", nothingLoaded)
	}
	if err := e.run.run(ctx, cmdSpec{name: "docker", args: []string{"run", "-d", "--name", reg.name, "--label", airgapLabel + "=1",
		"-p", "127.0.0.1::5000", spec.Name}, stdout: io.Discard, stderr: e.errw}); err != nil {
		reg.stop()
		return nil, fmt.Errorf("docker could not start the temporary registry (see above). %s", nothingLoaded)
	}
	out, err = output(ctx, e.run, cmdSpec{name: "docker", args: []string{"port", reg.name, "5000/tcp"}})
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "127.0.0.1:") {
			reg.addr = strings.TrimSpace(line)
			break
		}
	}
	if err != nil || reg.addr == "" {
		reg.stop()
		return nil, fmt.Errorf("the temporary registry has no port on 127.0.0.1. %s", nothingLoaded)
	}
	for i := 0; ; i++ {
		if e.reachable(ctx, "http://"+reg.addr+"/v2/") {
			break
		}
		if i == 30 || ctx.Err() != nil {
			reg.stop()
			return nil, fmt.Errorf("the temporary registry on %s does not answer. %s", reg.addr, nothingLoaded)
		}
		e.sleep(time.Second)
	}
	e.info("temporary registry on %s", reg.addr)
	return reg, nil
}

func (r *airgapRegistry) stop() {
	_ = r.e.run.run(r.ctx, cmdSpec{name: "docker", args: []string{"rm", "-f", "-v", r.name}, stdout: io.Discard, stderr: io.Discard})
	if r.dropImage {
		_ = r.e.run.run(r.ctx, cmdSpec{name: "docker", args: []string{"rmi", airgapRegistryName}, stdout: io.Discard, stderr: io.Discard})
	}
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── pull в install и upgrade ────────────────────────────────────────────────

// pullImages — docker compose pull образов установки, кроме тех, что уже
// лежат в Docker этой машины ровно по ссылке compose (CLAB-202): в закрытом
// контуре после clab server images load pull всех образов упал бы на
// первом же реестре, а с --registry — на сторонних образах Docker Hub.
// Нет ни одного локально — прежний pull всех; все есть — pull не нужен;
// часть — pull --policy missing.
func (i *installation) pullImages(ctx context.Context) error {
	images, err := output(ctx, i.e.run, i.compose("config", "--images"))
	var present, missing int
	if err == nil {
		for _, img := range uniqueSorted(strings.Fields(images)) {
			if _, err := output(ctx, i.e.run, i.docker("image", "inspect", "--format", "{{.Id}}", img)); err == nil {
				present++
			} else {
				missing++
			}
		}
	}
	switch {
	case present > 0 && missing == 0:
		i.e.info("all %d images are on this machine: nothing to pull", present)
		return nil
	case present > 0:
		return i.composeRun(ctx, false, "pull", "--quiet", "--policy", "missing")
	}
	return i.composeRun(ctx, false, "pull", "--quiet")
}
