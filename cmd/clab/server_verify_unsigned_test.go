package main

// Образы в реестре администратора без подписи (CLAB-278): архив загружен в
// CADENCE_REGISTRY с CADENCE_VERIFY=off или скопирован без referrers.
// install и upgrade по-прежнему отказывают, но текстом про потерянную
// подпись, а не про подмену; подпись, которая есть и не сходится, — прежний
// отказ дословно. Номера — критерии приёмки CLAB-278 (AC-n).

import (
	"context"
	"io"
	"strings"
	"testing"
)

// cosignNoSignaturesOut — дословный stderr cosign v3.1.3 (pin
// ci/check-airgap.sh) на `cosign verify --offline --trusted-root …
// --certificate-identity … --certificate-oidc-issuer …` по образу без
// referrers в registry:3 — с --allow-http-registry и без; код выхода 10.
// Тот же ответ — на digest, которого в реестре нет вовсе.
const cosignNoSignaturesOut = "Flag --offline has been deprecated, To verify in an airgapped environment, provide a --bundle with the signature and verification material, and a --trusted-root file with the service keys and certificates\n" +
	"Error: no signatures found\n" +
	"error during command execution: no signatures found\n"

// cosignNoSignaturesFail — ответ cosign на образ без подписей.
func cosignNoSignaturesFail(c cmdSpec) error {
	if c.stderr != nil {
		io.WriteString(c.stderr, cosignNoSignaturesOut)
	}
	return exitErr(cosignNoSignaturesCode)
}

// mirrorCosign — cosign verify --offline в реестре администратора
// (reg.example/cadence): answers[repo] — его ответ на образ repo, если
// задан; остальное — imagesRunner (подпись сходится, если не cosignBad).
type mirrorCosign struct {
	*imagesRunner
	answers map[string]func(c cmdSpec) error
}

func (m *mirrorCosign) run(ctx context.Context, c cmdSpec) error {
	if c.name == "cosign" && len(c.args) > 1 && c.args[0] == "verify" && c.args[1] == "--offline" {
		target := c.args[len(c.args)-1]
		if answer := m.answers[refRepo(target)]; answer != nil && strings.HasPrefix(target, testMirror+"/") && ctx.Err() == nil {
			m.record(c)
			return answer(c)
		}
	}
	return m.imagesRunner.run(ctx, c)
}

const testMirror = "reg.example/cadence"

// cosignAnswer — ответ cosign: stderr и код выхода.
func cosignAnswer(stderr string, code int) func(c cmdSpec) error {
	return func(c cmdSpec) error {
		if c.stderr != nil {
			io.WriteString(c.stderr, stderr)
		}
		return exitErr(code)
	}
}

// unsignedInstall — install в контуре из реестра администратора, где у
// образов unsigned нет подписей.
func unsignedInstall(t *testing.T, mode string, unsigned ...string) (*installFixture, *mirrorCosign) {
	t.Helper()
	vars := map[string]string{"CADENCE_REGISTRY": testMirror}
	if mode != "" {
		vars["CADENCE_VERIFY"] = mode
	}
	f, r := offlineInstall(t, vars, false)
	m := &mirrorCosign{imagesRunner: r, answers: map[string]func(cmdSpec) error{}}
	for _, repo := range unsigned {
		m.answers[repo] = cosignNoSignaturesFail
	}
	f.e.run = m
	return f, m
}

// unsignedRefusal — отказ на образ repo без подписи: что в нём должно быть
// и чего быть не должно.
func unsignedRefusal(t *testing.T, stderr, repo, version, pin, nothing string) {
	t.Helper()
	for _, want := range []string{
		testMirror + "/" + repo + "@" + pin + " (" + repo + " " + version + ") has no signature in " + testMirror + ":",
		"loaded there with CADENCE_VERIFY=off", "copied without their referrers",
		"Its digest is the pin of the signed package.",
		"Load the archive again with verification (cosign 3.x, without CADENCE_VERIFY=off): clab server images load FILE --registry " + testMirror,
		"or skip the signatures on purpose: run again with CADENCE_VERIFY=off",
		`docs/admin.md, "Air-gapped installation"`, nothing,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("no %q in:\n%s", want, stderr)
		}
	}
	for _, alarm := range []string{"altered or not ours", "SECURITY.md", "did not verify for"} {
		if strings.Contains(stderr, alarm) {
			t.Errorf("%q in the refusal of an unsigned image:\n%s", alarm, stderr)
		}
	}
	if n := strings.Count(stderr, "has no signature in"); n != 1 {
		t.Errorf("%d refusals:\n%s", n, stderr)
	}
}

// AC-1, AC-2: оба образа без подписи — отказ в auto и require кодом 1,
// .env нет; текст про загрузку без проверки с двумя выходами; второй образ
// после первого не проверяется.
func TestInstallMirrorUnsigned(t *testing.T) {
	for _, mode := range []string{"", "auto", "require"} {
		t.Run("mode "+mode, func(t *testing.T) {
			f, m := unsignedInstall(t, mode, verifyImageRepos...)
			if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 1 {
				t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
			}
			f.noEnv(f.dir("cadence"))
			unsignedRefusal(t, f.errw.String(), "sdlc-pipeline-backend", "v0.13.0", testBackendPin, nothingInstalled)
			if !strings.Contains(f.out.String(), "✓ the signature of checksums.txt of v0.13.0 verified offline") {
				t.Errorf("the package was not verified first:\n%s", f.out)
			}
			if calls := m.callsWith("cosign verify --offline"); len(calls) != 1 || !strings.HasSuffix(calls[0], " "+testMirror+"/sdlc-pipeline-backend@"+testBackendPin) {
				t.Errorf("cosign verify: %v", calls)
			}
			if m.calledContaining("docker compose") && m.calledContaining(" up ") {
				t.Errorf("started: %v", m.calls)
			}
		})
	}
}

// AC-4: без подписи только frontend — backend проходит с ✓, отказ
// называет frontend.
func TestInstallMirrorOneUnsigned(t *testing.T) {
	f, _ := unsignedInstall(t, "require", "sdlc-pipeline-frontend")
	if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 1 {
		t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
	}
	f.noEnv(f.dir("cadence"))
	want := "✓ " + testMirror + "/sdlc-pipeline-backend@" + testBackendPin + " is signed by " + verifyIdentity("sdlc-pipeline-backend", "v0.13.0") +
		" (verified offline with trusted_root.json of the mirror of the releases)"
	if !strings.Contains(f.out.String(), want) {
		t.Errorf("no %q in:\n%s", want, f.out)
	}
	unsignedRefusal(t, f.errw.String(), "sdlc-pipeline-frontend", "v0.13.0", testFrontendPin, nothingInstalled)
}

// AC-5: подпись не сошлась или cosign ответил не «нет подписей» — прежний
// отказ дословно (на него опираются ci/ пакета). Нужны и код 10, и строка.
func TestInstallMirrorNotUnsigned(t *testing.T) {
	identity := verifyIdentity("sdlc-pipeline-backend", "v0.13.0")
	cases := map[string]func(cmdSpec) error{
		// cosign v3.1.3 на бандл другой identity, registry:3 (CLAB-278).
		"foreign bundle": cosignAnswer("Error: no matching attestations: failed to verify certificate identity: no matching CertificateIdentity found, last error: expected SAN value \""+
			identity+"\", got \""+verifyIdentity("sdlc-pipeline-backend", "v0.12.0")+"\"\n", 1),
		"unauthorized": cosignAnswer("Error: GET https://reg.example/v2/cadence/sdlc-pipeline-backend/manifests/"+testBackendPin+
			": UNAUTHORIZED: authentication required; [map[Action:pull Class: Name:cadence/sdlc-pipeline-backend Type:repository]]\n", 1),
		// cosign v3.1.3 на реестр, который не отвечает.
		"connection refused": cosignAnswer("Error: Get \"https://reg.example/v2/\": dial tcp 10.0.0.5:443: connect: connection refused; "+
			"Get \"http://reg.example/v2/\": dial tcp 10.0.0.5:80: connect: connection refused\n", 1),
		"the line, another code": cosignAnswer("Error: no signatures found\n", 1),
		"code 10, another line":  cosignAnswer("Error: no signatures found in the cache\n", cosignNoSignaturesCode),
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			f, m := unsignedInstall(t, "auto")
			m.answers["sdlc-pipeline-backend"] = answer
			if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 1 {
				t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
			}
			f.noEnv(f.dir("cadence"))
			want := "the signature of " + testMirror + "/sdlc-pipeline-backend@" + testBackendPin + " (sdlc-pipeline-backend v0.13.0) did not verify for " + identity +
				": the image is altered or not ours.\n  Do not run it and report it privately (SECURITY.md). " + nothingInstalled
			if !strings.Contains(f.errw.String(), want) || strings.Contains(f.errw.String(), "has no signature in") {
				t.Errorf("not the refusal of an altered image:\n%s", f.errw)
			}
		})
	}
}

// AC-3: upgrade из реестра без подписей — тот же текст с «Nothing has
// changed.», установка на прежней версии, pull и up не звались.
func TestUpgradeMirrorUnsigned(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		t.Run(mode, func(t *testing.T) {
			f := verifyUpgradeSetup(t, "CADENCE_REGISTRY="+testMirror+"\n", map[string]string{"CADENCE_VERIFY": mode})
			publishAirgap(t, f.releases, "v0.14.0", true)
			r := newImagesRunner()
			r.fakeRunner = f.r
			r.offline = true
			f.r.ghcrDown = true
			m := &mirrorCosign{imagesRunner: r, answers: map[string]func(cmdSpec) error{"sdlc-pipeline-backend": cosignNoSignaturesFail}}
			f.e.run = m
			f.e.cli = "v0.14.0"
			before := tree(t, f.dir)
			if code := f.upgrade("v0.14.0"); code != 1 {
				t.Fatalf("upgrade: %d\n%s\n%s", code, f.out, f.errw)
			}
			unsignedRefusal(t, f.errw.String(), "sdlc-pipeline-backend", "v0.14.0", testBackendPin, nothingChanged)
			f.unchanged(t, before)
			if f.version(t) != "v0.12.0" || f.r.called("docker compose up") || m.calledContaining("compose pull") {
				t.Errorf("VERSION %s, calls %v", f.version(t), f.r.calls)
			}
		})
	}
}

// AC-7: после load с проверкой бандлы в реестре — повторный install
// проходит, оба образа ✓ verified offline; подписанный реестр — тот же
// вывод, что до CLAB-278.
func TestInstallMirrorSignedAgain(t *testing.T) {
	f, m := unsignedInstall(t, "require", verifyImageRepos...)
	if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 1 {
		t.Fatalf("first install: %d\n%s\n%s", code, f.out, f.errw)
	}
	f.noEnv(f.dir("cadence"))
	m.answers = map[string]func(cmdSpec) error{} // clab server images load без CADENCE_VERIFY=off
	f.out.Reset()
	f.errw.Reset()
	if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("install again: %d\n%s\n%s", code, f.out, f.errw)
	}
	for _, img := range []struct{ repo, pin string }{{"sdlc-pipeline-backend", testBackendPin}, {"sdlc-pipeline-frontend", testFrontendPin}} {
		want := "✓ " + testMirror + "/" + img.repo + "@" + img.pin + " is signed by " + verifyIdentity(img.repo, "v0.13.0") +
			" (verified offline with trusted_root.json of the mirror of the releases)"
		if !strings.Contains(f.out.String(), want) {
			t.Errorf("no %q in:\n%s", want, f.out)
		}
	}
	if strings.Contains(f.errw.String(), "has no signature") {
		t.Errorf("refused:\n%s", f.errw)
	}
}

// AC-8: CADENCE_VERIFY=off против того же реестра — cosign verify не
// зовётся, одна строка о выключенной проверке, установка идёт.
func TestInstallMirrorUnsignedOff(t *testing.T) {
	f, m := unsignedInstall(t, "off", verifyImageRepos...)
	if code := f.install("--version", "v0.13.0", "--dir", f.dir("cadence"), "--mode", "proxy", "--yes"); code != 0 {
		t.Fatalf("install: %d\n%s\n%s", code, f.out, f.errw)
	}
	if calls := m.callsWith("cosign verify"); len(calls) != 0 {
		t.Errorf("cosign verify with CADENCE_VERIFY=off: %v", calls)
	}
	all := f.out.String() + f.errw.String()
	if n := strings.Count(all, "▸ signature verification is off (CADENCE_VERIFY=off)"); n != 1 {
		t.Errorf("%d notes:\n%s", n, all)
	}
	if f.env(f.dir("cadence"))["CADENCE_REGISTRY"] != testMirror {
		t.Error("CADENCE_REGISTRY is not in .env")
	}
}

// R4: распознавание — только код 10 вместе со строкой cosign.
func TestCosignUnsigned(t *testing.T) {
	cases := []struct {
		err  error
		out  string
		want bool
	}{
		{exitErr(10), cosignNoSignaturesOut, true},
		{exitErr(10), "Error: no signatures found\n", true},
		{exitErr(1), cosignNoSignaturesOut, false},
		{exitErr(12), cosignNoSignaturesOut, false},
		{exitErr(10), "Error: no matching signatures: none of the expected identities matched\n", false},
		{exitErr(10), "", false},
		{context.Canceled, cosignNoSignaturesOut, false},
		{nil, cosignNoSignaturesOut, false},
	}
	for i, c := range cases {
		if got := cosignUnsigned(c.err, c.out); got != c.want {
			t.Errorf("%d: cosignUnsigned(%v, %q) = %v", i, c.err, c.out, got)
		}
	}
}
