package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Когда предупреждать (CLAB-87, критерий 10; CLAB-148, критерий 1): сервер
// новее CLI на любой выпуск — предупреждение; CLI новее сервера на одну
// минорную — нормальный случай; dev и SHA не сравниваются.
func TestVersionSkew(t *testing.T) {
	cases := []struct {
		cli, server string
		want        skew
	}{
		// Сервер новее: на патч, на минорную, на несколько, на мажорную.
		{"v0.5.1", "v0.5.2", skewCLIOlder},
		{"v0.5.3", "v0.6.0", skewCLIOlder},
		{"v0.3.4", "v0.5.0", skewCLIOlder},
		{"v0.4.9", "v0.5.0", skewCLIOlder},
		{"v0.9.9", "v1.0.0", skewCLIOlder},
		// CLI новее: в пределах одной минорной тихо, дальше — как раньше.
		{"v0.6.0", "v0.5.2", skewNone},
		{"v0.5.3", "v0.5.2", skewNone},
		{"v0.7.0", "v0.5.2", skewCLINewer},
		{"v1.5.0", "v0.5.0", skewCLINewer},
		{"v0.5.2", "v0.5.2", skewNone},
		{"v0.5.0", "v0.5.0", skewNone},
		{"dev", "v0.5.0", skewNone},
		{"v0.3.0", "dev", skewNone},
		{"v0.3.0", "8424c71", skewNone},
		{"8424c71", "v0.5.0", skewNone},
		// Суффикс после «-» отбрасывается, без «v» — не выпуск.
		{"v0.3.0", "v0.5.0-rc.1", skewCLIOlder},
		{"0.3.0", "v0.5.0", skewNone},
		{"v0.5.0", "0.3.0", skewNone},
	}
	for _, tc := range cases {
		if got := versionSkew(tc.cli, tc.server); got != tc.want {
			t.Errorf("CLI %s, сервер %s: %v, ждали %v", tc.cli, tc.server, got, tc.want)
		}
	}
}

// withExecutable подменяет путь к бинарю clab (уже после EvalSymlinks).
func withExecutable(t *testing.T, path string) {
	t.Helper()
	prev := executablePath
	executablePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { executablePath = prev })
}

// Критерий 4: подсказка по способу установки. Бинарь из Cellar — brew
// upgrade clab и рядом строка с CLAB_VERSION; иначе — install.sh тапа с
// версией сервера. CLI новее сервера — только install.sh: brew назад не
// откатит.
func TestSkewHintByInstall(t *testing.T) {
	install := "curl -fsSL https://raw.githubusercontent.com/cadence-lab-tech/homebrew-tap/main/install.sh | CLAB_VERSION=v0.6.1 sh"
	cases := []struct {
		name, exe     string
		brew, install bool
	}{
		{"brew на Apple Silicon", "/opt/homebrew/Cellar/clab/0.6.0/bin/clab", true, true},
		{"brew на Linux", "/home/linuxbrew/.linuxbrew/Cellar/clab/0.6.0/bin/clab", true, true},
		{"install.sh", "/usr/local/bin/clab", false, true},
		{"рядом с Cellar, но не формула", "/opt/homebrew/Cellar/clabber/bin/clab", false, true},
	}
	for _, tc := range cases {
		withExecutable(t, tc.exe)
		got := skewWarning(skewCLIOlder, "v0.6.0", "v0.6.1", "https://cadence.example", installedByBrew())
		if strings.Contains(got, "brew upgrade clab") != tc.brew {
			t.Errorf("%s: brew upgrade clab ждали %v: %q", tc.name, tc.brew, got)
		}
		if strings.Contains(got, install) != tc.install {
			t.Errorf("%s: команда с CLAB_VERSION ждали %v: %q", tc.name, tc.install, got)
		}
		first := strings.SplitN(got, "\n", 2)[0]
		for _, w := range []string{"v0.6.0", "v0.6.1", "https://cadence.example", "старше сервера"} {
			if !strings.Contains(first, w) {
				t.Errorf("%s: в первой строке нет %q: %q", tc.name, w, first)
			}
		}
	}

	withExecutable(t, "/opt/homebrew/Cellar/clab/0.7.0/bin/clab")
	newer := skewWarning(skewCLINewer, "v0.7.0", "v0.5.2", "https://cadence.example", false)
	if strings.Contains(newer, "brew upgrade") || !strings.Contains(newer, "CLAB_VERSION=v0.5.2 sh") || !strings.Contains(newer, "новее сервера") {
		t.Errorf("CLI новее сервера: %q", newer)
	}
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

// instanceStub — сервер версии serverVersion: заголовок в каждом ответе,
// /v1/instance и карточка задачи. Считает запросы.
func instanceStub(t *testing.T, serverVersion string) (*Client, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set(headerServerVersion, serverVersion)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/instance":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version": serverVersion, "tenancy": "single", "project_creation": "teams",
				"login": map[string]any{"methods": []string{"token"}},
			})
		default:
			_, _ = w.Write([]byte(`{"task":{"key":"SDLC-1","title":"t","status":"new"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: srv.URL, actor: "user:alice", http: srv.Client()}, &calls
}

// Критерий 8: clab version печатает обе версии и адрес; --json отдаёт
// version CLI, как раньше, и server.version.
func TestVersionShowsServer(t *testing.T) {
	withVersion(t, "v0.5.1")
	c, _ := instanceStub(t, "v0.5.0")

	withJSON(t, false)
	out, errOut, err := capture(t, func() error { return cmdVersion(context.Background(), c, nil) })
	if err != nil || errOut != "" {
		t.Fatalf("clab version: %v, stderr %q", err, errOut)
	}
	if !strings.Contains(out, "clab v0.5.1") || !strings.Contains(out, "v0.5.0") || !strings.Contains(out, c.origin) {
		t.Fatalf("вывод clab version: %q", out)
	}

	withJSON(t, true)
	out, _, err = capture(t, func() error { return cmdVersion(context.Background(), c, nil) })
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Version string `json:"version"`
		Server  struct {
			Version string `json:"version"`
			URL     string `json:"url"`
		} `json:"server"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("--json не JSON: %q", out)
	}
	if v.Version != "v0.5.1" || v.Server.Version != "v0.5.0" || v.Server.URL != c.origin {
		t.Fatalf("--json: %+v", v)
	}
}

// Критерий 9: сервер недоступен — версия CLI напечатана, о сервере сказано,
// ошибки нет (код выхода 0).
func TestVersionServerDown(t *testing.T) {
	withVersion(t, "v0.5.1")
	withJSON(t, false)
	srv := httptest.NewServer(http.NotFoundHandler())
	origin := srv.URL
	srv.Close()
	c := &Client{base: origin + "/v1", origin: origin, actor: "user:alice", http: &http.Client{}}

	out, _, err := capture(t, func() error { return cmdVersion(context.Background(), c, nil) })
	if err != nil {
		t.Fatalf("clab version при недоступном сервере вернул ошибку: %v", err)
	}
	if !strings.Contains(out, "clab v0.5.1") || !strings.Contains(out, "недоступен") {
		t.Fatalf("вывод: %q", out)
	}
}

// Критерии 11, 12: команда из нескольких запросов к серверу v0.5.0 из CLI
// v0.3.0 предупреждает в stderr ровно один раз готовой командой установки;
// stdout тот же, что без расхождения.
func TestSkewWarnsOncePerCommand(t *testing.T) {
	withJSON(t, true)
	run := func(cliVersion string) (string, string) {
		withVersion(t, cliVersion)
		c, calls := instanceStub(t, "v0.5.0")
		out, errOut, err := capture(t, func() error {
			if err := cmdShow(context.Background(), c, []string{"SDLC-1"}); err != nil {
				return err
			}
			if _, err := c.getRaw(context.Background(), "/tasks/SDLC-1/state"); err != nil {
				return err
			}
			return cmdShow(context.Background(), c, []string{"SDLC-1"})
		})
		if err != nil {
			t.Fatal(err)
		}
		if *calls != 3 {
			t.Fatalf("запросов %d, ждали 3: сверка не должна ходить на сервер сама", *calls)
		}
		return out, errOut
	}

	withExecutable(t, "/usr/local/bin/clab")
	out, errOut := run("v0.3.0")
	want := "curl -fsSL https://raw.githubusercontent.com/cadence-lab-tech/homebrew-tap/main/install.sh | CLAB_VERSION=v0.5.0 sh"
	if strings.Count(errOut, want) != 1 || strings.Count(errOut, "старше сервера") != 1 {
		t.Fatalf("предупреждение должно быть ровно одно, с командой установки: %q", errOut)
	}
	if !strings.Contains(errOut, "v0.3.0") {
		t.Fatalf("в предупреждении нет версии CLI: %q", errOut)
	}

	same, quiet := run("v0.5.0")
	if quiet != "" {
		t.Fatalf("без расхождения stderr пуст, а там %q", quiet)
	}
	if out != same {
		t.Fatalf("stdout зависит от предупреждения:\n%q\n%q", out, same)
	}
}

// Наш инстанс собран по SHA: CLI выпуска не предупреждает (критерий 14 в
// части CLI).
func TestNoWarningAgainstSHABuild(t *testing.T) {
	withJSON(t, true)
	withVersion(t, "v0.4.0")
	c, _ := instanceStub(t, "8424c71")
	_, errOut, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"SDLC-1"}) })
	if err != nil || errOut != "" {
		t.Fatalf("против сборки по SHA: %v, stderr %q", err, errOut)
	}
}

// CLAB-148, критерии 2, 3 и 5: сервер новее CLI на патч. clab version и
// команда чтения из нескольких запросов предупреждают в stderr ровно один
// раз — с обеими версиями, адресом и командой обновления; stdout, --json и
// ошибка те же, что у CLI той же версии, что сервер.
func TestServerNewerWarns(t *testing.T) {
	withExecutable(t, "/opt/homebrew/Cellar/clab/0.6.0/bin/clab")
	run := func(cliVersion string, json bool, cmd func(*Client) error) (string, string, string) {
		withVersion(t, cliVersion)
		withJSON(t, json)
		c, _ := instanceStub(t, "v0.6.1")
		out, errOut, err := capture(t, func() error { return cmd(c) })
		if err != nil {
			t.Fatalf("CLI %s: %v", cliVersion, err)
		}
		return out, errOut, c.origin
	}
	version := func(c *Client) error { return cmdVersion(context.Background(), c, nil) }
	// Два запроса к серверу, один JSON в stdout.
	show := func(c *Client) error {
		if _, err := c.getRaw(context.Background(), "/tasks/SDLC-1/state"); err != nil {
			return err
		}
		return cmdShow(context.Background(), c, []string{"SDLC-1"})
	}

	for _, tc := range []struct {
		name string
		json bool
		cmd  func(*Client) error
	}{
		{"clab version", false, version},
		{"clab version --json", true, version},
		{"clab show --json, два запроса", true, show},
	} {
		out, errOut, origin := run("v0.6.0", tc.json, tc.cmd)
		if strings.Count(errOut, "старше сервера") != 1 || strings.Count(errOut, "brew upgrade clab") != 1 ||
			strings.Count(errOut, "CLAB_VERSION=v0.6.1 sh") != 1 {
			t.Fatalf("%s: предупреждение должно быть ровно одно, с brew upgrade и CLAB_VERSION: %q", tc.name, errOut)
		}
		first := strings.SplitN(errOut, "\n", 2)[0]
		if !strings.Contains(first, "v0.6.0") || !strings.Contains(first, "v0.6.1") || !strings.Contains(first, origin) {
			t.Fatalf("%s: в строке предупреждения нет версий или адреса: %q", tc.name, first)
		}
		if strings.Contains(out, "старше сервера") {
			t.Fatalf("%s: предупреждение попало в stdout: %q", tc.name, out)
		}
		if tc.json && !jsonValid(out) {
			t.Fatalf("%s: stdout не JSON: %q", tc.name, out)
		}

		if strings.Contains(tc.name, "version") {
			// clab version печатает обе версии, как без предупреждения.
			if !strings.Contains(out, "v0.6.0") || !strings.Contains(out, "v0.6.1") {
				t.Fatalf("%s: в stdout нет обеих версий: %q", tc.name, out)
			}
			continue
		}
		// Команда чтения: stdout тот же, что у CLI версии сервера, где
		// предупреждения нет.
		same, quiet, _ := run("v0.6.1", tc.json, tc.cmd)
		if quiet != "" {
			t.Fatalf("%s: версии совпадают, а stderr %q", tc.name, quiet)
		}
		if out != same {
			t.Fatalf("%s: stdout зависит от предупреждения:\n%q\n%q", tc.name, out, same)
		}
	}
}

func jsonValid(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}
