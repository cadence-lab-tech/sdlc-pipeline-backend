package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestInstallScript(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/install.sh", (&Server{}).installScript)

	get := func(headers map[string]string) string {
		req := httptest.NewRequest(http.MethodGet, "http://api:8080/install.sh", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("код %d", w.Code)
		}
		return w.Body.String()
	}

	// За Caddy: адрес из X-Forwarded-*, плейсхолдера в теле не остаётся.
	body := get(map[string]string{"X-Forwarded-Host": "sdlc.example.ru", "X-Forwarded-Proto": "https"})
	if !strings.HasPrefix(body, "#!/bin/sh") {
		t.Fatalf("не shell-скрипт: %q", body[:20])
	}
	if !strings.Contains(body, `host="https://sdlc.example.ru"`) || strings.Contains(body, "__CLAB_HOST__") {
		t.Fatalf("адрес не подставлен")
	}

	// Без прокси: хост запроса и http.
	body = get(nil)
	if !strings.Contains(body, `host="http://api:8080"`) {
		t.Fatalf("без прокси ожидали хост запроса")
	}

	// Подозрительный хост в подсказку не попадает.
	body = get(map[string]string{"X-Forwarded-Host": `evil"; rm -rf /`})
	if !strings.Contains(body, `host=""`) {
		t.Fatalf("хост с кавычками должен быть отброшен")
	}
}
