package oauth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// ParsePublicURL проверяет SDLC_PUBLIC_URL: схема http или https и хост, без
// пути (одна завершающая косая допускается и снимается), запроса,
// фрагмента и учётных данных. Пусто — переменная не задана.
func ParsePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("не разбирается как адрес")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("нужна схема http или https и хост, например https://cadence.example.com")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("нет хоста")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", errors.New("только схема и хост: без логина, запроса и фрагмента")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("только схема и хост, без пути: issuer — адрес инстанса без пути")
	}
	return u.Scheme + "://" + u.Host, nil
}

// RequestBase — публичный адрес из запроса (решение 0079, п. 10): хост из
// Host, схема из X-Forwarded-Proto (только http или https, иное
// игнорируется), без заголовка — схема соединения.
func RequestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Прокси в цепочке дописывают значения через запятую; первое — то, что
	// видел клиент.
	proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	switch strings.ToLower(proto) {
	case "http", "https":
		scheme = strings.ToLower(proto)
	}
	return scheme + "://" + r.Host
}

// InsecureBase — адрес по http на хосте не петли: клиенты требуют https,
// администратору нужно предупреждение.
func InsecureBase(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	return u.Scheme == "http" && !IsLoopbackHost(u.Hostname())
}

// Отказы адреса redirect_uri при регистрации и в документе клиента.
var (
	ErrRedirectScheme = errors.New("redirect_uri: только https или http на петле (localhost, 127.0.0.1, [::1])")
	ErrRedirectForm   = errors.New("redirect_uri: абсолютный адрес без фрагмента и учётных данных")
)

// MaxRedirectURIs и MaxRedirectLen — пределы списка клиента: документ
// клиента и тело регистрации и так ограничены, но строка в базе — нет.
const (
	MaxRedirectURIs = 20
	MaxRedirectLen  = 2000
)

// CheckRedirectURI — адрес годен для регистрации: https или loopback по
// http, абсолютный, без фрагмента и учётных данных.
func CheckRedirectURI(raw string) error {
	if raw == "" || len(raw) > MaxRedirectLen {
		return ErrRedirectForm
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Fragment != "" || u.User != nil {
		return ErrRedirectForm
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if IsLoopbackRedirectHost(u.Hostname()) {
			return nil
		}
	}
	return ErrRedirectScheme
}

// IsLoopbackRedirectHost — хост loopback для сверки без порта: ровно
// localhost, 127.0.0.1 и ::1 (решение 0079, п. 5).
func IsLoopbackRedirectHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// IsLoopbackRedirect — адрес loopback по http.
func IsLoopbackRedirect(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http" && IsLoopbackRedirectHost(u.Hostname())
}

// MatchRedirect сверяет redirect_uri запроса с зарегистрированными:
// побайтно, а loopback по http — без учёта порта (схема, хост, путь и
// запрос совпадают).
func MatchRedirect(registered []string, given string) bool {
	for _, r := range registered {
		if r == given {
			return true
		}
	}
	g, err := url.Parse(given)
	if err != nil || g.Scheme != "http" || !IsLoopbackRedirectHost(g.Hostname()) ||
		g.Fragment != "" || g.User != nil || g.Opaque != "" {
		return false
	}
	for _, raw := range registered {
		r, err := url.Parse(raw)
		if err != nil || r.Scheme != "http" || !IsLoopbackRedirectHost(r.Hostname()) {
			continue
		}
		if r.Hostname() == g.Hostname() && r.EscapedPath() == g.EscapedPath() &&
			r.RawQuery == g.RawQuery && r.ForceQuery == g.ForceQuery {
			return true
		}
	}
	return false
}
