package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Тесты проверки подписей clab server (CLAB-197): подставной cosign
// (fakeRunner.runCosign), compose отвечает образами файлов пакета
// (fakeComposeImages), ghcr.io «отвечает» или нет (fakeRunner.ghcrDown).
// Строки таблицы режимов — те же, что ci/check-verify.sh и
// ci/check-get-cadence.sh гоняют для скриптов, с теми же текстами.

const (
	testBackendPin  = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testFrontendPin = "sha256:" + "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

// signedCompose — docker-compose.yml пакета в миниатюре: образы с pin по
// digest, как у настоящего.
const signedCompose = `name: cadence
x-image-backend: &image-backend
  image: ${CADENCE_REGISTRY:-ghcr.io/cadence-lab-tech}/sdlc-pipeline-backend:${CADENCE_VERSION:?CADENCE_VERSION is not set}${CADENCE_BACKEND_DIGEST-@` + testBackendPin + `}
services:
  api:
    <<: *image-backend
  worker:
    <<: *image-backend
  ui:
    image: ${CADENCE_REGISTRY:-ghcr.io/cadence-lab-tech}/sdlc-pipeline-frontend:${CADENCE_VERSION:?CADENCE_VERSION is not set}${CADENCE_FRONTEND_DIGEST-@` + testFrontendPin + `}
  caddy:
    image: caddy:2-alpine
`

func signedFiles(v string) map[string]string {
	return pkgFiles(v, map[string]string{
		".env.example":            testEnvExample,
		"docker-compose.yml":      signedCompose,
		"compose.external-db.yml": "services: {}\n",
	})
}

// publishSigned — релиз версии с бандлом подписи рядом (bundle=false —
// зеркало, которое его потеряло).
func publishSigned(t *testing.T, releases, v string, latest, bundle bool) {
	t.Helper()
	publish(t, releases, v, signedFiles(v), latest)
	if !bundle {
		return
	}
	dirs := []string{filepath.Join(releases, "download", v)}
	if latest {
		dirs = append(dirs, filepath.Join(releases, "latest", "download"))
	}
	for _, d := range dirs {
		mustWrite(t, filepath.Join(d, verifyBundle), `{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json"}`, 0o644)
	}
}

// verifyInstallSetup — релизы v0.12.0 (последний, подписан), v0.12.1 без
// бандла, v0.8.0 до подписей; clab v0.12.1, cosign 3.x в PATH.
func verifyInstallSetup(t *testing.T, vars map[string]string) *installFixture {
	t.Helper()
	f := installSetup(t, vars)
	publishSigned(t, f.releases, "v0.12.0", true, true)
	publishSigned(t, f.releases, "v0.12.1", false, false)
	publish(t, f.releases, "v0.8.0", pkgFiles("v0.8.0", map[string]string{".env.example": testEnvExample}), false)
	f.e.cli = "v0.12.1"
	f.r.cosign = true
	return f
}

// cosignCalls — вызовы cosign с аргументами.
func cosignCalls(r *fakeRunner) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, "cosign ") && c != "cosign version" {
			out = append(out, c)
		}
	}
	return out
}

func indexOf(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func countLines(s, prefix string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

var proxyYes = []string{"--mode", "proxy", "--yes"}

// AC-3, AC-6: подпись пакета до распаковки, образы по digest из compose в
// ghcr.io с identity своего release.yml на теге.
func TestVerifyInstallSigned(t *testing.T) {
	f := verifyInstallSetup(t, nil)
	if code := f.install(append([]string{"--dir", f.dir("cadence")}, proxyYes...)...); code != 0 {
		t.Fatalf("install: %d\n%s", code, f.errw)
	}
	calls := cosignCalls(f.r)
	blob := indexOf(calls, "cosign verify-blob --bundle ")
	if blob < 0 || !strings.Contains(calls[blob], "/"+verifyBundle+" --certificate-identity "+verifyPackageIdentity+
		" --certificate-oidc-issuer "+verifyIssuer+" ") || !strings.HasSuffix(calls[blob], "/checksums.txt") {
		t.Errorf("verify-blob of checksums.txt: %v", calls)
	}
	for _, img := range []struct{ repo, pin string }{{"sdlc-pipeline-backend", testBackendPin}, {"sdlc-pipeline-frontend", testFrontendPin}} {
		want := "cosign verify --certificate-identity " + verifyIdentity(img.repo, "v0.12.0") + " --certificate-oidc-issuer " + verifyIssuer +
			" " + verifyRegistry + "/" + img.repo + "@" + img.pin
		if i := indexOf(calls, want); i < 0 || i < blob {
			t.Errorf("no %q after verify-blob: %v", want, calls)
		}
		if !strings.Contains(f.out.String(), "✓ "+verifyRegistry+"/"+img.repo+"@"+img.pin+" is signed by "+verifyIdentity(img.repo, "v0.12.0")) {
			t.Errorf("output lacks the ✓ of %s:\n%s", img.repo, f.out)
		}
	}
	if len(calls) != 3 {
		t.Errorf("api and worker run one image: three checks, got %v", calls)
	}
	if !strings.Contains(f.out.String(), "✓ the signature of checksums.txt of v0.12.0 verified: signed by "+verifyPackageIdentity) {
		t.Errorf("output:\n%s", f.out)
	}
	// Зеркало реестра: проверяется тот же digest в ghcr.io.
	g := verifyInstallSetup(t, map[string]string{"CADENCE_REGISTRY": "localhost:5050"})
	if code := g.install(append([]string{"--dir", g.dir("cadence")}, proxyYes...)...); code != 0 {
		t.Fatalf("mirror: %d\n%s", code, g.errw)
	}
	for _, c := range cosignCalls(g.r) {
		if strings.Contains(c, "localhost:5050") {
			t.Errorf("verified at the mirror: %s", c)
		}
	}
	if !strings.Contains(g.out.String(), "✓ ghcr.io/cadence-lab-tech/sdlc-pipeline-backend@"+testBackendPin) {
		t.Errorf("mirror output:\n%s", g.out)
	}
}

// AC-1, AC-7: неверная подпись — отказ до каталога и до .env; образ с
// --external-db — раньше предпроверки базы.
func TestVerifyInstallBadSignature(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		f := verifyInstallSetup(t, map[string]string{"CADENCE_VERIFY": mode})
		f.r.cosignBad = "package"
		if code := f.install(append([]string{"--dir", f.dir("there")}, proxyYes...)...); code != 1 ||
			!strings.Contains(f.errw.String(), "the signature of checksums.txt of v0.12.0 did not verify for "+verifyPackageIdentity+": the release is altered or not ours.") ||
			!strings.Contains(f.errw.String(), "Nothing was installed.") {
			t.Errorf("%s, bad package: %d\n%s", mode, code, f.errw)
		}
		if _, err := os.Stat(f.dir("there")); err == nil {
			t.Errorf("%s: the directory was created", mode)
		}
		f.tmpEmpty()
	}

	f := verifyInstallSetup(t, nil)
	f.r.cosignBad = "sdlc-pipeline-backend"
	d := f.dir("cadence")
	args := []string{"--dir", d, "--external-db", "postgres://u:p@db.example.com:5432/cadence?sslmode=verify-full", "--mode", "proxy", "--yes"}
	if code := f.install(args...); code != 1 ||
		!strings.Contains(f.errw.String(), "the signature of "+verifyRegistry+"/sdlc-pipeline-backend@"+testBackendPin+
			" (sdlc-pipeline-backend v0.12.0) did not verify for "+verifyIdentity("sdlc-pipeline-backend", "v0.12.0")) ||
		!strings.Contains(f.errw.String(), "no matching signatures") {
		t.Errorf("bad image: %d\n%s", code, f.errw)
	}
	f.noEnv(d)
	if f.r.calledContaining("dbcheck") {
		t.Error("the precheck ran the image before its signature")
	}
	// Ghcr.io отвечает — это подпись, а не сеть: и auto отказывает.
	f.r.cosignBad = ""
	if code := f.install(args...); code != 0 {
		t.Errorf("the next run with a good signature: %d\n%s", code, f.errw)
	}
}

// AC-2: нет cosign — auto предупреждает один раз, require отказывает до
// Docker; off — без cosign и без предупреждений; незнакомое значение —
// отказ; cosign 2.x — как нет.
func TestVerifyInstallModes(t *testing.T) {
	f := verifyInstallSetup(t, nil)
	f.r.cosign = false
	if code := f.install(append([]string{"--dir", f.dir("cadence")}, proxyYes...)...); code != 0 {
		t.Fatalf("auto without cosign: %d\n%s", code, f.errw)
	}
	if n := countLines(f.errw.String(), "! cosign"); n != 1 ||
		!strings.Contains(f.errw.String(), "! cosign 3.x is not installed: the signatures of the release are not verified (CADENCE_VERIFY=auto)") {
		t.Errorf("auto without cosign: %d warnings\n%s", n, f.errw)
	}

	g := verifyInstallSetup(t, map[string]string{"CADENCE_VERIFY": "require"})
	g.r.cosign = false
	if code := g.install(append([]string{"--dir", g.dir("cadence")}, proxyYes...)...); code != 1 ||
		!strings.Contains(g.errw.String(), "CADENCE_VERIFY=require, but cosign 3.x is not installed: install cosign 3.x ("+verifyCosignURL+"), or set CADENCE_VERIFY=auto. Nothing was installed.") {
		t.Errorf("require without cosign: %d\n%s", code, g.errw)
	}
	if len(g.r.calls) != 0 {
		t.Errorf("require without cosign asked Docker: %v", g.r.calls)
	}

	h := verifyInstallSetup(t, map[string]string{"CADENCE_VERIFY": "off"})
	if code := h.install(append([]string{"--dir", h.dir("cadence")}, proxyYes...)...); code != 0 {
		t.Fatalf("off: %d\n%s", code, h.errw)
	}
	if len(cosignCalls(h.r)) != 0 || h.r.called("cosign version") {
		t.Errorf("off called cosign: %v", h.r.calls)
	}
	if countLines(h.out.String(), "▸ signature verification is off (CADENCE_VERIFY=off)") != 1 || strings.Contains(h.errw.String(), "! ") {
		t.Errorf("off output:\n%s\n%s", h.out, h.errw)
	}

	k := verifyInstallSetup(t, map[string]string{"CADENCE_VERIFY": "yes"})
	if code := k.install(append([]string{"--dir", k.dir("cadence")}, proxyYes...)...); code != 1 ||
		!strings.Contains(k.errw.String(), "CADENCE_VERIFY is 'yes', expected auto, require or off. Nothing was installed.") {
		t.Errorf("yes: %d\n%s", code, k.errw)
	}

	m := verifyInstallSetup(t, nil)
	m.r.cosignVersion = "v2.4.1"
	if code := m.install(append([]string{"--dir", m.dir("cadence")}, proxyYes...)...); code != 0 ||
		!strings.Contains(m.errw.String(), "! cosign v2.4.1 is older than 3.x: the signatures of the release are not verified") {
		t.Errorf("cosign 2.x: %d\n%s", code, m.errw)
	}
	if len(cosignCalls(m.r)) != 0 {
		t.Errorf("cosign 2.x was asked to verify: %v", cosignCalls(m.r))
	}
}

// AC-4, AC-5: версия до подписей; релиз без бандла при cosign.
func TestVerifyInstallUnsignedAndNoBundle(t *testing.T) {
	f := verifyInstallSetup(t, nil)
	if code := f.install(append([]string{"--version", "v0.8.0", "--dir", f.dir("cadence")}, proxyYes...)...); code != 0 ||
		!strings.Contains(f.errw.String(), "! v0.8.0 is not signed (signatures start with v0.9.0): its package and images are not verified") {
		t.Errorf("auto, v0.8.0: %d\n%s", code, f.errw)
	}
	if countLines(f.errw.String(), "! v0.8.0 is not signed") != 1 || len(cosignCalls(f.r)) != 0 {
		t.Errorf("auto, v0.8.0: once and no cosign: %v\n%s", cosignCalls(f.r), f.errw)
	}
	g := verifyInstallSetup(t, map[string]string{"CADENCE_VERIFY": "require"})
	if code := g.install(append([]string{"--version", "v0.8.0", "--dir", g.dir("cadence")}, proxyYes...)...); code != 1 ||
		!strings.Contains(g.errw.String(), "v0.8.0 is not signed (signatures start with v0.9.0), and CADENCE_VERIFY=require refuses it. Nothing was installed.") {
		t.Errorf("require, v0.8.0: %d\n%s", code, g.errw)
	}
	if _, err := os.Stat(g.dir("cadence")); err == nil {
		t.Error("require, v0.8.0: the directory was created")
	}
	for _, mode := range []string{"auto", "require"} {
		h := verifyInstallSetup(t, map[string]string{"CADENCE_VERIFY": mode})
		if code := h.install(append([]string{"--version", "v0.12.1", "--dir", h.dir("cadence")}, proxyYes...)...); code != 1 ||
			!strings.Contains(h.errw.String(), "the release v0.12.1 has no "+verifyBundle+" at file://"+h.releases+": the signature of its checksums.txt cannot be verified.") ||
			!strings.Contains(h.errw.String(), "Copy "+verifyBundle+" of the release to the mirror, or set CADENCE_VERIFY=off. Nothing was installed.") {
			t.Errorf("%s, no bundle: %d\n%s", mode, code, h.errw)
		}
		if _, err := os.Stat(h.dir("cadence")); err == nil {
			t.Errorf("%s, no bundle: the directory was created", mode)
		}
	}
}

// AC-8, AC-9: образ без pin; ghcr.io не отвечает.
func TestVerifyInstallUnpinnedAndUnreachable(t *testing.T) {
	unpinned := "ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.12.0 is not pinned to a digest (an empty CADENCE_BACKEND_DIGEST, or compose.override.yml)"
	f := verifyInstallSetup(t, map[string]string{"CADENCE_BACKEND_DIGEST": ""})
	if code := f.install(append([]string{"--dir", f.dir("cadence")}, proxyYes...)...); code != 0 ||
		!strings.Contains(f.errw.String(), "! "+unpinned+": not pinned, not verified") ||
		!strings.Contains(f.out.String(), "✓ "+verifyRegistry+"/sdlc-pipeline-frontend@"+testFrontendPin) {
		t.Errorf("auto, unpinned: %d\n%s\n%s", code, f.out, f.errw)
	}
	g := verifyInstallSetup(t, map[string]string{"CADENCE_BACKEND_DIGEST": "", "CADENCE_VERIFY": "require"})
	if code := g.install(append([]string{"--dir", g.dir("cadence")}, proxyYes...)...); code != 1 ||
		!strings.Contains(g.errw.String(), unpinned+", and CADENCE_VERIFY=require verifies images by digest only. Nothing was installed.") {
		t.Errorf("require, unpinned: %d\n%s", code, g.errw)
	}
	g.noEnv(g.dir("cadence"))

	for _, mode := range []string{"auto", "require"} {
		h := verifyInstallSetup(t, map[string]string{"CADENCE_VERIFY": mode})
		h.r.cosignBad, h.r.ghcrDown = "sdlc-pipeline-backend", true
		code := h.install(append([]string{"--dir", h.dir("cadence")}, proxyYes...)...)
		switch mode {
		case "auto":
			if code != 0 || !strings.Contains(h.errw.String(), "! images not verified: ghcr.io is unreachable from this host") {
				t.Errorf("auto, ghcr.io down: %d\n%s", code, h.errw)
			}
		default:
			if code != 1 || !strings.Contains(h.errw.String(), "the images cannot be verified: ghcr.io is unreachable from this host, and CADENCE_VERIFY=require needs it") {
				t.Errorf("require, ghcr.io down: %d\n%s", code, h.errw)
			}
			h.noEnv(h.dir("cadence"))
		}
	}
}

// AC-10: установка на месте проверяет образы и не ищет подпись пакета.
func TestVerifyInstallInPlace(t *testing.T) {
	f := verifyInstallSetup(t, nil)
	d := f.dir("unpacked")
	if err := extractTarGz(filepath.Join(f.releases, "download", "v0.12.0", packageName("v0.12.0")), d, nil); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(d, "compose.override.yml"),
		"services:\n  api:\n    image: registry.example.com/mine/sdlc-pipeline-backend:custom\n", 0o644)
	if code := f.install(append([]string{"--dir", d}, proxyYes...)...); code != 0 {
		t.Fatalf("in place: %d\n%s", code, f.errw)
	}
	calls := cosignCalls(f.r)
	if indexOf(calls, "cosign verify-blob") >= 0 {
		t.Errorf("in place looked for the signature of the package: %v", calls)
	}
	if indexOf(calls, "cosign verify --certificate-identity "+verifyIdentity("sdlc-pipeline-frontend", "v0.12.0")) < 0 {
		t.Errorf("in place did not verify the images: %v", calls)
	}
	// Образ из compose.override.yml, что лежал в каталоге до установки.
	if !strings.Contains(f.errw.String(), "! registry.example.com/mine/sdlc-pipeline-backend:custom is not pinned to a digest") {
		t.Errorf("the override image:\n%s", f.errw)
	}
}

// ── апгрейд ───────────────────────────────────────────────────────────────────

// verifyUpgradeSetup — установка v0.12.0 и релизы v0.13.0 (подписан),
// v0.13.1 без бандла, v0.8.x до подписей не нужен: апгрейд вниз запрещён.
func verifyUpgradeSetup(t *testing.T, envLines string, vars map[string]string) *upgradeFixture {
	t.Helper()
	files := signedFiles("v0.12.0")
	dir := installed(t, "v0.12.0", map[string]string{
		".env.example": files[".env.example"], "docker-compose.yml": files["docker-compose.yml"],
		"compose.external-db.yml": files["compose.external-db.yml"],
	})
	if envLines != "" {
		raw := readFile(t, filepath.Join(dir, ".env"))
		mustWrite(t, filepath.Join(dir, ".env"), raw+envLines, 0o600)
	}
	releases := t.TempDir()
	publishSigned(t, releases, "v0.12.0", false, true)
	publishSigned(t, releases, "v0.13.0", true, true)
	publishSigned(t, releases, "v0.13.1", false, false)
	all := map[string]string{"CADENCE_RELEASES_URL": "file://" + releases}
	for k, v := range vars {
		all[k] = v
	}
	r := newFakeRunner()
	r.cosign = true
	e, out, errw := testEnv(t, dir, r, all)
	e.cli = "v0.13.1"
	return &upgradeFixture{dir: dir, releases: releases, r: r, e: e, out: out, errw: errw}
}

// unchanged — отказ апгрейда ничего не тронул: файлы, бэкапов нет, pull и
// dbcheck не звались, .upgrade.* убран.
func (f *upgradeFixture) unchanged(t *testing.T, before string) {
	t.Helper()
	if tree(t, f.dir) != before {
		t.Error("the files of the installation changed")
	}
	if left, _ := os.ReadDir(filepath.Join(f.dir, "backups")); len(left) != 0 {
		t.Errorf("backups: %v", left)
	}
	if f.r.called("docker compose pull") || f.r.calledContaining("pg_dump") || f.r.calledContaining("dbcheck") {
		t.Errorf("the refused upgrade went on: %v", f.r.calls)
	}
	if m, _ := filepath.Glob(filepath.Join(f.dir, ".upgrade.*")); len(m) != 0 {
		t.Errorf("left: %v", m)
	}
	if !strings.Contains(f.errw.String(), "Nothing has changed.") {
		t.Errorf("no 'Nothing has changed.':\n%s", f.errw)
	}
}

func (f *fakeRunner) calledContaining(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, s) {
			return true
		}
	}
	return false
}

// AC-11, AC-12, AC-13: подпись пакета и образов новой версии до бэкапа,
// файлов и pull; отказ ничего не меняет.
func TestVerifyUpgrade(t *testing.T) {
	f := verifyUpgradeSetup(t, "", nil)
	if code := f.upgrade("v0.13.0"); code != 0 {
		t.Fatalf("upgrade: %d\n%s", code, f.errw)
	}
	calls := cosignCalls(f.r)
	blob := indexOf(calls, "cosign verify-blob --bundle "+filepath.Join(f.dir, ".upgrade."))
	img := indexOf(calls, "cosign verify --certificate-identity "+verifyIdentity("sdlc-pipeline-backend", "v0.13.0")+
		" --certificate-oidc-issuer "+verifyIssuer+" "+verifyRegistry+"/sdlc-pipeline-backend@"+testBackendPin)
	if blob < 0 || img < blob {
		t.Errorf("verify-blob, then the images of v0.13.0: %v", calls)
	}
	if !strings.Contains(f.out.String(), "✓ the signature of checksums.txt of v0.13.0 verified") ||
		!strings.Contains(f.out.String(), "is signed by "+verifyIdentity("sdlc-pipeline-frontend", "v0.13.0")) {
		t.Errorf("output:\n%s", f.out)
	}

	for _, bad := range []string{"package", "sdlc-pipeline-frontend"} {
		g := verifyUpgradeSetup(t, "", nil)
		g.r.cosignBad = bad
		before := tree(t, g.dir)
		if code := g.upgrade("v0.13.0"); code != 1 || !strings.Contains(g.errw.String(), "did not verify for") {
			t.Errorf("bad %s: %d\n%s", bad, code, g.errw)
		}
		g.unchanged(t, before)
		if g.version(t) != "v0.12.0" {
			t.Errorf("bad %s: VERSION %s", bad, g.version(t))
		}
	}

	// Внешняя база: подпись образа — раньше проверки базы образом новой
	// версии.
	h := verifyUpgradeSetup(t, "CADENCE_DB=external\nSDLC_DATABASE_URL='postgres://u:p@db.example.com:5432/cadence?sslmode=verify-full'\n"+
		"COMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.external-db.yml:compose.override.yml\n", nil)
	h.r.cosignBad = "sdlc-pipeline-backend"
	before := tree(t, h.dir)
	if code := h.upgrade("v0.13.0"); code != 1 ||
		!strings.Contains(h.errw.String(), "the signature of "+verifyRegistry+"/sdlc-pipeline-backend@"+testBackendPin+" (sdlc-pipeline-backend v0.13.0) did not verify") {
		t.Errorf("external, bad image: %d\n%s", code, h.errw)
	}
	if h.r.calledContaining("dbcheck") {
		t.Error("the external database was checked by an image with a bad signature")
	}
	h.unchanged(t, before)
}

// AC-5, AC-14, q7: релиз без бандла; CADENCE_VERIFY из .env перекрывает
// окружение, .env апгрейд не трогает.
func TestVerifyUpgradeModes(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		f := verifyUpgradeSetup(t, "", map[string]string{"CADENCE_VERIFY": mode})
		before := tree(t, f.dir)
		if code := f.upgrade("v0.13.1"); code != 1 ||
			!strings.Contains(f.errw.String(), "the release v0.13.1 has no "+verifyBundle+" at file://"+f.releases) ||
			!strings.Contains(f.errw.String(), "Copy "+verifyBundle+" of the release to the mirror, or set CADENCE_VERIFY=off. Nothing has changed.") {
			t.Errorf("%s, no bundle: %d\n%s", mode, code, f.errw)
		}
		f.unchanged(t, before)
	}

	g := verifyUpgradeSetup(t, "CADENCE_VERIFY=off\n", map[string]string{"CADENCE_VERIFY": "require"})
	env := readFile(t, filepath.Join(g.dir, ".env"))
	if code := g.upgrade("v0.13.0"); code != 0 {
		t.Fatalf("off in .env: %d\n%s", code, g.errw)
	}
	if g.r.called("cosign") {
		t.Errorf("off in .env, require in the environment: cosign called %v", g.r.calls)
	}
	if !strings.Contains(g.out.String(), "▸ signature verification is off (CADENCE_VERIFY=off)") {
		t.Errorf("output:\n%s", g.out)
	}
	if readFile(t, filepath.Join(g.dir, ".env")) != env {
		t.Error(".env changed")
	}

	h := verifyUpgradeSetup(t, "CADENCE_VERIFY=require\n", nil)
	h.r.cosign = false
	before := tree(t, h.dir)
	if code := h.upgrade("v0.13.0"); code != 1 ||
		!strings.Contains(h.errw.String(), "CADENCE_VERIFY=require, but cosign 3.x is not installed") {
		t.Errorf("require in .env without cosign: %d\n%s", code, h.errw)
	}
	h.unchanged(t, before)

	// Без pin в .env: auto предупреждает и идёт дальше.
	k := verifyUpgradeSetup(t, "CADENCE_FRONTEND_DIGEST=\n", nil)
	if code := k.upgrade("v0.13.0"); code != 0 ||
		!strings.Contains(k.errw.String(), "! ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v0.13.0 is not pinned to a digest (an empty CADENCE_FRONTEND_DIGEST, or compose.override.yml): not pinned, not verified") {
		t.Errorf("unpinned in .env: %d\n%s", code, k.errw)
	}
}

// AC-4: апгрейд на версию до подписей.
func TestVerifyUpgradeUnsigned(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		dir := installed(t, "v0.7.0", nil)
		releases := t.TempDir()
		publish(t, releases, "v0.8.0", pkgFiles("v0.8.0", nil), true)
		r := newFakeRunner()
		r.cosign = true
		e, out, errw := testEnv(t, dir, r, map[string]string{"CADENCE_RELEASES_URL": "file://" + releases, "CADENCE_VERIFY": mode})
		e.cli = "v0.8.0"
		f := &upgradeFixture{dir: dir, releases: releases, r: r, e: e, out: out, errw: errw}
		before := tree(t, dir)
		code := f.upgrade("v0.8.0")
		if mode == "auto" {
			if code != 0 || countLines(errw.String(), "! v0.8.0 is not signed (signatures start with v0.9.0)") != 1 || len(cosignCalls(r)) != 0 {
				t.Errorf("auto: %d %v\n%s", code, cosignCalls(r), errw)
			}
			continue
		}
		if code != 1 || !strings.Contains(errw.String(), "v0.8.0 is not signed (signatures start with v0.9.0), and CADENCE_VERIFY=require refuses it. Nothing has changed.") {
			t.Errorf("require: %d\n%s", code, errw)
		}
		f.unchanged(t, before)
	}
}

// Разбор версии cosign и подстановка образов compose.
func TestVerifyHelpers(t *testing.T) {
	for ver, usable := range map[string]bool{"v3.1.3": true, "v3.0.0": true, "v10.0.0": true, "v2.4.1": false, "v1.13.6": false, "": false} {
		r := newFakeRunner()
		r.cosign = true
		r.cosignVersion = ver
		if ver == "" {
			r.cosignVersion = "devel"
		}
		e, _, _ := testEnv(t, t.TempDir(), r, nil)
		v, err := newVerifier(context.Background(), e, "", nothingInstalled)
		if err != nil || v.cosign != usable {
			t.Errorf("cosign %q: usable %v, err %v", ver, v != nil && v.cosign, err)
		}
	}
	refs := imageRefs([]string{
		"ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v1@" + testBackendPin,
		"ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v1@" + testBackendPin,
		"sdlc-pipeline-backend-extra:1", "caddy:2-alpine", "sdlc-pipeline-backend",
	}, "sdlc-pipeline-backend")
	if len(refs) != 2 {
		t.Errorf("refs: %v", refs)
	}
}
