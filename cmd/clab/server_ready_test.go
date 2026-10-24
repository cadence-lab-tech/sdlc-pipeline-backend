package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CLAB-247, критерий AC-1: waitReady считает интерфейс готовым, если он
// отвечает на :8080 или на :80, — clab дожидается стека на образах любой
// версии: до v0.14.0 (только :80), v0.14.0 (оба) и после CLAB-248 (только
// :8080). Функция одна на install, upgrade и restore, поэтому каждый случай
// проходит через все три команды.

const (
	probe8080 = "docker compose exec -T caddy wget -q -O /dev/null http://ui:8080/"
	probe80   = "docker compose exec -T caddy wget -q -O /dev/null http://ui:80/"
)

// readyCommand — команда clab server, которая ждёт готовности стека: код
// выхода, stderr и подставной исполнитель после неё.
type readyCommand struct {
	name, notReady string
	run            func(t *testing.T, uiDown map[string]bool, vars map[string]string) (int, string, *fakeRunner)
}

var readyCommands = []readyCommand{
	{"install", "the stack did not become ready", func(t *testing.T, uiDown map[string]bool, vars map[string]string) (int, string, *fakeRunner) {
		f := installSetup(t, vars)
		f.r.uiDown = uiDown
		code := f.install("--dir", f.dir("cadence"), "--mode", "proxy", "--yes")
		return code, f.errw.String(), f.r
	}},
	{"upgrade", "did not become ready", func(t *testing.T, uiDown map[string]bool, vars map[string]string) (int, string, *fakeRunner) {
		f := upgradeSetup(t, vars)
		f.r.uiDown = uiDown
		code := f.upgrade("v0.6.1")
		return code, f.errw.String(), f.r
	}},
	{"restore", "the stack did not become ready after the restore", func(t *testing.T, uiDown map[string]bool, vars map[string]string) (int, string, *fakeRunner) {
		dir := installed(t, "v0.6.1", nil)
		dump := filepath.Join(dir, "backups", "cadence-v0.6.1-20261008-030000.sql.gz")
		mustWrite(t, dump, gzipped(t, "CREATE TABLE x();\n"), 0o600)
		r := newFakeRunner()
		r.uiDown = uiDown
		e, _, errw := testEnv(t, dir, r, vars)
		code := runServer(context.Background(), e, []string{"restore", "--yes", dump})
		return code, errw.String(), r
	}},
}

// probes — сколько раз clab спросил ui по адресу.
func probes(r *fakeRunner, call string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestWaitReadyUIPort(t *testing.T) {
	cases := []struct {
		name   string
		uiDown map[string]bool
		ready  bool
		// Опрос на первом шаге: :8080 спрашивается всегда, :80 — только
		// если :8080 не ответил.
		want8080, want80 int
	}{
		{"only :80, images up to v0.13.0", map[string]bool{"8080": true}, true, 1, 1},
		{"only :8080, images after CLAB-248", map[string]bool{"80": true}, true, 1, 0},
		{"both, images v0.14.0", nil, true, 1, 0},
		// Таймаут 9 с — три шага по 3 с, на каждом оба адреса.
		{"neither", map[string]bool{"80": true, "8080": true}, false, 3, 3},
	}
	for _, cmd := range readyCommands {
		for _, c := range cases {
			t.Run(cmd.name+"/"+c.name, func(t *testing.T) {
				code, errw, r := cmd.run(t, c.uiDown, map[string]string{"CADENCE_READY_TIMEOUT": "9"})
				if c.ready && code != 0 {
					t.Fatalf("must be ready: code %d\n%s", code, errw)
				}
				if !c.ready && (code == 0 || !strings.Contains(errw, cmd.notReady)) {
					t.Fatalf("must not be ready: code %d\n%s", code, errw)
				}
				if got := probes(r, probe8080); got != c.want8080 {
					t.Errorf("probes of :8080: %d, want %d (%v)", got, c.want8080, r.calls)
				}
				if got := probes(r, probe80); got != c.want80 {
					t.Errorf("probes of :80: %d, want %d (%v)", got, c.want80, r.calls)
				}
			})
		}
		// Нулевой таймаут — «не готов» без единой проверки (сценарий отката
		// в ci/), даже когда ui отвечает на обоих портах.
		t.Run(cmd.name+"/timeout 0", func(t *testing.T) {
			code, errw, r := cmd.run(t, nil, map[string]string{"CADENCE_READY_TIMEOUT": "0"})
			if code == 0 || !strings.Contains(errw, cmd.notReady) {
				t.Fatalf("must not be ready: code %d\n%s", code, errw)
			}
			if r.called("docker compose exec -T caddy wget") || r.called("docker inspect -f {{.State.Health.Status}}") {
				t.Errorf("timeout 0 must not probe: %v", r.calls)
			}
		})
	}
}

// CLAB-267: готовность проверяет и маршруты Caddy — апстримы загруженного
// конфига из admin API в обоих режимах и / маршрутом в режиме proxy.
// Каждый случай идёт через install, upgrade и restore в режимах tls и proxy.

const (
	probeUpstreams = "docker compose exec -T caddy wget -q -T 5 -O - http://127.0.0.1:2019/reverse_proxy/upstreams"
	probeRoot      = "docker compose exec -T caddy wget -q -T 5 -O /dev/null http://localhost/"
	// Апстримы своего Caddyfile с маршрутом на ui:80 при образе только на :8080.
	upstreamsUI80 = `[{"address":"api:8080","num_requests":0,"fails":0},{"address":"ui:80","num_requests":0,"fails":0}]`
)

// tlsEnv — .env установки в режиме tls.
const tlsEnv = "CADENCE_MODE=tls\nCOMPOSE_FILE=docker-compose.yml:compose.tls.yml:compose.override.yml\n" +
	"SDLC_DOMAIN=cadence.example.com\nACME_EMAIL=admin@example.com\nPOSTGRES_PASSWORD=secret\nSDLC_ADMIN_TOKEN='sdlc_abc'\n"

type routeRun struct {
	code      int
	out, errw string
	r         *fakeRunner
	sleeps    int
}

// routeCommand — команда clab server, ждущая готовности, в режиме mode с
// подставным исполнителем, подготовленным prep.
type routeCommand struct {
	name, success string
	run           func(t *testing.T, mode string, prep func(*fakeRunner), vars map[string]string) routeRun
}

// counted — serverEnv считает шаги ожидания.
func counted(e *serverEnv, n *int) { e.sleep = func(time.Duration) { *n++ } }

var routeCommands = []routeCommand{
	{"install", "is up", func(t *testing.T, mode string, prep func(*fakeRunner), vars map[string]string) routeRun {
		f := installSetup(t, vars)
		prep(f.r)
		var n int
		counted(f.e, &n)
		args := []string{"--dir", f.dir("cadence"), "--mode", mode, "--yes"}
		if mode == "tls" {
			args = append(args, "--domain", "cadence.example.com")
		}
		code := f.install(args...)
		return routeRun{code, f.out.String(), f.errw.String(), f.r, n}
	}},
	{"upgrade", "upgraded", func(t *testing.T, mode string, prep func(*fakeRunner), vars map[string]string) routeRun {
		f := upgradeSetup(t, vars)
		if mode == "tls" {
			mustWrite(t, filepath.Join(f.dir, ".env"), tlsEnv, 0o600)
		}
		prep(f.r)
		var n int
		counted(f.e, &n)
		code := f.upgrade("v0.6.1")
		return routeRun{code, f.out.String(), f.errw.String(), f.r, n}
	}},
	{"restore", "restored", func(t *testing.T, mode string, prep func(*fakeRunner), vars map[string]string) routeRun {
		dir := installed(t, "v0.6.1", nil)
		if mode == "tls" {
			mustWrite(t, filepath.Join(dir, ".env"), tlsEnv, 0o600)
		}
		dump := filepath.Join(dir, "backups", "cadence-v0.6.1-20261008-030000.sql.gz")
		mustWrite(t, dump, gzipped(t, "CREATE TABLE x();\n"), 0o600)
		r := newFakeRunner()
		prep(r)
		e, out, errw := testEnv(t, dir, r, vars)
		var n int
		counted(e, &n)
		code := runServer(context.Background(), e, []string{"restore", "--yes", dump})
		return routeRun{code, out.String(), errw.String(), r, n}
	}},
}

var modes = []string{"tls", "proxy"}

// ready300 — таймаут по умолчанию: быстрый отказ не должен его ждать.
// Свежая карта на каждый запуск: installSetup дописывает в неё адрес релизов.
func ready300() map[string]string { return map[string]string{"CADENCE_READY_TIMEOUT": "300"} }

// mustRefuseRoute — команда отказала по маршруту: код 1, причина, лечение,
// ссылка на admin.md, никакого успеха; у upgrade — команды отката.
func mustRefuseRoute(t *testing.T, cmd routeCommand, got routeRun, want ...string) {
	t.Helper()
	if got.code != 1 {
		t.Fatalf("code %d, want 1\nout:\n%s\nerr:\n%s", got.code, got.out, got.errw)
	}
	for _, w := range append([]string{"ui:8080", "  export CADENCE_VERSION=$(cat VERSION)\n  docker compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile\n", `"The UI on port 8080"`,
		"everywhere except /v1, /mcp, /webhooks, /healthz and /readyz", "curl "}, want...) {
		if !strings.Contains(got.errw, w) {
			t.Errorf("stderr must name %q:\n%s", w, got.errw)
		}
	}
	for _, bad := range []string{"upgraded", "is up", "restored"} {
		if strings.Contains(got.out, bad) || strings.Contains(got.errw, bad) {
			t.Errorf("output must not say %q:\nout:\n%s\nerr:\n%s", bad, got.out, got.errw)
		}
	}
	if cmd.name == "upgrade" && !strings.Contains(got.errw, "return to v0.6.0 and its backup:\n") {
		t.Errorf("upgrade must print the rollback:\n%s", got.errw)
	}
	if !strings.HasSuffix(lastLogs(got.r), " caddy") {
		t.Errorf("the logs of caddy must be shown: %v", got.r.calls)
	}
}

// lastLogs — последний вызов docker compose logs.
func lastLogs(r *fakeRunner) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for j := len(r.calls) - 1; j >= 0; j-- {
		if strings.HasPrefix(r.calls[j], "docker compose logs --tail 40") {
			return r.calls[j]
		}
	}
	return ""
}

// AC-1, AC-3: свой маршрут на ui:80, а ui отвечает только на :8080 —
// отказ с причиной и лечением, и не через 300 с, а за три шага после первой
// удачной прямой пробы.
func TestWaitReadyRouteUpstreamDown(t *testing.T) {
	for _, cmd := range routeCommands {
		for _, mode := range modes {
			t.Run(cmd.name+"/"+mode, func(t *testing.T) {
				got := cmd.run(t, mode, func(r *fakeRunner) {
					r.uiDown = map[string]bool{"80": true}
					r.upstreams = upstreamsUI80
				}, ready300())
				mustRefuseRoute(t, cmd, got, "the upstream ui:80 of the loaded Caddy configuration does not answer")
				if n := probes(got.r, probeUpstreams); n != routeFailSteps {
					t.Errorf("upstream checks: %d, want %d", n, routeFailSteps)
				}
				if got.sleeps > routeFailSteps {
					t.Errorf("waited %d steps, want at most %d", got.sleeps, routeFailSteps)
				}
				if strings.Contains(got.errw, "did not become ready in 300s") {
					t.Errorf("the fast refusal must not be the timeout:\n%s", got.errw)
				}
			})
		}
	}
}

// AC-2: штатный Caddyfile — апстримы api:8080 и ui:8080 отвечают, в proxy
// / отвечает 200; готов с первого шага, без предупреждений о маршрутах.
func TestWaitReadyRouteDefault(t *testing.T) {
	for _, cmd := range routeCommands {
		for _, mode := range modes {
			t.Run(cmd.name+"/"+mode, func(t *testing.T) {
				got := cmd.run(t, mode, func(*fakeRunner) {}, ready300())
				if got.code != 0 || !strings.Contains(got.out, cmd.success) {
					t.Fatalf("must be ready: code %d\nout:\n%s\nerr:\n%s", got.code, got.out, got.errw)
				}
				if strings.Contains(got.errw, "routes of Caddy are not checked") {
					t.Errorf("no warning expected:\n%s", got.errw)
				}
				for _, call := range []string{probeUpstreams,
					"docker compose exec -T caddy wget -q -T 5 -O /dev/null http://api:8080/",
					"docker compose exec -T caddy wget -q -T 5 -O /dev/null http://ui:8080/"} {
					if probes(got.r, call) != 1 {
						t.Errorf("want one %q: %v", call, got.r.calls)
					}
				}
				if want := map[string]int{"tls": 0, "proxy": 1}[mode]; probes(got.r, probeRoot) != want {
					t.Errorf("probes of / through Caddy: %d, want %d", probes(got.r, probeRoot), want)
				}
				if got.sleeps != 0 {
					t.Errorf("waited %d steps, want 0", got.sleeps)
				}
			})
		}
	}
}

// AC-4: в режиме proxy / через Caddy — провал только 502 и 504; 200, 401
// своего basicauth и 404 — ответ маршрута.
func TestWaitReadyRouteRoot(t *testing.T) {
	for _, cmd := range routeCommands {
		for _, status := range []int{502, 504, 200, 401, 404} {
			t.Run(fmt.Sprintf("%s/%d", cmd.name, status), func(t *testing.T) {
				got := cmd.run(t, "proxy", func(r *fakeRunner) { r.rootStatus = status }, ready300())
				if status == 502 || status == 504 {
					mustRefuseRoute(t, cmd, got, fmt.Sprintf("the route / through Caddy (http://localhost/ in the caddy container) answers %d", status))
					return
				}
				if got.code != 0 {
					t.Fatalf("must be ready on %d: code %d\n%s", status, got.code, got.errw)
				}
			})
		}
	}
}

// AC-5: admin API не отвечает (свой Caddyfile с admin off) — маршруты не
// проверены: в tls предупреждение и готов, в proxy / всё равно пробуется, и
// 502 — отказ.
func TestWaitReadyRouteNoAdminAPI(t *testing.T) {
	for _, cmd := range routeCommands {
		t.Run(cmd.name+"/tls", func(t *testing.T) {
			got := cmd.run(t, "tls", func(r *fakeRunner) { r.upstreams = "off" }, ready300())
			if got.code != 0 || !strings.Contains(got.out, cmd.success) {
				t.Fatalf("must be ready: code %d\n%s", got.code, got.errw)
			}
			if strings.Count(got.errw, "! the routes of Caddy are not checked: its admin API (http://127.0.0.1:2019/reverse_proxy/upstreams in the caddy container) does not answer") != 1 {
				t.Errorf("want one warning:\n%s", got.errw)
			}
		})
		t.Run(cmd.name+"/proxy 200", func(t *testing.T) {
			got := cmd.run(t, "proxy", func(r *fakeRunner) { r.upstreams = "off" }, ready300())
			if got.code != 0 || !strings.Contains(got.errw, "readiness is the direct probes only and / through Caddy") {
				t.Fatalf("must be ready with the warning: code %d\n%s", got.code, got.errw)
			}
		})
		t.Run(cmd.name+"/proxy 502", func(t *testing.T) {
			got := cmd.run(t, "proxy", func(r *fakeRunner) { r.upstreams = "off"; r.rootStatus = 502 }, ready300())
			mustRefuseRoute(t, cmd, got, "answers 502")
		})
	}
}

// AC-6: в режиме tls пробы — только plain HTTP на ui, admin API и
// апстримы: ни https://, ни SDLC_DOMAIN, ни /.
func TestWaitReadyRouteTLSPlainHTTP(t *testing.T) {
	for _, cmd := range routeCommands {
		t.Run(cmd.name, func(t *testing.T) {
			got := cmd.run(t, "tls", func(r *fakeRunner) {
				r.uiDown = map[string]bool{"80": true}
				r.upstreams = upstreamsUI80
			}, ready300())
			allowed := map[string]bool{
				probe8080: true, probe80: true, probeUpstreams: true,
				"docker compose exec -T caddy wget -q -T 5 -O /dev/null http://api:8080/": true,
				"docker compose exec -T caddy wget -q -T 5 -O /dev/null http://ui:80/":    true,
			}
			got.r.mu.Lock()
			defer got.r.mu.Unlock()
			for _, c := range got.r.calls {
				if strings.Contains(c, "https://") || strings.Contains(c, "cadence.example.com") && strings.Contains(c, "caddy wget") {
					t.Errorf("no HTTPS and no domain in the probes: %s", c)
				}
				if strings.HasPrefix(c, "docker compose exec -T caddy wget") && !allowed[c] {
					t.Errorf("unexpected probe in tls mode: %s", c)
				}
			}
		})
	}
}

// Апстримы, которые не host:port (сокет unix, плейсхолдер), не пробуются,
// повторы спрашиваются один раз; апстрим с ответом 4xx отвечает.
func TestCaddyUpstreamsParse(t *testing.T) {
	got := routeCommands[1].run(t, "tls", func(r *fakeRunner) {
		r.upstreams = `[{"address":"api:8080"},{"address":"ui:8080"},{"address":"ui:8080"},{"address":"unix//run/x.sock"},{"address":"{http.request.host}:80"}]`
	}, ready300())
	if got.code != 0 {
		t.Fatalf("must be ready: %d\n%s", got.code, got.errw)
	}
	if n := probes(got.r, "docker compose exec -T caddy wget -q -T 5 -O /dev/null http://ui:8080/"); n != 1 {
		t.Errorf("ui:8080 asked %d times, want 1", n)
	}
	if got.r.called("docker compose exec -T caddy wget -q -T 5 -O /dev/null http://unix") ||
		got.r.called("docker compose exec -T caddy wget -q -T 5 -O /dev/null http://{") {
		t.Errorf("non host:port upstreams must not be asked: %v", got.r.calls)
	}
}

// Маршрут починился раньше третьего шага (администратор поправил Caddyfile
// и сделал reload) — готов, без отказа.
func TestWaitReadyRouteRecovers(t *testing.T) {
	f := upgradeSetup(t, ready300())
	f.r.uiDown = map[string]bool{"80": true}
	f.r.upstreams = upstreamsUI80
	steps := 0
	f.e.sleep = func(time.Duration) {
		steps++
		if steps == 2 {
			f.r.mu.Lock()
			f.r.upstreams = ""
			f.r.mu.Unlock()
		}
	}
	if code := f.upgrade("v0.6.1"); code != 0 || !strings.Contains(f.out.String(), "upgraded") {
		t.Fatalf("must be ready after the fix: %d\n%s", code, f.errw)
	}
}
