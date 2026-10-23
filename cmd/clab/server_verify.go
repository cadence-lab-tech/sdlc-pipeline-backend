package main

// Проверка подписей релиза у клиента (CLAB-197, решения 0046 и 0054):
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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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

	// Закрытый контур (CLAB-202, решение 0059): trusted_root.json Sigstore
	// из зеркала релизов той же версии — root, когда он взят; releases и
	// work — откуда и куда его брать для образов, если подпись пакета его
	// не взяла (установка на месте); offline — ghcr.io не отвечает, образы
	// проверяются без него.
	root           string
	releases, work string
	offline        bool

	// Реестры, куда Docker этой машины ходит по http (CLAB-280): docker
	// info читается один раз за запуск, и только если есть что проверять в
	// зеркале CADENCE_REGISTRY; nil после чтения — Docker не ответил.
	registries     *dockerRegistries
	registriesRead bool
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
		if err := e.run.run(ctx, cmdSpec{name: "cosign", args: []string{"version"}, stdout: &out, stderr: &out}); err != nil && interrupted(ctx, e) {
			return nil, fmt.Errorf("interrupted. %s", nothing)
		}
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
// tail -n 5 у скриптов. Прерванной команде сказать нечего: её ответ на
// Ctrl-C — не причина отказа (O4 тестирования CLAB-202).
func (v *verifier) tail(ctx context.Context, s string) {
	if interrupted(ctx, v.e) {
		return
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	if text := strings.Join(lines, "\n"); strings.TrimSpace(text) != "" {
		fmt.Fprintln(v.e.errw, text)
	}
}

// interruptGrace — сколько ждать отмены контекста после отказа внешней
// команды. Ctrl-C терминала приходит всей группе процессов переднего
// плана: дочерняя команда умирает от него и отвечает ошибкой чуть раньше,
// чем signal.NotifyContext отменит ctx.
const interruptGrace = 300 * time.Millisecond

// interrupted — команду прервали (SIGINT, SIGTERM). Зовётся на пути отказа:
// прерванная команда говорит «interrupted», а не причину, которую ей
// приписал бы оборванный вызов (O4 тестирования CLAB-202).
func interrupted(ctx context.Context, e *serverEnv) bool {
	if ctx.Err() != nil {
		return true
	}
	e.sleep(interruptGrace)
	return ctx.Err() != nil
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
	out, err := v.cosignBlob(ctx, dir, "")
	if err != nil {
		// Sigstore TUF не отвечает — закрытый контур: корень из зеркала
		// релизов той же версии (архив clab server images save). Нет его —
		// отказ, как раньше.
		if root := v.mirrorRoot(ctx, releases, version, dir); root != "" {
			if out, err = v.cosignBlob(ctx, dir, root); err == nil {
				v.e.ok("the signature of checksums.txt of %s verified offline with %s of the mirror: signed by %s", version, trustedRootAsset, verifyPackageIdentity)
				return nil
			}
		}
	}
	if err != nil {
		v.tail(ctx, out)
		return fmt.Errorf("the signature of checksums.txt of %s did not verify for %s: the release is altered or not ours.\n"+
			"  Do not use it and report it privately (SECURITY.md). %s", version, verifyPackageIdentity, v.nothing)
	}
	v.e.ok("the signature of checksums.txt of %s verified: signed by %s", version, verifyPackageIdentity)
	return nil
}

// cosignBlob — cosign verify-blob checksums.txt каталога dir бандлом рядом;
// root — trusted_root.json для проверки без сети.
func (v *verifier) cosignBlob(ctx context.Context, dir, root string) (string, error) {
	args := []string{"verify-blob", "--bundle", filepath.Join(dir, verifyBundle), "--certificate-identity", verifyPackageIdentity,
		"--certificate-oidc-issuer", verifyIssuer}
	if root != "" {
		args = append(args, "--trusted-root", root)
	}
	return v.runCosign(ctx, append(args, filepath.Join(dir, "checksums.txt"))...)
}

// mirrorRoot — trusted_root.json версии из зеркала релизов в dir, если
// Sigstore TUF с этой машины не отвечает; иначе (или его нет) — пусто.
func (v *verifier) mirrorRoot(ctx context.Context, releases, version, dir string) string {
	if releases == "" || v.e.reachable(ctx, verifyTUFProbe) {
		return ""
	}
	dst := filepath.Join(dir, trustedRootAsset)
	if err := v.e.fetch(ctx, releases+"/download/"+version+"/"+trustedRootAsset, dst); err != nil {
		os.Remove(dst)
		return ""
	}
	v.root = dst
	return dst
}

// verifyChecksumsOffline — подпись checksums.txt каталога dir без сети, по
// корню root (clab server images load).
func (v *verifier) verifyChecksumsOffline(ctx context.Context, version, dir, root string) error {
	if out, err := v.cosignBlob(ctx, dir, root); err != nil {
		v.tail(ctx, out)
		return fmt.Errorf("the signature of checksums.txt of %s in the archive did not verify for %s: the archive is altered or not ours.\n"+
			"  Do not use it and report it privately (SECURITY.md). %s", version, verifyPackageIdentity, v.nothing)
	}
	v.e.ok("the signature of checksums.txt of %s verified offline with %s of the archive: signed by %s", version, trustedRootAsset, verifyPackageIdentity)
	return nil
}

// verifyImageOffline — подпись образа repo по ссылке с digest target без
// сети, по корню root; where — откуда корень, для вывода; allowHTTP —
// cosign можно к реестру по http (--allow-http-registry, CLAB-280).
func (v *verifier) verifyImageOffline(ctx context.Context, version, repo, target, root, where string, allowHTTP bool) error {
	identity := verifyIdentity(repo, version)
	args := []string{"verify", "--offline", "--trusted-root", root}
	if allowHTTP {
		args = append(args, "--allow-http-registry")
	}
	out, err := v.runCosign(ctx, append(args, "--certificate-identity", identity, "--certificate-oidc-issuer", verifyIssuer, target)...)
	if err != nil {
		v.tail(ctx, out)
		if cosignUnsigned(err, out) {
			// cosign отвечает так же, когда образа с этим digest в реестре
			// нет вовсе: отказ называет оба случая (тестирование
			// CLAB-278), выходы у них те же — load заново кладёт и образ.
			registry := target[:strings.LastIndex(target, "/"+repo+"@")]
			return fmt.Errorf("%s (%s %s) has no signature in %s, or no image with this digest is there: this happens when the archive is loaded there "+
				"with CADENCE_VERIFY=off, the images are copied without their referrers, or this version is not loaded there at all. "+
				"Its digest is the pin of the signed package.\n"+
				"  Load the archive again with verification (cosign 3.x, without CADENCE_VERIFY=off): clab server images load FILE --registry %s,\n"+
				"  or skip the signatures on purpose: run again with CADENCE_VERIFY=off (docs/admin.md, \"Air-gapped installation\"). %s",
				target, repo, version, registry, registry, v.nothing)
		}
		return fmt.Errorf("the signature of %s (%s %s) did not verify for %s: the image is altered or not ours.\n"+
			"  Do not run it and report it privately (SECURITY.md). %s", target, repo, version, identity, v.nothing)
	}
	v.e.ok("%s is signed by %s (verified offline with %s of %s)", target, identity, trustedRootAsset, where)
	return nil
}

// cosignNoSignatures — ответ cosign 3.x на verify образа, у которого в
// реестре нет ни одной подписи: код выхода 10 и строка ошибки, как у
// v3.1.3 на registry:3 с образом без referrers (CLAB-278). Тот же ответ —
// на digest, которого в реестре нет вовсе; выходы для него те же (load
// заново кладёт и образ). Чужой бандл отвечает иначе — «no matching …»,
// код 1.
const (
	cosignNoSignaturesCode = 10
	cosignNoSignatures     = "Error: no signatures found"
)

// cosignUnsigned — cosign отказал потому, что подписей нет вовсе, а не
// потому, что они не сошлись. Нужны и код, и строка: всё прочее — реестр не
// ответил, не пустил, незнакомый ответ — остаётся отказом «altered or not
// ours» (R4 CLAB-278). Исход от этого не меняется, только текст: образ по
// pin подписанного пакета — содержимое релиза, и потеряна у него лишь
// подпись.
func cosignUnsigned(err error, out string) bool {
	if exitCode(err) != cosignNoSignaturesCode {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == cosignNoSignatures {
			return true
		}
	}
	return false
}

// imagesRoot — корень для проверки образов без ghcr.io: взятый подписью
// пакета, иначе — из зеркала релизов версии (установка на месте).
func (v *verifier) imagesRoot(ctx context.Context, version string) string {
	if v.root != "" || v.releases == "" || v.work == "" {
		return v.root
	}
	dst := filepath.Join(v.work, trustedRootAsset)
	if err := v.e.fetch(ctx, v.releases+"/download/"+version+"/"+trustedRootAsset, dst); err != nil {
		os.Remove(dst)
		return ""
	}
	v.root = dst
	return dst
}

// verifyClosed — образ ref (с pin; «@» на at) в закрытом контуре (q10
// CLAB-202). В зеркале реестра (CADENCE_REGISTRY) — cosign verify --offline
// там по digest. Под именем ghcr.io — образ в Docker этой машины ровно по
// ссылке compose с pin: его подпись проверил clab server images load, а
// digest равен pin пакета, подпись checksums.txt которого проверена, —
// значит, это тот же образ. Нет его здесь — решение по режиму, как с
// недоступным ghcr.io без корня; stop — дальше не проверять.
func (v *verifier) verifyClosed(ctx context.Context, version, repo, ref string, at int) (stop bool, err error) {
	registry := ref[:strings.LastIndex(ref[:at], "/"+repo)]
	if registry != verifyRegistry {
		return false, v.verifyImageOffline(ctx, version, repo, registry+"/"+repo+"@"+ref[at+1:], v.root, "the mirror of the releases",
			v.httpRegistry(ctx, registry))
	}
	if dockerImagePresent(ctx, v.e, ref) {
		v.e.ok("%s verified when loaded: same digest as the pin of the signed package", ref)
		return false, nil
	}
	if v.mode != "auto" {
		return true, fmt.Errorf("the images cannot be verified: ghcr.io is unreachable from this host, and %s is not on this machine (load the archive of the version first: clab server images load). %s", ref, v.nothing)
	}
	v.e.warn("images not verified: ghcr.io is unreachable from this host, and %s is not on this machine (load the archive of the version first: clab server images load)", ref)
	return true, nil
}

// dockerRegistries — RegistryConfig из docker info: что демон собрал из
// insecure-registries своего daemon.json — имена host[:port] с Secure=false
// и сети CIDR.
type dockerRegistries struct {
	InsecureRegistryCIDRs []string
	IndexConfigs          map[string]struct{ Secure bool }
}

// httpRegistry — Docker этой машины ходит к реестру registry
// (host[:port]/путь, CADENCE_REGISTRY) по http, значит, и cosign можно
// туда по http (CLAB-280). Сам cosign берёт http только у loopback и
// частных IP-литералов; зеркалу по http под именем хоста без флага он
// отвечает ложной тревогой «altered or not ours».
//
// Флаг — только по слову Docker, а не всегда: с ним cosign на отказ https
// пробует http, и на https-реестре атакующий в сети понизит соединение и
// получит учётные данные docker login. Подпись так не подделать (проверка
// офлайн по корню, identity и pin), но кредов жаль, а http администратор
// уже разрешил там, где разрешил его Docker'у. Флаг «небезопасного»
// реестра cosign не годится: он ещё и не проверяет сертификат. Docker не
// ответил или ответил непонятно — флага нет, как до CLAB-280, без отказа и
// предупреждения.
func (v *verifier) httpRegistry(ctx context.Context, registry string) bool {
	if !v.registriesRead {
		v.registriesRead = true
		v.registries = readDockerRegistries(ctx, v.e)
	}
	if v.registries == nil {
		return false
	}
	host, _, _ := strings.Cut(registry, "/")
	return v.registries.insecure(host, v.e.lookupHost)
}

// readDockerRegistries — docker info --format '{{json .RegistryConfig}}';
// nil — ошибка или не JSON.
func readDockerRegistries(ctx context.Context, e *serverEnv) *dockerRegistries {
	out, err := output(ctx, e.run, cmdSpec{name: "docker", args: []string{"info", "--format", "{{json .RegistryConfig}}"}})
	if err != nil {
		return nil
	}
	var r *dockerRegistries
	if json.Unmarshal([]byte(out), &r) != nil {
		return nil
	}
	return r
}

// insecure — правило Docker isSecureIndex наоборот: реестр host[:port],
// объявленный по имени, решает его Secure; иначе — адрес хоста (IP-литерал
// или то, во что разрешается имя) в одной из сетей InsecureRegistryCIDRs.
// Имя не разрешилось — не в сети, как у Docker.
func (r *dockerRegistries) insecure(host string, lookup func(string) ([]string, error)) bool {
	if index, ok := r.IndexConfigs[host]; ok {
		return !index.Secure
	}
	if len(r.InsecureRegistryCIDRs) == 0 {
		return false
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	var addrs []net.IP
	if ip := net.ParseIP(name); ip != nil {
		addrs = []net.IP{ip}
	} else if lookup != nil {
		resolved, err := lookup(name)
		if err != nil {
			return false
		}
		for _, a := range resolved {
			if ip := net.ParseIP(a); ip != nil {
				addrs = append(addrs, ip)
			}
		}
	}
	for _, cidr := range r.InsecureRegistryCIDRs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		for _, ip := range addrs {
			if n.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// composeImages — образы итоговой конфигурации compose: stdout c (docker
// compose … config --images) построчно. Ошибка compose — его stderr к
// человеку.
func composeImages(ctx context.Context, e *serverEnv, c cmdSpec) ([]string, error) {
	var out, errb bytes.Buffer
	c.stdout, c.stderr = &out, &errb
	if err := e.run.run(ctx, c); err != nil {
		(&verifier{e: e}).tail(ctx, errb.String())
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
			if v.offline {
				if stop, err := v.verifyClosed(ctx, version, repo, ref, at); stop || err != nil {
					return err
				}
				continue
			}
			out, err := v.runCosign(ctx, "verify", "--certificate-identity", identity, "--certificate-oidc-issuer", verifyIssuer, target)
			if err == nil {
				v.e.ok("%s is signed by %s", target, identity)
				continue
			}
			if !v.e.reachable(ctx, verifyRegistryProbe) {
				// Закрытый контур с корнем Sigstore в зеркале релизов
				// (CLAB-202): дальше без ghcr.io.
				if v.imagesRoot(ctx, version) != "" {
					v.offline = true
					if stop, err := v.verifyClosed(ctx, version, repo, ref, at); stop || err != nil {
						return err
					}
					continue
				}
				if v.mode != "auto" {
					return fmt.Errorf("the images cannot be verified: ghcr.io is unreachable from this host, and CADENCE_VERIFY=require needs it (the signatures are checked there by digest). %s", v.nothing)
				}
				v.e.warn("images not verified: ghcr.io is unreachable from this host (a registry mirror in a closed network?); verify them where it is reachable: docs/admin.md, \"Verifying a release\"")
				return nil
			}
			v.tail(ctx, out)
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
