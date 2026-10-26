package main

// Конфиг CLI (решение 0032): куда ходить и чем представляться. Живёт в
// ~/.config/clab/config.json, по хосту на запись — так один clab работает и
// с продом, и с локальной сборкой. Переменные окружения SDLC_API_URL и
// SDLC_TOKEN перекрывают конфиг: в CI они остаются единственным способом.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	// Хост, с которым работает clab без --host.
	DefaultHost string                `json:"default_host,omitempty"`
	Hosts       map[string]HostConfig `json:"hosts"`
}

type HostConfig struct {
	// Адрес интерфейса: https://sdlc.example.com. API — под /v1, MCP — под
	// /mcp; оба выводятся отсюда, как и адрес страницы подтверждения.
	URL     string `json:"url"`
	Token   string `json:"token,omitempty"`
	TokenID string `json:"token_id,omitempty"`
	// Кем вошли: user:alice. Для «clab auth status» и подсказок.
	Handle string `json:"handle,omitempty"`
}

// configDir — $CLAB_CONFIG_DIR, иначе $XDG_CONFIG_HOME/clab, иначе
// ~/.config/clab: та же договорённость, что у gh.
func configDir() string {
	if dir := os.Getenv("CLAB_CONFIG_DIR"); dir != "" {
		return dir
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "clab")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".clab")
	}
	return filepath.Join(home, ".config", "clab")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

// loadConfig читает конфиг; отсутствие файла — не ошибка, а пустой конфиг.
func loadConfig() (Config, error) {
	cfg := Config{Hosts: map[string]HostConfig{}}
	raw, err := os.ReadFile(configPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("конфиг %s: %w", configPath(), err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("конфиг %s не разбирается: %w", configPath(), err)
	}
	if cfg.Hosts == nil {
		cfg.Hosts = map[string]HostConfig{}
	}
	return cfg, nil
}

// saveConfig пишет конфиг с правами только для владельца: в нём токен.
func saveConfig(cfg Config) error { return saveConfigAt(configDir(), cfg) }

// saveConfigAt — то же в заданный каталог: так clab token task --config-dir
// пишет конфиг исполнителя этапа, не трогая конфиг человека.
func saveConfigAt(dir string, cfg Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("каталог конфига: %w", err)
	}
	path := filepath.Join(dir, "config.json")
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("запись конфига: %w", err)
	}
	return os.Rename(tmp, path)
}

// normalizeHost принимает «sdlc.example.com», «https://sdlc.example.com» или
// «http://127.0.0.1:8080/v1» и возвращает ключ хоста и адрес интерфейса без
// хвоста /v1.
func normalizeHost(raw string) (host, origin string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("пустой адрес")
	}
	if !strings.Contains(raw, "://") {
		if strings.HasPrefix(raw, "localhost") || strings.HasPrefix(raw, "127.0.0.1") {
			raw = "http://" + raw
		} else {
			raw = "https://" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("адрес %q не разбирается", raw)
	}
	path := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	origin = u.Scheme + "://" + u.Host + path
	return u.Host, origin, nil
}

// globals — флаги, общие для всех команд. Разбираются до имени команды и в
// любом месте после него: «clab tasks --json» и «clab --json tasks» — одно.
type globals struct {
	host string
	json bool
}

func splitGlobals(args []string) (globals, []string) {
	var g globals
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			// Дальше — чужая команда (clab heartbeat K -- cmd): её --json и
			// --host не наши.
			return g, append(rest, args[i:]...)
		case a == "--json":
			g.json = true
		case a == "--host" && i+1 < len(args):
			g.host = args[i+1]
			i++
		case strings.HasPrefix(a, "--host="):
			g.host = strings.TrimPrefix(a, "--host=")
		default:
			rest = append(rest, a)
		}
	}
	return g, rest
}

// resolveHost — какой хост имеется в виду: --host, SDLC_API_URL, хост по
// умолчанию из конфига. Возвращает ключ хоста, адрес интерфейса и запись
// конфига, если она есть.
func resolveHost(cfg Config, g globals) (host, origin string, hc HostConfig, found bool, err error) {
	switch {
	case g.host != "":
		host, origin, err = normalizeHost(g.host)
	case os.Getenv("SDLC_API_URL") != "":
		host, origin, err = normalizeHost(os.Getenv("SDLC_API_URL"))
	case cfg.DefaultHost != "":
		host = cfg.DefaultHost
		if hc, found = cfg.Hosts[host]; found {
			origin = hc.URL
		}
	default:
		return "", "", HostConfig{}, false, nil
	}
	if err != nil {
		return "", "", HostConfig{}, false, err
	}
	if !found {
		hc, found = cfg.Hosts[host]
		if found && origin == "" {
			origin = hc.URL
		}
	}
	return host, origin, hc, found, nil
}
