package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Сверка версии CLI с сервером (CLAB-87).
//
// Сервер кладёт версию сборки в заголовок каждого ответа, и CLI сверяет её
// на первом же ответе: отдельного запроса на каждую команду нет, а «одно
// предупреждение за команду» держит sync.Once клиента. Предупреждение уходит
// в stderr — stdout, --json и код выхода остаются прежними, скрипты не
// ломаются.

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

// versionSkew — расходятся ли CLI и сервер настолько, что стоит предупредить:
// другая мажорная или больше одной минорной в любую сторону. CLI новее
// сервера на одну минорную — обычный случай: brew ставит последний CLI, а
// компания обновляет сервер не сразу.
func versionSkew(cli, server string) bool {
	c, ok := parseRelease(cli)
	if !ok {
		return false
	}
	s, ok := parseRelease(server)
	if !ok {
		return false
	}
	if c.major != s.major {
		return true
	}
	d := c.minor - s.minor
	return d > 1 || d < -1
}

func skewWarning(cli, server, host string) string {
	return fmt.Sprintf("clab: версия CLI %s расходится с сервером %s (%s) больше чем на одну минорную.\n"+
		"Поставить CLI версии сервера:\n  curl -fsSL %s | CLAB_VERSION=%s sh\n",
		cli, host, server, installScript, server)
}

// checkServerVersion сверяет версию по первому ответу с заголовком.
func (c *Client) checkServerVersion(resp *http.Response) {
	server := resp.Header.Get(headerServerVersion)
	if server == "" {
		return
	}
	c.versionOnce.Do(func() {
		if versionSkew(version, server) {
			fmt.Fprint(os.Stderr, skewWarning(version, server, c.origin))
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
// спрашивают как раз тогда, когда что-то не работает.
func cmdVersion(ctx context.Context, c *Client, _ []string) error {
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
