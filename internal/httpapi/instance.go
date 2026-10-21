package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/metrics"
)

// HeaderVersion — версия сборки сервера в каждом ответе. По ней CLI сверяет
// себя с сервером на первом же ответе, без отдельного запроса на каждую
// команду (CLAB-87).
const HeaderVersion = "X-Clab-Version"

// WithVersion задаёт версию сборки: её подставляет cmd/api из -X main.version.
func (s *Server) WithVersion(v string) *Server {
	s.version = v
	return s
}

// WithMetrics подключает метрики запросов и вебхуков. Сами метрики отдаёт
// отдельный слушатель (metrics.Start), а не этот роутер: основной порт
// смотрит в интернет.
func (s *Server) WithMetrics(m *metrics.API) *Server {
	s.ops = m
	s.audit.OnFailure(m.AuditWriteFailed)
	return s
}

// WithTracing подключает серверный спан на запрос (CLAB-204); nil —
// трассировки выключены, и слой пропускает запрос как есть.
func (s *Server) WithTracing(tp trace.TracerProvider) *Server {
	s.tracer = tp
	return s
}

func (s *Server) buildVersion() string {
	if s.version == "" {
		return "dev"
	}
	return s.version
}

func serverVersion(v string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header(HeaderVersion, v)
		c.Next()
	}
}

// instance отдаёт сведения об инстансе. Ручка открытая в любом режиме
// аутентификации: её спрашивают до входа.
func (s *Server) instance(c *gin.Context) {
	c.JSON(http.StatusOK, s.auth.Instance(c.Request.Context(), s.buildVersion()))
}
