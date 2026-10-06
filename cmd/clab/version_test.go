package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Критерий 10: когда предупреждать. CLI новее сервера на одну минорную —
// нормальный случай; dev и SHA не сравниваются.
func TestVersionSkew(t *testing.T) {
	cases := []struct {
		cli, server string
		warn        bool
	}{
		{"v0.3.4", "v0.5.0", true},
		{"v0.4.9", "v0.5.0", false},
		{"v0.6.0", "v0.5.2", false},
		{"v0.7.0", "v0.5.2", true},
		{"v1.5.0", "v0.5.0", true},
		{"dev", "v0.5.0", false},
		{"v0.3.0", "dev", false},
		{"v0.3.0", "8424c71", false},
		// Суффикс после «-» отбрасывается, без «v» — не выпуск.
		{"v0.3.0", "v0.5.0-rc.1", true},
		{"0.3.0", "v0.5.0", false},
		{"v0.5.0", "v0.5.0", false},
	}
	for _, tc := range cases {
		if got := versionSkew(tc.cli, tc.server); got != tc.warn {
			t.Errorf("CLI %s, сервер %s: предупреждение %v, ждали %v", tc.cli, tc.server, got, tc.warn)
		}
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

	out, errOut := run("v0.3.0")
	want := "curl -fsSL https://raw.githubusercontent.com/cadence-lab-tech/homebrew-tap/main/install.sh | CLAB_VERSION=v0.5.0 sh"
	if strings.Count(errOut, want) != 1 || strings.Count(errOut, "расходится") != 1 {
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
