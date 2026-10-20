package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countingTransport считает запросы и ни одного не выполняет: без хоста
// clab в сеть не ходит вовсе (CLAB-147, критерий 3).
type countingTransport struct{ calls int }

func (t *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, errors.New("запроса быть не должно")
}

// noHostClient — клиент так, как его строит main: пустой конфиг, окружение
// без адреса и токена. config — содержимое config.json, пустое — файла нет.
func noHostClient(t *testing.T, config string) (*Client, *countingTransport) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAB_CONFIG_DIR", dir)
	t.Setenv("SDLC_API_URL", "")
	t.Setenv("SDLC_TOKEN", "")
	t.Setenv("SDLC_PROJECT", "")
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := NewClient(globals{})
	if err != nil {
		t.Fatalf("NewClient без хоста упал: %v", err)
	}
	rt := &countingTransport{}
	c.http = &http.Client{Transport: rt}
	return c, rt
}

func noLocalhost(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, "localhost") || strings.Contains(s, "8088") {
		t.Errorf("%s упоминает dev-адрес: %q", what, s)
	}
}

const noHostHint = "хост не настроен: clab auth login --host"

// Критерии 1-3: version без хоста — ровно версия и подсказка, код 0, без
// запроса; --json — server: null.
func TestVersionWithoutHost(t *testing.T) {
	withVersion(t, "v0.0.0-test")
	c, rt := noHostClient(t, "")
	if c.base != "" || c.origin != "" {
		t.Fatalf("без хоста у клиента адрес: base %q origin %q", c.base, c.origin)
	}

	withJSON(t, false)
	out, errOut, err := capture(t, func() error { return cmdVersion(context.Background(), c, nil) })
	if err != nil || errOut != "" {
		t.Fatalf("clab version: %v, stderr %q", err, errOut)
	}
	want := "clab v0.0.0-test\nхост не настроен: clab auth login --host https://sdlc.example.com\n"
	if out != want {
		t.Fatalf("вывод clab version:\n%q\nвместо\n%q", out, want)
	}
	noLocalhost(t, "clab version", out)

	withJSON(t, true)
	out, _, err = capture(t, func() error { return cmdVersion(context.Background(), c, nil) })
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("--json не JSON: %q", out)
	}
	server, present := v["server"]
	if v["version"] != "v0.0.0-test" || !present || server != nil || len(v) != 2 {
		t.Fatalf("--json: %q", out)
	}
	if rt.calls != 0 {
		t.Fatalf("clab version без хоста сделал %d запросов", rt.calls)
	}
}

// Критерий 4: команды с сервером падают с подсказкой до любого запроса.
func TestServerCommandsWithoutHost(t *testing.T) {
	withJSON(t, false)
	cases := map[string]func(c *Client) error{
		"tasks":  func(c *Client) error { return cmdTasks(context.Background(), c, nil) },
		"whoami": func(c *Client) error { return cmdWhoami(context.Background(), c, nil) },
		"claim":  func(c *Client) error { return cmdClaim(context.Background(), c, []string{"X-1"}) },
		"show":   func(c *Client) error { return cmdShow(context.Background(), c, []string{"X-1"}) },
	}
	for name, run := range cases {
		c, rt := noHostClient(t, "")
		out, _, err := capture(t, func() error { return run(c) })
		if err == nil || !strings.Contains(err.Error(), noHostHint) {
			t.Errorf("clab %s без хоста: %v", name, err)
			continue
		}
		noLocalhost(t, "clab "+name, err.Error())
		if strings.Contains(err.Error(), "connection refused") || out != "" {
			t.Errorf("clab %s: ошибка %q, stdout %q", name, err, out)
		}
		if rt.calls != 0 {
			t.Errorf("clab %s без хоста сделал %d запросов", name, rt.calls)
		}
	}
}

// Критерий 5: mcp с SDLC_PROJECT без хоста не собирает адрес из пустого
// origin и не зовёт claude.
func TestMCPWithoutHost(t *testing.T) {
	withJSON(t, false)
	for _, sub := range []string{"url", "config", "install"} {
		c, rt := noHostClient(t, "")
		t.Setenv("SDLC_PROJECT", "X")
		// claude в PATH, который записал бы вызов: его быть не должно.
		bin := t.TempDir()
		marker := filepath.Join(bin, "called")
		script := "#!/bin/sh\ntouch " + marker + "\n"
		if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)

		out, _, err := capture(t, func() error { return cmdMCP(context.Background(), c, []string{sub}) })
		if err == nil || !strings.Contains(err.Error(), noHostHint) {
			t.Errorf("clab mcp %s без хоста: %v", sub, err)
		}
		if strings.Contains(out, "/mcp/projects/") {
			t.Errorf("clab mcp %s напечатал адрес: %q", sub, out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("clab mcp %s позвал claude", sub)
		}
		if rt.calls != 0 {
			t.Errorf("clab mcp %s сделал %d запросов", sub, rt.calls)
		}
	}
}

// Критерий 6: default_host без записи в hosts — тоже «хост не настроен», с
// именем хоста.
func TestBrokenDefaultHost(t *testing.T) {
	withVersion(t, "v0.0.0-test")
	withJSON(t, false)
	c, rt := noHostClient(t, `{"default_host":"gone.example.com","hosts":{}}`)

	out, _, err := capture(t, func() error { return cmdVersion(context.Background(), c, nil) })
	if err != nil {
		t.Fatalf("clab version: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 || lines[0] != "clab v0.0.0-test" ||
		!strings.HasPrefix(lines[1], noHostHint) || !strings.Contains(lines[1], "gone.example.com") {
		t.Fatalf("вывод clab version: %q", out)
	}
	noLocalhost(t, "clab version", out)

	t.Setenv("SDLC_PROJECT", "X")
	_, _, err = capture(t, func() error { return cmdTasks(context.Background(), c, nil) })
	if err == nil || !strings.HasPrefix(err.Error(), noHostHint) || !strings.Contains(err.Error(), "gone.example.com") {
		t.Fatalf("clab tasks при битом default_host: %v", err)
	}
	if rt.calls != 0 {
		t.Fatalf("при битом default_host сделано %d запросов", rt.calls)
	}
}

// Критерий 7: dev-адрес работает, когда назван явно.
func TestExplicitDevHost(t *testing.T) {
	t.Setenv("CLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("SDLC_TOKEN", "")

	t.Setenv("SDLC_API_URL", "http://localhost:8088/v1")
	c, err := NewClient(globals{})
	if err != nil || c.base != "http://localhost:8088/v1" || c.origin != "http://localhost:8088" || c.requireHost() != nil {
		t.Fatalf("SDLC_API_URL: %+v %v", c, err)
	}

	t.Setenv("SDLC_API_URL", "")
	c, err = NewClient(globals{host: "localhost:8088"})
	if err != nil || c.base != "http://localhost:8088/v1" || c.requireHost() != nil {
		t.Fatalf("--host localhost:8088: %+v %v", c, err)
	}

	// И с ним version идёт на сервер, как раньше.
	withVersion(t, "v0.5.1")
	withJSON(t, false)
	stub, calls := instanceStub(t, "v0.5.1")
	out, _, err := capture(t, func() error { return cmdVersion(context.Background(), stub, nil) })
	if err != nil || *calls != 1 || !strings.Contains(out, "сервер "+stub.origin+" v0.5.1") {
		t.Fatalf("version с хостом: %q, запросов %d, %v", out, *calls, err)
	}
}

// Критерий 9: команды, которым хост не нужен, ведут себя как раньше.
func TestAuthWithoutHost(t *testing.T) {
	withJSON(t, false)
	c, rt := noHostClient(t, "")

	out, _, err := capture(t, func() error { return cmdAuth(context.Background(), c, []string{"status"}) })
	if err != nil || !strings.Contains(out, "Входов нет: clab auth login --host") {
		t.Fatalf("clab auth status: %q %v", out, err)
	}
	_, _, err = capture(t, func() error { return cmdAuth(context.Background(), c, []string{"login"}) })
	if err == nil || !strings.Contains(err.Error(), "куда входить? clab auth login --host") {
		t.Fatalf("clab auth login без --host: %v", err)
	}
	if rt.calls != 0 {
		t.Fatalf("auth без хоста сделал %d запросов", rt.calls)
	}
}
