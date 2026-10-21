package httpapi

import (
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/metrics"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

const (
	ctxRequestID = "request_id"
	ctxIdentity  = "identity"
	// Код apperr ответа с ошибкой и токен, отбитый аутентификацией, — для
	// журнала действий (audit.go).
	ctxErrorCode = "error_code"
	ctxRejected  = "rejected_identity"

	// ActorHeader несёт хэндл действующего лица в режиме без аутентификации
	// (SDLC_AUTH_MODE=header). В режиме токенов игнорируется: кто пришёл,
	// определяет токен, а не то, что клиент написал о себе.
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

// requestMetrics учитывает запрос по шаблону маршрута (c.FullPath), а не по
// пути: в пути ключи проектов и задач, им не место в метках. Стоит до
// recovery, чтобы паника, превращённая в 500, тоже попала в счёт.
func requestMetrics(m *metrics.API) gin.HandlerFunc {
	return func(c *gin.Context) {
		if m == nil {
			c.Next()
			return
		}
		start := time.Now()
		c.Next()
		m.Request(c.Request.Method, c.FullPath(), c.Writer.Status(), time.Since(start))
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

// authenticate определяет, кто пришёл.
//
// Предъявленный токен (Authorization: Bearer) проверяется всегда. Без токена
// поведение зависит от режима: в режиме заголовка актор берётся из
// X-SDLC-Actor и заводится в principals при первом обращении, как в фазах
// 1-3; в режиме токенов запрос без токена отбивается.
func (s *Server) authenticate(defaultKind domain.PrincipalKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		if bearer, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer "); ok {
			id, err := s.auth.Authenticate(c.Request.Context(), bearer)
			if err != nil {
				// Отозванный или истёкший токен журнал пишет со ссылкой на
				// него: отказ — главный сигнал, и важно, чей он.
				if rejected, ok := s.auth.RejectedIdentity(c.Request.Context(), bearer); ok {
					c.Set(ctxRejected, rejected)
				}
				fail(c, s.log, err)
				return
			}
			c.Set(ctxIdentity, id)
			c.Next()
			return
		}

		if s.auth.Enforced() {
			fail(c, s.log, apperr.Unauthenticated("нужен токен: заголовок Authorization: Bearer <token>"))
			return
		}

		raw := strings.TrimSpace(c.GetHeader(ActorHeader))
		if raw == "" {
			raw = s.cfg.DefaultActor
		}
		kind, handle := workflow.SplitHandle(raw, defaultKind)
		if handle == "" {
			handle = s.cfg.DefaultActor
		}
		p, err := s.store.ResolveActor(c.Request.Context(), s.store.DB(), handle, kind)
		if err != nil {
			fail(c, s.log, err)
			return
		}
		c.Set(ctxIdentity, workflow.Identity{Principal: p})
		c.Next()
	}
}

func identity(c *gin.Context) workflow.Identity {
	v, _ := c.Get(ctxIdentity)
	id, _ := v.(workflow.Identity)
	return id
}

func actor(c *gin.Context) domain.Principal {
	return identity(c).Principal
}

// level — действующий уровень актора в проекте из пути. Нужен ручкам, за
// которыми стоит правило движка, зависящее от уровня (например, «агент
// заводит только детей своей задачи»).
func (s *Server) level(c *gin.Context) (domain.MemberRole, error) {
	// Проект берётся из пути так же, как у guard: правку постановки просят по
	// ключу задачи, и ключа проекта в таком пути нет вовсе. В режиме header
	// прав нет, и лишнего запроса к базе тоже.
	if !s.auth.Enforced() {
		return s.auth.Level(c.Request.Context(), identity(c), uuid.Nil)
	}
	projectID, err := s.projectOf(c)
	if err != nil {
		return "", err
	}
	return s.auth.Level(c.Request.Context(), identity(c), projectID)
}

// guard проверяет уровень прав в проекте, к которому относится запрос.
//
// Проект вычисляется из пути: ключ проекта, ключ задачи, версия цикла,
// артефакт или сигнал дрейфа — что есть. Ручки при этом остаются без
// логики прав: правило «кому что можно» живёт в workflow.Auth и одинаково
// для REST и MCP.
func (s *Server) guard(need domain.MemberRole) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.auth.Enforced() {
			c.Next()
			return
		}
		projectID, err := s.projectOf(c)
		if err != nil {
			fail(c, s.log, err)
			return
		}
		if err := s.auth.Require(c.Request.Context(), identity(c), projectID, need); err != nil {
			fail(c, s.log, err)
			return
		}
		c.Next()
	}
}

// requireAdmin — действия администратора системы.
func (s *Server) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := s.auth.RequireAdmin(identity(c)); err != nil {
			fail(c, s.log, err)
			return
		}
		c.Next()
	}
}

func (s *Server) projectOf(c *gin.Context) (uuid.UUID, error) {
	ctx, q := c.Request.Context(), s.store.DB()
	switch {
	case c.Param("projectKey") != "":
		p, err := s.store.GetProjectByKey(ctx, q, c.Param("projectKey"))
		return p.ID, err
	case c.Param("taskKey") != "":
		t, err := s.store.GetTaskByKey(ctx, q, c.Param("taskKey"))
		return t.ProjectID, err
	case c.Param("workflowID") != "":
		id, err := uuid.Parse(c.Param("workflowID"))
		if err != nil {
			return uuid.Nil, apperr.InvalidInput("идентификатор версии цикла не UUID")
		}
		wf, err := s.store.WorkflowHead(ctx, q, id)
		return wf.ProjectID, err
	case c.Param("artifactID") != "":
		id, err := uuid.Parse(c.Param("artifactID"))
		if err != nil {
			return uuid.Nil, apperr.InvalidInput("идентификатор артефакта не UUID")
		}
		a, err := s.store.GetArtifact(ctx, q, id)
		return a.ProjectID, err
	case c.Param("signalID") != "":
		id, err := uuid.Parse(c.Param("signalID"))
		if err != nil {
			return uuid.Nil, apperr.InvalidInput("идентификатор сигнала не UUID")
		}
		sig, err := s.store.GetDriftSignal(ctx, q, id)
		if err != nil {
			return uuid.Nil, err
		}
		t, err := s.store.GetTaskByID(ctx, q, sig.TaskID)
		return t.ProjectID, err
	}
	return uuid.Nil, apperr.Internal(nil, "маршрут %s не привязан к проекту", c.FullPath())
}
