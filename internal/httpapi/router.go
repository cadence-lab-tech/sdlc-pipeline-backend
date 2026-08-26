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
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/workflow"
)

type Server struct {
	cfg    config.Config
	log    *slog.Logger
	store  *postgres.Store
	engine *workflow.Engine
}

func NewServer(cfg config.Config, log *slog.Logger, store *postgres.Store, engine *workflow.Engine) *Server {
	return &Server{cfg: cfg, log: log, store: store, engine: engine}
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

		v1.GET("/tasks/:taskKey", s.getTask)
		v1.GET("/tasks/:taskKey/timeline", s.listEvents)
		v1.PATCH("/tasks/:taskKey/scope", s.updateScope)

		v1.GET("/tasks/:taskKey/artifacts", s.listArtifacts)
		v1.POST("/tasks/:taskKey/artifacts", s.submitArtifact)

		v1.GET("/tasks/:taskKey/transitions", s.listTransitions)
		v1.POST("/tasks/:taskKey/transitions/:transitionKey", s.executeTransition)
		// Гейт закрывается отдельно от перехода: на переходе их несколько и
		// закрывать их можно в любом порядке и разными людьми.
		v1.POST("/tasks/:taskKey/transitions/:transitionKey/gates/:gateKey/decision", s.decideGate)

		v1.GET("/artifacts/:artifactID/content", s.getArtifactContent)
		v1.POST("/artifacts/:artifactID/approve", s.approveArtifact)
	}

	return r
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
