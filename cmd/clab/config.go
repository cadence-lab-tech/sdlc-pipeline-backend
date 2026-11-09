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

// errNoConfigDir — конфигу негде жить: ни CLAB_CONFIG_DIR, ни
// XDG_CONFIG_HOME, ни домашнего каталога. Запасного «./.clab» нет (решение
// 0077, п. 6): токен в текущем каталоге оказался бы в репозитории.
var errNoConfigDir = errors.New("нет домашнего каталога: задайте CLAB_CONFIG_DIR")

// configDir — $CLAB_CONFIG_DIR, иначе $XDG_CONFIG_HOME/clab, иначе
// ~/.config/clab: та же договорённость, что у gh. Без всех трёх — ошибка
// errNoConfigDir.
func configDir() (string, error) {
	if dir := os.Getenv("CLAB_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "clab"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errNoConfigDir
	}
	return filepath.Join(home, ".config", "clab"), nil
}

func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// configPathLabel — путь конфига для сообщений: без каталога конфига вместо
// пути говорится, почему его нет.
func configPathLabel() string {
	p, err := configPath()
	if err != nil {
		return err.Error()
	}
	return p
}

// loadConfig читает конфиг; отсутствие файла — не ошибка, а пустой конфиг.
// Без каталога конфига — тоже пустой конфиг: clab с SDLC_API_URL и
// SDLC_TOKEN и clab version работают и без домашнего каталога, а запись
// (saveConfig) откажет.
func loadConfig() (Config, error) {
	cfg := Config{Hosts: map[string]HostConfig{}}
	path, err := configPath()
	if err != nil {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("конфиг %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("конфиг %s не разбирается: %w", path, err)
	}
	if cfg.Hosts == nil {
		cfg.Hosts = map[string]HostConfig{}
	}
	return cfg, nil
}

// saveConfig пишет конфиг с правами только для владельца: в нём токен.
func saveConfig(cfg Config) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	return saveConfigAt(dir, cfg)
}

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

// Откуда взят хост: те же слова — в host_source у clab whoami --json.
const (
	hostFromFlag    = "flag"
	hostFromEnv     = "env"
	hostFromMarker  = "marker"
	hostFromDefault = "default_host"
)

// hostChoice — выбранный хост: ключ, адрес интерфейса, запись конфига и
// источник. marker — маркер каталога, если он участвовал в выборе (хост из
// него или маркер без хоста перед default_host).
type hostChoice struct {
	host, origin string
	hc           HostConfig
	found        bool
	source       string
	marker       *dirMarker
}

// chooseHost — порядок выбора хоста (решение 0077, п. 6): --host,
// SDLC_API_URL, host маркера каталога, default_host. marker — источник
// маркера; nil — маркер не читается (CLAB_CONFIG_DIR). Хост маркера
// возвращается и без записи в конфиге (found=false): API-командам это
// ошибка «войдите», auth login — адрес входа. Адрес хоста маркера с
// записью — из записи: токен уходит только туда, куда входили.
//
// strict — битый маркер ошибка (auth login, logout, token без --host);
// иначе он хоста не задаёт, а его ошибку скажет выбор проекта.
func chooseHost(cfg Config, g globals, marker func() (*dirMarker, error), strict bool) (hostChoice, error) {
	var ch hostChoice
	var err error
	switch {
	case g.host != "":
		ch.source = hostFromFlag
		ch.host, ch.origin, err = normalizeHost(g.host)
	case os.Getenv("SDLC_API_URL") != "":
		ch.source = hostFromEnv
		ch.host, ch.origin, err = normalizeHost(os.Getenv("SDLC_API_URL"))
	default:
		var mk *dirMarker
		if marker != nil {
			var mErr error
			if mk, mErr = marker(); mErr != nil && strict {
				return hostChoice{}, fmt.Errorf("%w; хост можно назвать явно: --host <адрес>", mErr)
			}
		}
		ch.marker = mk
		if mk != nil && mk.Host != "" {
			ch.source, ch.host, ch.origin = hostFromMarker, mk.Host, mk.Origin
			if hc, ok := cfg.Hosts[mk.Host]; ok {
				ch.hc, ch.found = hc, true
				if hc.URL != "" {
					ch.origin = hc.URL
				}
			}
			return ch, nil
		}
		if cfg.DefaultHost == "" {
			return ch, nil
		}
		ch.source, ch.host = hostFromDefault, cfg.DefaultHost
		if ch.hc, ch.found = cfg.Hosts[ch.host]; ch.found {
			ch.origin = ch.hc.URL
		}
		return ch, nil
	}
	if err != nil {
		return hostChoice{}, err
	}
	ch.hc, ch.found = cfg.Hosts[ch.host]
	if ch.found && ch.origin == "" {
		ch.origin = ch.hc.URL
	}
	return ch, nil
}

// resolveHost — какой хост имеется в виду без маркера каталога: --host,
// SDLC_API_URL, хост по умолчанию из конфига. Возвращает ключ хоста, адрес
// интерфейса и запись конфига, если она есть.
func resolveHost(cfg Config, g globals) (host, origin string, hc HostConfig, found bool, err error) {
	ch, err := chooseHost(cfg, g, nil, false)
	if err != nil {
		return "", "", HostConfig{}, false, err
	}
	return ch.host, ch.origin, ch.hc, ch.found, nil
}

// loginHint — подсказка входа на хост.
func loginHint(origin string) string {
	return fmt.Sprintf("Войдите на %s: clab auth login --host %s", origin, origin)
}

// markerHostError — хост маркера без записи в конфиге: запрос не уходит, на
// default_host clab молча не переключается.
func markerHostError(mk *dirMarker) error {
	return fmt.Errorf("в маркере %s хост %s, входа на него нет. %s", mk.Path, mk.Origin, loginHint(mk.Origin))
}
