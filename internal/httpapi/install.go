package httpapi

import (
	_ "embed"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

// Скрипт установки CLI. Сервис отдаёт его сам, чтобы коллеге хватило одной
// строки: curl -fsSL https://<адрес>/install.sh | sh. Адрес, с которого
// скрипт взяли, подставляется в подсказку про clab auth login — свой
// публичный адрес сервис иначе не знает.
//
//go:embed install.sh
var installScript string

// Сюда попадает хост из запроса, поэтому в подсказку допускаются только
// символы имени хоста и порта: иное ломало бы shell-строку у того, кто
// сам же и прислал такой заголовок, но проверить дешевле, чем объяснять.
var hostChars = regexp.MustCompile(`^[A-Za-z0-9.\-:\[\]]+$`)

func (s *Server) installScript(c *gin.Context) {
	body := strings.ReplaceAll(installScript, "__CLAB_HOST__", publicOrigin(c.Request))
	c.Header("Cache-Control", "no-cache")
	c.Data(http.StatusOK, "text/x-shellscript; charset=utf-8", []byte(body))
}

// publicOrigin — адрес, по которому клиент пришёл: за Caddy это заголовки
// X-Forwarded-*, без него — сам запрос. Пустая строка, если хост
// подозрительный; скрипт тогда печатает подсказку без адреса.
func publicOrigin(r *http.Request) string {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if host == "" || !hostChars.MatchString(host) {
		return ""
	}
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme != "https" && scheme != "http" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	return scheme + "://" + host
}
