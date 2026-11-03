package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CLAB-369, AC-18: `up -d` падает ошибкой зависимости, пока api мигрирует
// базу дольше healthcheck'а compose, — install, upgrade и restore ждут api до
// CADENCE_READY_TIMEOUT и доводят стек повторным `up -d`; срок вышел —
// понятный отказ без ложного отката; ошибка другого рода — прежний текст.

type migrationRun struct {
	code      int
	out, errw string
	r         *fakeRunner
	elapsed   time.Duration
}

type migrationCommand struct {
	name, success, upFailed string
	run                     func(t *testing.T, prep func(*fakeRunner), vars map[string]string) migrationRun
}

// onClock — подставной исполнитель и serverEnv на общих часах; prep
// настраивает исполнитель.
func onClock(r *fakeRunner, e *serverEnv, prep func(*fakeRunner)) *fakeClock {
	c := newFakeClock()
	c.use(e)
	r.clock = c
	prep(r)
	return c
}

var migrationCommands = []migrationCommand{
	{"install", "is up", "docker compose up failed (see above). .env is kept",
		func(t *testing.T, prep func(*fakeRunner), vars map[string]string) migrationRun {
			f := installSetup(t, vars)
			c := onClock(f.r, f.e, prep)
			start := c.now()
			code := f.install("--dir", f.dir("cadence"), "--mode", "proxy", "--yes")
			return migrationRun{code, f.out.String(), f.errw.String(), f.r, c.now().Sub(start)}
		}},
	{"upgrade", "upgraded v0.6.0 -> v0.6.1", "docker compose up failed for v0.6.1. To return to v0.6.0",
		func(t *testing.T, prep func(*fakeRunner), vars map[string]string) migrationRun {
			f := upgradeSetup(t, vars)
			c := onClock(f.r, f.e, prep)
			start := c.now()
			code := f.upgrade("v0.6.1")
			return migrationRun{code, f.out.String(), f.errw.String(), f.r, c.now().Sub(start)}
		}},
	{"restore", "restored", "docker compose up failed:",
		func(t *testing.T, prep func(*fakeRunner), vars map[string]string) migrationRun {
			dir := installed(t, "v0.6.1", nil)
			dump := filepath.Join(dir, "backups", "cadence-v0.6.1-20261008-030000.sql.gz")
			mustWrite(t, dump, gzipped(t, "CREATE TABLE x();\n"), 0o600)
			r := newFakeRunner()
			e, out, errw := testEnv(t, dir, r, vars)
			c := onClock(r, e, prep)
			start := c.now()
			code := runServer(context.Background(), e, []string{"restore", "--yes", dump})
			return migrationRun{code, out.String(), errw.String(), r, c.now().Sub(start)}
		}},
}

// ups — сколько раз команда запускала стек целиком.
func ups(r *fakeRunner) int { return probes(r, "docker compose up -d --remove-orphans") }

func readyIn(sec string) map[string]string { return map[string]string{"CADENCE_READY_TIMEOUT": sec} }

func TestStartWaitsForMigratingAPI(t *testing.T) {
	for _, cmd := range migrationCommands {
		t.Run(cmd.name+"/api becomes healthy", func(t *testing.T) {
			got := cmd.run(t, func(r *fakeRunner) {
				r.upFails, r.healthy, r.healthyAfter = 1, false, 4
			}, readyIn("300"))
			if got.code != 0 || !strings.Contains(got.out+got.errw, cmd.success) {
				t.Fatalf("code %d, want success\nout:\n%s\nerr:\n%s", got.code, got.out, got.errw)
			}
			all := got.out + got.errw
			for _, want := range []string{"the api is not ready yet", "database migration may be running",
				"docker compose logs -f api", "CADENCE_READY_TIMEOUT (300s", "the api is ready; starting the rest of the stack"} {
				if !strings.Contains(all, want) {
					t.Errorf("no %q in the output:\n%s", want, all)
				}
			}
			if n := ups(got.r); n != 2 {
				t.Errorf("up -d ran %d times, want 2 (the second one starts the worker): %v", n, got.r.calls)
			}
		})

		t.Run(cmd.name+"/timeout", func(t *testing.T) {
			got := cmd.run(t, func(r *fakeRunner) { r.upFails, r.healthy = 1, false }, readyIn("30"))
			if got.code != 1 {
				t.Fatalf("code %d, want 1\nout:\n%s\nerr:\n%s", got.code, got.out, got.errw)
			}
			if strings.Contains(got.out+got.errw, cmd.success) {
				t.Fatalf("success printed without a ready stack:\n%s", got.out)
			}
			for _, want := range []string{"the api is not ready after CADENCE_READY_TIMEOUT (30s)",
				"a database migration may still be running (docker compose logs -f api)",
				"The worker is not started", "docker compose up -d --remove-orphans"} {
				if !strings.Contains(got.errw, want) {
					t.Errorf("no %q in the error:\n%s", want, got.errw)
				}
			}
			if cmd.name == "upgrade" {
				i := strings.Index(got.errw, "only if the api has failed")
				j := strings.Index(got.errw, "--yes")
				if i < 0 || j < 0 || j < i {
					t.Errorf("the rollback must come after the caveat:\n%s", got.errw)
				}
			}
			if strings.Contains(got.errw, cmd.upFailed) {
				t.Errorf("the old up -d failure text for a migrating api:\n%s", got.errw)
			}
			if n := ups(got.r); n != 1 {
				t.Errorf("up -d ran %d times, want 1", n)
			}
			if got.elapsed > 30*time.Second {
				t.Errorf("waited %s, more than CADENCE_READY_TIMEOUT", got.elapsed)
			}
		})

		// Круг 2, решение человека: CADENCE_READY_TIMEOUT по умолчанию — 600 с,
		// как DEPLOY_READY_TIMEOUT выкладки. Без переменной миграция дольше
		// прежних 300 с дожидается, а срок выходит на 600 с.
		t.Run(cmd.name+"/default timeout", func(t *testing.T) {
			got := cmd.run(t, func(r *fakeRunner) { r.upFails, r.healthy = 1, false }, map[string]string{})
			if got.code != 1 || !strings.Contains(got.errw, "the api is not ready after CADENCE_READY_TIMEOUT (600s)") {
				t.Fatalf("code %d, want the timeout of 600s\nout:\n%s\nerr:\n%s", got.code, got.out, got.errw)
			}
			if got.elapsed <= 300*time.Second || got.elapsed > 600*time.Second {
				t.Errorf("waited %s, want more than 300 s and no more than 600 s", got.elapsed)
			}
		})

		// R23: `up -d`, который сам ждёт api, срок не продлевает.
		t.Run(cmd.name+"/slow up counts", func(t *testing.T) {
			got := cmd.run(t, func(r *fakeRunner) {
				r.upFails, r.healthy, r.upTakes = 1, false, 25*time.Second
			}, readyIn("30"))
			if got.code != 1 || !strings.Contains(got.errw, "not ready after CADENCE_READY_TIMEOUT (30s)") {
				t.Fatalf("code %d\n%s", got.code, got.errw)
			}
			if got.elapsed > 30*time.Second {
				t.Errorf("waited %s with a 25 s up -d and a 30 s timeout", got.elapsed)
			}
		})

		// R25: api не создан или упал — прежний текст, без ожидания.
		for name, prep := range map[string]func(*fakeRunner){
			"api missing":    func(r *fakeRunner) { r.upFails, r.apiContainer = 1, "" },
			"api exited":     func(r *fakeRunner) { r.upFails, r.healthy, r.apiState = 1, false, "exited unhealthy 0" },
			"api restarting": func(r *fakeRunner) { r.upFails, r.healthy, r.apiState = 1, false, "running starting 3" },
			"no healthcheck": func(r *fakeRunner) { r.upFails, r.apiState = 1, "running none 0" },
		} {
			t.Run(cmd.name+"/"+name, func(t *testing.T) {
				if cmd.name == "upgrade" && name == "api missing" {
					t.Skip("upgrade needs the api container for its backup before the start")
				}
				got := cmd.run(t, prep, readyIn("300"))
				if got.code != 1 || !strings.Contains(got.errw, cmd.upFailed) {
					t.Fatalf("code %d, want the old failure %q\n%s", got.code, cmd.upFailed, got.errw)
				}
				if strings.Contains(got.out+got.errw, "the api is not ready yet") {
					t.Errorf("waited for an api that is not starting:\n%s", got.out)
				}
				if got.elapsed != 0 {
					t.Errorf("waited %s", got.elapsed)
				}
			})
		}

		// Api стал здоров между падением `up -d` и проверкой: ждать нечего,
		// повторный `up -d` доводит стек (одна проверка — одно состояние).
		t.Run(cmd.name+"/api healthy by the check", func(t *testing.T) {
			got := cmd.run(t, func(r *fakeRunner) { r.upFails = 1 }, readyIn("300"))
			if got.code != 0 || !strings.Contains(got.out+got.errw, cmd.success) {
				t.Fatalf("code %d\nout:\n%s\nerr:\n%s", got.code, got.out, got.errw)
			}
			if strings.Contains(got.out+got.errw, "the api is not ready yet") || ups(got.r) != 2 {
				t.Errorf("want no wait and a second up -d (%d):\n%s", ups(got.r), got.out)
			}
		})

		// R22: успех — только с запущенным воркером.
		t.Run(cmd.name+"/worker down", func(t *testing.T) {
			got := cmd.run(t, func(r *fakeRunner) { r.workerDown = true }, readyIn("300"))
			if got.code != 1 || strings.Contains(got.out, cmd.success) ||
				!strings.Contains(got.errw, "the worker is not running") {
				t.Fatalf("code %d\nout:\n%s\nerr:\n%s", got.code, got.out, got.errw)
			}
		})
	}
}

// Круг 2: умолчание CADENCE_READY_TIMEOUT — 600 с; .env и окружение его
// перекрывают; --help команд, которые ждут готовности, называют 600.
func TestReadyTimeoutDefault(t *testing.T) {
	for _, c := range []struct {
		env, getenv map[string]string
		want        int
	}{
		{nil, nil, 600},
		{map[string]string{"CADENCE_READY_TIMEOUT": "1800"}, nil, 1800},
		{nil, map[string]string{"CADENCE_READY_TIMEOUT": "0"}, 0},
	} {
		getenv := c.getenv
		i := &installation{e: &serverEnv{getenv: func(k string) string { return getenv[k] }}, env: c.env}
		if got, err := i.readyTimeout(); err != nil || got != c.want {
			t.Errorf("env %v, process %v: readyTimeout %d, %v; want %d", c.env, c.getenv, got, err, c.want)
		}
	}
	for name, help := range map[string]string{"install": installHelp, "upgrade": upgradeHelp, "restore": restoreHelp, "starter": starterHelp} {
		if !strings.Contains(help, "CADENCE_READY_TIMEOUT") || !strings.Contains(help, "default 600") || strings.Contains(help, "default 300") {
			t.Errorf("clab server %s --help does not name the default of 600 s:\n%s", name, help)
		}
	}
}
