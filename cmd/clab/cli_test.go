package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	cases := map[string][2]string{
		"sdlc.example.com":             {"sdlc.example.com", "https://sdlc.example.com"},
		"https://sdlc.example.com/":    {"sdlc.example.com", "https://sdlc.example.com"},
		"https://sdlc.example.com/v1":  {"sdlc.example.com", "https://sdlc.example.com"},
		"http://localhost:8088/v1":     {"localhost:8088", "http://localhost:8088"},
		"localhost:8088":               {"localhost:8088", "http://localhost:8088"},
		"https://example.com/clab/v1/": {"example.com", "https://example.com/clab"},
	}
	for raw, want := range cases {
		host, origin, err := normalizeHost(raw)
		if err != nil || host != want[0] || origin != want[1] {
			t.Errorf("%s: %s %s %v", raw, host, origin, err)
		}
	}
}

func TestSplitGlobals(t *testing.T) {
	g, rest := splitGlobals([]string{"--host", "x", "tasks", "--json", "--status", "new", "--host=y"})
	if !g.json || g.host != "y" {
		t.Fatalf("глобальные флаги: %+v", g)
	}
	if len(rest) != 3 || rest[0] != "tasks" || rest[1] != "--status" || rest[2] != "new" {
		t.Fatalf("остаток: %v", rest)
	}
}

// Конфиг пишется с правами владельца и читается обратно; окружение и --host
// перекрывают хост по умолчанию.
func TestConfigRoundTripAndResolve(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAB_CONFIG_DIR", dir)
	t.Setenv("SDLC_API_URL", "")

	cfg, err := loadConfig()
	if err != nil || len(cfg.Hosts) != 0 {
		t.Fatalf("пустой конфиг: %+v %v", cfg, err)
	}
	cfg.Hosts["sdlc.example.com"] = HostConfig{URL: "https://sdlc.example.com", Token: "sdlc_x", Handle: "user:alice"}
	cfg.DefaultHost = "sdlc.example.com"
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("права конфига: %v %v", info.Mode(), err)
	}
	cfg, err = loadConfig()
	if err != nil || cfg.Hosts["sdlc.example.com"].Token != "sdlc_x" {
		t.Fatalf("конфиг после чтения: %+v %v", cfg, err)
	}

	host, origin, hc, found, err := resolveHost(cfg, globals{})
	if err != nil || host != "sdlc.example.com" || origin != "https://sdlc.example.com" || !found || hc.Handle != "user:alice" {
		t.Fatalf("хост по умолчанию: %s %s %v %v", host, origin, found, err)
	}
	host, origin, _, found, err = resolveHost(cfg, globals{host: "other.example.com"})
	if err != nil || host != "other.example.com" || origin != "https://other.example.com" || found {
		t.Fatalf("--host: %s %s %v %v", host, origin, found, err)
	}
	t.Setenv("SDLC_API_URL", "https://sdlc.example.com/v1")
	host, origin, _, found, err = resolveHost(cfg, globals{})
	if err != nil || host != "sdlc.example.com" || origin != "https://sdlc.example.com" || !found {
		t.Fatalf("SDLC_API_URL: %s %s %v %v", host, origin, found, err)
	}

	c, err := NewClient(globals{})
	if err != nil || c.base != "https://sdlc.example.com/v1" || c.token != "sdlc_x" {
		t.Fatalf("клиент из конфига: %+v %v", c, err)
	}
}
