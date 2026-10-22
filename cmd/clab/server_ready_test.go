package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
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
