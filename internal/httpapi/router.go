// Package httpapi — REST-транспорт над движком цикла.
//
// Ручки не содержат логики: они разбирают запрос, зовут движок или хранилище и
// сериализуют ответ. Так MCP-сервер встаёт на те же сервисы, не переписывая
// правила (решение 0011). Права проверяет middleware guard по уровню участия
// в проекте (решение 0021); правило «кому что можно» живёт в workflow.Auth.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/metrics"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

type Server struct {
	cfg    config.Config
	log    *slog.Logger
	store  *postgres.Store
	engine *workflow.Engine
	// Редактирование ролей и цикла отделено от движка: движок исполняет цикл,
	// authoring его собирает, и правила иммутабельности живут там.
	authoring *workflow.Authoring
	auth      *workflow.Auth
	// nil, если MCP выключен настройкой.
	mcp *mcp.Server
	// Версия сборки (-X main.version в cmd/api); пусто — dev.
	version string
	// Метрики эксплуатации (CLAB-126); nil — выключены, методы nil-безопасны.
	ops *metrics.API
}

func NewServer(cfg config.Config, log *slog.Logger, store *postgres.Store, engine *workflow.Engine, authoring *workflow.Authoring, auth *workflow.Auth, mcpServer *mcp.Server) *Server {
	return &Server{
		cfg: cfg, log: log, store: store, engine: engine,
		authoring: authoring, auth: auth, mcp: mcpServer,
	}
}

func (s *Server) Handler() http.Handler {
	if s.cfg.Env != "development" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(requestID(), requestLog(s.log), requestMetrics(s.ops), recovery(s.log))
	r.Use(serverVersion(s.buildVersion()))

	// Пробы вне группы с резолвом актора: kubelet не присылает заголовков и не
	// должен заводить принципала на каждый опрос.
	r.GET("/healthz", s.healthz)
	r.GET("/readyz", s.readyz)

	// Вебхуки провайдера: подпись вместо токена.
	r.POST("/webhooks/github", s.githubWebhook)

	// Вход через корпоративный IdP — вне группы с аутентификацией: у того,
	// кто входит, токена ещё нет (решение 0027). Доказательством служит
	// ID-токен провайдера, а не свой.
	r.GET("/v1/auth/oidc", s.oidcSettings)
	r.POST("/v1/auth/oidc/login", s.oidcLogin)
	// Вход через GitHub (решение 0030): код авторизации меняет бэкенд —
	// секрет клиента живёт у него.
	r.GET("/v1/auth/github", s.githubSettings)
	r.POST("/v1/auth/github/login", s.githubLogin)
	// Вход CLI через браузер (решение 0032): запрос заводит и опрашивает
	// CLI без токена, подтверждает — вошедший из интерфейса.
	r.POST("/v1/auth/cli", s.cliLoginStart)
	r.POST("/v1/auth/cli/poll", s.cliLoginPoll)
	// Сведения об инстансе (CLAB-87): версия, профиль и способы входа одним
	// открытым запросом — его читают и страница входа, и clab version.
	r.GET("/v1/instance", s.instance)

	viewer := s.guard(domain.MemberViewer)
	agent := s.guard(domain.MemberAgent)
	maintainer := s.guard(domain.MemberMaintainer)
	projectAdmin := s.guard(domain.MemberAdmin)

	v1 := r.Group("/v1", s.authenticate(domain.PrincipalUser))
	{
		// Кто я, токены. Права на выдачу проверяет сам сервис: токен себе
		// может любой, другому — администратор проекта или системы.
		v1.GET("/auth/me", s.me)
		v1.GET("/auth/tokens", s.listTokens)
		v1.POST("/auth/tokens", s.issueToken)
		v1.DELETE("/auth/tokens/:tokenID", s.revokeToken)
		// Выход отзывает предъявленную сессию; выданный руками токен так не
		// отзывается — он отзывается по имени из списка.
		v1.POST("/auth/logout", s.logout)
		v1.GET("/auth/cli/:code", s.cliLoginShow)
		v1.POST("/auth/cli/:code", s.cliLoginDecide)

		// Команды: группы принципалов с правами на проекты (решение 0027).
		// Права разные у каждой ручки — их проверяет сам сервис.
		// Кто заведён в системе: список для страницы администратора.
		v1.GET("/principals", s.listPrincipals)

		v1.GET("/teams", s.listTeams)
		v1.POST("/teams", s.createTeam)
		v1.GET("/teams/:teamKey", s.getTeam)
		v1.PATCH("/teams/:teamKey", s.updateTeam)
		v1.DELETE("/teams/:teamKey", s.deleteTeam)
		v1.POST("/teams/:teamKey/convert", s.convertTeam)
		v1.PUT("/teams/:teamKey/plan", s.setTeamPlan)
		v1.GET("/plans", s.listPlans)
		v1.PUT("/teams/:teamKey/members", s.setTeamMember)
		v1.DELETE("/teams/:teamKey/members/:handle", s.removeTeamMember)
		v1.GET("/teams/:teamKey/idp-groups", s.teamIdPGroups)
		v1.PUT("/teams/:teamKey/idp-groups", s.setTeamIdPGroups)
		// Квоты по стоимости: предел ставит администратор системы, расход
		// видят участники команды.
		v1.GET("/teams/:teamKey/budgets", s.teamBudgets)
		v1.PUT("/teams/:teamKey/budgets", s.setTeamBudget)
		// Установки GitHub App на команде (решение 0034): подключение —
		// возврат от GitHub с кодом повторной авторизации.
		v1.GET("/teams/:teamKey/github/installations", s.teamInstallations)
		v1.POST("/teams/:teamKey/github/connect", s.connectGitHub)
		// Приглашения ссылкой (решение 0035): в команду выдаёт лид, в проект —
		// администратор. Просмотр и приём — по токену в теле, любым вошедшим.
		v1.POST("/teams/:teamKey/invites", s.createTeamInvite)
		v1.GET("/teams/:teamKey/invites", s.listTeamInvites)
		v1.DELETE("/invites/:inviteID", s.revokeInvite)
		v1.POST("/invites/lookup", s.lookupInvite)
		v1.POST("/invites/accept", s.acceptInvite)

		v1.GET("/projects", s.listProjects)
		// Кто может завести проект, решает Auth: любой вошедший при открытом
		// создании, иначе администратор системы или лид команды с правом на
		// это. Guard'а по уровню здесь нет: проверка внутри.
		v1.POST("/projects", s.createProject)
		v1.GET("/projects/:projectKey", viewer, s.getProject)
		// Удаление — уровнем выше правки: оно уносит задачи со всей их
		// историей, индекс репозиториев и доступ к проекту целиком.
		v1.DELETE("/projects/:projectKey", projectAdmin, s.deleteProject)
		v1.GET("/projects/:projectKey/workflow", viewer, s.getWorkflow)
		v1.GET("/projects/:projectKey/tasks", viewer, s.listTasks)
		// Заводить задачи вправе и агент, но только дочерние к той, которую он
		// держит, и внутри её области — правило исполняет движок (решение 0024).
		v1.POST("/projects/:projectKey/tasks", agent, s.createTask)

		v1.GET("/projects/:projectKey/teams", viewer, s.listProjectTeams)
		v1.PUT("/projects/:projectKey/teams", maintainer, s.setProjectTeam)
		v1.DELETE("/projects/:projectKey/teams/:teamKey", maintainer, s.removeProjectTeam)

		// Доступ к GitHub у проекта: установка приложения либо именованный
		// секрет (решение 0028).
		v1.GET("/projects/:projectKey/github", maintainer, s.projectGitHub)
		v1.PUT("/projects/:projectKey/github", maintainer, s.setProjectGitHub)
		v1.GET("/projects/:projectKey/github/check", maintainer, s.checkProjectGitHub)
		v1.POST("/projects/:projectKey/github/repositories", maintainer, s.attachRepositories)

		v1.GET("/projects/:projectKey/members", viewer, s.listMembers)
		v1.PUT("/projects/:projectKey/members", maintainer, s.setMember)
		v1.DELETE("/projects/:projectKey/members/:handle", maintainer, s.removeMember)
		// Гард — admin, как у правила в Auth: приглашение — отложенное
		// добавление участника, а его делает администратор проекта.
		v1.POST("/projects/:projectKey/invites", projectAdmin, s.createProjectInvite)
		v1.GET("/projects/:projectKey/invites", projectAdmin, s.listProjectInvites)

		// Перенос правил: экспорт и импорт связки, каталог репозиториев и
		// документов (решение 0022).
		v1.GET("/projects/:projectKey/export", viewer, s.exportBundle)
		v1.POST("/projects/:projectKey/import", maintainer, s.importBundle)
		v1.POST("/projects/:projectKey/repositories", maintainer, s.addRepository)
		// Отвязка — уровнем выше привязки: привязка обратима, отвязка уносит
		// знание репозитория и документы из него.
		v1.DELETE("/projects/:projectKey/repositories/:owner/:name", projectAdmin, s.removeRepository)
		v1.GET("/projects/:projectKey/documents", viewer, s.listDocuments)
		v1.POST("/projects/:projectKey/documents", maintainer, s.putDocument)
		v1.GET("/projects/:projectKey/documents/:documentID", viewer, s.getDocument)
		v1.GET("/projects/:projectKey/documents/:documentID/content", viewer, s.getDocument)
		v1.DELETE("/projects/:projectKey/documents/:documentID", maintainer, s.deleteDocument)

		v1.GET("/projects/:projectKey/metrics", viewer, s.metrics)

		// Роли. Версия роли иммутабельна: правка инструкции — новая версия,
		// потому что пройденные этапы ссылаются на ту, по которой работали.
		v1.GET("/projects/:projectKey/roles", viewer, s.listRoles)
		v1.POST("/projects/:projectKey/roles", maintainer, s.createRole)
		v1.GET("/projects/:projectKey/roles/:roleKey", viewer, s.getRole)
		v1.PATCH("/projects/:projectKey/roles/:roleKey", maintainer, s.updateRole)
		v1.DELETE("/projects/:projectKey/roles/:roleKey", maintainer, s.archiveRole)
		v1.POST("/projects/:projectKey/roles/:roleKey/restore", maintainer, s.restoreRole)
		v1.GET("/projects/:projectKey/roles/:roleKey/versions", viewer, s.listRoleVersions)
		v1.POST("/projects/:projectKey/roles/:roleKey/versions", maintainer, s.createRoleVersion)
		v1.GET("/projects/:projectKey/roles/:roleKey/versions/:version", viewer, s.getRoleVersion)

		// Версии цикла. Редактируется только черновик; активация переключает
		// проект на него целиком.
		v1.GET("/projects/:projectKey/workflows", viewer, s.listWorkflows)
		v1.POST("/projects/:projectKey/workflows", maintainer, s.createWorkflow)

		v1.GET("/tasks/:taskKey", viewer, s.getTask)
		v1.GET("/tasks/:taskKey/timeline", viewer, s.listEvents)
		// Постановка задачи: заголовок, описание, приоритет, метки, ссылка на
		// основной трекер. Уровень agent — точное право проверяет движок, как
		// и у области.
		v1.PATCH("/tasks/:taskKey", agent, s.updateTask)
		v1.PATCH("/tasks/:taskKey/scope", agent, s.updateScope)
		v1.POST("/tasks/:taskKey/links", agent, s.linkTasks)
		v1.PUT("/tasks/:taskKey/assignee", agent, s.assignTask)
		// Стоимость прогона: самоотчёт агента или раннера (решение 0026).
		v1.GET("/tasks/:taskKey/usage", viewer, s.taskUsage)
		v1.POST("/tasks/:taskKey/usage", agent, s.reportUsage)
		v1.POST("/tasks/:taskKey/runs/:runID/usage", agent, s.reportUsage)

		// Агентская часть: те же сервисы, что у MCP-тулов. Сюда ходят CLI и
		// CI, локальный агент — в /mcp.
		v1.GET("/projects/:projectKey/queue", viewer, s.listQueue)
		// Поиск по знанию проекта: код репозиториев, артефакты задач,
		// проектные документы. Гибридный — вектор плюс полнотекст.
		v1.GET("/projects/:projectKey/search", viewer, s.search)
		v1.POST("/projects/:projectKey/search", viewer, s.search)
		v1.GET("/projects/:projectKey/index", viewer, s.indexStatus)
		// Сводка для оболочки интерфейса (CLAB-79): отметки в меню на каждой
		// странице проекта. Уровень viewer — её зовут все участники; только
		// кэш состояния приложения и счётчик из базы.
		v1.GET("/projects/:projectKey/health", viewer, s.projectHealth)
		v1.GET("/tasks/:taskKey/state", viewer, s.getTaskState)
		v1.POST("/tasks/:taskKey/claim", agent, s.claimTask)
		v1.POST("/tasks/:taskKey/heartbeat", agent, s.heartbeat)
		v1.POST("/tasks/:taskKey/release", agent, s.releaseTask)
		v1.GET("/tasks/:taskKey/repo-refs", viewer, s.listRepoRefs)
		v1.POST("/tasks/:taskKey/repo-refs", agent, s.linkRepoRef)
		v1.GET("/tasks/:taskKey/drift", viewer, s.getDrift)
		v1.POST("/drift-signals/:signalID/resolve", agent, s.resolveDrift)

		v1.GET("/tasks/:taskKey/artifacts", viewer, s.listArtifacts)
		v1.POST("/tasks/:taskKey/artifacts", agent, s.submitArtifact)

		v1.GET("/tasks/:taskKey/transitions", viewer, s.listTransitions)
		v1.POST("/tasks/:taskKey/transitions/:transitionKey", agent, s.executeTransition)
		// Гейт закрывается отдельно от перехода: на переходе их несколько и
		// закрывать их можно в любом порядке и разными людьми. Решение по
		// гейту — за сопровождающим; human_approval движок дополнительно
		// требует человека, а не агента.
		v1.POST("/tasks/:taskKey/transitions/:transitionKey/gates/:gateKey/decision", maintainer, s.decideGate)

		v1.GET("/workflows/:workflowID", viewer, s.getWorkflowVersion)
		// Типы задач правятся только у черновика: у активной версии по ним уже
		// разложены задачи.
		v1.PATCH("/workflows/:workflowID", maintainer, s.updateWorkflowVersion)
		v1.DELETE("/workflows/:workflowID", maintainer, s.deleteWorkflow)
		v1.GET("/workflows/:workflowID/validation", viewer, s.validateWorkflow)
		v1.POST("/workflows/:workflowID/activate", maintainer, s.activateWorkflow)

		v1.POST("/workflows/:workflowID/statuses", maintainer, s.createStatus)
		// Порядок колонок меняется одним запросом целиком: уникальность
		// (workflow_id, position) не переживает перестановку по одному.
		v1.PUT("/workflows/:workflowID/statuses", maintainer, s.reorderStatuses)
		v1.PATCH("/workflows/:workflowID/statuses/:statusKey", maintainer, s.updateStatus)
		v1.DELETE("/workflows/:workflowID/statuses/:statusKey", maintainer, s.deleteStatus)
		v1.PUT("/workflows/:workflowID/statuses/:statusKey/roles", maintainer, s.setStatusRoles)

		v1.POST("/workflows/:workflowID/transitions", maintainer, s.createTransition)
		v1.PATCH("/workflows/:workflowID/transitions/:transitionKey", maintainer, s.updateTransition)
		v1.DELETE("/workflows/:workflowID/transitions/:transitionKey", maintainer, s.deleteTransition)
		v1.POST("/workflows/:workflowID/transitions/:transitionKey/gates", maintainer, s.createGate)
		v1.PATCH("/workflows/:workflowID/transitions/:transitionKey/gates/:gateKey", maintainer, s.updateGate)
		v1.DELETE("/workflows/:workflowID/transitions/:transitionKey/gates/:gateKey", maintainer, s.deleteGate)

		v1.GET("/artifacts/:artifactID/content", viewer, s.getArtifactContent)
		v1.POST("/artifacts/:artifactID/approve", maintainer, s.approveArtifact)
	}

	// MCP-сервер живёт в том же процессе и на тех же сервисах: REST и MCP —
	// два транспорта над одним ядром (решение 0011). Привязка к проекту — в
	// пути, поэтому агент физически не может обратиться к чужому проекту.
	// Уровень прав каждый тул проверяет сам: у них он разный.
	if s.mcp != nil {
		handler := s.mcp.Handler()
		group := r.Group("/mcp/projects/:projectKey", s.authenticate(domain.PrincipalAgent))
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
		ctx = mcp.WithIdentity(ctx, identity(c))
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
