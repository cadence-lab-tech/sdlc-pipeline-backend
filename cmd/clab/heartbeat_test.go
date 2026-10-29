package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// CLAB-288: clab heartbeat держит аренду сам — циклом (--every/--for) или
// пока работает долгая команда (-- cmd). Подставной сервер отвечает на
// POST /v1/tasks/K/heartbeat по номеру вызова.

const leaseRun = "5ca900b3-57f1-4df7-bd43-e69b594d53d4"

type leaseServer struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (s *leaseServer) calls() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

// newLeaseClient — клиент к подставному серверу; reply получает номер вызова
// с нуля и отдаёт код и тело ответа.
func newLeaseClient(t *testing.T, reply func(n int) (int, string)) (*Client, *leaseServer) {
	t.Helper()
	rec := &leaseServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/CLAB-7/heartbeat" {
			t.Errorf("запрос %s %s", r.Method, r.URL.Path)
		}
		body := map[string]any{}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("тело не JSON: %q", raw)
		}
		rec.mu.Lock()
		n := len(rec.bodies)
		rec.bodies = append(rec.bodies, body)
		rec.mu.Unlock()
		code, out := reply(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: "https://ui.example", actor: "user:alice", http: srv.Client()}, rec
}

func leaseOK(expiresIn time.Duration, interval int) (int, string) {
	return http.StatusOK, fmt.Sprintf(`{"stage_run_id":%q,"expires_at":%q,"heartbeat_interval_seconds":%d}`,
		leaseRun, time.Now().Add(expiresIn).UTC().Format(time.RFC3339Nano), interval)
}

func apiFail(status int, code, msg string) (int, string) {
	return status, fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, msg)
}

// Каждое продление после первого адресовано прогону из первого ответа.
func assertPinned(t *testing.T, calls []map[string]any) {
	t.Helper()
	if len(calls) == 0 || len(calls[0]) != 0 {
		t.Fatalf("первое продление не пустым телом: %v", calls)
	}
	for i, b := range calls[1:] {
		if b["stage_run_id"] != leaseRun {
			t.Fatalf("продление %d не закреплено за прогоном: %v", i+1, b)
		}
	}
}

// Критерий 10: без флагов — одно продление и тот же JSON, что до правки.
func TestHeartbeatOnceUnchanged(t *testing.T) {
	withJSON(t, false)
	reply := `{"stage_run_id":"` + leaseRun + `","expires_at":"2026-10-26T12:30:00Z","heartbeat_interval_seconds":300}`
	c, rec := newLeaseClient(t, func(int) (int, string) { return http.StatusOK, reply })
	out, _, err := capture(t, func() error { return cmdHeartbeat(context.Background(), c, []string{"CLAB-7"}) })
	if err != nil {
		t.Fatal(err)
	}
	calls := rec.calls()
	if len(calls) != 1 || len(calls[0]) != 0 {
		t.Fatalf("продлений %d, тело %v — ждали одно с пустым телом", len(calls), calls)
	}
	var v any
	_ = json.Unmarshal([]byte(reply), &v)
	want, _ := json.MarshalIndent(v, "", "  ")
	if out != string(want)+"\n" {
		t.Fatalf("вывод изменился:\n%s\nждали:\n%s", out, want)
	}
}

// Критерии 1 и 2: цикл продлевает закреплённый прогон, а когда аренды не
// стало (снята, переход), выходит с кодом 0 и строкой с причиной.
func TestHeartbeatLoopStopsWhenLeaseGone(t *testing.T) {
	withJSON(t, false)
	c, rec := newLeaseClient(t, func(n int) (int, string) {
		if n < 4 {
			return leaseOK(10*time.Second, 300)
		}
		return apiFail(http.StatusConflict, "lease_expired", "аренда истекла, задача возвращена в pending — нужен повторный захват")
	})
	out, _, err := capture(t, func() error {
		return cmdHeartbeat(context.Background(), c, []string{"CLAB-7", "--every", "20ms"})
	})
	if err != nil {
		t.Fatalf("уход аренды — не ошибка: %v", err)
	}
	calls := rec.calls()
	if len(calls) != 5 {
		t.Fatalf("продлений %d, ждали 5: цикл обязан остановиться на первом отказе", len(calls))
	}
	assertPinned(t, calls)
	for _, want := range []string{"продлена до", "каждые 20ms", "аренды прогона 5ca900b3 больше нет", "lease_expired"} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет %q:\n%s", want, out)
		}
	}
}

// Критерий 3: первое продление на чужой или истёкшей аренде — ошибка с
// кодом и текстом сервера, цикла нет.
func TestHeartbeatFirstFailureIsError(t *testing.T) {
	for _, code := range []string{"lease_held_by_other", "lease_expired"} {
		for _, args := range [][]string{
			{"CLAB-7", "--every", "20ms"},
			{"CLAB-7", "--", "sh", "-c", "echo never"},
		} {
			c, rec := newLeaseClient(t, func(int) (int, string) {
				return apiFail(http.StatusConflict, code, "аренду держит agent:other")
			})
			out, _, err := capture(t, func() error { return cmdHeartbeat(context.Background(), c, args) })
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != code {
				t.Fatalf("%v: ждали %s, получили %v", args, code, err)
			}
			if len(rec.calls()) != 1 || strings.Contains(out, "never") {
				t.Fatalf("%v: после отказа были продления или запуск команды: %d, %q", args, len(rec.calls()), out)
			}
		}
	}
}

// Критерий 4: 5xx и сеть цикл переживает, 401/403 и lease_* — нет.
func TestHeartbeatRetriesTransient(t *testing.T) {
	withJSON(t, false)
	c, rec := newLeaseClient(t, func(n int) (int, string) {
		switch n {
		case 1:
			return apiFail(http.StatusServiceUnavailable, "unavailable", "база недоступна")
		case 2:
			return http.StatusBadGateway, "<html>bad gateway</html>"
		case 4:
			return apiFail(http.StatusUnauthorized, "unauthenticated", "токен задачи погас: отозван")
		}
		return leaseOK(10*time.Second, 300)
	})
	out, errOut, err := capture(t, func() error {
		return cmdHeartbeat(context.Background(), c, []string{"CLAB-7", "--every", "20ms"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rec.calls()); n != 5 {
		t.Fatalf("продлений %d, ждали 5: после 503 и 502 цикл продолжает, на 401 — выходит", n)
	}
	if strings.Count(errOut, "повтор через 20ms") != 2 {
		t.Errorf("предупреждения о повторе:\n%s", errOut)
	}
	if !strings.Contains(out, "токен задачи погас") {
		t.Errorf("нет причины выхода:\n%s", out)
	}

	gone := map[error]bool{
		&APIError{Status: 401, Code: "unauthenticated"}:     true,
		&APIError{Status: 403, Code: "forbidden"}:           true,
		&APIError{Status: 404, Code: "not_found"}:           true,
		&APIError{Status: 409, Code: "lease_held_by_other"}: true,
		&APIError{Status: 409, Code: "lease_expired"}:       true,
		&APIError{Status: 429}:                              false,
		&APIError{Status: 500}:                              false,
		&APIError{Status: 503, Code: "unavailable"}:         false,
		errors.New("dial tcp: connection refused"):          false,
		fmt.Errorf("обёртка: %w", &APIError{Status: 409}):   true,
	}
	for err, want := range gone {
		if leaseGone(err) != want {
			t.Errorf("leaseGone(%v) = %v", err, !want)
		}
	}
}

// Критерий 4, сеть: сервер недоступен на тике — повтор, а не выход.
func TestHeartbeatSurvivesNetworkError(t *testing.T) {
	c, rec := newLeaseClient(t, func(n int) (int, string) {
		if n >= 2 {
			return apiFail(http.StatusConflict, "lease_expired", "истекла")
		}
		return leaseOK(10*time.Second, 300)
	})
	real := c.http.Transport
	var mu sync.Mutex
	ticks := 0
	c.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		ticks++
		n := ticks
		mu.Unlock()
		if n == 2 {
			return nil, errors.New("connection refused")
		}
		return real.RoundTrip(r)
	})
	var log bytes.Buffer
	h := &heartbeater{c: c, task: "CLAB-7", every: 20 * time.Millisecond, out: &log, warn: &log}
	if err := h.loop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.calls()); n != 3 || !strings.Contains(log.String(), "connection refused") {
		t.Fatalf("продлений на сервере %d (ждали 3), журнал:\n%s", n, log.String())
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Критерий 5: --for, отказ при интервале не меньше срока аренды, интервал
// по умолчанию из ответа.
func TestHeartbeatForAndInterval(t *testing.T) {
	withJSON(t, false)
	c, rec := newLeaseClient(t, func(int) (int, string) { return leaseOK(10*time.Second, 300) })
	start := time.Now()
	out, _, err := capture(t, func() error {
		return cmdHeartbeat(context.Background(), c, []string{"CLAB-7", "--every", "20ms", "--for", "150ms"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 150*time.Millisecond || took > 2*time.Second {
		t.Fatalf("--for 150ms длилось %s", took)
	}
	if n := len(rec.calls()); n < 3 || !strings.Contains(out, "--for 150ms истекло") {
		t.Fatalf("продлений %d, вывод:\n%s", n, out)
	}
	assertPinned(t, rec.calls())

	// Интервал не меньше оставшегося срока — отказ до цикла, оба значения названы.
	c, rec = newLeaseClient(t, func(int) (int, string) { return leaseOK(60*time.Second, 300) })
	_, _, err = capture(t, func() error {
		return cmdHeartbeat(context.Background(), c, []string{"CLAB-7", "--every", "5m"})
	})
	if err == nil || !strings.Contains(err.Error(), "интервал продления 5m0s") || !strings.Contains(err.Error(), "срока аренды 1m0s") {
		t.Fatalf("отказ по интервалу: %v", err)
	}
	if len(rec.calls()) != 1 {
		t.Fatalf("после отказа были продления: %d", len(rec.calls()))
	}
	// Без --every — интервал сервера: 300 с при аренде 60 с — тот же отказ.
	c, _ = newLeaseClient(t, func(int) (int, string) { return leaseOK(60*time.Second, 300) })
	_, _, err = capture(t, func() error {
		return cmdHeartbeat(context.Background(), c, []string{"CLAB-7", "--for", "1s"})
	})
	if err == nil || !strings.Contains(err.Error(), "интервал продления 5m0s") {
		t.Fatalf("интервал сервера не взят: %v", err)
	}
	c, _ = newLeaseClient(t, func(int) (int, string) { return leaseOK(30*time.Minute, 300) })
	h := &heartbeater{c: c, task: "CLAB-7", out: io.Discard, warn: io.Discard}
	if err := h.first(context.Background()); err != nil || h.every != 5*time.Minute || h.runID != leaseRun {
		t.Fatalf("интервал по умолчанию: %s, прогон %s, %v", h.every, h.runID, err)
	}
	// Старый сервер без интервала — треть срока.
	c, _ = newLeaseClient(t, func(int) (int, string) { return leaseOK(30*time.Minute, 0) })
	h = &heartbeater{c: c, task: "CLAB-7", out: io.Discard, warn: io.Discard}
	if err := h.first(context.Background()); err != nil || h.every < 9*time.Minute || h.every > 10*time.Minute {
		t.Fatalf("интервал без heartbeat_interval_seconds: %s %v", h.every, err)
	}
}

// Неверные сочетания флагов отбиваются до запроса.
func TestHeartbeatFlagErrors(t *testing.T) {
	c, rec := newLeaseClient(t, func(int) (int, string) { return leaseOK(10*time.Second, 300) })
	for args, want := range map[string]string{
		"CLAB-7 --every 0s":             "--every должен быть больше нуля",
		"CLAB-7 --for -1s":              "--for должен быть больше нуля",
		"CLAB-7 --":                     "после -- нет команды",
		"CLAB-7 --for 1m -- sh -c true": "--for не сочетается с командой",
		"CLAB-7 --every 1m sh -c true":  "команду передают после --",
		"--every 1m":                    "не указан ключ задачи",
	} {
		_, _, err := capture(t, func() error {
			return cmdHeartbeat(context.Background(), c, strings.Fields(args))
		})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, ждали %q", args, err, want)
		}
	}
	if n := len(rec.calls()); n != 0 {
		t.Fatalf("неверные флаги дошли до сервера: %d", n)
	}
}

// «--» отделяет чужую команду и от общих флагов clab: её --json и --host
// не наши.
func TestSplitGlobalsStopsAtDoubleDash(t *testing.T) {
	g, rest := splitGlobals([]string{"--json", "heartbeat", "K", "--", "tool", "--json", "--host", "x"})
	if !g.json || g.host != "" {
		t.Fatalf("глобальные: %+v", g)
	}
	if strings.Join(rest, " ") != "heartbeat K -- tool --json --host x" {
		t.Fatalf("остаток: %v", rest)
	}
}

func wrapHeartbeat(t *testing.T, c *Client, every time.Duration, argv []string, stdin string, sigs <-chan os.Signal) (int, string, string, string, error) {
	t.Helper()
	var log, stdout, stderr bytes.Buffer
	h := &heartbeater{c: c, task: "CLAB-7", every: every, out: &log, warn: &log}
	code, err := h.wrap(context.Background(), argv, strings.NewReader(stdin), &stdout, &stderr, sigs)
	return code, stdout.String(), stderr.String(), log.String(), err
}

func needSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("нет sh")
	}
}

// Критерий 6: код выхода — код команды, вывод и ввод проходят как есть,
// аренда продлевается, пока команда работает.
func TestHeartbeatWrapExitCodeAndIO(t *testing.T) {
	needSh(t)
	c, rec := newLeaseClient(t, func(int) (int, string) { return leaseOK(10*time.Second, 300) })
	code, stdout, stderr, log, err := wrapHeartbeat(t, c, 20*time.Millisecond,
		[]string{"sh", "-c", "read x; echo \"out $x\"; echo err >&2; sleep 0.3; exit 3"}, "hello\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 || stdout != "out hello\n" || stderr != "err\n" {
		t.Fatalf("код %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	calls := rec.calls()
	if len(calls) < 5 {
		t.Fatalf("продлений за 300 мс при интервале 20 мс: %d", len(calls))
	}
	assertPinned(t, calls)
	if !strings.Contains(log, "продлена до") || strings.Contains(log, "потеряна") {
		t.Fatalf("журнал обёртки:\n%s", log)
	}
	// После выхода команды продлений больше нет.
	n := len(rec.calls())
	time.Sleep(80 * time.Millisecond)
	if len(rec.calls()) != n {
		t.Fatal("продление идёт после выхода команды")
	}

	// Команды нет — ошибка запуска, а не код.
	if _, _, _, _, err := wrapHeartbeat(t, c, 20*time.Millisecond, []string{"/nonexistent/clab-288-cmd"}, "", nil); err == nil ||
		!strings.Contains(err.Error(), "команда не запустилась") {
		t.Fatalf("несуществующая команда: %v", err)
	}
}

// Критерий 7: аренду сняли посреди команды — предупреждение, команда
// доработала, код её.
func TestHeartbeatWrapLeaseLostKeepsCommand(t *testing.T) {
	needSh(t)
	c, rec := newLeaseClient(t, func(n int) (int, string) {
		if n < 2 {
			return leaseOK(10*time.Second, 300)
		}
		return apiFail(http.StatusConflict, "lease_expired", "аренда истекла")
	})
	code, stdout, _, log, err := wrapHeartbeat(t, c, 20*time.Millisecond,
		[]string{"sh", "-c", "sleep 0.3; echo done; exit 5"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != 5 || stdout != "done\n" {
		t.Fatalf("команда прервана: код %d, вывод %q", code, stdout)
	}
	if !strings.Contains(log, "аренда прогона 5ca900b3 потеряна, команда продолжает работу") || !strings.Contains(log, "lease_expired") {
		t.Fatalf("нет предупреждения:\n%s", log)
	}
	if n := len(rec.calls()); n != 3 {
		t.Fatalf("продлений %d, ждали 3: после потери аренды продлевать нечего", n)
	}
}

// Критерий 7: INT и TERM, пришедшие clab, получает команда; убитая сигналом
// команда даёт 128+номер.
func TestHeartbeatWrapForwardsSignals(t *testing.T) {
	needSh(t)
	c, _ := newLeaseClient(t, func(int) (int, string) { return leaseOK(10*time.Second, 300) })
	for _, s := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		sigs := make(chan os.Signal, 1)
		go func() {
			time.Sleep(300 * time.Millisecond)
			sigs <- s
		}()
		start := time.Now()
		code, stdout, _, _, err := wrapHeartbeat(t, c, 20*time.Millisecond,
			[]string{"sh", "-c", "trap 'echo got; exit 42' INT TERM; i=0; while [ $i -lt 100 ]; do sleep 0.05; i=$((i+1)); done; exit 1"}, "", sigs)
		if err != nil {
			t.Fatal(err)
		}
		if code != 42 || stdout != "got\n" || time.Since(start) > 3*time.Second {
			t.Fatalf("%v: код %d, вывод %q за %s — сигнал не дошёл до команды", s, code, stdout, time.Since(start))
		}
	}
	code, _, _, _, err := wrapHeartbeat(t, c, 20*time.Millisecond, []string{"sh", "-c", "kill -TERM $$"}, "", nil)
	if err != nil || code != 128+int(syscall.SIGTERM) {
		t.Fatalf("убитая сигналом команда: код %d, %v", code, err)
	}
}

// Через cmdHeartbeat: обёртка с успешной командой возвращает nil, вывод
// команды — в stdout, строки clab — в stderr.
func TestHeartbeatWrapViaCommand(t *testing.T) {
	needSh(t)
	withJSON(t, false)
	c, rec := newLeaseClient(t, func(int) (int, string) { return leaseOK(10*time.Second, 300) })
	out, errOut, err := capture(t, func() error {
		return cmdHeartbeat(context.Background(), c, []string{"CLAB-7", "--every", "20ms", "--", "sh", "-c", "sleep 0.1; echo --json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "--json\n" || !strings.Contains(errOut, "продлена до") || len(rec.calls()) < 2 {
		t.Fatalf("stdout %q, stderr %q, продлений %d", out, errOut, len(rec.calls()))
	}
}

// CLAB-306: команда под обёрткой идёт без учётных данных clab.

// helperClab: тестовый бинарь, запущенный как clab — «os.Args[0]
// -test.run=^TestHelperClab$ -- <аргументы clab>». Без CLAB_TEST_HELPER=1
// тест ничего не делает.
func TestHelperClab(t *testing.T) {
	if os.Getenv("CLAB_TEST_HELPER") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "--" {
			os.Args = append([]string{"clab"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

// credentialEnv — окружение исполнителя этапа: токен, адрес, проект, лицо и
// каталог конфига с хостом по умолчанию. Возвращает каталог конфига.
func credentialEnv(t *testing.T, hostURL string) string {
	t.Helper()
	dir := t.TempDir()
	host := strings.TrimPrefix(hostURL, "http://")
	if err := saveConfigAt(dir, Config{DefaultHost: host, Hosts: map[string]HostConfig{host: {URL: hostURL, Token: "task-token"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAB_CONFIG_DIR", dir)
	t.Setenv("SDLC_TOKEN", "task-token")
	t.Setenv("SDLC_API_URL", "https://prod.example/v1")
	t.Setenv("SDLC_PROJECT", "CLAB")
	t.Setenv("SDLC_ACTOR", "user:alice")
	t.Setenv("FOO", "bar")
	return dir
}

func envMap(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

// AC-1: по умолчанию окружение команды без учётных данных, CLAB_CONFIG_DIR
// — другой пустой каталог, который уходит вместе с командой; остальное
// окружение проходит.
func TestHeartbeatWrapIsolatesCredentials(t *testing.T) {
	needSh(t)
	c, _ := newLeaseClient(t, func(int) (int, string) { return leaseOK(10*time.Second, 300) })
	parentDir := credentialEnv(t, "http://127.0.0.1:1")
	script := `env; echo ENTRIES=$(ls -A "$CLAB_CONFIG_DIR" | wc -l | tr -d ' ')`
	code, stdout, _, log, err := wrapHeartbeat(t, c, 20*time.Millisecond, []string{"sh", "-c", script}, "", nil)
	if err != nil || code != 0 {
		t.Fatalf("код %d: %v", code, err)
	}
	env := envMap(stdout)
	for _, name := range []string{"SDLC_TOKEN", "SDLC_API_URL", "SDLC_PROJECT", "SDLC_ACTOR"} {
		if v, ok := env[name]; ok {
			t.Errorf("%s дошёл до команды: %q", name, v)
		}
	}
	dir := env["CLAB_CONFIG_DIR"]
	if dir == "" || dir == parentDir {
		t.Fatalf("CLAB_CONFIG_DIR команды %q, родителя %q", dir, parentDir)
	}
	if env["ENTRIES"] != "0" {
		t.Errorf("каталог конфига команды не пуст: %q", env["ENTRIES"])
	}
	if env["FOO"] != "bar" || env["PATH"] != os.Getenv("PATH") {
		t.Errorf("остальное окружение не прошло: FOO=%q PATH=%q", env["FOO"], env["PATH"])
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("временный каталог остался после выхода команды: %v", err)
	}
	// Сама обёртка продлевала аренду своими данными, родительский конфиг цел.
	if _, err := os.Stat(parentDir + "/config.json"); err != nil {
		t.Errorf("конфиг родителя тронут: %v", err)
	}
	// AC-5: одна строка об изоляции, с названием флага.
	var notes []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "без учётных данных clab") {
			notes = append(notes, l)
		}
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "--with-credentials") {
		t.Errorf("строка об изоляции: %q\nжурнал:\n%s", notes, log)
	}
}

// Права каталога — только владельцу (os.MkdirTemp даёт 0700).
func TestHeartbeatWrapConfigDirMode(t *testing.T) {
	needSh(t)
	c, _ := newLeaseClient(t, func(int) (int, string) { return leaseOK(10*time.Second, 300) })
	code, stdout, _, _, err := wrapHeartbeat(t, c, 20*time.Millisecond,
		[]string{"sh", "-c", `stat -c %a "$CLAB_CONFIG_DIR" 2>/dev/null || stat -f %Lp "$CLAB_CONFIG_DIR"`}, "", nil)
	if err != nil || code != 0 {
		t.Fatalf("код %d: %v", code, err)
	}
	if mode := strings.TrimSpace(stdout); mode != "700" {
		t.Fatalf("права каталога %q, ждали 700", mode)
	}
}

// AC-4: --with-credentials — окружение как у родителя, строки об изоляции нет.
func TestHeartbeatWrapWithCredentialsInherits(t *testing.T) {
	needSh(t)
	c, _ := newLeaseClient(t, func(int) (int, string) { return leaseOK(10*time.Second, 300) })
	parentDir := credentialEnv(t, "http://127.0.0.1:1")
	var log, stdout, stderr bytes.Buffer
	h := &heartbeater{c: c, task: "CLAB-7", every: 20 * time.Millisecond, out: &log, warn: &log, withCredentials: true}
	code, err := h.wrap(context.Background(), []string{"sh", "-c", "env"}, strings.NewReader(""), &stdout, &stderr, nil)
	if err != nil || code != 0 {
		t.Fatalf("код %d: %v", code, err)
	}
	env := envMap(stdout.String())
	want := map[string]string{"SDLC_TOKEN": "task-token", "SDLC_API_URL": "https://prod.example/v1", "SDLC_PROJECT": "CLAB",
		"SDLC_ACTOR": "user:alice", "CLAB_CONFIG_DIR": parentDir, "FOO": "bar"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s=%q, ждали %q", k, env[k], v)
		}
	}
	if strings.Contains(log.String(), "без учётных данных") {
		t.Errorf("строка об изоляции при --with-credentials:\n%s", log.String())
	}
}

// AC-2: внутренний clab без адреса не ходит на хост по умолчанию из конфига
// исполнителя; на сервер приходит только heartbeat обёртки.
func TestHeartbeatWrapInnerClabStaysOffConfigHost(t *testing.T) {
	needSh(t)
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/CLAB-7/heartbeat" {
			code, out := leaseOK(10*time.Second, 300)
			w.WriteHeader(code)
			_, _ = w.Write([]byte(out))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	credentialEnv(t, srv.URL)
	t.Setenv("SDLC_API_URL", "") // как у исполнителя: адрес берётся из конфига
	t.Setenv("CLAB_TEST_HELPER", "1")
	c := &Client{base: srv.URL + "/v1", origin: srv.URL, actor: "user:alice", token: "task-token", http: srv.Client()}

	inner := []string{os.Args[0], "-test.run=^TestHelperClab$", "--", "show", "CLAB-7"}
	code, _, stderr, _, err := wrapHeartbeat(t, c, 20*time.Millisecond, inner, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 || !strings.Contains(stderr, "хост не настроен") {
		t.Fatalf("внутренний clab: код %d, stderr %q", code, stderr)
	}
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
	got := requests()
	for _, r := range got {
		if r != "POST /v1/tasks/CLAB-7/heartbeat" {
			t.Errorf("на хост из конфига ушёл запрос %q (все: %v)", r, got)
		}
	}
	if len(got) == 0 {
		t.Error("обёртка не продлевала аренду")
	}

	// С флагом внутренний clab берёт хост и токен исполнителя — и доходит до сервера.
	h := &heartbeater{c: c, task: "CLAB-7", every: 5 * time.Second, out: io.Discard, warn: io.Discard, withCredentials: true}
	if _, err = h.wrap(context.Background(), inner, strings.NewReader(""), io.Discard, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	var reached bool
	for _, r := range requests() {
		reached = reached || strings.HasPrefix(r, "GET /v1/")
	}
	if !reached {
		t.Errorf("с --with-credentials внутренний clab не дошёл до хоста конфига: %v", requests())
	}
}

// AC-4: --with-credentials без команды — ошибка флагов, сервер не тронут.
func TestHeartbeatWithCredentialsNeedsCommand(t *testing.T) {
	c, rec := newLeaseClient(t, func(int) (int, string) { return leaseOK(10*time.Second, 300) })
	for _, args := range []string{"CLAB-7 --with-credentials", "CLAB-7 --every 5m --with-credentials"} {
		_, _, err := capture(t, func() error { return cmdHeartbeat(context.Background(), c, strings.Fields(args)) })
		if err == nil || !strings.Contains(err.Error(), "--with-credentials относится к команде после --") {
			t.Errorf("%q: %v", args, err)
		}
	}
	if n := len(rec.calls()); n != 0 {
		t.Fatalf("запросов к серверу %d", n)
	}
}

// Ключи окружения сравниваются точно: SDLC_TOKEN_FILE и похожие остаются.
func TestIsolatedEnvExactNames(t *testing.T) {
	got := isolatedEnv([]string{"A=1", "SDLC_TOKEN=x", "SDLC_TOKEN_FILE=y", "CLAB_CONFIG_DIR=/p", "SDLC_ACTOR=z", "SDLC_API_URL=u", "SDLC_PROJECT=P", "X=SDLC_TOKEN=q"}, "/tmp/d")
	want := "A=1 SDLC_TOKEN_FILE=y X=SDLC_TOKEN=q CLAB_CONFIG_DIR=/tmp/d"
	if strings.Join(got, " ") != want {
		t.Fatalf("%v", got)
	}
}
