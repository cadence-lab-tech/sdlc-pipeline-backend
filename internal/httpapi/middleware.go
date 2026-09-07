package httpapi

import (
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
)

const (
	ctxRequestID = "request_id"
	ctxActor     = "actor"

	// ActorHeader несёт хэндл действующего лица.
	//
	// Аутентификации в фазе 1 нет сознательно (решение 0010): заголовку никто
	// не мешает соврать. Но все аудит-поля уже ссылаются на principals, а не
	// хранят строки, поэтому включение auth в фазе 2 — это замена вот этого
	// одного middleware, без миграции данных.
	ActorHeader = "X-SDLC-Actor"
)

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(ctxRequestID, id)
		c.Header("X-Request-Id", id)
		c.Next()
	}
}

func requestLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		level := slog.LevelInfo
		if c.Writer.Status() >= 500 {
			level = slog.LevelError
		}
		log.Log(c.Request.Context(), level, "запрос",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", c.GetString(ctxRequestID))
	}
}

func recovery(log *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, recovered any) {
		log.Error("паника в обработчике",
			"panic", recovered,
			"path", c.Request.URL.Path,
			"request_id", c.GetString(ctxRequestID))
		var body errorBody
		body.Error.Code = "internal"
		body.Error.Message = "внутренняя ошибка"
		c.AbortWithStatusJSON(500, body)
	})
}

// resolveActor материализует действующее лицо в principals.
//
// Формат заголовка: "handle" или "agent:handle". Пустой заголовок даёт
// принципала по умолчанию — иначе локальную отладку пришлось бы вести с
// обязательным заголовком в каждом curl.
func resolveActor(store *postgres.Store, log *slog.Logger, defaultActor string) gin.HandlerFunc {
	return resolveActorAs(store, log, defaultActor, domain.PrincipalUser)
}

// resolveActorAs отличается только видом принципала по умолчанию: в REST за
// заголовком стоит человек, в MCP — агент. Явный префикс в заголовке
// перекрывает и то, и другое.
func resolveActorAs(store *postgres.Store, log *slog.Logger, defaultActor string, defaultKind domain.PrincipalKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(ActorHeader))
		if raw == "" {
			raw = defaultActor
		}

		kind := defaultKind
		handle := raw
		if before, after, found := strings.Cut(raw, ":"); found {
			switch domain.PrincipalKind(before) {
			case domain.PrincipalAgent:
				kind, handle = domain.PrincipalAgent, after
			case domain.PrincipalUser:
				kind, handle = domain.PrincipalUser, after
			}
		}
		if handle == "" {
			handle = defaultActor
		}

		p, err := store.ResolveActor(c.Request.Context(), store.DB(), handle, kind)
		if err != nil {
			fail(c, log, err)
			return
		}
		c.Set(ctxActor, p)
		c.Next()
	}
}

func actor(c *gin.Context) domain.Principal {
	p, _ := c.Get(ctxActor)
	principal, _ := p.(domain.Principal)
	return principal
}
