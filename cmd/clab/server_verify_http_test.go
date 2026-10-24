package main

// Зеркало администратора по http (CLAB-280): cosign verify --offline в
// CADENCE_REGISTRY получает --allow-http-registry, только если Docker этой
// машины сам ходит туда по http (insecure-registries). Номера — критерии
// приёмки CLAB-280 (AC-n).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const (
	httpMirror = "registry.corp:5000/cadence"
	// Ответ Docker 27 на docker info --format '{{json .RegistryConfig}}' с
	// daemon.json {"insecure-registries": ["registry.corp:5000"]}.
	registryConfigInsecure = `{"AllowNondistributableArtifactsCIDRs":null,"AllowNondistributableArtifactsHostnames":null,` +
		`"InsecureRegistryCIDRs":["::1/128","127.0.0.0/8"],"IndexConfigs":{"docker.io":{"Name":"docker.io","Mirrors":[],"Secure":true,"Official":true},` +
		`"registry.corp:5000":{"Name":"registry.corp:5000","Mirrors":[],"Secure":false,"Official":false}},"Mirrors":null}` + "\n"
	// Тот же Docker без insecure-registries.
	registryConfigDefault = `{"AllowNondistributableArtifactsCIDRs":null,"AllowNondistributableArtifactsHostnames":null,` +
		`"InsecureRegistryCIDRs":["::1/128","127.0.0.0/8"],"IndexConfigs":{"docker.io":{"Name":"docker.io","Mirrors":[],"Secure":true,"Official":true}},"Mirrors":null}` + "\n"
)

// dockerInfoRunner — docker info --format '{{json .RegistryConfig}}' с
// ответом answer (fail — отказ демона); остальное — runner под ним.
type dockerInfoRunner struct {
	runner
	answer string
	fail   bool

	mu    sync.Mutex
	asked int
}

func (d *dockerInfoRunner) run(ctx context.Context, c cmdSpec) error {
	if c.name == "docker" && len(c.args) == 3 && c.args[0] == "info" && c.args[1] == "--format" {
		d.mu.Lock()
		d.asked++
		d.mu.Unlock()
		if c.args[2] != "{{json .RegistryConfig}}" {
			return errors.New("unexpected format " + c.args[2])
		}
		if d.fail {
			if c.stderr != nil {
				c.stderr.Write([]byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"))
			}
			return exitErr(1)
		}
		if c.stdout != nil {
			c.stdout.Write([]byte(d.answer))
		}
		return nil
	}
	return d.runner.run(ctx, c)
}

func (d *dockerInfoRunner) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked
}

// httpInstall — install в контуре из зеркала mirror; Docker отвечает
// answer (fail — не отвечает).
func httpInstall(t *testing.T, mirror, answer string, fail bool) (*installFixture, *imagesRunner, *dockerInfoRunner) {
	t.Helper()
	f, r := offlineInstall(t, map[string]string{"CADENCE_VERIFY": "require", "CADENCE_REGISTRY": mirror}, false)
	d := &dockerInfoRunner{runner: r, answer: answer, fail: fail}
	f.e.run = d
	return f, r, d
}

// mirrorVerify — ожидаемый вызов cosign verify --offline образа repo в
// зеркале mirror: аргументы main и, если flag, --allow-http-registry после
// корня.
func mirrorVerify(root, mirror, repo, pin, version string, flag bool) string {
	s := "cosign verify --offline --trusted-root " + root
	if flag {
		s += " --allow-http-registry"
	}
	return s + " --certificate-identity " + verifyIdentity(repo, version) + " --certificate-oidc-issuer " + verifyIssuer + " " + mirror + "/" + repo + "@" + pin
}

// offlineCalls — вызовы cosign verify --offline; root — корень в них.
func offlineCalls(t *testing.T, r *imagesRunner) ([]string, string) {
	t.Helper()
	calls := r.callsWith("cosign verify --offline --trusted-root ")
	if len(calls) == 0 {
		t.Fatalf("no cosign verify --offline: %v", r.callsWith("cosign"))
	}
	root := strings.Fields(calls[0])[4]
	return calls, root
}

var testImages = []struct{ repo, pin string }{{"sdlc-pipeline-backend", testBackendPin}, {"sdlc-pipeline-frontend", testFrontendPin}}

// checkMirrorCalls — оба образа проверены в зеркале ровно ожидаемым
// вызовом, ✓ напечатан, docker info спрошен не больше раза.
func checkMirrorCalls(t *testing.T, f *installFixture, r *imagesRunner, d *dockerInfoRunner, mirror, version string, flag bool) {
	t.Helper()
	calls, root := offlineCalls(t, r)
	if len(calls) != 2 {
		t.Errorf("two images, two calls: %v", calls)
	}
	for _, img := range testImages {
		want := mirrorVerify(root, mirror, img.repo, img.pin, version, flag)
		if indexOf(calls, want) < 0 {
			t.Errorf("no\n  %s\nin\n  %s", want, strings.Join(calls, "\n  "))
		}
		ok := "✓ " + mirror + "/" + img.repo + "@" + img.pin + " is signed by " + verifyIdentity(img.repo, version) +
			" (verified offline with trusted_root.json of the mirror of the releases)"
		if !strings.Contains(f.out.String(), ok) {
			t.Errorf("no %q in:\n%s", ok, f.out)
		}
	}
	if n := d.count(); n != 1 {
		t.Errorf("docker info asked %d times", n)
	}
}

// AC-1: зеркало объявлено Docker'у insecure по имени — оба вызова cosign
// с --allow-http-registry и прежними аргументами, установка проходит.
func TestInstallHTTPMirror(t *testing.T) {
	f, r, d := httpInstall(t, httpMirror, registryConfigInsecure, false)
	if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
	}
	checkMirrorCalls(t, f, r, d, httpMirror, "v0.13.0", true)
	if f.env(f.dir("cadence"))["CADENCE_REGISTRY"] != httpMirror {
		t.Error("CADENCE_REGISTRY is not in .env")
	}
}

// AC-2: upgrade с CADENCE_REGISTRY из .env установки — флаг передан.
func TestUpgradeHTTPMirror(t *testing.T) {
	f := verifyUpgradeSetup(t, "CADENCE_REGISTRY="+httpMirror+"\n", map[string]string{"CADENCE_VERIFY": "require"})
	publishAirgap(t, f.releases, "v0.14.0", true)
	r := newImagesRunner()
	r.fakeRunner = f.r
	r.offline = true
	f.r.ghcrDown = true
	d := &dockerInfoRunner{runner: r, answer: registryConfigInsecure}
	f.e.run = d
	f.e.cli = "v0.14.0"
	if code := f.upgrade("v0.14.0"); code != 0 {
		t.Fatalf("upgrade: %d\n%s\n%s", code, f.out, f.errw)
	}
	calls, root := offlineCalls(t, r)
	for _, img := range testImages {
		if want := mirrorVerify(root, httpMirror, img.repo, img.pin, "v0.14.0", true); indexOf(calls, want) < 0 {
			t.Errorf("no\n  %s\nin\n  %s", want, strings.Join(calls, "\n  "))
		}
	}
	if n := d.count(); n != 1 {
		t.Errorf("docker info asked %d times", n)
	}
	if f.version(t) != "v0.14.0" {
		t.Errorf("VERSION %s", f.version(t))
	}
}

// AC-3: реестр по IP-литералу и по имени, разрешающемуся в сеть из
// InsecureRegistryCIDRs, — флаг передан.
func TestInstallHTTPMirrorCIDR(t *testing.T) {
	config := `{"InsecureRegistryCIDRs":["::1/128","127.0.0.0/8","10.0.0.0/8"],"IndexConfigs":{"docker.io":{"Name":"docker.io","Secure":true,"Official":true}}}`
	for _, c := range []struct{ name, mirror string }{{"ip", "10.1.2.3:5000/cadence"}, {"name", "mirror.corp:5000/cadence"}} {
		t.Run(c.name, func(t *testing.T) {
			f, r, d := httpInstall(t, c.mirror, config, false)
			var looked []string
			f.e.lookupHost = func(host string) ([]string, error) {
				looked = append(looked, host)
				if host == "mirror.corp" {
					return []string{"10.9.8.7"}, nil
				}
				return nil, errors.New("no such host")
			}
			if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
				t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
			}
			checkMirrorCalls(t, f, r, d, c.mirror, "v0.13.0", true)
			if c.name == "name" && indexOf(looked, "mirror.corp") < 0 {
				t.Errorf("mirror.corp not resolved: %v", looked)
			}
			if c.name == "ip" && indexOf(looked, "10.1.2.3") >= 0 {
				t.Errorf("an IP literal resolved: %v", looked)
			}
		})
	}
}

// AC-4: реестр не объявлен (нет в IndexConfigs, Secure=true, адрес вне
// сетей) — флага нет, аргументы cosign те же, что на main.
func TestInstallHTTPMirrorSecure(t *testing.T) {
	cases := map[string]string{
		"not declared": registryConfigDefault,
		"secure":       `{"InsecureRegistryCIDRs":["10.0.0.0/8"],"IndexConfigs":{"registry.corp:5000":{"Name":"registry.corp:5000","Secure":true}}}`,
		"outside":      `{"InsecureRegistryCIDRs":["10.0.0.0/8"],"IndexConfigs":{}}`,
		"other port":   `{"InsecureRegistryCIDRs":[],"IndexConfigs":{"registry.corp:5001":{"Name":"registry.corp:5001","Secure":false}}}`,
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			f, r, d := httpInstall(t, httpMirror, config, false)
			f.e.lookupHost = func(host string) ([]string, error) {
				if name == "secure" {
					return []string{"10.1.1.1"}, nil // Secure по имени важнее сети, как у Docker
				}
				return []string{"192.0.2.10"}, nil
			}
			if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
				t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
			}
			checkMirrorCalls(t, f, r, d, httpMirror, "v0.13.0", false)
		})
	}
}

// AC-5: Docker не ответил или ответил не JSON — флага нет, ни отказа, ни
// предупреждения: вывод тот же, что у необъявленного реестра.
func TestInstallHTTPMirrorNoDockerInfo(t *testing.T) {
	base, _, _ := httpInstall(t, httpMirror, registryConfigDefault, false)
	if code := base.install("--version", "v0.13.0", "--dir", base.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("install: %d\n%s\n%s", code, base.out, base.errw)
	}
	// Каталоги и токен администратора у каждой установки свои.
	token := regexp.MustCompile(`sdlc_[0-9a-f]+`)
	normalize := func(f *installFixture, s string) string {
		return token.ReplaceAllString(strings.ReplaceAll(s, f.root, "ROOT"), "TOKEN")
	}
	cases := map[string]struct {
		answer string
		fail   bool
	}{
		"daemon down": {fail: true},
		"not json":    {answer: "template: :1:7: executing \"\" at <.RegistryConfig>: map has no entry for key \"RegistryConfig\"\n"},
		"null":        {answer: "null\n"},
		"empty":       {answer: ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, r, d := httpInstall(t, httpMirror, c.answer, c.fail)
			if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
				t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
			}
			checkMirrorCalls(t, f, r, d, httpMirror, "v0.13.0", false)
			if got, want := normalize(f, f.errw.String()), normalize(base, base.errw.String()); got != want {
				t.Errorf("stderr differs:\n%s\nwant:\n%s", got, want)
			}
			if got, want := normalize(f, f.out.String()), normalize(base, base.out.String()); got != want {
				t.Errorf("stdout differs:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// AC-6: исходы те же и с флагом — подписи нет: текст CLAB-278; подпись
// не сошлась: тревога.
func TestInstallHTTPMirrorOutcomes(t *testing.T) {
	t.Run("unsigned", func(t *testing.T) {
		f, r, _ := httpInstall(t, httpMirror, registryConfigInsecure, false)
		r.onCall = func(c cmdSpec) error {
			if c.name == "cosign" && len(c.args) > 1 && c.args[1] == "--offline" && strings.Contains(c.args[len(c.args)-1], "/sdlc-pipeline-backend@") {
				return cosignNoSignaturesFail(c)
			}
			return nil
		}
		if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 1 {
			t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
		}
		f.noEnv(f.dir("cadence"))
		want := httpMirror + "/sdlc-pipeline-backend@" + testBackendPin + " (sdlc-pipeline-backend v0.13.0) has no signature in " + httpMirror +
			", or no image with this digest is there:"
		if !strings.Contains(f.errw.String(), want) || strings.Contains(f.errw.String(), "altered or not ours") {
			t.Errorf("not the refusal of an unsigned image:\n%s", f.errw)
		}
		if calls := r.callsWith("cosign verify --offline"); len(calls) != 1 || !strings.Contains(calls[0], " --allow-http-registry ") {
			t.Errorf("cosign verify: %v", calls)
		}
	})
	t.Run("altered", func(t *testing.T) {
		f, r, _ := httpInstall(t, httpMirror, registryConfigInsecure, false)
		f.r.cosignBad = "sdlc-pipeline-backend"
		if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 1 {
			t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
		}
		f.noEnv(f.dir("cadence"))
		if !strings.Contains(f.errw.String(), "the signature of "+httpMirror+"/sdlc-pipeline-backend@"+testBackendPin+
			" (sdlc-pipeline-backend v0.13.0) did not verify for ") || !strings.Contains(f.errw.String(), "the image is altered or not ours") {
			t.Errorf("not the alarm:\n%s", f.errw)
		}
		if calls := r.callsWith("cosign verify --offline"); len(calls) != 1 || !strings.Contains(calls[0], " --allow-http-registry ") {
			t.Errorf("cosign verify: %v", calls)
		}
	})
}

// AC-6: путь с ghcr.io и «verified when loaded» — без docker info и без
// флага.
func TestInstallHTTPMirrorOtherPaths(t *testing.T) {
	// ghcr.io отвечает: образы проверяются там, зеркало не спрашивается.
	f := verifyInstallSetup(t, map[string]string{"CADENCE_REGISTRY": httpMirror})
	d := &dockerInfoRunner{runner: f.r, answer: registryConfigInsecure}
	f.e.run = d
	if code := f.install(append([]string{"--dir", f.dir("cadence")}, proxyYes...)...); code != 0 {
		t.Fatalf("online: %d\n%s", code, f.errw)
	}
	for _, c := range cosignCalls(f.r) {
		if strings.Contains(c, "--allow-http-registry") || strings.Contains(c, httpMirror) {
			t.Errorf("online: %s", c)
		}
	}
	if n := d.count(); n != 0 {
		t.Errorf("online: docker info asked %d times", n)
	}
	// Контур без своего реестра: образы в Docker этой машины.
	g, r := offlineInstall(t, map[string]string{"CADENCE_VERIFY": "require"}, true)
	e := &dockerInfoRunner{runner: r, answer: registryConfigInsecure}
	g.e.run = e
	if code := g.install("--version", "v0.13.0", "--dir", g.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("local: %d\n%s\n%s", code, g.out, g.errw)
	}
	if !strings.Contains(g.out.String(), "verified when loaded") || len(r.callsWith("cosign verify --offline")) != 0 {
		t.Errorf("local:\n%s\n%v", g.out, r.callsWith("cosign verify"))
	}
	if n := e.count(); n != 0 {
		t.Errorf("local: docker info asked %d times", n)
	}
}

// AC-3, AC-4: правило Docker isSecureIndex.
func TestDockerRegistriesInsecure(t *testing.T) {
	r := &dockerRegistries{
		InsecureRegistryCIDRs: []string{"::1/128", "127.0.0.0/8", "10.0.0.0/8", "not a cidr"},
		IndexConfigs: map[string]struct{ Secure bool }{
			"registry.corp:5000": {Secure: false},
			"secure.corp:5000":   {Secure: true},
			"plain.corp":         {Secure: false},
		},
	}
	hosts := map[string][]string{"in.corp": {"192.0.2.1", "10.2.3.4"}, "out.corp": {"192.0.2.1"}, "secure.corp": {"10.0.0.1"}}
	lookup := func(host string) ([]string, error) {
		if a, ok := hosts[host]; ok {
			return a, nil
		}
		return nil, errors.New("no such host")
	}
	cases := map[string]bool{
		"registry.corp:5000": true,
		"registry.corp:5001": false, // имя объявлено с другим портом, адрес не разрешается
		"registry.corp":      false,
		"plain.corp":         true,
		"secure.corp:5000":   false, // Secure по имени важнее сети
		"secure.corp:5001":   true,  // не объявлено — по адресу в 10.0.0.0/8
		"10.1.2.3:5000":      true,
		"10.1.2.3":           true,
		"192.168.1.1:5000":   false,
		"127.0.0.1:5000":     true,
		"in.corp:5000":       true,
		"out.corp:5000":      false,
		"unknown.corp:5000":  false,
	}
	for host, want := range cases {
		if got := r.insecure(host, lookup); got != want {
			t.Errorf("insecure(%q) = %v", host, got)
		}
	}
	if (&dockerRegistries{}).insecure("10.1.2.3:5000", lookup) {
		t.Error("no CIDRs, insecure")
	}
	if (&dockerRegistries{InsecureRegistryCIDRs: []string{"10.0.0.0/8"}}).insecure("in.corp:5000", nil) {
		t.Error("no resolver, insecure")
	}
}

// AC-6: --allow-insecure-registry (без проверки сертификата) clab не
// передаёт нигде.
func TestNoAllowInsecureRegistry(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "--allow-insecure-registry") {
			t.Errorf("%s passes --allow-insecure-registry", name)
		}
	}
}
