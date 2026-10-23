package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Тесты clab server images (CLAB-202): без Docker, regctl и сети.
// imagesRunner отвечает за regctl и за то, что Docker делает с образами
// (load, run, port, pull, tag, rmi, image inspect), остальное — обычный
// fakeRunner. Хранилище Docker — containerd (pin находится после pull и
// tag) или классическое (не находится). Номера — критерии приёмки
// CLAB-202 (AC-n).

const (
	testPg17Digest = "sha256:" + "1717171717171717171717171717171717171717171717171717171717171717"
	testRegAddr    = "127.0.0.1:41234"
)

// airgapCompose — docker-compose.yml пакета в миниатюре: наши образы с pin,
// база и caddy по тегу.
const airgapCompose = signedCompose + `  postgres:
    image: pgvector/pgvector:pg17
`

// airgapExternalDB — compose.external-db.yml: клиент базы в профиле tools.
const airgapExternalDB = `services:
  db-client:
    profiles: ["tools"]
    image: pgvector/pgvector:pg18
`

// airgapImages — перечень версии: compose пакета и helperImage, по порядку.
func airgapImages(v string) []string {
	return []string{
		helperImage,
		"caddy:2-alpine",
		"ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:" + v + "@" + testBackendPin,
		"ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:" + v + "@" + testFrontendPin,
		"pgvector/pgvector:pg17",
		"pgvector/pgvector:pg18",
	}
}

func airgapFiles(v string) map[string]string {
	return pkgFiles(v, map[string]string{
		".env.example":            testEnvExample,
		"docker-compose.yml":      airgapCompose,
		"compose.external-db.yml": airgapExternalDB,
	})
}

// publishAirgap — подписанный релиз версии с бандлом; root — с
// trusted_root.json в зеркале (как после load).
func publishAirgap(t *testing.T, releases, v string, root bool) {
	t.Helper()
	publish(t, releases, v, airgapFiles(v), true)
	d := filepath.Join(releases, "download", v)
	mustWrite(t, filepath.Join(d, verifyBundle), `{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json"}`, 0o644)
	mustWrite(t, filepath.Join(releases, "latest", "download", verifyBundle), `{}`, 0o644)
	if root {
		mustWrite(t, filepath.Join(d, trustedRootAsset), `{"mediaType": "application/vnd.dev.sigstore.trustedroot+json;version=0.1"}`, 0o644)
	}
}

// fakeRef — referrer образа в архиве: бандл Sigstore (typ пуст) с типом
// содержимого pred и тем, проверяется ли он identity версии, или referrer
// другого artifactType; child — приложен к платформенному манифесту, а не
// к индексу.
type fakeRef struct {
	pred  string
	bad   bool
	typ   string
	child bool
}

func (f fakeRef) artifactType() string {
	if f.typ == "" {
		return "application/vnd.dev.sigstore.bundle.v0.3+json"
	}
	return f.typ
}

// fakeChild, fakeAttestation — манифесты внутри индекса образа в архиве:
// платформа и аттестация BuildKit к ней, которую regctl показывает её
// referrer.
const (
	fakeChild       = "sha256:child"
	fakeAttestation = "sha256:attestation"
)

// layoutRepo — образ ссылки на OCI-layout архива или реестр.
func layoutRepo(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	for _, repo := range verifyImageRepos {
		if strings.Contains(name, "_"+repo+"_") {
			return repo
		}
	}
	return refRepo(ref)
}

const signPredicate = "https://sigstore.dev/cosign/sign/v1"

// defaultRefs — как у релиза v0.13.0: подпись и OpenVEX у обоих.
func defaultRefs() map[string][]fakeRef {
	return map[string][]fakeRef{
		"sdlc-pipeline-backend":  {{pred: signPredicate}, {pred: openvexPredicate}},
		"sdlc-pipeline-frontend": {{pred: signPredicate}, {pred: openvexPredicate}},
	}
}

// refRepo — образ ссылки host/path/repo@digest.
func refRepo(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	return name[strings.LastIndex(name, "/")+1:]
}

// imagesRunner — regctl и образы Docker поверх fakeRunner.
type imagesRunner struct {
	*fakeRunner
	local        map[string]string     // ссылка в Docker → digest образа
	containers   map[string]string     // контейнер → значение метки airgapLabel
	late         map[string]string     // контейнеры, которые демон покажет на следующем docker ps
	onDockerRun  func(c cmdSpec) error // хук docker run временного реестра; nil — контейнер создан
	onRun        func(c cmdSpec)       // хук на каждую команду
	onCall       func(c cmdSpec) error // хук на каждую команду: не nil — её ответ (SIGINT посреди вызова)
	refs         map[string][]fakeRef  // referrers образа в архиве: repo → referrers
	regRefs      map[string][]fakeRef  // referrers, перенесённые во временный реестр
	layoutDigest map[string]string     // repo → digest под тегом раскладки в архиве, если не pin
	containerd   bool
	arch         string
	regctlVer    string
	regctlFail   string // regctl падает на вызове, где есть эта строка
	pushed       map[string]string
	offline      bool   // нет сети: cosign без --trusted-root падает
	composeImgs  string // ответ docker compose config --images без -f (pull)
	onRegctlCopy func() // хук: вызов regctl image copy в реестр (SIGINT)
	requireVar   string // compose config --images по всем файлам требует эту переменную (${X:?})
}

func newImagesRunner() *imagesRunner {
	r := newFakeRunner()
	r.cosign = true
	return &imagesRunner{fakeRunner: r, local: map[string]string{}, containers: map[string]string{}, late: map[string]string{}, refs: defaultRefs(), regRefs: map[string][]fakeRef{},
		containerd: true, arch: "amd64", regctlVer: "v0.9.2", pushed: map[string]string{}}
}

func (r *imagesRunner) record(c cmdSpec) {
	r.mu.Lock()
	r.calls = append(r.calls, c.String())
	r.mu.Unlock()
}

func (r *imagesRunner) run(ctx context.Context, c cmdSpec) error {
	if r.onRun != nil {
		r.onRun(c)
	}
	// Как exec.CommandContext: после отмены команда не запускается.
	if err := ctx.Err(); err != nil {
		r.record(c)
		return err
	}
	if r.onCall != nil {
		if err := r.onCall(c); err != nil {
			r.record(c)
			return err
		}
	}
	args := strings.Join(c.args, " ")
	write := func(s string) {
		if c.stdout != nil {
			io.WriteString(c.stdout, s)
		}
	}
	fail := func(msg string) error {
		if c.stderr != nil {
			io.WriteString(c.stderr, msg+"\n")
		}
		return exitErr(1)
	}
	switch c.name {
	case "regctl":
		r.record(c)
		return r.regctl(c, write, fail)
	case "cosign":
		switch {
		case len(c.args) > 0 && c.args[0] == "initialize":
			r.record(c)
			for _, kv := range c.env {
				if dir, ok := strings.CutPrefix(kv, "TUF_ROOT="); ok {
					p := filepath.Join(dir, "tuf-repo-cdn.sigstore.dev", "targets", trustedRootAsset)
					os.MkdirAll(filepath.Dir(p), 0o755)
					return os.WriteFile(p, []byte(`{"root": "tuf"}`), 0o644)
				}
			}
			return fail("no TUF_ROOT")
		case r.offline && (c.args[0] == "verify" || c.args[0] == "verify-blob") && !strings.Contains(args, "--trusted-root"):
			r.record(c)
			return fail("Error: getting TUF client: dial tcp: lookup tuf-repo-cdn.sigstore.dev: no such host")
		case (c.args[0] == "verify" || c.args[0] == "verify-attestation") && strings.HasPrefix(c.args[len(c.args)-1], testRegAddr+"/"):
			target := c.args[len(c.args)-1]
			if r.cosignBad != "" && strings.Contains(target, "/"+r.cosignBad+"@") {
				return r.fakeRunner.run(ctx, c)
			}
			r.record(c)
			var good []string
			for _, ref := range r.regRefs[refRepo(target)] {
				if !ref.bad && (c.args[0] == "verify" || ref.pred == openvexPredicate) {
					good = append(good, `{"critical":{"type":"`+ref.pred+`"}}`)
				}
			}
			if len(r.regRefs[refRepo(target)]) == 0 && c.args[0] == "verify" {
				return cosignNoSignaturesFail(c)
			}
			if len(good) == 0 {
				return fail("Error: no matching attestations: none of the expected identities matched")
			}
			if c.args[0] == "verify" {
				write("[" + strings.Join(good, ",") + "]\n")
			} else {
				write(strings.Join(good, "\n") + "\n")
			}
			return nil
		}
		return r.fakeRunner.run(ctx, c)
	case "docker":
	default:
		return r.fakeRunner.run(ctx, c)
	}
	switch {
	case args == "compose config --images" && r.composeImgs != "":
		r.record(c)
		write(r.composeImgs)
		return nil
	case r.requireVar != "" && strings.HasSuffix(args, " --profile * config --images") && !hasEnv(c.env, r.requireVar):
		r.record(c)
		return fail("error while interpolating services.api.environment." + r.requireVar + ": required variable " + r.requireVar + " is missing a value: not set")
	case args == "compose pull --quiet --policy missing":
		r.record(c)
		return nil
	case strings.HasPrefix(args, "image inspect --format {{.Id}} "):
		r.record(c)
		if d := r.inspect(c.args[len(c.args)-1]); d != "" {
			write(d + "\n")
			return nil
		}
		return fail("Error response from daemon: No such image")
	case args == "version --format {{.Server.Arch}}":
		r.record(c)
		write(r.arch + "\n")
		return nil
	case strings.HasPrefix(args, "load -q -i "):
		r.record(c)
		r.local[airgapRegistryName] = "sha256:registry"
		return nil
	case strings.HasPrefix(args, "run -d --name clab-images-load-"):
		r.record(c)
		if r.onDockerRun != nil {
			return r.onDockerRun(c)
		}
		r.containers[c.args[3]] = strings.TrimPrefix(c.args[5], airgapLabel+"=")
		return nil
	case strings.HasPrefix(args, "ps -aq --filter label="+airgapLabel+"="):
		r.record(c)
		for n, l := range r.late {
			r.containers[n] = l
			delete(r.late, n)
		}
		want := strings.TrimPrefix(c.args[3], "label="+airgapLabel+"=")
		for n, l := range r.containers {
			if l == want {
				write(n + "\n")
			}
		}
		return nil
	case strings.HasPrefix(args, "port clab-images-load-"):
		r.record(c)
		write(testRegAddr + "\n")
		return nil
	case strings.HasPrefix(args, "rm -f -v "):
		r.record(c)
		if _, ok := r.containers[c.args[3]]; !ok {
			return fail("Error response from daemon: No such container: " + c.args[3])
		}
		delete(r.containers, c.args[3])
		return nil
	case strings.HasPrefix(args, "pull -q "):
		r.record(c)
		src := c.args[2]
		_, d, _ := strings.Cut(src, "@")
		r.local[src] = d
		return nil
	case strings.HasPrefix(args, "tag "):
		r.record(c)
		r.local[c.args[2]] = r.local[c.args[1]]
		return nil
	case strings.HasPrefix(args, "rmi "):
		r.record(c)
		delete(r.local, c.args[1])
		return nil
	}
	return r.fakeRunner.run(ctx, c)
}

func hasEnv(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// inspect — docker image inspect: ссылка с digest на containerd находится
// по имени с тегом и digest, на классическом хранилище — только та, под
// которой образ скачан.
func (r *imagesRunner) inspect(ref string) string {
	if d, ok := r.local[ref]; ok {
		return d
	}
	name, digest, ok := strings.Cut(ref, "@")
	if !ok || !r.containerd {
		return ""
	}
	if d := r.local[name]; d == digest {
		return d
	}
	// repo@digest без тега — любой тег того же репозитория.
	for k, d := range r.local {
		if d == digest && strings.HasPrefix(k, name+":") {
			return d
		}
	}
	return ""
}

func (r *imagesRunner) regctl(c cmdSpec, write func(string), fail func(string) error) error {
	args := strings.Join(c.args, " ")
	if r.regctlFail != "" && strings.Contains(args, r.regctlFail) {
		return fail("failed to copy: " + r.regctlFail + ": connection refused")
	}
	switch {
	case args == "version --format {{.VCSTag}}":
		write(r.regctlVer + "\n")
		return nil
	case strings.HasPrefix(args, "image copy "):
		src, dst := c.args[len(c.args)-2], c.args[len(c.args)-1]
		if p, ok := strings.CutPrefix(dst, "ocidir://"); ok {
			dir := p[:strings.LastIndex(p, ":")]
			os.MkdirAll(filepath.Join(dir, "blobs"), 0o755)
			return os.WriteFile(filepath.Join(dir, "index.json"), []byte(src+"\n"), 0o644)
		}
		if r.onRegctlCopy != nil && strings.HasPrefix(src, "ocidir://") {
			r.onRegctlCopy()
		}
		// Referrer из архива во временный реестр — по своему digest.
		if _, d, ok := strings.Cut(src, "@sha256:ref"); ok && strings.HasPrefix(src, "ocidir://") {
			repo := layoutRepo(src)
			n, _ := strconv.Atoi(d)
			r.regRefs[repo] = append(r.regRefs[repo], r.refs[repo][n])
		}
		r.pushed[dst] = src
		return nil
	case strings.HasPrefix(args, "manifest get ocidir://"):
		write(`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"` + fakeChild + `"},{"digest":"` + fakeAttestation + `"}]}` + "\n")
		return nil
	case strings.HasPrefix(args, "artifact list "):
		// В архиве — referrers r.refs, во временном реестре — r.regRefs.
		ref := c.args[len(c.args)-3]
		refs := r.refs[layoutRepo(ref)]
		if !strings.HasPrefix(ref, "ocidir://") {
			ref = c.args[len(c.args)-1]
			refs = r.regRefs[refRepo(ref)]
		}
		_, subject, _ := strings.Cut(ref, "@")
		var descs []string
		if subject == fakeChild {
			descs = append(descs, `{"digest":"`+fakeAttestation+`","artifactType":"application/vnd.docker.attestation.manifest.v1+json"}`)
		}
		for i, ref := range refs {
			if ref.child != (subject == fakeChild) || subject == fakeAttestation {
				continue
			}
			descs = append(descs, fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:ref%d","artifactType":%q,"annotations":{%q:%q}}`,
				i, ref.artifactType(), predicateAnnotation, ref.pred))
		}
		if descs == nil {
			write("null\n")
		} else {
			write("[" + strings.Join(descs, ",") + "]\n")
		}
		return nil
	case strings.HasPrefix(args, "image digest ocidir://"):
		repo := layoutRepo(c.args[len(c.args)-1])
		switch {
		case r.layoutDigest[repo] != "":
			write(r.layoutDigest[repo] + "\n")
		case repo == "sdlc-pipeline-backend":
			write(testBackendPin + "\n")
		case repo == "sdlc-pipeline-frontend":
			write(testFrontendPin + "\n")
		default:
			write(testPg17Digest + "\n")
		}
		return nil
	case strings.HasPrefix(args, "image export "):
		dst := c.args[len(c.args)-1]
		return os.WriteFile(dst, []byte("registry image "+c.args[3]+"\n"), 0o644)
	}
	return fail("unexpected regctl call: " + args)
}

// loaded — образы в Docker, как после load на containerd.
func (r *imagesRunner) loaded(images []string) {
	for _, img := range images {
		ref := parseImageRef(img)
		d := ref.digest
		if d == "" {
			d = testPg17Digest
		}
		r.local[ref.named()] = d
	}
}

// airgapFixture — каталог команды, релизы v0.13.0 в file://, clab v0.13.0.
type airgapFixture struct {
	t         *testing.T
	root      string
	releases  string
	r         *imagesRunner
	e         *serverEnv
	out, errw *bytes.Buffer
	vars      map[string]string
}

func airgapSetup(t *testing.T, vars map[string]string) *airgapFixture {
	t.Helper()
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	publishAirgap(t, releases, "v0.13.0", false)
	tmp := filepath.Join(root, "tmp")
	os.MkdirAll(tmp, 0o700)
	t.Setenv("TMPDIR", tmp)
	all := map[string]string{"CADENCE_RELEASES_URL": "file://" + releases}
	for k, v := range vars {
		all[k] = v
	}
	f := &airgapFixture{t: t, root: root, releases: releases, vars: all}
	f.fresh()
	return f
}

// fresh — новая машина: пустой Docker, тот же каталог.
func (f *airgapFixture) fresh() {
	f.r = newImagesRunner()
	work := filepath.Join(f.root, "work")
	os.MkdirAll(work, 0o755)
	f.e, f.out, f.errw = testEnv(f.t, work, f.r.fakeRunner, f.vars)
	f.e.run = f.r
	f.e.cli = "v0.13.0"
	f.e.reachable = func(_ context.Context, u string) bool {
		return strings.HasPrefix(u, "http://127.0.0.1:") || !f.r.offline
	}
}

func (f *airgapFixture) images(args ...string) int {
	return runServer(context.Background(), f.e, append([]string{"images"}, args...))
}

// save — архив v0.13.0 в work/a.tar.
func (f *airgapFixture) save() string {
	f.t.Helper()
	if code := f.images("save", "--version", "v0.13.0", "--output", "a.tar"); code != 0 {
		f.t.Fatalf("save: %d\n%s\n%s", code, f.out, f.errw)
	}
	return filepath.Join(f.e.cwd, "a.tar")
}

// entries — содержимое tar: имя → данные.
func tarEntries(t *testing.T, p string) map[string]string {
	t.Helper()
	fh, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	tr := tar.NewReader(fh)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

// rewriteTar — тот же архив с правкой одного файла (подмена в пути).
func rewriteTar(t *testing.T, p, name string, edit func(string) string) {
	t.Helper()
	ents := tarEntries(t, p)
	if _, ok := ents[name]; !ok {
		t.Fatalf("no %s in the archive", name)
	}
	names := make([]string, 0, len(ents))
	for n := range ents {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		body := ents[n]
		if n == name {
			body = edit(body)
		}
		if strings.HasSuffix(n, "/") {
			tw.WriteHeader(&tar.Header{Name: n, Mode: 0o755, Typeflag: tar.TypeDir})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		io.WriteString(tw, body)
	}
	tw.Close()
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// leftovers — что лежит в каталоге команды и во временном, кроме keep.
func (f *airgapFixture) leftovers(keep ...string) []string {
	k := map[string]bool{}
	for _, n := range keep {
		k[n] = true
	}
	var left []string
	for _, d := range []string{f.e.cwd, filepath.Join(f.root, "tmp")} {
		ents, _ := os.ReadDir(d)
		for _, en := range ents {
			if !k[en.Name()] {
				left = append(left, filepath.Join(d, en.Name()))
			}
		}
	}
	return left
}

func (r *imagesRunner) callsWith(prefix string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// ── поверхность ────────────────────────────────────────────────────────────

// AC-13: справки, коды, Windows.
func TestImagesSurface(t *testing.T) {
	f := airgapSetup(t, nil)
	ctx := context.Background()
	if code := runServer(ctx, f.e, []string{"--help"}); code != 0 || !strings.Contains(f.out.String(), "\n  images    the images of a version") {
		t.Errorf("clab server --help: %d\n%s", code, f.out)
	}
	for args, want := range map[string]string{
		"images --help": imagesHelp, "images list --help": imagesListHelp,
		"images save --version v0.13.0 --help": imagesSaveHelp, "images load -h": imagesLoadHelp,
	} {
		f.out.Reset()
		if code := runServer(ctx, f.e, strings.Fields(args)); code != 0 || f.out.String() != want+"\n" {
			t.Errorf("%s: %d\n%s", args, code, f.out)
		}
	}
	for _, args := range [][]string{{"images"}, {"images", "push"}, {"images", "list", "--nope"},
		{"images", "list", "--dir", "x", "--version", "v0.13.0"}, {"images", "list", "--version", "13"},
		{"images", "save"}, {"images", "save", "--version", "v0.13.0", "extra"},
		{"images", "load"}, {"images", "load", "a.tar", "b.tar"}, {"images", "load", "a.tar", "--registry", "https://reg.example"}} {
		if code := runServer(ctx, f.e, args); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
	f.e.goos = "windows"
	if code := runServer(ctx, f.e, []string{"images", "list"}); code != 1 {
		t.Errorf("windows: %d", code)
	}
}

// ── list ───────────────────────────────────────────────────────────────────

// AC-1: перечень версии — все файлы compose пакета во всех профилях и
// helperImage, по строке, без повторов; служебные строки — не в stdout.
func TestImagesListVersion(t *testing.T) {
	f := airgapSetup(t, nil)
	if code := f.images("list", "--version", "v0.13.0"); code != 0 {
		t.Fatalf("list: %d\n%s", code, f.errw)
	}
	if got, want := f.out.String(), strings.Join(airgapImages("v0.13.0"), "\n")+"\n"; got != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(f.errw.String(), "✓ the signature of checksums.txt of v0.13.0 verified") {
		t.Errorf("stderr:\n%s", f.errw)
	}
	cfg := f.r.callsWith("docker compose ")
	found := false
	for _, c := range cfg {
		if strings.HasSuffix(c, " --profile * config --images") && strings.Contains(c, "/compose.external-db.yml ") &&
			strings.Contains(c, "/docker-compose.yml ") {
			found = true
		}
	}
	if !found {
		t.Errorf("no config --images over every file and profile: %v", cfg)
	}
	// Подпись не сходится — отказ по 0054.
	g := airgapSetup(t, nil)
	g.r.cosignBad = "package"
	if code := g.images("list", "--version", "v0.13.0"); code != 1 || g.out.Len() != 0 ||
		!strings.Contains(g.errw.String(), "the signature of checksums.txt of v0.13.0 did not verify") {
		t.Errorf("bad signature: %d\n%s%s", code, g.out, g.errw)
	}
	// Релиза нет.
	if code := g.images("list", "--version", "v0.99.0"); code != 1 || !strings.Contains(g.errw.String(), "there is no release v0.99.0") {
		t.Errorf("no release: %d\n%s", code, g.errw)
	}
}

// AC-1: перечень установки — её версия и CADENCE_REGISTRY её .env;
// compose.override.yml установки — не пакет.
// Файлы пакета вместе требуют переменной режима (${SDLC_DATABASE_URL:?} в
// compose.external-db.yml v0.12.0): она получает заглушку, перечень тот же,
// и save проверяет подписи той же командой compose.
func TestImagesListRequiredVariable(t *testing.T) {
	f := airgapSetup(t, nil)
	f.r.requireVar = "SDLC_DATABASE_URL"
	if code := f.images("list", "--version", "v0.13.0"); code != 0 || f.out.String() != strings.Join(airgapImages("v0.13.0"), "\n")+"\n" {
		t.Fatalf("list: %d\n%s\n%s", code, f.out, f.errw)
	}
	f.save()
	if calls := f.r.callsWith("docker compose "); len(calls) < 2 {
		t.Errorf("no retry: %v", calls)
	}
}

func TestImagesListDir(t *testing.T) {
	files := airgapFiles("v0.12.0")
	dir := installed(t, "v0.12.0", map[string]string{"docker-compose.yml": files["docker-compose.yml"],
		"compose.external-db.yml": files["compose.external-db.yml"]})
	mustWrite(t, filepath.Join(dir, ".env"), readFile(t, filepath.Join(dir, ".env"))+"CADENCE_REGISTRY=mirror.example/cadence\n", 0o600)
	mustWrite(t, filepath.Join(dir, "compose.override.yml"), "services:\n  x:\n    image: mine/custom:1\n", 0o644)
	r := newImagesRunner()
	e, out, errw := testEnv(t, dir, r.fakeRunner, nil)
	e.run = r
	e.cli = "v0.13.0"
	if code := runServer(context.Background(), e, []string{"images", "list"}); code != 0 {
		t.Fatalf("list: %d\n%s", code, errw)
	}
	want := []string{helperImage, "caddy:2-alpine",
		"mirror.example/cadence/sdlc-pipeline-backend:v0.12.0@" + testBackendPin,
		"mirror.example/cadence/sdlc-pipeline-frontend:v0.12.0@" + testFrontendPin,
		"pgvector/pgvector:pg17", "pgvector/pgvector:pg18"}
	if got := out.String(); got != strings.Join(want, "\n")+"\n" {
		t.Errorf("stdout:\n%s", got)
	}
}

func TestParseImageRef(t *testing.T) {
	for ref, want := range map[string]imageRef{
		"alpine:3":               {ref: "alpine:3", domain: "docker.io", path: "library/alpine", tag: "3"},
		"pgvector/pgvector:pg17": {ref: "pgvector/pgvector:pg17", domain: "docker.io", path: "pgvector/pgvector", tag: "pg17"},
		"ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.13.0@" + testBackendPin: {ref: "ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.13.0@" + testBackendPin,
			domain: "ghcr.io", path: "cadence-lab-tech/sdlc-pipeline-backend", tag: "v0.13.0", digest: testBackendPin},
		"localhost:5050/x/y": {ref: "localhost:5050/x/y", domain: "localhost:5050", path: "x/y", tag: "latest"},
	} {
		if got := parseImageRef(ref); got != want {
			t.Errorf("%s: %+v", ref, got)
		}
	}
	r := parseImageRef("ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v0.13.0@" + testFrontendPin)
	if r.named() != "ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v0.13.0" || r.repo() != "sdlc-pipeline-frontend" || !r.ours() ||
		r.layout() != "images/ghcr.io_cadence-lab-tech_sdlc-pipeline-frontend_v0.13.0" {
		t.Errorf("%+v %s %s %s", r, r.named(), r.repo(), r.layout())
	}
}

// ── save ───────────────────────────────────────────────────────────────────

// AC-2: архив — OCI-layout каждого образа, релиз с trusted_root.json,
// образ реестра обеих платформ, манифест; sha256 в выводе — файла.
func TestImagesSave(t *testing.T) {
	f := airgapSetup(t, nil)
	p := f.save()
	ents := tarEntries(t, p)
	for _, n := range []string{"cadence-images.json", "download/v0.13.0/" + packageName("v0.13.0"), "download/v0.13.0/checksums.txt",
		"download/v0.13.0/" + verifyBundle, "download/v0.13.0/get-cadence.sh", "download/v0.13.0/" + trustedRootAsset,
		"registry/registry-linux-amd64.tar", "registry/registry-linux-arm64.tar"} {
		if _, ok := ents[n]; !ok {
			t.Errorf("no %s in the archive", n)
		}
	}
	var m imagesArchive
	if err := json.Unmarshal([]byte(ents["cadence-images.json"]), &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "v0.13.0" || m.Format != archiveFormat || m.Regctl != "v0.9.2" || m.Cosign != "v3.1.3" || m.TrustedRoot == "" ||
		m.Registry.Image != airgapRegistryImage || len(m.Registry.Files) != 2 {
		t.Errorf("manifest: %+v", m)
	}
	var refs []string
	for _, img := range m.Images {
		refs = append(refs, img.Ref)
		if _, ok := ents[img.Layout+"/index.json"]; !ok {
			t.Errorf("no layout of %s", img.Ref)
		}
		r := parseImageRef(img.Ref)
		switch {
		case r.digest != "" && img.Digest != r.digest:
			t.Errorf("%s: digest %s, want the pin", img.Ref, img.Digest)
		case r.digest == "" && img.Digest != testPg17Digest:
			t.Errorf("%s: digest %s", img.Ref, img.Digest)
		}
	}
	if strings.Join(refs, " ") != strings.Join(airgapImages("v0.13.0"), " ") {
		t.Errorf("images: %v", refs)
	}
	copies := f.r.callsWith("regctl image copy --referrers --digest-tags ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.13.0@" + testBackendPin + " ocidir://")
	if len(copies) != 1 {
		t.Errorf("backend copied: %v", f.r.callsWith("regctl"))
	}
	if exp := f.r.callsWith("regctl image export --platform linux/arm64 --name " + airgapRegistryName + " " + airgapRegistryImage); len(exp) != 1 {
		t.Errorf("registry export: %v", f.r.callsWith("regctl"))
	}
	// Подписи — до копирования образов.
	calls := f.r.calls
	verify, copied := indexOf(calls, "cosign verify --certificate-identity "+verifyIdentity("sdlc-pipeline-frontend", "v0.13.0")), indexOf(calls, "regctl image copy")
	if verify < 0 || copied < 0 || verify > copied || indexOf(calls, "cosign verify-blob") > verify {
		t.Errorf("order: %v", calls)
	}
	sum, _ := sha256File(p)
	if !strings.Contains(f.out.String(), "SHA-256:") || !strings.Contains(f.out.String(), sum) ||
		!strings.Contains(f.out.String(), "images load a.tar") {
		t.Errorf("output:\n%s", f.out)
	}
	if left := f.leftovers("a.tar"); len(left) > 0 {
		t.Errorf("left: %v", left)
	}
}

// AC-3: отказы save — ничего не записано, временного нет.
func TestImagesSaveRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(f *airgapFixture)
		vars  map[string]string
		want  string
		calls bool // успел ли он что-то скачать или скопировать
	}{
		"bad image signature": {setup: func(f *airgapFixture) { f.r.cosignBad = "sdlc-pipeline-backend" },
			want: "did not verify for " + verifyIdentity("sdlc-pipeline-backend", "v0.13.0")},
		"bad package signature": {setup: func(f *airgapFixture) { f.r.cosignBad = "package" },
			want: "the signature of checksums.txt of v0.13.0 did not verify"},
		"no regctl": {setup: func(f *airgapFixture) {
			look := lookPath
			lookPath = func(n string) (string, error) {
				if n == "regctl" {
					return "", errors.New("not found")
				}
				return look(n)
			}
		}, want: "regctl is not installed", calls: true},
		"old regctl": {setup: func(f *airgapFixture) { f.r.regctlVer = "v0.8.1" }, want: "regctl v0.8.1 is older than v0.9.2", calls: true},
		"require without cosign": {setup: func(f *airgapFixture) { f.r.cosign = false }, vars: map[string]string{"CADENCE_VERIFY": "require"},
			want: "CADENCE_VERIFY=require, but cosign 3.x is not installed", calls: true},
		"auto without cosign": {setup: func(f *airgapFixture) { f.r.cosign = false },
			want: "cosign 3.x is not installed: save verifies the release", calls: true},
		"regctl fails": {setup: func(f *airgapFixture) { f.r.regctlFail = "pgvector/pgvector:pg18" },
			want: "regctl could not copy pgvector/pgvector:pg18"},
	} {
		t.Run(name, func(t *testing.T) {
			f := airgapSetup(t, c.vars)
			c.setup(f)
			if code := f.images("save", "--version", "v0.13.0", "--output", "a.tar"); code != 1 ||
				!strings.Contains(f.errw.String(), c.want) || !strings.Contains(f.errw.String(), nothingSaved) {
				t.Errorf("code %d\n%s", code, f.errw)
			}
			if left := f.leftovers(); len(left) > 0 {
				t.Errorf("left: %v", left)
			}
			if c.calls && (len(f.r.callsWith("regctl image")) > 0 || len(f.r.callsWith("docker compose")) > 0) {
				t.Errorf("went on: %v", f.r.calls)
			}
		})
	}
	// Архив уже есть — не перезаписывается.
	f := airgapSetup(t, nil)
	mustWrite(t, filepath.Join(f.e.cwd, "a.tar"), "mine", 0o644)
	if code := f.images("save", "--version", "v0.13.0", "--output", "a.tar"); code != 1 ||
		readFile(t, filepath.Join(f.e.cwd, "a.tar")) != "mine" || !strings.Contains(f.errw.String(), "is not overwritten") {
		t.Errorf("exists: %d\n%s", code, f.errw)
	}
}

// AC-3: CADENCE_VERIFY=off — одна строка ▸, без подписей и корня.
func TestImagesSaveOff(t *testing.T) {
	f := airgapSetup(t, map[string]string{"CADENCE_VERIFY": "off"})
	f.r.cosign = false
	p := f.save()
	if countLines(f.out.String(), "▸ signature verification is off") != 1 || len(cosignCalls(f.r.fakeRunner)) != 0 {
		t.Errorf("off:\n%s\n%v", f.out, f.r.calls)
	}
	ents := tarEntries(t, p)
	if _, ok := ents["download/v0.13.0/"+trustedRootAsset]; ok {
		t.Error("trusted_root.json without verification")
	}
	if _, ok := ents["download/v0.13.0/"+verifyBundle]; !ok {
		t.Error("the bundle of the release is an asset of the mirror even with off")
	}
}

// ── load ───────────────────────────────────────────────────────────────────

// loadSetup — архив v0.13.0, сохранённый на «другой машине», и новая
// машина без сети с пустым Docker.
func loadSetup(t *testing.T, vars map[string]string) (*airgapFixture, string) {
	t.Helper()
	f := airgapSetup(t, vars)
	p := f.save()
	f.fresh()
	f.r.offline = true
	return f, p
}

// clean — после load нет временного реестра, его образа и ссылок на него.
func (f *airgapFixture) clean() {
	f.t.Helper()
	if len(f.r.containers) != 0 || len(f.r.late) != 0 {
		f.t.Errorf("containers left: %v %v", f.r.containers, f.r.late)
	}
	for ref := range f.r.local {
		if ref == airgapRegistryName || strings.HasPrefix(ref, testRegAddr) {
			f.t.Errorf("left in Docker: %s", ref)
		}
	}
	if len(f.r.callsWith("docker run -d")) > 0 && len(f.r.callsWith("docker rm -f -v clab-images-load-")) == 0 {
		f.t.Error("the temporary registry was not removed")
	}
}

// AC-4, AC-5, AC-8: без своего реестра на containerd — проверка без сети,
// pin находится, временный реестр убран, зеркало релизов рядом с архивом,
// повтор — без новых образов.
func TestImagesLoad(t *testing.T) {
	f, p := loadSetup(t, nil)
	if code := f.images("load", p); code != 0 {
		t.Fatalf("load: %d\n%s\n%s", code, f.out, f.errw)
	}
	out := f.out.String()
	if !strings.Contains(out, "✓ the signature of checksums.txt of v0.13.0 verified offline with trusted_root.json of the archive") {
		t.Errorf("checksums:\n%s", out)
	}
	for _, img := range []struct{ repo, pin string }{{"sdlc-pipeline-backend", testBackendPin}, {"sdlc-pipeline-frontend", testFrontendPin}} {
		target := testRegAddr + "/cadence-lab-tech/" + img.repo + "@" + img.pin
		if len(f.r.callsWith("cosign verify --offline --trusted-root ")) == 0 ||
			!strings.Contains(out, "✓ "+target+" is signed by "+verifyIdentity(img.repo, "v0.13.0")+" (verified offline") {
			t.Errorf("verify %s:\n%s", img.repo, out)
		}
	}
	backend := testRegAddr + "/cadence-lab-tech/sdlc-pipeline-backend@" + testBackendPin
	frontend := testRegAddr + "/cadence-lab-tech/sdlc-pipeline-frontend@" + testFrontendPin
	if att := f.r.callsWith("cosign verify-attestation --type openvex --offline --trusted-root "); len(att) != 2 ||
		!strings.HasSuffix(att[0], " "+backend) || !strings.HasSuffix(att[1], " "+frontend) ||
		!strings.Contains(out, "✓ the OpenVEX of "+backend+" is signed by "+verifyIdentity("sdlc-pipeline-backend", "v0.13.0")+" (verified offline") ||
		!strings.Contains(out, "✓ the OpenVEX of "+frontend+" is signed by "+verifyIdentity("sdlc-pipeline-frontend", "v0.13.0")+" (verified offline") {
		t.Errorf("OpenVEX: %v\n%s", att, out)
	}
	for _, img := range airgapImages("v0.13.0") {
		if f.r.inspect(img) == "" {
			t.Errorf("%s is not found in Docker", img)
		}
		if !strings.Contains(out, "✓ "+img+"\n") {
			t.Errorf("no ✓ %s", img)
		}
	}
	if f.r.inspect("ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.13.0@"+testBackendPin) != testBackendPin {
		t.Error("the ID of our image is not its pin")
	}
	f.clean()
	mirror := filepath.Dir(p)
	for _, n := range []string{packageName("v0.13.0"), "checksums.txt", verifyBundle, "get-cadence.sh", trustedRootAsset} {
		if !isFile(filepath.Join(mirror, "download", "v0.13.0", n)) {
			t.Errorf("no %s in the mirror", n)
		}
	}
	if strings.Contains(out, "CADENCE_REGISTRY") || !strings.Contains(out, "CADENCE_RELEASES_URL=file://"+mirror+" ") ||
		!strings.Contains(out, "server install --version v0.13.0") || !strings.Contains(out, "./upgrade.sh v0.13.0") {
		t.Errorf("next:\n%s", out)
	}
	// Образы — без --referrers, бандлы — по одному (F3, 0059); аттестация
	// BuildKit внутри индекса — часть образа, отброшенной не названа.
	regs := f.r.callsWith("regctl image copy --host=reg=" + testRegAddr + ",tls=disabled ocidir://")
	bundles := 0
	for _, c := range regs {
		if strings.Contains(c, "@sha256:ref") {
			bundles++
		}
	}
	if len(regs) != 10 || bundles != 4 || len(f.r.callsWith("regctl image copy --referrers")) != 0 ||
		len(f.r.regRefs["sdlc-pipeline-backend"]) != 2 || len(f.r.regRefs["sdlc-pipeline-frontend"]) != 2 {
		t.Errorf("copies into the temporary registry: %v", f.r.callsWith("regctl"))
	}
	if strings.Contains(f.errw.String(), "not carried") {
		t.Errorf("dropped in a genuine archive:\n%s", f.errw)
	}
	// Подписи — до того, как что-то попало в Docker.
	calls := f.r.calls
	if v, pull := indexOf(calls, "cosign verify --offline"), indexOf(calls, "docker pull"); v < 0 || pull < v {
		t.Errorf("order: %v", calls)
	}
	// Повтор: тот же набор, ничего не скачано заново.
	before := len(f.r.local)
	f.r.calls, f.out = nil, &bytes.Buffer{}
	f.e.out = f.out
	if code := f.images("load", p); code != 0 || len(f.r.local) != before || len(f.r.callsWith("docker pull")) != 0 {
		t.Errorf("again: %d %d→%d %v\n%s", code, before, len(f.r.local), f.r.callsWith("docker pull"), f.errw)
	}
	f.clean()
}

// AC-6 (б), q11: классическое хранилище — pin не находится: отказ с
// подсказкой, временные ссылки и теги убраны.
func TestImagesLoadClassicStorage(t *testing.T) {
	f, p := loadSetup(t, nil)
	f.r.containerd = false
	if code := f.images("load", p); code != 1 {
		t.Fatalf("code %d\n%s", code, f.out)
	}
	msg := f.errw.String()
	for _, s := range []string{"is not found by its digest", "containerd image store", "--registry HOST[/PATH]", nothingLoaded} {
		if !strings.Contains(msg, s) {
			t.Errorf("no %q:\n%s", s, msg)
		}
	}
	if len(f.r.local) != 0 {
		t.Errorf("left in Docker: %v", f.r.local)
	}
	f.clean()
}

// AC-7: в свой реестр — наши индексом с тем же digest, сторонние в Docker
// по тегу; работает и на классическом хранилище.
func TestImagesLoadRegistry(t *testing.T) {
	f, p := loadSetup(t, nil)
	f.r.containerd = false
	if code := f.images("load", p, "--registry", "reg.example:5443/cadence/"); code != 0 {
		t.Fatalf("load: %d\n%s\n%s", code, f.out, f.errw)
	}
	for _, img := range []struct{ repo, pin string }{{"sdlc-pipeline-backend", testBackendPin}, {"sdlc-pipeline-frontend", testFrontendPin}} {
		src := f.r.pushed["reg.example:5443/cadence/"+img.repo+":v0.13.0"]
		if src != testRegAddr+"/cadence-lab-tech/"+img.repo+"@"+img.pin {
			t.Errorf("%s pushed from %q: %v", img.repo, src, f.r.pushed)
		}
		if f.r.inspect("ghcr.io/cadence-lab-tech/"+img.repo+":v0.13.0") != "" {
			t.Errorf("%s in Docker with --registry", img.repo)
		}
	}
	for _, img := range []string{"pgvector/pgvector:pg17", "pgvector/pgvector:pg18", helperImage, "caddy:2-alpine"} {
		if f.r.inspect(img) == "" {
			t.Errorf("%s is not in Docker", img)
		}
	}
	if out := f.out.String(); !strings.Contains(out, "set CADENCE_REGISTRY=reg.example:5443/cadence") ||
		!strings.Contains(out, "image.registry=reg.example:5443/cadence") ||
		!strings.Contains(out, "CADENCE_REGISTRY=reg.example:5443/cadence CADENCE_RELEASES_URL=") {
		t.Errorf("output:\n%s", out)
	}
	// Образ и два его проверенных бандла, без --referrers и --digest-tags.
	if push := f.r.callsWith("regctl image copy --host=reg=" + testRegAddr + ",tls=disabled " + testRegAddr); len(push) != 6 {
		t.Errorf("pushes: %v", f.r.callsWith("regctl"))
	}
	for _, img := range []string{"sdlc-pipeline-backend", "sdlc-pipeline-frontend"} {
		for _, b := range []string{"sha256:ref0", "sha256:ref1"} {
			if src := f.r.pushed["reg.example:5443/cadence/"+img+"@"+b]; src != testRegAddr+"/cadence-lab-tech/"+img+"@"+b {
				t.Errorf("bundle %s of %s: %q", b, img, src)
			}
		}
	}
	f.clean()

	// Реестр недоступен — отказ с адресом, ничего в Docker.
	g, q := loadSetup(t, nil)
	g.r.regctlFail = "reg.example"
	if code := g.images("load", q, "--registry", "reg.example/cadence"); code != 1 ||
		!strings.Contains(g.errw.String(), "to reg.example/cadence (see above)") || len(g.r.local) != 0 {
		t.Errorf("unreachable: %d %v\n%s", code, g.r.local, g.errw)
	}
	g.clean()
}

// AC-4: подмена в архиве — отказ до Docker и реестра администратора.
func TestImagesLoadTampered(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(f *airgapFixture, p string)
		want  string
	}{
		"image signature": {func(f *airgapFixture, _ string) { f.r.cosignBad = "sdlc-pipeline-frontend" },
			"did not verify for " + verifyIdentity("sdlc-pipeline-frontend", "v0.13.0")},
		"checksums signature": {func(f *airgapFixture, _ string) { f.r.cosignBad = "package" },
			"the signature of checksums.txt of v0.13.0 in the archive did not verify"},
		"package": {func(f *airgapFixture, p string) {
			rewriteTar(t, p, "download/v0.13.0/"+packageName("v0.13.0"), func(s string) string { return s + "x" })
		}, "does not match its checksums.txt"},
		"manifest digest": {func(f *airgapFixture, p string) {
			rewriteTar(t, p, "cadence-images.json", func(s string) string { return strings.Replace(s, testBackendPin, testPg17Digest, 1) })
		}, "the archive does not hold ghcr.io/cadence-lab-tech/sdlc-pipeline-backend"},
		// Тег раскладки указывает не на pin (index_redigested тестирования).
		"index redigested": {func(f *airgapFixture, _ string) {
			f.r.layoutDigest = map[string]string{"sdlc-pipeline-backend": testPg17Digest}
		}, "the image ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.13.0 in the archive is not " + testBackendPin},
		"not an archive": {func(f *airgapFixture, p string) {
			rewriteTar(t, p, "cadence-images.json", func(string) string { return "{" })
		}, "not an archive of clab server images save"},
	} {
		t.Run(name, func(t *testing.T) {
			f, p := loadSetup(t, nil)
			c.setup(f, p)
			if code := f.images("load", p, "--registry", "reg.example/cadence"); code != 1 ||
				!strings.Contains(f.errw.String(), c.want) {
				t.Errorf("code %d\n%s", code, f.errw)
			}
			if len(f.r.local) != 0 {
				t.Errorf("loaded: %v %v", f.r.local, f.r.pushed)
			}
			for dst := range f.r.pushed {
				if strings.HasPrefix(dst, "reg.example") {
					t.Errorf("pushed to the registry: %s", dst)
				}
			}
			f.clean()
		})
	}
}

// OpenVEX в load (решение человека по O2 тестирования): подменённый,
// подложенный рядом или пропавший OpenVEX и любой непроверяемый бандл
// Sigstore — отказ до Docker и реестра администратора. Подпись удалена,
// OpenVEX цел — проходит: cosign засчитывает подписью и аттестацию той же
// identity (0059, AC-4 — «не осталось действительной подписи»).
func TestImagesLoadVEX(t *testing.T) {
	sign, vex := fakeRef{pred: signPredicate}, fakeRef{pred: openvexPredicate}
	for name, c := range map[string]struct {
		refs []fakeRef // бандлы backend
		ok   bool
		want string
	}{
		"VEX tampered":           {[]fakeRef{sign, {pred: openvexPredicate, bad: true}}, false, "1 of the 2 Sigstore bundles attached to"},
		"VEX forged next to it":  {[]fakeRef{sign, vex, {pred: openvexPredicate, bad: true}}, false, "1 of the 3 Sigstore bundles"},
		"VEX only, tampered":     {[]fakeRef{{pred: openvexPredicate, bad: true}}, false, "the signature of " + testRegAddr},
		"VEX missing":            {[]fakeRef{sign}, false, "the archive has no OpenVEX of " + testRegAddr},
		"signature tampered":     {[]fakeRef{{pred: signPredicate, bad: true}, vex}, false, "1 of the 2 Sigstore bundles"},
		"forged bundle":          {[]fakeRef{sign, vex, {pred: signPredicate, bad: true}}, false, "1 of the 3 Sigstore bundles"},
		"signature removed (O1)": {[]fakeRef{vex}, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f, p := loadSetup(t, nil)
			f.r.refs["sdlc-pipeline-backend"] = c.refs
			code := f.images("load", p, "--registry", "reg.example/cadence")
			if c.ok {
				if code != 0 {
					t.Errorf("code %d\n%s", code, f.errw)
				}
				f.clean()
				return
			}
			if code != 1 || !strings.Contains(f.errw.String(), c.want) || !strings.Contains(f.errw.String(), nothingLoaded) {
				t.Errorf("code %d\n%s", code, f.errw)
			}
			if len(f.r.local) != 0 {
				t.Errorf("loaded: %v", f.r.local)
			}
			for dst := range f.r.pushed {
				if strings.HasPrefix(dst, "reg.example") {
					t.Errorf("pushed to the registry: %s", dst)
				}
			}
			f.clean()
		})
	}
	// Подменённый OpenVEX, который verify засчитал (бандлов столько же),
	// ловит verify-attestation.
	f, p := loadSetup(t, nil)
	f.r.onRun = func(c cmdSpec) {
		if len(c.args) > 0 && c.args[0] == "verify-attestation" {
			f.r.regRefs["sdlc-pipeline-backend"] = []fakeRef{{pred: signPredicate}, {pred: openvexPredicate, bad: true}}
		}
	}
	if code := f.images("load", p); code != 1 || !strings.Contains(f.errw.String(), "the OpenVEX of "+testRegAddr) || len(f.r.local) != 0 {
		t.Errorf("verify-attestation: %d %v\n%s", code, f.r.local, f.errw)
	}
	f.clean()
}

// F3 тестирования, правило «в реестр только проверенное» (решение
// человека, 0059): из архива дальше идут только образ и проверенные бандлы
// Sigstore; любой другой referrer — подложенный OpenVEX не бандлом
// (in-toto, неподписанный от oras attach), бандл на платформенном
// манифесте, бандлы без проверки — не попадает ни во временный реестр, ни
// к администратору и назван одной строкой; исход load он не меняет.
func TestImagesLoadForeignReferrers(t *testing.T) {
	sign, vex := fakeRef{pred: signPredicate}, fakeRef{pred: openvexPredicate}
	for name, c := range map[string]struct {
		vars    map[string]string
		refs    []fakeRef // backend
		carried int       // бандлов backend у администратора
		line    string
	}{
		"in-toto VEX next to it (vex_relabeled)": {nil, []fakeRef{sign, vex, {pred: openvexPredicate, typ: "application/vnd.in-toto+json"}}, 2,
			"application/vnd.in-toto+json of sdlc-pipeline-backend"},
		"unsigned OpenVEX (vex_oras)": {nil, []fakeRef{sign, vex, {typ: "application/vnd.openvex+json"}, {typ: "application/vnd.openvex+json"}}, 2,
			"application/vnd.openvex+json ×2 of sdlc-pipeline-backend"},
		"bundle on a platform manifest": {nil, []fakeRef{sign, vex, {pred: openvexPredicate, child: true}}, 2,
			"application/vnd.dev.sigstore.bundle.v0.3+json of sdlc-pipeline-backend"},
		"signatures not verified (off)": {map[string]string{"CADENCE_VERIFY": "off"}, []fakeRef{sign, vex}, 0,
			"application/vnd.dev.sigstore.bundle.v0.3+json ×2 of sdlc-pipeline-backend, application/vnd.dev.sigstore.bundle.v0.3+json ×2 of sdlc-pipeline-frontend"},
	} {
		t.Run(name, func(t *testing.T) {
			f, p := loadSetup(t, c.vars)
			f.r.refs["sdlc-pipeline-backend"] = c.refs
			if code := f.images("load", p, "--registry", "reg.example/cadence"); code != 0 {
				t.Fatalf("code %d\n%s", code, f.errw)
			}
			if got := countLines(f.errw.String(), "! not carried from the archive, not a verified Sigstore bundle of the release: "+c.line); got != 1 {
				t.Errorf("the line of the dropped:\n%s", f.errw)
			}
			want := 0
			if c.vars == nil {
				want = 2
			}
			if len(f.r.regRefs["sdlc-pipeline-backend"]) != want {
				t.Errorf("in the temporary registry: %v", f.r.regRefs)
			}
			carried := 0
			for dst, src := range f.r.pushed {
				if !strings.HasPrefix(dst, "reg.example/cadence/sdlc-pipeline-backend@") {
					continue
				}
				carried++
				n, _ := strconv.Atoi(strings.TrimPrefix(dst, "reg.example/cadence/sdlc-pipeline-backend@sha256:ref"))
				if ref := c.refs[n]; ref.typ != "" || ref.child || !strings.HasSuffix(src, "@sha256:ref"+strconv.Itoa(n)) {
					t.Errorf("carried %s from %s: %+v", dst, src, ref)
				}
			}
			if carried != c.carried {
				t.Errorf("carried %d, want %d: %v", carried, c.carried, f.r.pushed)
			}
			f.clean()
		})
	}
}

// O4 тестирования: SIGINT в любой точке load — только «interrupted»: ни
// причины, которую приписал бы оборванный вызов, ни его ответа на сигнал;
// Docker в исходном, временного реестра нет.
func TestImagesLoadInterruptedAnywhere(t *testing.T) {
	for _, point := range []string{"docker compose version", "docker info", "regctl version", "cosign verify-blob",
		"docker version", "docker image inspect", "regctl image copy", "regctl manifest get", "regctl artifact list",
		"cosign verify --offline", "cosign verify-attestation", "docker pull", "docker tag", "docker compose --project-directory", "cosign version", "regctl image digest",
		// Ctrl-C группе процессов обрывает и docker rmi ссылки временного реестра после tag.
		"docker rmi " + testRegAddr} {
		t.Run(point, func(t *testing.T) {
			f, p := loadSetup(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hit := false
			f.r.onCall = func(c cmdSpec) error {
				if hit || c.detach || !strings.HasPrefix(c.String(), point) {
					return nil
				}
				hit = true
				cancel()
				if c.stderr != nil {
					io.WriteString(c.stderr, "Error: context canceled\n")
				}
				return exitErr(130)
			}
			code := runServer(ctx, f.e, []string{"images", "load", p})
			if !hit {
				t.Fatalf("%s is not called: %v", point, f.r.calls)
			}
			if msg := f.errw.String(); code != 1 || msg != "✗ interrupted. "+nothingLoaded+"\n" {
				t.Errorf("code %d:\n%s", code, msg)
			}
			f.clean()
			if len(f.r.local) != 0 {
				t.Errorf("left in Docker: %v", f.r.local)
			}
		})
	}
	// Демон поставил тег, а docker tag оборван Ctrl-C: тег тоже убирается.
	{
		f, p := loadSetup(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.r.onCall = func(c cmdSpec) error {
			if !strings.HasPrefix(c.String(), "docker tag") {
				return nil
			}
			f.r.local[c.args[2]] = f.r.local[c.args[1]]
			cancel()
			return exitErr(130)
		}
		if code := runServer(ctx, f.e, []string{"images", "load", p}); code != 1 || f.errw.String() != "✗ interrupted. "+nothingLoaded+"\n" {
			t.Errorf("tag applied: %d\n%s", code, f.errw)
		}
		f.clean()
		if len(f.r.local) != 0 {
			t.Errorf("tag applied, left in Docker: %v", f.r.local)
		}
	}
	// В реестр администратора уже ушёл backend — его и называет.
	f, p := loadSetup(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.r.onCall = func(c cmdSpec) error {
		if strings.HasPrefix(c.String(), "regctl image copy") && strings.Contains(c.String(), " reg.example/cadence/sdlc-pipeline-frontend:") {
			cancel()
			return exitErr(130)
		}
		return nil
	}
	if code := runServer(ctx, f.e, []string{"images", "load", p, "--registry", "reg.example/cadence"}); code != 1 ||
		f.errw.String() != "✗ interrupted. Already copied: reg.example/cadence/sdlc-pipeline-backend:v0.13.0@"+testBackendPin+". Nothing was loaded into Docker.\n" {
		t.Errorf("registry: %d\n%s", code, f.errw)
	}
	f.clean()
	// Образ backend уже у администратора, его бандл — ещё нет: образ назван.
	g, q := loadSetup(t, nil)
	gctx, gcancel := context.WithCancel(context.Background())
	defer gcancel()
	g.r.onCall = func(c cmdSpec) error {
		if strings.HasPrefix(c.String(), "regctl image copy") && strings.Contains(c.String(), " reg.example/cadence/sdlc-pipeline-backend@") {
			gcancel()
			return exitErr(130)
		}
		return nil
	}
	if code := runServer(gctx, g.e, []string{"images", "load", q, "--registry", "reg.example/cadence"}); code != 1 ||
		g.errw.String() != "✗ interrupted. Already copied: reg.example/cadence/sdlc-pipeline-backend:v0.13.0@"+testBackendPin+". Nothing was loaded into Docker.\n" {
		t.Errorf("registry, bundle: %d\n%s", code, g.errw)
	}
	g.clean()
}

// OpenVEX обязателен с той версии, с которой его прикладывает release.yml:
// у frontend — с v0.13.0 (CLAB-234), раньше его отсутствие — не подмена.
func TestArchiveVEXSince(t *testing.T) {
	f, _ := loadSetup(t, nil)
	f.r.regRefs["sdlc-pipeline-frontend"] = []fakeRef{{pred: signPredicate}}
	ctx := context.Background()
	v, err := newVerifier(ctx, f.e, "auto", nothingLoaded)
	if err != nil {
		t.Fatal(err)
	}
	host := "--host=reg=" + testRegAddr + ",tls=disabled"
	target := testRegAddr + "/cadence-lab-tech/sdlc-pipeline-frontend@" + testFrontendPin
	if err := verifyArchiveImage(ctx, f.e, v, host, "v0.12.0", "sdlc-pipeline-frontend", target, "root.json"); err != nil {
		t.Errorf("v0.12.0: %v", err)
	}
	if err := verifyArchiveImage(ctx, f.e, v, host, "v0.13.0", "sdlc-pipeline-frontend", target, "root.json"); err == nil ||
		!strings.Contains(err.Error(), "the archive has no OpenVEX of "+target) {
		t.Errorf("v0.13.0: %v", err)
	}
}

// AC-4, AC-8: режимы проверки, regctl, версия архива — отказы до действий.
func TestImagesLoadModes(t *testing.T) {
	// auto без cosign — одно предупреждение и загрузка.
	f, p := loadSetup(t, nil)
	f.r.cosign = false
	if code := f.images("load", p); code != 0 || countLines(f.errw.String(), "! cosign 3.x is not installed") != 1 {
		t.Errorf("auto: %d\n%s", code, f.errw)
	}
	// require без cosign — до любых действий.
	g, q := loadSetup(t, map[string]string{"CADENCE_VERIFY": "require"})
	g.r.cosign = false
	if code := g.images("load", q); code != 1 || len(g.r.callsWith("docker load")) != 0 ||
		!strings.Contains(g.errw.String(), "CADENCE_VERIFY=require, but cosign 3.x is not installed") {
		t.Errorf("require: %d\n%s", code, g.errw)
	}
	// off — одна строка ▸, cosign не зовётся.
	h, r := loadSetup(t, map[string]string{"CADENCE_VERIFY": "off"})
	if code := h.images("load", r); code != 0 || countLines(h.out.String(), "▸ signature verification is off") != 1 ||
		len(cosignCalls(h.r.fakeRunner)) != 0 {
		t.Errorf("off: %d\n%s\n%v", code, h.out, h.r.calls)
	}
	// Нет regctl — отказ до Docker.
	k, s := loadSetup(t, nil)
	look := lookPath
	lookPath = func(n string) (string, error) {
		if n == "regctl" {
			return "", errors.New("not found")
		}
		return look(n)
	}
	if code := k.images("load", s); code != 1 || len(k.r.callsWith("docker")) != 0 || !strings.Contains(k.errw.String(), "regctl is not installed") {
		t.Errorf("no regctl: %d %v\n%s", code, k.r.calls, k.errw)
	}
	lookPath = look
	// Архив новее clab — подсказка взять clab пакета архива.
	m, u := loadSetup(t, nil)
	m.e.cli = "v0.12.0"
	if code := m.images("load", u); code != 1 || !strings.Contains(m.errw.String(), "Use the clab of the package in the archive") ||
		len(m.r.callsWith("docker load")) != 0 {
		t.Errorf("newer: %d\n%s", code, m.errw)
	}
}

// Архив без trusted_root.json (save с off): require — отказ, auto — !.
func TestImagesLoadNoRoot(t *testing.T) {
	f := airgapSetup(t, map[string]string{"CADENCE_VERIFY": "off"})
	p := f.save()
	f.vars["CADENCE_VERIFY"] = "require"
	f.fresh()
	if code := f.images("load", p); code != 1 || !strings.Contains(f.errw.String(), "CADENCE_VERIFY=require refuses it") ||
		len(f.r.callsWith("docker load")) != 0 {
		t.Errorf("require: %d\n%s", code, f.errw)
	}
	f.vars["CADENCE_VERIFY"] = "auto"
	f.fresh()
	if code := f.images("load", p); code != 0 || countLines(f.errw.String(), "! the archive has no trusted_root.json") != 1 {
		t.Errorf("auto: %d\n%s", code, f.errw)
	}
}

// AC-8: прерывание посреди копирования — временный реестр убран.
func TestImagesLoadInterrupted(t *testing.T) {
	f, p := loadSetup(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.r.onRegctlCopy = func() {
		cancel()
		f.r.regctlFail = "image copy"
	}
	if code := runServer(ctx, f.e, []string{"images", "load", p}); code != 1 {
		t.Errorf("code %d", code)
	}
	f.clean()
	if len(f.r.local) != 0 {
		t.Errorf("left: %v", f.r.local)
	}
}

// AC-8, F1 тестирования: SIGINT во время docker run временного реестра.
// Прерванный посреди запроса docker run демон дописывает уже после rm
// уборки; поэтому docker load и docker run идут с detach (своя группа
// процессов, без отмены) и доходят до конца, а уборка видит контейнер.
// Без detach подставной Docker ведёт себя как настоящий: CLI оборван,
// контейнер появляется позже.
func TestImagesLoadInterruptedRun(t *testing.T) {
	f, p := loadSetup(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var detached []string
	f.r.onDockerRun = func(c cmdSpec) error {
		cancel()
		if !c.detach {
			f.r.late[c.args[3]] = strings.TrimPrefix(c.args[5], airgapLabel+"=")
			return exitErr(130)
		}
		f.r.containers[c.args[3]] = strings.TrimPrefix(c.args[5], airgapLabel+"=")
		return nil
	}
	f.r.onRun = func(c cmdSpec) {
		if c.detach {
			detached = append(detached, c.String())
		}
	}
	if code := runServer(ctx, f.e, []string{"images", "load", p}); code != 1 {
		t.Errorf("code %d\n%s", code, f.errw)
	}
	f.clean()
	if len(f.r.local) != 0 {
		t.Errorf("left in Docker: %v", f.r.local)
	}
	for _, want := range []string{"docker load -q -i ", "docker run -d --name clab-images-load-", "docker rm -f -v clab-images-load-", "docker rmi " + airgapRegistryName} {
		found := false
		for _, d := range detached {
			found = found || strings.HasPrefix(d, want)
		}
		if !found {
			t.Errorf("%s is not detached: %v", want, detached)
		}
	}
	if len(f.r.callsWith("regctl image copy")) != 0 {
		t.Error("copied after the interruption")
	}
}

// docker run не дошёл до успеха, а контейнер появился у демона после rm —
// уборка находит его по метке и убирает с томом.
func TestAirgapRegistryLateContainer(t *testing.T) {
	f, p := loadSetup(t, nil)
	f.r.onDockerRun = func(c cmdSpec) error {
		f.r.late[c.args[3]] = strings.TrimPrefix(c.args[5], airgapLabel+"=")
		return exitErr(1)
	}
	if code := f.images("load", p); code != 1 || !strings.Contains(f.errw.String(), "docker could not start the temporary registry") {
		t.Errorf("code %d\n%s", code, f.errw)
	}
	f.clean()
	if rm := f.r.callsWith("docker rm -f -v "); len(rm) != 2 {
		t.Errorf("rm: %v", rm)
	}
	if len(f.r.local) != 0 {
		t.Errorf("left in Docker: %v", f.r.local)
	}
}

// ── закрытый контур в install и upgrade (A4) ───────────────────────────────

// pullImages (решение человека по F2 тестирования, вариант б): образ с
// pin, который уже здесь, не тянется; сторонний по тегу тянется всегда,
// когда его реестр отвечает, а без ответа остаётся локальный; остальные
// отсутствующие — pull --policy missing; ничего локально — прежний pull.
func TestPullImagesLocal(t *testing.T) {
	dir := installed(t, "v0.13.0", nil)
	ours := "ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.13.0@" + testBackendPin
	pg := "pgvector/pgvector:pg17"
	local := "localhost:5050/sdlc-pipeline-frontend:v0.13.0"
	for name, c := range map[string]struct {
		local   []string
		down    bool
		pulls   []string // docker compose pull и docker pull, по порядку
		nothing bool     // «nothing to pull»
		kept    bool     // предупреждение о неответивших реестрах
		one     bool     // ...об одном
	}{
		"one kept":              {local: []string{ours, pg}, down: true, pulls: []string{"docker compose pull --quiet --policy missing"}, one: true},
		"all, online":           {local: []string{ours, pg, local}, pulls: []string{"docker pull -q " + local, "docker pull -q " + pg}},
		"all, offline":          {local: []string{ours, pg, local}, down: true, nothing: true, kept: true},
		"pinned only, offline":  {local: []string{ours}, down: true, pulls: []string{"docker compose pull --quiet --policy missing"}},
		"ours missing, online":  {local: []string{pg}, pulls: []string{"docker pull -q " + pg, "docker compose pull --quiet --policy missing"}},
		"ours missing, offline": {local: []string{pg, local}, down: true, kept: true, pulls: []string{"docker compose pull --quiet --policy missing"}},
		"none":                  {pulls: []string{"docker compose pull --quiet"}},
		"none, offline":         {down: true, pulls: []string{"docker compose pull --quiet"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newImagesRunner()
			r.composeImgs = strings.Join([]string{ours, pg, local, pg}, "\n") + "\n"
			for _, l := range c.local {
				r.local[l] = "sha256:x"
			}
			e, out, errw := testEnv(t, dir, r.fakeRunner, nil)
			e.run = r
			var probes []string
			e.reachable = func(_ context.Context, u string) bool {
				probes = append(probes, u)
				return !c.down
			}
			inst, err := openInstallation(e, dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := inst.pullImages(context.Background()); err != nil {
				t.Fatal(err)
			}
			var pulls []string
			for _, call := range r.calls {
				if strings.HasPrefix(call, "docker compose pull") || strings.HasPrefix(call, "docker pull") {
					pulls = append(pulls, call)
				}
			}
			if fmt.Sprint(pulls) != fmt.Sprint(c.pulls) {
				t.Errorf("pulls: %v, want %v", pulls, c.pulls)
			}
			if got := strings.Contains(out.String(), "images are on this machine: nothing to pull"); got != c.nothing {
				t.Errorf("nothing to pull: %v\n%s", got, out)
			}
			if got := strings.Contains(errw.String(), "do not answer, the images on this machine are kept"); got != c.kept {
				t.Errorf("kept: %v\n%s", got, errw)
			}
			if got := strings.Contains(errw.String(), "! "+pg+": its registry does not answer, the image on this machine is kept"); got != c.one {
				t.Errorf("one kept: %v\n%s", got, errw)
			}
			for _, p := range probes {
				if p != "https://registry-1.docker.io/v2/" && p != "http://localhost:5050/v2/" {
					t.Errorf("probe %s", p)
				}
			}
		})
	}
}

func TestRegistryProbe(t *testing.T) {
	for ref, want := range map[string]string{
		"pgvector/pgvector:pg17":                          "https://registry-1.docker.io/v2/",
		"alpine:3":                                        "https://registry-1.docker.io/v2/",
		"ghcr.io/cadence-lab-tech/x:v1":                   "https://ghcr.io/v2/",
		"reg.corp.example:5443/cadence/x:v1":              "https://reg.corp.example:5443/v2/",
		"localhost:5050/sdlc-pipeline-backend:v0.13.0":    "http://localhost:5050/v2/",
		"127.0.0.1:5000/cadence/sdlc-pipeline-backend:v1": "http://127.0.0.1:5000/v2/",
	} {
		if got := registryProbe(ref); got != want {
			t.Errorf("%s: %s, want %s", ref, got, want)
		}
	}
}

// AC-9, q4: CADENCE_REGISTRY окружения установки — в .env, пустой — нет.
func TestInstallKeepsRegistry(t *testing.T) {
	f := installSetup(t, map[string]string{"CADENCE_REGISTRY": "reg.example/cadence"})
	if code := f.install("--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("install: %d\n%s", code, f.errw)
	}
	if got := f.env(f.dir("cadence"))["CADENCE_REGISTRY"]; got != "reg.example/cadence" {
		t.Errorf("CADENCE_REGISTRY in .env: %q", got)
	}
	g := installSetup(t, nil)
	if code := g.install("--dir", g.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("install: %d\n%s", code, g.errw)
	}
	if strings.Contains(readFile(t, filepath.Join(g.dir("cadence"), ".env")), "\nCADENCE_REGISTRY=") {
		t.Error("an empty CADENCE_REGISTRY written")
	}
}

// offlineInstall — install в контуре: релизы v0.13.0 с trusted_root.json
// (как после load), сети нет, образы — в Docker (local) или в зеркале.
func offlineInstall(t *testing.T, vars map[string]string, local bool) (*installFixture, *imagesRunner) {
	t.Helper()
	f := installSetup(t, vars)
	publishAirgap(t, f.releases, "v0.13.0", true)
	r := newImagesRunner()
	r.fakeRunner = f.r
	f.r.cosign = true
	r.offline = true
	f.r.ghcrDown = true
	f.e.run = r
	f.e.cli = "v0.13.0"
	if local {
		r.loaded(airgapImages("v0.13.0"))
	}
	return f, r
}

// AC-9, q10: без своего реестра — checksums.txt по trusted_root.json
// зеркала, образы «verified when loaded», require проходит.
func TestInstallOfflineLocal(t *testing.T) {
	f, r := offlineInstall(t, map[string]string{"CADENCE_VERIFY": "require"}, true)
	if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
	}
	out := f.out.String()
	if !strings.Contains(out, "✓ the signature of checksums.txt of v0.13.0 verified offline with trusted_root.json of the mirror") {
		t.Errorf("checksums:\n%s", out)
	}
	for _, img := range airgapImages("v0.13.0")[2:4] {
		if !strings.Contains(out, "✓ "+img+" verified when loaded: same digest as the pin of the signed package") {
			t.Errorf("no verified when loaded for %s:\n%s", img, out)
		}
	}
	if strings.Contains(f.errw.String(), "ghcr.io is unreachable") {
		t.Errorf("refused or warned:\n%s", f.errw)
	}
	if blob := r.callsWith("cosign verify-blob"); len(blob) != 2 || !strings.Contains(blob[1], "--trusted-root ") {
		t.Errorf("verify-blob: %v", blob)
	}
	// Образа нет в Docker — require отказывает с подсказкой load.
	g, _ := offlineInstall(t, map[string]string{"CADENCE_VERIFY": "require"}, false)
	if code := g.install("--version", "v0.13.0", "--dir", g.dir("cadence"), "--mode", "proxy", "--yes"); code != 1 ||
		!strings.Contains(g.errw.String(), "is not on this machine (load the archive of the version first") {
		t.Errorf("not loaded: %d\n%s", code, g.errw)
	}
	g.noEnv(g.dir("cadence"))
}

// AC-9: путь --registry — cosign verify --offline в CADENCE_REGISTRY.
func TestInstallOfflineMirror(t *testing.T) {
	f, r := offlineInstall(t, map[string]string{"CADENCE_VERIFY": "require", "CADENCE_REGISTRY": "reg.example/cadence"}, false)
	if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
	}
	for _, img := range []struct{ repo, pin string }{{"sdlc-pipeline-backend", testBackendPin}, {"sdlc-pipeline-frontend", testFrontendPin}} {
		want := "cosign verify --offline --trusted-root "
		found := false
		for _, c := range r.callsWith(want) {
			if strings.HasSuffix(c, " reg.example/cadence/"+img.repo+"@"+img.pin) {
				found = true
			}
		}
		if !found {
			t.Errorf("no offline verify of %s: %v", img.repo, r.callsWith("cosign verify"))
		}
	}
	if f.env(f.dir("cadence"))["CADENCE_REGISTRY"] != "reg.example/cadence" {
		t.Error("CADENCE_REGISTRY is not in .env")
	}
	// Подпись в зеркале не сходится — отказ.
	g, _ := offlineInstall(t, map[string]string{"CADENCE_REGISTRY": "reg.example/cadence"}, false)
	g.r.cosignBad = "sdlc-pipeline-backend"
	if code := g.install("--version", "v0.13.0", "--dir", g.dir("cadence"), "--mode", "proxy", "--yes"); code != 1 ||
		!strings.Contains(g.errw.String(), "the image is altered or not ours") {
		t.Errorf("bad: %d\n%s", code, g.errw)
	}
}

// AC-11: апгрейд в контуре — подпись по корню зеркала, образы локально,
// pull не ходит в сеть.
func TestUpgradeOffline(t *testing.T) {
	f := verifyUpgradeSetup(t, "", map[string]string{"CADENCE_VERIFY": "require"})
	publishAirgap(t, f.releases, "v0.14.0", true)
	r := newImagesRunner()
	r.fakeRunner = f.r
	r.offline = true
	f.r.ghcrDown = true
	f.e.run = r
	f.e.cli = "v0.14.0"
	imgs := airgapImages("v0.14.0")
	r.loaded(imgs)
	r.composeImgs = strings.Join(imgs[1:], "\n") + "\n"
	if code := f.upgrade("v0.14.0"); code != 0 {
		t.Fatalf("upgrade: %d\n%s\n%s", code, f.out, f.errw)
	}
	out := f.out.String()
	if !strings.Contains(out, "verified offline with trusted_root.json of the mirror") || !strings.Contains(out, "verified when loaded") ||
		!strings.Contains(out, "images are on this machine: nothing to pull") || len(r.callsWith("docker compose pull")) != 0 {
		t.Errorf("output:\n%s\n%v", out, r.callsWith("docker compose pull"))
	}
	if fmt.Sprint(f.version(t)) != "v0.14.0" {
		t.Errorf("VERSION %s", f.version(t))
	}
}
