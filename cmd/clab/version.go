package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Сверка версии CLI с сервером (CLAB-87, CLAB-148).
//
// Сервер кладёт версию сборки в заголовок каждого ответа, и CLI сверяет её
// на первом же ответе: отдельного запроса на каждую команду нет, а «одно
// предупреждение за команду» держит sync.Once клиента. Предупреждение уходит
// в stderr — stdout, --json и код выхода остаются прежними, скрипты не
// ломаются. Переменной, которая его глушит, нет: CLI старше сервера надо
// обновить, а не замолчать.

// headerServerVersion — тот же заголовок, что httpapi.HeaderVersion.
const headerServerVersion = "X-Clab-Version"

// installScript — установка CLI нужной версии из публичного тапа (решение
// 0038): install.sh понимает CLAB_VERSION, архивы всех версий лежат в его
// релизах.
const installScript = "https://raw.githubusercontent.com/cadence-lab-tech/homebrew-tap/main/install.sh"

// semver — версия выпуска vMAJOR.MINOR.PATCH.
type semver struct{ major, minor, patch int }

// parseRelease разбирает только версии выпуска. dev, SHA нашего инстанса и
// всё прочее не сравниваются: иначе инстанс, собранный по SHA, давал бы
// предупреждение каждому. Суффикс после «-» или «+» отбрасывается.
func parseRelease(v string) (semver, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "v") {
		return semver{}, false
	}
	v = v[1:]
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 || p == "" {
			return semver{}, false
		}
		n[i] = x
	}
	return semver{n[0], n[1], n[2]}, true
}

// less — a старше b.
func (a semver) less(b semver) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	return a.patch < b.patch
}

// skew — в какую сторону CLI разошёлся с сервером так, что стоит
// предупредить.
type skew int

const (
	skewNone     skew = iota
	skewCLIOlder      // сервер новее CLI на любой выпуск
	skewCLINewer      // CLI новее сервера больше чем на одну минорную
)

// versionSkew — о чём предупреждать (CLAB-148). Сервер новее CLI на любой
// выпуск, хоть на патч, — CLI пора обновить: после upgrade.sh клиентский clab
// остаётся старым, и молчать об этом нельзя. CLI новее сервера в пределах
// одной минорной — обычный случай: brew ставит последний CLI, а компания
// обновляет сервер не сразу; дальше — предупреждение, как раньше. dev, SHA и
// прочее не сравниваются.
func versionSkew(cli, server string) skew {
	c, ok := parseRelease(cli)
	if !ok {
		return skewNone
	}
	s, ok := parseRelease(server)
	if !ok {
		return skewNone
	}
	if c.less(s) {
		return skewCLIOlder
	}
	if c.major != s.major || c.minor-s.minor > 1 {
		return skewCLINewer
	}
	return skewNone
}

// executablePath — путь к бинарю clab после разрешения симлинков; в тестах
// подменяется. Brew кладёт в PATH симлинк на .../Cellar/clab/<версия>/bin/clab.
var executablePath = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// installedByBrew — clab стоит из формулы тапа.
func installedByBrew() bool {
	p, err := executablePath()
	return err == nil && strings.Contains(filepath.ToSlash(p), "/Cellar/clab/")
}

// installCommand — поставить CLI ровно версии v из тапа.
func installCommand(v string) string {
	return fmt.Sprintf("curl -fsSL %s | CLAB_VERSION=%s sh", installScript, v)
}

// skewWarning — одна строка предупреждения и под ней команды. CLI старше:
// из brew — brew upgrade clab (brew ставит последний выпуск, CLI новее
// сервера — допустимо), рядом команда на ровно версию сервера; иначе —
// install.sh тапа с CLAB_VERSION. CLI новее: brew назад не откатит, поэтому
// только install.sh с версией сервера.
func skewWarning(k skew, cli, server, host string, brew bool) string {
	if k == skewCLIOlder {
		if brew {
			return fmt.Sprintf("clab: CLI %s старше сервера %s (%s), обновите CLI:\n"+
				"  brew upgrade clab\n"+
				"  # или ровно версию сервера: %s\n", cli, server, host, installCommand(server))
		}
		return fmt.Sprintf("clab: CLI %s старше сервера %s (%s), обновите CLI до версии сервера:\n  %s\n",
			cli, server, host, installCommand(server))
	}
	return fmt.Sprintf("clab: CLI %s новее сервера %s (%s) больше чем на одну минорную; поставить CLI версии сервера:\n  %s\n",
		cli, server, host, installCommand(server))
}

// checkServerVersion сверяет версию по первому ответу с заголовком.
func (c *Client) checkServerVersion(resp *http.Response) {
	server := resp.Header.Get(headerServerVersion)
	if server == "" {
		return
	}
	c.versionOnce.Do(func() {
		if k := versionSkew(version, server); k != skewNone {
			fmt.Fprint(os.Stderr, skewWarning(k, version, server, c.origin, k == skewCLIOlder && installedByBrew()))
		}
	})
}

// instanceInfo — то, что clab version берёт из /v1/instance.
type instanceInfo struct {
	Version         string `json:"version"`
	Tenancy         string `json:"tenancy,omitempty"`
	ProjectCreation string `json:"project_creation,omitempty"`
}

// cmdVersion печатает версию CLI и сервера текущего хоста. Сервер
// недоступен — версия CLI всё равно печатается, и команда не падает: её
// спрашивают как раз тогда, когда что-то не работает. Хост не настроен —
// сервера нет вовсе: запроса нет, вместо адреса подсказка, код выхода 0 (на
// это опирается тест формулы тапа).
func cmdVersion(ctx context.Context, c *Client, _ []string) error {
	if noHost := c.requireHost(); noHost != nil {
		return emit(map[string]any{"version": version, "server": nil}, func() error {
			fmt.Println("clab", version)
			fmt.Println(noHost)
			return nil
		})
	}
	server := map[string]any{"url": c.origin}
	var info instanceInfo
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := c.get(qctx, "/instance", nil, &info)
	cancel()
	if err != nil {
		server["error"] = err.Error()
	} else {
		server["version"] = info.Version
		server["tenancy"] = info.Tenancy
		server["project_creation"] = info.ProjectCreation
	}
	return emit(map[string]any{"version": version, "server": server}, func() error {
		fmt.Println("clab", version)
		var apiErr *APIError
		switch {
		case errors.As(err, &apiErr):
			// Сервер ответил, но ручки нет — сборка старше /v1/instance.
			fmt.Printf("сервер %s не сообщил версию: %v\n", c.origin, err)
			return nil
		case err != nil:
			fmt.Printf("сервер %s недоступен: %v\n", c.origin, err)
			return nil
		}
		fmt.Printf("сервер %s %s\n", c.origin, info.Version)
		return nil
	})
}
