package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// cmdSpec — запуск внешней команды: docker, docker compose, git.
type cmdSpec struct {
	dir    string
	env    []string // nil — окружение процесса
	name   string
	args   []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	// detach — команда, после прерывания которой у демона остаётся
	// состояние, которое надо убрать (docker run временного реестра):
	// она идёт в своей группе процессов, и Ctrl-C терминала до неё не
	// доходит, а отмена ctx её не обрывает — запрос доходит до конца, и
	// уборка видит то, что он создал (CLAB-202, AC-8). Предел — detachTimeout.
	detach bool
}

// detachTimeout — предел команды с detach: она не обрывается сигналом, но и
// висеть бесконечно ей нельзя.
const detachTimeout = 2 * time.Minute

func (c cmdSpec) String() string { return strings.Join(append([]string{c.name}, c.args...), " ") }

// runner исполняет внешние команды. Настоящий — os/exec; в тестах —
// подставной, который отвечает за docker и git без Docker в верстаке.
type runner interface {
	run(ctx context.Context, c cmdSpec) error
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, c cmdSpec) error {
	if c.detach {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), detachTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.name, c.args...)
	if c.detach {
		ownProcessGroup(cmd)
	}
	cmd.Dir = c.dir
	cmd.Env = c.env
	cmd.Stdin = c.stdin
	cmd.Stdout = c.stdout
	cmd.Stderr = c.stderr
	return cmd.Run()
}

// lookPath — есть ли команда в PATH; в тестах подменяется.
var lookPath = exec.LookPath

// output — stdout команды строкой; stderr уходит к человеку, если задан.
func output(ctx context.Context, r runner, c cmdSpec) (string, error) {
	var out bytes.Buffer
	c.stdout = &out
	if c.stderr == nil {
		c.stderr = io.Discard
	}
	err := r.run(ctx, c)
	return out.String(), err
}

// requireDocker — lib.sh require_docker: Docker с плагином compose v2 и
// отвечающий демон.
func requireDocker(ctx context.Context, e *serverEnv) error {
	if _, err := lookPath("docker"); err != nil {
		return errors.New("Docker is not installed. Install Docker Engine with the compose plugin: https://docs.docker.com/engine/install/")
	}
	if _, err := output(ctx, e.run, cmdSpec{name: "docker", args: []string{"compose", "version"}}); err != nil {
		return errors.New("Docker Compose v2 is missing ('docker compose version' failed). Install the docker-compose-plugin package: https://docs.docker.com/compose/install/linux/")
	}
	if _, err := output(ctx, e.run, cmdSpec{name: "docker", args: []string{"info"}}); err != nil {
		return errors.New("The Docker daemon does not answer. Start it (systemctl start docker) or run as a user in the docker group.")
	}
	return nil
}

// docker — команда docker в каталоге установки с её окружением.
func (i *installation) docker(args ...string) cmdSpec {
	return cmdSpec{dir: i.dir, env: i.composeEnv(), name: "docker", args: args}
}

// compose — docker compose в каталоге установки: COMPOSE_FILE и прочее
// приходят из .env через окружение, версия образов — из VERSION.
func (i *installation) compose(args ...string) cmdSpec {
	return i.docker(append([]string{"compose"}, args...)...)
}

// composeRun — docker compose с выводом к человеку; stdout можно заглушить,
// как >/dev/null у скриптов.
func (i *installation) composeRun(ctx context.Context, quiet bool, args ...string) error {
	c := i.compose(args...)
	c.stdout, c.stderr = i.e.out, i.e.errw
	if quiet {
		c.stdout = io.Discard
	}
	return i.e.run.run(ctx, c)
}

// containerOf — lib.sh container_of: ID контейнера сервиса, запущенного или
// остановленного.
func (i *installation) containerOf(ctx context.Context, service string) string {
	out, err := output(ctx, i.e.run, i.compose("ps", "-a", "-q", service))
	if err != nil {
		return ""
	}
	return firstLine(out)
}

// running — lib.sh running: у сервиса есть запущенный контейнер.
func (i *installation) running(ctx context.Context, service string) bool {
	out, err := output(ctx, i.e.run, i.compose("ps", "-q", "--status", "running", service))
	return err == nil && strings.TrimSpace(out) != ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if j := strings.IndexByte(s, '\n'); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

// uiProbes — адреса, по которым waitReady ждёт интерфейс, по порядку.
// Порт ui зависит от версии образа, а не от версии clab: до v0.14.0 образ
// слушает только :80 (root), с v0.14.0 — :80 и :8080 (CLAB-236), дальше —
// только :8080 (CLAB-248). Готовность при upgrade ждёт clab исходной версии,
// поэтому проверка терпима к обоим портам: clab этой версии дождётся стека
// любой из них, а образ без :80 выходит через релиз после неё (CLAB-247).
var uiProbes = []string{"http://ui:8080/", "http://ui:80/"}

// Маршруты Caddy (CLAB-267). Прямая проба ui не видит маршрута Caddyfile:
// свой маршрут администратора на ui:80 после образа только на :8080 отдаёт
// 502, а прямая проба довольна. Поэтому после прямых проб waitReady
// спрашивает admin API Caddy об апстримах загруженного конфига и пробует
// каждый из контейнера caddy, а в режиме proxy ещё и / маршрутом —
// http://localhost/ внутри контейнера. Всё — plain HTTP: в режиме tls ни
// HTTPS, ни SDLC_DOMAIN не спрашиваются, сертификат на готовность не влияет,
// а busybox wget не запускает ssl_client (docs/admin.md, «Third-party
// images»).
const (
	caddyUpstreamsURL = "http://127.0.0.1:2019/reverse_proxy/upstreams"
	caddyRootURL      = "http://localhost/"
	// routeProbeTimeout — -T busybox wget для проб маршрута: апстрим, который
	// не отвечает и не отказывает, не должен держать шаг 900 с по умолчанию.
	routeProbeTimeout = "5"
	// routeFailSteps — сколько шагов подряд маршрут может не проходить при
	// готовых api и ui, прежде чем ожидание кончится: конфиг Caddy после
	// reload сам не исправится, ждать CADENCE_READY_TIMEOUT незачем.
	routeFailSteps = 3
)

// errNotReady — стек не дождался готовности за таймаут.
var errNotReady = errors.New("not ready")

// routeError — api и ui готовы, а маршрут Caddy не доходит до апстрима:
// какие апстримы загруженного конфига не отвечают и чем ответил / в режиме
// proxy (502 или 504; 0 — не провал).
type routeError struct {
	down []string
	root int
}

func (r *routeError) Error() string {
	var what []string
	if len(r.down) > 0 {
		what = append(what, fmt.Sprintf("the upstream %s of the loaded Caddy configuration does not answer from the caddy container", strings.Join(r.down, ", ")))
	}
	if r.root != 0 {
		what = append(what, fmt.Sprintf("the route / through Caddy (%s in the caddy container) answers %d", caddyRootURL, r.root))
	}
	return strings.Join(what, ", and ")
}

// explain — отказ готовности по маршруту для человека: что сломано, что это
// значит для сайта, как лечить и чем проверить.
func (r *routeError) explain(i *installation) string {
	status := 502
	if r.root != 0 {
		status = r.root
	}
	return fmt.Sprintf("%s.\n"+
		"  The api and ui services are up (ui answers directly), but the site answers %d everywhere except /v1, /mcp, /webhooks, /healthz and /readyz.\n"+
		"  The UI image listens on port 8080 only from v0.16.0: point the route of your Caddyfile at ui:8080, then reload Caddy:\n"+
		"%s  docker compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile\n"+
		"  Check the site: curl -sS -o /dev/null -w '%%{http_code}\\n' %s\n"+
		"  See docs/admin.md, \"The UI on port 8080\".",
		r.Error(), status, i.cdHint(), i.siteURL())
}

// siteURL — адрес сайта для проверки curl: SDLC_DOMAIN в режиме tls, адрес
// публикации в режиме proxy.
func (i *installation) siteURL() string {
	if i.getDefault("CADENCE_MODE", "tls") == "proxy" {
		return "http://" + i.getDefault("CADENCE_HTTP_ADDR", "127.0.0.1:8080") + "/ (and the public address of your proxy)"
	}
	return "https://" + i.get("SDLC_DOMAIN") + "/"
}

// waitReady — lib.sh wait_ready: API здоров по своей проверке (/readyz:
// сервис поднят и база отвечает), интерфейс отвечает из контейнера caddy на
// одном из uiProbes, и маршруты Caddy доходят до своих апстримов
// (routesAnswer). Ожидание считается шагами по 3 секунды, как в скрипте:
// нулевой таймаут — «не готов» без единой проверки, на этом стоит сценарий
// отката в ci/. nil — готов; *routeError — маршрут не прошёл routeFailSteps
// шагов подряд при готовых api и ui (или до таймаута); errNotReady — иначе.
func (i *installation) waitReady(ctx context.Context, timeout int) error {
	var routeErr *routeError
	fails := 0
	warned := false
	for waited := 0; waited < timeout; waited += 3 {
		if ctx.Err() != nil {
			return errNotReady
		}
		if i.apiHealthy(ctx) && i.uiAnswers(ctx) {
			err, checked := i.routesAnswer(ctx)
			if !checked && !warned {
				warned = true
				also := ""
				if i.getDefault("CADENCE_MODE", "tls") == "proxy" {
					also = " and / through Caddy"
				}
				i.e.warn("the routes of Caddy are not checked: its admin API (%s in the caddy container) does not answer, as with 'admin off' in your own Caddyfile; readiness is the direct probes only%s", caddyUpstreamsURL, also)
			}
			if err == nil {
				return nil
			}
			routeErr = err
			if fails++; fails >= routeFailSteps {
				return routeErr
			}
		} else {
			fails = 0
		}
		i.e.sleep(3 * time.Second)
	}
	if fails > 0 {
		return routeErr
	}
	return errNotReady
}

// apiHealthy — healthcheck контейнера api — healthy.
func (i *installation) apiHealthy(ctx context.Context) bool {
	api := i.containerOf(ctx, "api")
	if api == "" {
		return false
	}
	health, _ := output(ctx, i.e.run, i.docker("inspect", "-f", "{{.State.Health.Status}}", api))
	return strings.TrimSpace(health) == "healthy"
}

// uiAnswers — интерфейс отвечает на одном из uiProbes: wget из контейнера
// caddy, как в lib.sh, прямо в ui, мимо маршрута — его проверяет
// routesAnswer.
func (i *installation) uiAnswers(ctx context.Context) bool {
	for _, url := range uiProbes {
		if _, err := output(ctx, i.e.run, i.compose("exec", "-T", "caddy", "wget", "-q", "-O", "/dev/null", url)); err == nil {
			return true
		}
	}
	return false
}

// caddyWget — busybox wget в контейнере caddy с пределом routeProbeTimeout:
// тело ответа (dst "-") и сообщение об ошибке.
func (i *installation) caddyWget(ctx context.Context, dst, url string) (string, string, error) {
	var stderr bytes.Buffer
	c := i.compose("exec", "-T", "caddy", "wget", "-q", "-T", routeProbeTimeout, "-O", dst, url)
	c.stderr = &stderr
	out, err := output(ctx, i.e.run, c)
	return out, stderr.String(), err
}

// httpError — код ответа из «server returned error: HTTP/1.1 502 Bad
// Gateway» busybox wget; 0 — ответа HTTP не было (отказ соединения, имя не
// резолвится, таймаут).
var httpError = regexp.MustCompile(`server returned error: HTTP/[0-9.]+ ([0-9]{3})`)

func httpStatus(stderr string) int {
	m := httpError.FindStringSubmatch(stderr)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// routesAnswer — маршруты Caddy доходят до апстримов. checked — admin API
// ответил списком апстримов; не ответил — маршруты не проверены, и это
// предупреждение, а не отказ (свой Caddyfile с admin off). Апстрим отвечает,
// если на http://<апстрим>/ пришёл любой ответ HTTP, 4xx тоже. В режиме
// proxy / маршрутом проверяется всегда, провал — только 502 и 504: 401
// своего basicauth и 404 — ответ маршрута.
func (i *installation) routesAnswer(ctx context.Context) (*routeError, bool) {
	var r routeError
	upstreams, checked := i.caddyUpstreams(ctx)
	for _, addr := range upstreams {
		_, stderr, err := i.caddyWget(ctx, "/dev/null", "http://"+addr+"/")
		if err != nil && httpStatus(stderr) == 0 {
			r.down = append(r.down, addr)
		}
	}
	if i.getDefault("CADENCE_MODE", "tls") == "proxy" {
		if _, stderr, err := i.caddyWget(ctx, "/dev/null", caddyRootURL); err != nil {
			if s := httpStatus(stderr); s == 502 || s == 504 {
				r.root = s
			}
		}
	}
	if len(r.down) == 0 && r.root == 0 {
		return nil, checked
	}
	return &r, checked
}

// caddyUpstreams — адреса апстримов загруженного конфига из admin API, без
// повторов; то, что не host:port (сокеты unix, плейсхолдеры), не
// пробуется. false — admin API не ответил списком.
func (i *installation) caddyUpstreams(ctx context.Context) ([]string, bool) {
	out, _, err := i.caddyWget(ctx, "-", caddyUpstreamsURL)
	if err != nil {
		return nil, false
	}
	var list []struct {
		Address string `json:"address"`
	}
	if json.Unmarshal([]byte(out), &list) != nil {
		return nil, false
	}
	seen := map[string]bool{}
	var addrs []string
	for _, u := range list {
		a := u.Address
		if seen[a] || strings.ContainsAny(a, "/{} ") {
			continue
		}
		if host, port, err := net.SplitHostPort(a); err != nil || host == "" || port == "" {
			continue
		}
		seen[a] = true
		addrs = append(addrs, a)
	}
	return addrs, true
}

// showFailure — что скрипты печатают, когда стек не поднялся: состояние
// сервисов и хвост логов.
func (i *installation) showFailure(ctx context.Context, services ...string) {
	_ = i.composeRun(ctx, false, "ps")
	_ = i.composeRun(ctx, false, append([]string{"logs", "--tail", "40"}, services...)...)
}

// readyTimeout — CADENCE_READY_TIMEOUT в секундах, по умолчанию 300.
func (i *installation) readyTimeout() (int, error) {
	raw := i.getDefault("CADENCE_READY_TIMEOUT", "300")
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n < 0 || fmt.Sprint(n) != strings.TrimSpace(raw) {
		return 0, fmt.Errorf("CADENCE_READY_TIMEOUT is '%s', expected a number of seconds", raw)
	}
	return n, nil
}
