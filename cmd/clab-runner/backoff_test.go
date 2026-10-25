package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// CLAB-215: сбои подряд, пропуск после K, пауза по лимиту подписки. Сервер
// — queueAPI с очередью из нескольких задач, агент — скрипт, который ветвится
// по ключу задачи в промпте.

// queueAPI — сервер раннера с очередью из нескольких задач. Задача уходит из
// очереди, когда о ней отчитались (doneOnUsage): так успешный прогон не
// забирается снова на каждом опросе.
type queueAPI struct {
	srv *httptest.Server
	// Область задачи в ответе захвата.
	repos []map[string]any
	// Ответ на выпуск токена задачи; nil — выпуск проходит.
	issue       func(w http.ResponseWriter)
	doneOnUsage bool

	mu    sync.Mutex
	order []string
	runs  map[string]string
	done  map[string]bool
	calls []timedCall
}

type timedCall struct {
	apiCall
	Task string
	At   time.Time
}

func newQueueAPI(t *testing.T, tasks ...string) *queueAPI {
	f := &queueAPI{runs: map[string]string{}, done: map[string]bool{}}
	for _, task := range tasks {
		f.order = append(f.order, task)
		f.runs[task] = "run-" + task
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *queueAPI) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	call := timedCall{apiCall: apiCall{Method: req.Method, Path: req.URL.Path, Body: string(body)}, At: time.Now()}
	if rest, ok := strings.CutPrefix(call.Path, "/v1/tasks/"); ok {
		call.Task, _, _ = strings.Cut(rest, "/")
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	switch {
	case req.Method == http.MethodGet && call.Path == "/v1/projects/CLAB/queue":
		var queue []any
		f.mu.Lock()
		for _, task := range f.order {
			if !f.done[task] {
				queue = append(queue, map[string]any{"task": map[string]any{"key": task},
					"stage_run": map[string]any{"id": f.runs[task]}, "role_key": "developer", "claimable": true})
			}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"queue": queue})
	case strings.HasSuffix(call.Path, "/claim"):
		f.mu.Lock()
		run := f.runs[call.Task]
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"stage_run_id": run, "role": map[string]any{"key": "developer"},
			"context": map[string]any{"repositories": f.repos}})
	case strings.HasSuffix(call.Path, "/task-token"):
		if f.issue != nil {
			f.issue(w)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"token": "clab_task_" + call.Task,
			"record": map[string]any{"id": "tok-" + call.Task}})
	case strings.HasSuffix(call.Path, "/usage"):
		if f.doneOnUsage {
			f.mu.Lock()
			f.done[call.Task] = true
			f.mu.Unlock()
		}
		w.Write([]byte("{}"))
	case req.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Write([]byte("{}"))
	}
}

func (f *queueAPI) setRun(task, run string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[task] = run
}

// of — вызовы вида suffix (claim, release, usage) по задаче.
func (f *queueAPI) of(task, suffix string) []timedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []timedCall
	for _, c := range f.calls {
		if c.Task == task && strings.HasSuffix(c.Path, "/"+suffix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *queueAPI) claimsAfter(at time.Time) []timedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []timedCall
	for _, c := range f.calls {
		if strings.HasSuffix(c.Path, "/claim") && !c.At.Before(at) {
			out = append(out, c)
		}
	}
	return out
}

func noteOf(c timedCall) string {
	var body struct{ Note string }
	json.Unmarshal([]byte(c.Body), &body)
	return body.Note
}

func notesOf(calls []timedCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, noteOf(c))
	}
	return out
}

// syncBuffer — журнал раннера, который читают, пока слоты пишут.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// scriptAgent — агент-скрипт: $1 — промпт с ключом задачи, $DIR — каталог
// для счётчиков и меток.
func scriptAgent(t *testing.T, body string) (command, dir string) {
	t.Helper()
	dir = t.TempDir()
	path := filepath.Join(dir, "agent.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nDIR="+dir+"\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return "/bin/sh " + path + " {prompt}", dir
}

// countRun — номер запуска агента по задаче в $N (с 1).
const countRun = `N=$(( $(cat "$DIR/count" 2>/dev/null || echo 0) + 1 )); echo $N > "$DIR/count"`

func backoffRunner(t *testing.T, api *queueAPI, command string, logs io.Writer) *runner {
	t.Helper()
	cfg := Config{
		APIURL: api.srv.URL + "/v1", Token: runnerToken, Project: "CLAB", Slots: 1,
		Interval: 20 * time.Millisecond, Timeout: 20 * time.Second, Command: command,
		MaxFailures: 3, RetryDelay: 10 * time.Millisecond, RetryDelayMax: 10 * time.Millisecond,
		LimitPause: time.Hour,
	}
	r := newRunner(cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r.environ = func() []string { return parentEnv }
	return r
}

// runUntil крутит раннер, пока не выполнится условие, и останавливает его.
func runUntil(t *testing.T, r *runner, timeout time.Duration, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.run(ctx) }()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("за %s условие не выполнилось", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Пауза растёт до потолка, после K сбоев — пропуск (п. 3, 4 требований).
func TestFailuresDelayAndSkip(t *testing.T) {
	at := time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC)
	f := newFailures(5, time.Minute, 5*time.Minute)
	var got []string
	for range 5 {
		got = append(got, f.fail("run-1", at).note())
	}
	want := []string{
		"сбой 1 из 5, повторный захват не раньше чем через 1m",
		"сбой 2 из 5, повторный захват не раньше чем через 2m",
		"сбой 3 из 5, повторный захват не раньше чем через 4m",
		"сбой 4 из 5, повторный захват не раньше чем через 5m",
		"сбой 5 из 5, пропускаю до перезапуска раннера",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("причины:\n%s\nждали:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !f.blocked("run-1", at.AddDate(1, 0, 0)) {
		t.Fatal("после K сбоев прогон пропускается навсегда")
	}
	if f.blocked("run-2", at) {
		t.Fatal("другой прогон не на паузе")
	}

	f = newFailures(3, time.Minute, 15*time.Minute)
	f.fail("run-1", at)
	if !f.blocked("run-1", at.Add(59*time.Second)) || f.blocked("run-1", at.Add(time.Minute)) {
		t.Fatal("пауза после первого сбоя — ровно минута")
	}
	f.reset("run-1")
	if v := f.fail("run-1", at); v.n != 1 {
		t.Fatalf("успех обнуляет счёт, а сбой — %d", v.n)
	}
	for d, want := range map[time.Duration]string{time.Hour: "1h", 90 * time.Second: "1m30s",
		300 * time.Millisecond: "300ms", 2 * time.Hour: "2h"} {
		if got := shortDuration(d); got != want {
			t.Errorf("%v: %s, ждали %s", d, got, want)
		}
	}
}

// AC 1, 2: агент всегда падает — три захвата, паузы растут, причины с номером
// сбоя; дальше прогон не забирается.
func TestRepeatedFailuresBackoff(t *testing.T) {
	api := newQueueAPI(t, "CLAB-1")
	command, _ := scriptAgent(t, "exit 1")
	var logs syncBuffer
	r := backoffRunner(t, api, command, &logs)
	r.cfg.RetryDelay, r.cfg.RetryDelayMax = 300*time.Millisecond, 450*time.Millisecond
	r.fails = newFailures(3, r.cfg.RetryDelay, r.cfg.RetryDelayMax)

	start := time.Now()
	runUntil(t, r, 10*time.Second, func() bool { return len(api.of("CLAB-1", "release")) == 3 })
	// Ещё несколько опросов после третьего сбоя: захватов быть не должно.
	runUntil(t, r, 10*time.Second, func() bool { return time.Since(start) > 2*time.Second })

	claims, releases := api.of("CLAB-1", "claim"), api.of("CLAB-1", "release")
	if len(claims) != 3 || len(releases) != 3 {
		t.Fatalf("захватов %d, отпусков %d; ждали по три", len(claims), len(releases))
	}
	if gap := claims[1].At.Sub(releases[0].At); gap < 300*time.Millisecond {
		t.Fatalf("после первого сбоя пауза %s, ждали не меньше 300ms", gap)
	}
	if gap := claims[2].At.Sub(releases[1].At); gap < 450*time.Millisecond {
		t.Fatalf("после второго сбоя пауза %s, ждали не меньше 450ms (потолок)", gap)
	}
	want := []string{
		"агент завершился с ошибкой; сбой 1 из 3, повторный захват не раньше чем через 300ms",
		"агент завершился с ошибкой; сбой 2 из 3, повторный захват не раньше чем через 450ms",
		"агент завершился с ошибкой; сбой 3 из 3, пропускаю до перезапуска раннера",
	}
	if got := notesOf(releases); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("причины release:\n%s", strings.Join(got, "\n"))
	}
	if !strings.Contains(logs.String(), "прогон пропускается до перезапуска раннера") {
		t.Fatalf("журнал не говорит о пропуске:\n%s", logs.String())
	}
}

// AC 3: пока прогон A на паузе, свободная задача B забирается ближайшим
// опросом.
func TestOtherTaskClaimedWhileBlocked(t *testing.T) {
	api := newQueueAPI(t, "CLAB-1", "CLAB-2")
	api.doneOnUsage = true
	command, _ := scriptAgent(t, `case "$1" in *CLAB-1*) exit 1;; esac`+"\n"+agentOK)
	var logs syncBuffer
	r := backoffRunner(t, api, command, &logs)
	r.cfg.RetryDelay, r.cfg.RetryDelayMax = time.Minute, time.Minute
	r.fails = newFailures(3, time.Minute, time.Minute)

	runUntil(t, r, 5*time.Second, func() bool { return len(api.of("CLAB-2", "usage")) == 1 })
	first := api.of("CLAB-1", "release")
	b := api.of("CLAB-2", "claim")
	if len(first) != 1 || len(b) != 1 || len(api.of("CLAB-1", "claim")) != 1 {
		t.Fatalf("A: %d отпусков, B: %d захватов", len(first), len(b))
	}
	if gap := b[0].At.Sub(first[0].At); gap > time.Second {
		t.Fatalf("B забрана через %s после сбоя A, а не ближайшим опросом", gap)
	}
}

// AC 4: перезапуск раннера возвращает пропущенный прогон, счёт с начала.
func TestRestartClaimsSkippedRunAgain(t *testing.T) {
	api := newQueueAPI(t, "CLAB-1")
	command, _ := scriptAgent(t, "exit 1")
	var logs syncBuffer
	r := backoffRunner(t, api, command, &logs)
	runUntil(t, r, 5*time.Second, func() bool { return len(api.of("CLAB-1", "release")) == 3 })
	time.Sleep(100 * time.Millisecond)
	if n := len(api.of("CLAB-1", "claim")); n != 3 {
		t.Fatalf("до перезапуска %d захватов", n)
	}

	restarted := backoffRunner(t, api, command, &logs)
	runUntil(t, restarted, 5*time.Second, func() bool { return len(api.of("CLAB-1", "release")) >= 4 })
	if note := noteOf(api.of("CLAB-1", "release")[3]); !strings.Contains(note, "сбой 1 из 3") {
		t.Fatalf("после перезапуска причина %q", note)
	}
}

// AC 5: сбои на стороне раннера — тот же счётчик.
func TestRunnerSideFailuresCount(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(api *queueAPI, r *runner)
		reason string
	}{
		"рабочая копия": {
			setup: func(api *queueAPI, r *runner) {
				api.repos = []map[string]any{{"owner": "acme", "name": "tap", "default_branch": "main"}}
				r.ws = newWorkspaces(t.TempDir(), t.TempDir(), quietLog())
			},
			reason: "раннер не подготовил рабочую копию",
		},
		"токен задачи": {
			setup: func(api *queueAPI, r *runner) {
				api.issue = func(w http.ResponseWriter) { domainError(w, http.StatusConflict, "lease_expired") }
			},
			reason: "раннер не выпустил токен задачи: lease_expired",
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newQueueAPI(t, "CLAB-1")
			command, dir := scriptAgent(t, `touch "$DIR/started"`)
			var logs syncBuffer
			r := backoffRunner(t, api, command, &logs)
			tc.setup(api, r)

			runUntil(t, r, 5*time.Second, func() bool { return len(api.of("CLAB-1", "release")) == 3 })
			time.Sleep(100 * time.Millisecond)
			if n := len(api.of("CLAB-1", "claim")); n != 3 {
				t.Fatalf("захватов %d, после третьего сбоя прогон пропускается", n)
			}
			notes := notesOf(api.of("CLAB-1", "release"))
			for i, note := range notes {
				if !strings.Contains(note, tc.reason) || !strings.Contains(note, fmt.Sprintf("сбой %d из 3", i+1)) {
					t.Fatalf("причина %d: %q", i+1, note)
				}
			}
			if !strings.Contains(notes[2], "пропускаю до перезапуска раннера") {
				t.Fatalf("третья причина: %q", notes[2])
			}
			if exists(filepath.Join(dir, "started")) {
				t.Fatal("агент запущен, хотя раннер не подготовил запуск")
			}
		})
	}
}

// AC 6: успешный прогон обнуляет счёт.
func TestSuccessResetsFailures(t *testing.T) {
	api := newQueueAPI(t, "CLAB-1")
	command, _ := scriptAgent(t, countRun+"\n"+`[ "$N" = 3 ] && { `+agentOK+`; exit 0; }`+"\nexit 1")
	var logs syncBuffer
	r := backoffRunner(t, api, command, &logs)

	runUntil(t, r, 5*time.Second, func() bool { return len(api.of("CLAB-1", "release")) == 3 })
	notes := notesOf(api.of("CLAB-1", "release"))
	if len(api.of("CLAB-1", "usage")) != 1 {
		t.Fatal("третий запуск успешный и отчитывается о стоимости")
	}
	if !strings.Contains(notes[1], "сбой 2 из 3") || !strings.Contains(notes[2], "сбой 1 из 3") {
		t.Fatalf("после успеха счёт с начала: %q", notes)
	}
}

// AC 7: задача вернулась новым прогоном — он забирается сразу, счёт с нуля.
func TestNewRunStartsFromZero(t *testing.T) {
	api := newQueueAPI(t, "CLAB-1")
	command, _ := scriptAgent(t, "exit 1")
	var logs syncBuffer
	r := backoffRunner(t, api, command, &logs)

	runUntil(t, r, 10*time.Second, func() bool {
		if len(api.of("CLAB-1", "release")) == 3 {
			api.setRun("CLAB-1", "run-CLAB-1-again")
		}
		return len(api.of("CLAB-1", "release")) >= 4
	})
	releases := api.of("CLAB-1", "release")
	if note := noteOf(releases[3]); !strings.Contains(note, "сбой 1 из 3") {
		t.Fatalf("новый прогон: %q", note)
	}
	if gap := api.of("CLAB-1", "claim")[3].At.Sub(releases[2].At); gap > time.Second {
		t.Fatalf("новый прогон забран через %s", gap)
	}
}

// limitJSON — строки скрипта, печатающие ответ Claude Code
// --output-format json с ошибкой лимита; кончаются переводом строки.
func limitJSON(result string) string {
	raw, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": true,
		"result": result, "total_cost_usd": 0.01, "model": "claude-sonnet-5"})
	return "cat <<'JSON'\n" + string(raw) + "\nJSON\n"
}

// AC 8: лимит со временем сброса T — задача отпущена с T в причине, до T
// ни одного захвата, хотя очередь не пуста; после T захваты идут.
func TestLimitPausesUntilReset(t *testing.T) {
	api := newQueueAPI(t, "CLAB-1", "CLAB-2")
	api.doneOnUsage = true
	reset := time.Unix(time.Now().Add(2*time.Second).Unix(), 0)
	// Первый запуск A упирается в лимит, после сброса A отрабатывает.
	command, _ := scriptAgent(t, `case "$1" in *CLAB-1*) `+countRun+"\n"+`if [ "$N" = 1 ]; then`+"\n"+
		limitJSON(fmt.Sprintf("Claude AI usage limit reached|%d", reset.Unix()))+"exit 1\nfi;;\nesac\n"+agentOK)
	var logs syncBuffer
	r := backoffRunner(t, api, command, &logs)

	runUntil(t, r, 10*time.Second, func() bool { return len(api.of("CLAB-2", "usage")) == 1 })

	released := api.of("CLAB-1", "release")
	if len(released) == 0 {
		t.Fatal("задача с лимитом не отпущена")
	}
	note := noteOf(released[0])
	if !strings.Contains(note, "лимит подписки агента исчерпан") || !strings.Contains(note, reset.Format(time.RFC3339)) {
		t.Fatalf("причина release: %q", note)
	}
	for _, c := range api.claimsAfter(released[0].At) {
		if c.At.Before(reset) {
			t.Fatalf("захват %s в %s, до сброса лимита %s", c.Task, c.At.Format(time.StampMilli), reset)
		}
	}
	if b := api.of("CLAB-2", "claim"); len(b) != 1 || b[0].At.Before(reset) {
		t.Fatalf("B забрана до сброса: %+v", b)
	}
	journal := logs.String()
	if !strings.Contains(journal, "раннер на паузе до сброса") || !strings.Contains(journal, "resume_at="+reset.Format(time.RFC3339)) {
		t.Fatalf("журнал не говорит о паузе до %s:\n%s", reset, journal)
	}
	if !strings.Contains(journal, "пауза по лимиту подписки снята") {
		t.Fatalf("журнал не говорит о выходе из паузы:\n%s", journal)
	}
	if strings.Contains(note, "сбой") {
		t.Fatalf("лимит не сбой задачи: %q", note)
	}
}

// onceRunner — раннер одного захвата с остановленными часами.
func onceRunner(t *testing.T, api *queueAPI, command string, logs io.Writer, now time.Time) *runner {
	r := backoffRunner(t, api, command, logs)
	r.cfg.Once = true
	r.now = func() time.Time { return now }
	return r
}

// AC 9, 10, 11: лимит при коде 0 — тот же исход; время сброса неизвестно,
// в прошлом или дальше 7 суток — пауза SDLC_RUNNER_LIMIT_PAUSE; лимит в
// счётчик не идёт.
func TestLimitResultAndUnknownReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for name, tc := range map[string]struct {
		output string
		exit   int
		want   time.Time
	}{
		"код 0, время известно": {output: fmt.Sprintf("Claude AI usage limit reached|%d", now.Add(3*time.Hour).Unix()),
			want: now.Add(3 * time.Hour)},
		"код 1, без времени":         {output: "You've hit your session limit", exit: 1, want: now.Add(time.Hour)},
		"код 0, неразборчивое время": {output: "You've hit your session limit · resets soon", want: now.Add(time.Hour)},
		"время в прошлом": {output: fmt.Sprintf("Claude AI usage limit reached|%d", now.Add(-time.Minute).Unix()),
			exit: 1, want: now.Add(time.Hour)},
		"дальше 7 суток": {output: fmt.Sprintf("Claude AI usage limit reached|%d", now.Add(8*24*time.Hour).Unix()),
			exit: 1, want: now.Add(time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			api := newQueueAPI(t, "CLAB-1")
			command, dir := scriptAgent(t, countRun+"\n"+`if [ "$N" = 1 ]; then`+"\n"+limitJSON(tc.output)+
				fmt.Sprintf("exit %d\nfi\nexit 1", tc.exit))
			var logs syncBuffer
			r := onceRunner(t, api, command, &logs, now)
			if err := r.run(context.Background()); err != nil {
				t.Fatal(err)
			}

			if !r.paused || !r.pauseUntil.Equal(tc.want) {
				t.Fatalf("пауза %v до %s, ждали до %s", r.paused, r.pauseUntil, tc.want)
			}
			usage, release := api.of("CLAB-1", "usage"), api.of("CLAB-1", "release")
			if len(release) != 1 || len(usage) != 1 || usage[0].At.After(release[0].At) {
				t.Fatalf("учёт до release: usage %d, release %d", len(usage), len(release))
			}
			note := noteOf(release[0])
			if !strings.Contains(note, "раннер на паузе до "+tc.want.Format(time.RFC3339)) {
				t.Fatalf("причина: %q", note)
			}
			unknown := tc.want.Equal(now.Add(time.Hour))
			if unknown != strings.Contains(note, "время сброса неизвестно") ||
				unknown != strings.Contains(logs.String(), "время сброса неизвестно") {
				t.Fatalf("неизвестное время сброса названо не так: %q\n%s", note, logs.String())
			}

			// AC 11: после паузы обычный сбой той же задачи — первый.
			r.now = func() time.Time { return tc.want.Add(time.Second) }
			if err := r.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := readOut(t, dir, "count"); strings.TrimSpace(got) != "2" {
				t.Fatalf("второй запуск агента не случился: %s", got)
			}
			release = api.of("CLAB-1", "release")
			if len(release) != 2 || !strings.Contains(noteOf(release[1]), "сбой 1 из 3") {
				t.Fatalf("после лимита: %q", notesOf(release))
			}
		})
	}
}

// AC 12: лимит у агента одного слота не прерывает агента другого, а новых
// захватов до сброса нет.
func TestLimitKeepsRunningAgents(t *testing.T) {
	api := newQueueAPI(t, "CLAB-1", "CLAB-2", "CLAB-3")
	api.doneOnUsage = true
	reset := time.Unix(time.Now().Add(3*time.Second).Unix(), 0)
	command, dir := scriptAgent(t, `case "$1" in
*CLAB-1*) sleep 1; touch "$DIR/a-done"; `+agentOK+`; exit 0;;
*CLAB-2*) `+limitJSON(fmt.Sprintf("Claude AI usage limit reached|%d", reset.Unix()))+`exit 1;;
esac
`+agentOK)
	var logs syncBuffer
	r := backoffRunner(t, api, command, &logs)
	r.cfg.Slots = 2
	r.cfg.ReposDir = t.TempDir()
	r.ws = newWorkspaces(r.cfg.ReposDir, filepath.Join(r.cfg.ReposDir, ".worktrees"), quietLog())

	runUntil(t, r, 10*time.Second, func() bool { return len(api.of("CLAB-3", "usage")) == 1 })

	if !exists(filepath.Join(dir, "a-done")) || len(api.of("CLAB-1", "usage")) != 1 || len(api.of("CLAB-1", "release")) != 0 {
		t.Fatal("агент A прерван паузой")
	}
	limited := api.of("CLAB-2", "release")
	if len(limited) == 0 || !strings.Contains(noteOf(limited[0]), "лимит подписки") {
		t.Fatalf("B: %q", notesOf(limited))
	}
	for _, c := range api.claimsAfter(limited[0].At) {
		if c.At.Before(reset) {
			t.Fatalf("захват %s до сброса лимита", c.Task)
		}
	}
}

// Лимит у второго агента во время паузы продлевает её до более поздней
// границы, но не сокращает.
func TestLimitPauseOnlyExtends(t *testing.T) {
	api := newQueueAPI(t)
	var logs syncBuffer
	r := backoffRunner(t, api, "true", &logs)
	now := time.Now()
	r.now = func() time.Time { return now }
	log := r.log
	if got := r.pauseForLimit(log, agentLimit{resetAt: now.Add(2 * time.Hour)}); !got.Equal(now.Add(2 * time.Hour)) {
		t.Fatal(got)
	}
	if got := r.pauseForLimit(log, agentLimit{resetAt: now.Add(time.Hour)}); !got.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("пауза сократилась до %s", got)
	}
	if got := r.pauseForLimit(log, agentLimit{resetAt: now.Add(3 * time.Hour)}); !got.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("пауза не продлилась: %s", got)
	}
	if !strings.Contains(logs.String(), "пауза продлена") {
		t.Fatalf("журнал:\n%s", logs.String())
	}
}

// AC 16: сухой прогон и остановка раннера в счётчик не идут.
func TestDryRunAndStopAreNotFailures(t *testing.T) {
	api := newQueueAPI(t, "CLAB-1")
	command, _ := scriptAgent(t, "exit 1")
	var logs syncBuffer
	r := backoffRunner(t, api, command, &logs)
	r.cfg.Once, r.cfg.DryRun = true, true
	for range 4 {
		if err := r.run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, note := range notesOf(api.of("CLAB-1", "release")) {
		if note != "сухой прогон раннера" {
			t.Fatalf("сухой прогон: %q", note)
		}
	}
	if len(r.fails.runs) != 0 {
		t.Fatalf("сухой прогон попал в счётчик: %+v", r.fails.runs)
	}

	slow, _ := scriptAgent(t, "exec sleep 30")
	r = backoffRunner(t, api, slow, &logs)
	ctx, cancel := context.WithCancel(context.Background())
	item := queueItem{}
	item.Task.Key, item.Run.ID = "CLAB-1", "run-CLAB-1"
	time.AfterFunc(300*time.Millisecond, cancel)
	r.work(ctx, item, claimResult{StageRunID: "run-CLAB-1"})
	rel := api.of("CLAB-1", "release")
	if note := noteOf(rel[len(rel)-1]); !strings.Contains(note, "раннер остановлен") || strings.Contains(note, "сбой") {
		t.Fatalf("остановка раннера: %q", note)
	}
	if len(r.fails.runs) != 0 {
		t.Fatal("остановка раннера попала в счётчик")
	}
}

// AC 14, 15: значения по умолчанию и отказ старта с именем переменной.
func TestLoadConfigRetry(t *testing.T) {
	t.Setenv("SDLC_API_URL", "https://c.example.com/v1")
	t.Setenv("SDLC_PROJECT", "CLAB")
	for _, k := range []string{"SDLC_RUNNER_MAX_FAILURES", "SDLC_RUNNER_RETRY_DELAY",
		"SDLC_RUNNER_RETRY_DELAY_MAX", "SDLC_RUNNER_LIMIT_PAUSE", "SDLC_RUNNER_INTERVAL", "SDLC_RUNNER_TIMEOUT"} {
		t.Setenv(k, "")
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxFailures != 3 || cfg.RetryDelay != time.Minute || cfg.RetryDelayMax != 15*time.Minute || cfg.LimitPause != time.Hour {
		t.Fatalf("значения по умолчанию: %d %s %s %s", cfg.MaxFailures, cfg.RetryDelay, cfg.RetryDelayMax, cfg.LimitPause)
	}

	t.Setenv("SDLC_RUNNER_MAX_FAILURES", "5")
	t.Setenv("SDLC_RUNNER_RETRY_DELAY", "30s")
	t.Setenv("SDLC_RUNNER_RETRY_DELAY_MAX", "30s")
	t.Setenv("SDLC_RUNNER_LIMIT_PAUSE", "2h")
	if cfg, err = loadConfig(); err != nil || cfg.MaxFailures != 5 || cfg.RetryDelay != 30*time.Second ||
		cfg.RetryDelayMax != 30*time.Second || cfg.LimitPause != 2*time.Hour {
		t.Fatalf("заданные значения: %+v, %v", cfg, err)
	}

	for _, tc := range []struct{ key, value, other string }{
		{key: "SDLC_RUNNER_MAX_FAILURES", value: "0"},
		{key: "SDLC_RUNNER_MAX_FAILURES", value: "-1"},
		{key: "SDLC_RUNNER_MAX_FAILURES", value: "abc"},
		{key: "SDLC_RUNNER_RETRY_DELAY", value: "0s"},
		{key: "SDLC_RUNNER_RETRY_DELAY", value: "-1s"},
		{key: "SDLC_RUNNER_RETRY_DELAY", value: "abc"},
		{key: "SDLC_RUNNER_RETRY_DELAY_MAX", value: "10s"},
		{key: "SDLC_RUNNER_LIMIT_PAUSE", value: "0s"},
		{key: "SDLC_RUNNER_LIMIT_PAUSE", value: "abc"},
		{key: "SDLC_RUNNER_INTERVAL", value: "0s"},
		{key: "SDLC_RUNNER_TIMEOUT", value: "-5m"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			_, err := loadConfig()
			if err == nil || !strings.HasPrefix(err.Error(), tc.key+":") {
				t.Fatalf("ждали отказ с именем %s, а не %v", tc.key, err)
			}
		})
	}
}

// Прогон очереди читается из поля stage_run, как его отдаёт сервер
// (postgres.OpenRun): по нему ведётся счётчик сбоев.
func TestQueueItemCarriesStageRun(t *testing.T) {
	var item queueItem
	raw := `{"task":{"key":"CLAB-1"},"stage_run":{"id":"d4244e1e","kind":"stage"},"role_key":"developer","claimable":true}`
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatal(err)
	}
	if item.Run.ID != "d4244e1e" || runKey(item) != "d4244e1e" {
		t.Fatalf("прогон очереди: %+v", item.Run)
	}
	item.Run.ID = ""
	if runKey(item) != "task:CLAB-1" {
		t.Fatalf("без прогона ключ — задача: %s", runKey(item))
	}
}
