// Package httpapi — REST-транспорт над движком цикла.
//
// Ручки не содержат логики: они разбирают запрос, зовут движок или хранилище и
// сериализуют ответ. Так MCP-сервер в фазе 2 сможет встать на те же сервисы,
// не переписывая правила (решение 0011).
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/config"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/mcp"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/workflow"
)

type Server struct {
	cfg    config.Config
	log    *slog.Logger
	store  *postgres.Store
	engine *workflow.Engine
	// Редактирование ролей и цикла отделено от движка: движок исполняет цикл,
	// authoring его собирает, и правила иммутабельности живут там.
	authoring *workflow.Authoring
	// nil, если MCP выключен настройкой.
	mcp *mcp.Server
}

func NewServer(cfg config.Config, log *slog.Logger, store *postgres.Store, engine *workflow.Engine, authoring *workflow.Authoring, mcpServer *mcp.Server) *Server {
	return &Server{
		cfg: cfg, log: log, store: store, engine: engine,
		authoring: authoring, mcp: mcpServer,
	}
}

func (s *Server) Handler() http.Handler {
	if s.cfg.Env != "development" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(requestID(), requestLog(s.log), recovery(s.log))

	// Пробы вне группы с резолвом актора: kubelet не присылает заголовков и не
	// должен заводить принципала на каждый опрос.
	r.GET("/healthz", s.healthz)
	r.GET("/readyz", s.readyz)

	v1 := r.Group("/v1", resolveActor(s.store, s.log, s.cfg.DefaultActor))
	{
		v1.GET("/projects", s.listProjects)
		v1.POST("/projects", s.createProject)
		v1.GET("/projects/:projectKey", s.getProject)
		v1.GET("/projects/:projectKey/workflow", s.getWorkflow)
		v1.GET("/projects/:projectKey/tasks", s.listTasks)
		v1.POST("/projects/:projectKey/tasks", s.createTask)

		// Роли. Версия роли иммутабельна: правка инструкции — новая версия,
		// потому что пройденные этапы ссылаются на ту, по которой работали.
		v1.GET("/projects/:projectKey/roles", s.listRoles)
		v1.POST("/projects/:projectKey/roles", s.createRole)
		v1.GET("/projects/:projectKey/roles/:roleKey", s.getRole)
		v1.PATCH("/projects/:projectKey/roles/:roleKey", s.updateRole)
		v1.DELETE("/projects/:projectKey/roles/:roleKey", s.archiveRole)
		v1.POST("/projects/:projectKey/roles/:roleKey/restore", s.restoreRole)
		v1.GET("/projects/:projectKey/roles/:roleKey/versions", s.listRoleVersions)
		v1.POST("/projects/:projectKey/roles/:roleKey/versions", s.createRoleVersion)
		v1.GET("/projects/:projectKey/roles/:roleKey/versions/:version", s.getRoleVersion)

		// Версии цикла. Редактируется только черновик; активация переключает
		// проект на него целиком.
		v1.GET("/projects/:projectKey/workflows", s.listWorkflows)
		v1.POST("/projects/:projectKey/workflows", s.createWorkflow)

		v1.GET("/tasks/:taskKey", s.getTask)
		v1.GET("/tasks/:taskKey/timeline", s.listEvents)
		v1.PATCH("/tasks/:taskKey/scope", s.updateScope)

		// Агентская часть: те же сервисы, что у MCP-тулов. Сюда ходят CLI и
		// CI, локальный агент — в /mcp.
		v1.GET("/projects/:projectKey/queue", s.listQueue)
		v1.GET("/tasks/:taskKey/state", s.getTaskState)
		v1.POST("/tasks/:taskKey/claim", s.claimTask)
		v1.POST("/tasks/:taskKey/heartbeat", s.heartbeat)
		v1.POST("/tasks/:taskKey/release", s.releaseTask)
		v1.GET("/tasks/:taskKey/repo-refs", s.listRepoRefs)
		v1.POST("/tasks/:taskKey/repo-refs", s.linkRepoRef)
		v1.GET("/tasks/:taskKey/drift", s.getDrift)
		v1.POST("/drift-signals/:signalID/resolve", s.resolveDrift)

		v1.GET("/tasks/:taskKey/artifacts", s.listArtifacts)
		v1.POST("/tasks/:taskKey/artifacts", s.submitArtifact)

		v1.GET("/tasks/:taskKey/transitions", s.listTransitions)
		v1.POST("/tasks/:taskKey/transitions/:transitionKey", s.executeTransition)
		// Гейт закрывается отдельно от перехода: на переходе их несколько и
		// закрывать их можно в любом порядке и разными людьми.
		v1.POST("/tasks/:taskKey/transitions/:transitionKey/gates/:gateKey/decision", s.decideGate)

		v1.GET("/workflows/:workflowID", s.getWorkflowVersion)
		v1.DELETE("/workflows/:workflowID", s.deleteWorkflow)
		v1.GET("/workflows/:workflowID/validation", s.validateWorkflow)
		v1.POST("/workflows/:workflowID/activate", s.activateWorkflow)

		v1.POST("/workflows/:workflowID/statuses", s.createStatus)
		// Порядок колонок меняется одним запросом целиком: уникальность
		// (workflow_id, position) не переживает перестановку по одному.
		v1.PUT("/workflows/:workflowID/statuses", s.reorderStatuses)
		v1.PATCH("/workflows/:workflowID/statuses/:statusKey", s.updateStatus)
		v1.DELETE("/workflows/:workflowID/statuses/:statusKey", s.deleteStatus)
		v1.PUT("/workflows/:workflowID/statuses/:statusKey/roles", s.setStatusRoles)

		v1.POST("/workflows/:workflowID/transitions", s.createTransition)
		v1.PATCH("/workflows/:workflowID/transitions/:transitionKey", s.updateTransition)
		v1.DELETE("/workflows/:workflowID/transitions/:transitionKey", s.deleteTransition)
		v1.POST("/workflows/:workflowID/transitions/:transitionKey/gates", s.createGate)
		v1.PATCH("/workflows/:workflowID/transitions/:transitionKey/gates/:gateKey", s.updateGate)
		v1.DELETE("/workflows/:workflowID/transitions/:transitionKey/gates/:gateKey", s.deleteGate)

		v1.GET("/artifacts/:artifactID/content", s.getArtifactContent)
		v1.POST("/artifacts/:artifactID/approve", s.approveArtifact)
	}

	// MCP-сервер живёт в том же процессе и на тех же сервисах: REST и MCP —
	// два транспорта над одним ядром (решение 0011). Привязка к проекту — в
	// пути, поэтому агент физически не может обратиться к чужому проекту.
	if s.mcp != nil {
		handler := s.mcp.Handler()
		group := r.Group("/mcp/projects/:projectKey",
			resolveActorAs(s.store, s.log, s.cfg.DefaultActor, domain.PrincipalAgent))
		group.Any("", s.serveMCP(handler))
		group.Any("/*rest", s.serveMCP(handler))
	}

	return r
}

// serveMCP кладёт проект и актора в контекст запроса: тулы читают их оттуда,
// потому что MCP-сессия stateless и своего состояния не держит.
func (s *Server) serveMCP(h http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := mcp.WithProject(c.Request.Context(), c.Param("projectKey"))
		ctx = mcp.WithActor(ctx, actor(c))
		h.ServeHTTP(c.Writer, c.Request.WithContext(ctx))
	}
}

func (s *Server) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz проверяет базу: без неё сервис жив, но бесполезен, и балансировщик
// не должен слать на него трафик.
func (s *Server) readyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "degraded", "database": "unreachable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
}
