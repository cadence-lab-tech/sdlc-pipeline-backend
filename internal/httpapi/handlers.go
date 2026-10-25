package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// ── проекты ─────────────────────────────────────────────────────────────────

// listProjects отдаёт проекты, доступные актору: в режиме токенов — те, где
// он участник, администратору — все.
func (s *Server) listProjects(c *gin.Context) {
	projects, err := s.auth.VisibleProjects(c.Request.Context(), identity(c))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	// ?repo=owner/name — проекты, к которым привязан репозиторий: так CLI
	// узнаёт проект по git remote текущего каталога.
	if repo := strings.TrimSpace(c.Query("repo")); repo != "" {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" {
			fail(c, s.log, apperr.InvalidInput("repo: ожидается owner/name"))
			return
		}
		ids, err := s.store.ProjectIDsByRepository(c.Request.Context(), s.store.DB(), owner, name)
		if err != nil {
			fail(c, s.log, err)
			return
		}
		filtered := projects[:0]
		for _, p := range projects {
			if _, ok := ids[p.ID]; ok {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

type createProjectRequest struct {
	Key         string  `json:"key" binding:"required"`
	Name        string  `json:"name" binding:"required"`
	Description *string `json:"description"`
	// Команда-владелец. Пусто — проект заводит администратор системы вне
	// команд (решение 0027).
	Team string `json:"team"`
}

func (s *Server) createProject(c *gin.Context) {
	var req createProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	// Право завести проект проверяет Auth: администратор системы или лид
	// команды, которой это право выдано.
	ownerTeam, err := s.auth.OwnerTeamFor(c.Request.Context(), identity(c), req.Team)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	project, err := s.engine.CreateProject(c.Request.Context(), actor(c).ID,
		postgres.CreateProjectInput{
			Key: req.Key, Name: req.Name, Description: req.Description,
			OwnerTeamID: ownerTeam,
		})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, project)
}

// getProject отдаёт проект вместе с репозиториями и определениями свойств
// задач: фронтенду они нужны, чтобы нарисовать форму создания задачи, не зная
// заранее, какие свойства завёл проект.
func (s *Server) getProject(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()

	project, err := s.store.GetProjectByKey(ctx, q, c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	repos, err := s.store.ListRepositories(ctx, q, project.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	props, err := s.store.ListTaskPropertyDefs(ctx, q, project.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	// Действующие циклы с типами задач, которые они ведут: по ним форма
	// создания задачи понимает, какой тип вообще можно завести.
	workflows, err := s.store.ListActiveWorkflows(ctx, q, project.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	// Доступ актора: уровень и роли, которые он может забирать. Считает их
	// тот же Auth, что и захват, — clab whoami только печатает ответ, а
	// эффективные роли зависят от путей через команды и от токена, которых
	// клиент не видит. allowed_roles: null — любые роли.
	id := identity(c)
	level, err := s.auth.ProjectRole(ctx, id, project.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	allowed, err := s.auth.AllowedRoles(ctx, id, project.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"project":            project,
		"repositories":       repos,
		"task_property_defs": props,
		"active_workflows":   workflows,
		"access":             gin.H{"level": level, "allowed_roles": allowed},
	})
}

// getWorkflow отдаёт активный цикл целиком — статусы с ролями и переходы с
// гейтами. Ровно из этого фронтенд рисует доску: колонки берутся отсюда,
// а не из константы в коде.
func (s *Server) getWorkflow(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()

	project, err := s.store.GetProjectByKey(ctx, q, c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	// Цикл выбирается типом задач: доска обычных задач и доска эпиков — это
	// разные наборы статусов.
	wf, err := s.store.GetActiveWorkflowForType(ctx, q, project.ID, c.Query("type"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, wf)
}

// ── задачи ──────────────────────────────────────────────────────────────────

func (s *Server) listTasks(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()

	project, err := s.store.GetProjectByKey(ctx, q, c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	tasks, err := s.store.ListTasks(ctx, q, project.ID, postgres.ListTasksFilter{
		StatusKey: c.Query("status"),
		ParentKey: c.Query("parent"),
		Type:      c.Query("type"),
		External:  c.Query("external"),
		Limit:     limit,
	})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	if s.auth.SummaryOnly(identity(c)) {
		// Список проекта токеном задачи — сводка (решение 0049 п. 5).
		items := make([]gin.H, 0, len(tasks))
		for _, t := range tasks {
			items = append(items, taskSummary(t))
		}
		c.JSON(http.StatusOK, gin.H{"tasks": items})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

type createTaskRequest struct {
	Title       string  `json:"title" binding:"required"`
	Description *string `json:"description"`
	// Ключ родительской задачи и тип: по типу выбирается цикл (решение 0024).
	ParentKey    string          `json:"parent_key"`
	Type         string          `json:"type"`
	Priority     string          `json:"priority"`
	Assignee     string          `json:"assignee"`
	Labels       []string        `json:"labels"`
	Properties   json.RawMessage `json:"properties"`
	Repositories []string        `json:"repositories"`
	// Ключи задач, которые новая блокирует.
	Blocks []string `json:"blocks"`
	// Ссылка на основной трекер: {"jira": {"key", "url"}}. Проверяет движок.
	ExternalRef json.RawMessage `json:"external_ref"`
	// Уровень модели задачи и настройки этапов (решение 0060):
	// {"development": {"model_level": "high"}}. Проверяет движок.
	ModelLevel    *string         `json:"model_level"`
	StageSettings json.RawMessage `json:"stage_settings"`
}

func (s *Server) createTask(c *gin.Context) {
	var req createTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	// Токен задачи заводит только детей своей задачи.
	if err := s.auth.RequireOwnTaskKey(identity(c), req.ParentKey); err != nil {
		fail(c, s.log, err)
		return
	}
	// Уровень актора решает, вправе ли он завести задачу где угодно или
	// только дочернюю к той, которую держит: правило исполняет движок.
	level, err := s.level(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}

	task, err := s.engine.CreateTask(c.Request.Context(), c.Param("projectKey"), actor(c).ID,
		workflow.CreateTaskInput{
			CreateTaskInput: postgres.CreateTaskInput{
				ParentKey:     req.ParentKey,
				TaskType:      req.Type,
				Title:         req.Title,
				Description:   req.Description,
				Priority:      req.Priority,
				Labels:        req.Labels,
				Properties:    req.Properties,
				Repositories:  req.Repositories,
				ExternalRef:   req.ExternalRef,
				ModelLevel:    req.ModelLevel,
				StageSettings: req.StageSettings,
			},
			AssigneeHandle: req.Assignee,
			Blocks:         req.Blocks,
			Level:          level,
		})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, task)
}

// getTask отдаёт карточку задачи целиком: сама задача, история попыток,
// актуальные артефакты и вердикт гейтов по каждому доступному переходу.
// Одним запросом — потому что именно это и есть экран карточки.
func (s *Server) getTask(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()

	task, err := s.store.GetTaskByKey(ctx, q, c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	// У каждого прогона — его учёт (CLAB-264): одним запросом на задачу.
	runs, err := s.store.CardStageRuns(ctx, q, task.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	artifacts, err := s.store.ListArtifacts(ctx, q, task.ID, false)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	transitions, err := s.engine.Preview(ctx, task)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	// Дети, связи и стоимость едут вместе с задачей: карточка эпика — это
	// дерево нарезки со счётом, а не отдельный экран.
	children, err := s.store.ChildTasks(ctx, q, task.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	links, err := s.store.ListTaskLinks(ctx, q, task.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	usage, err := s.store.TaskUsage(ctx, q, task.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	// Предел этапа — соседним полем к transitions (CLAB-221): форма
	// массива переходов не меняется, старые клиенты поля не видят.
	limit, err := s.engine.StageLimit(ctx, task)
	if err != nil {
		fail(c, s.log, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"task":        task,
		"stage_runs":  runs,
		"artifacts":   artifacts,
		"transitions": transitions,
		"stage_limit": limit,
		"children":    children,
		"links":       links,
		"usage":       usage,
	})
}

type linkTasksRequest struct {
	Task string `json:"task" binding:"required"`
	Rel  string `json:"rel"`
	Note string `json:"note"`
	// true — снять связь, а не завести.
	Remove bool `json:"remove"`
}

func (s *Server) linkTasks(c *gin.Context) {
	var req linkTasksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	if req.Rel == "" {
		req.Rel = domain.TaskLinkBlocks
	}
	// Токен задачи связывает только внутри круга своей задачи (CLAB-208).
	if err := s.auth.RequireLink(c.Request.Context(), identity(c), c.Param("taskKey"), req.Task, req.Rel); err != nil {
		fail(c, s.log, err)
		return
	}
	links, err := s.engine.LinkTasks(c.Request.Context(), c.Param("taskKey"), req.Task,
		req.Rel, req.Note, actor(c).ID, req.Remove)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"links": links})
}

type assignRequest struct {
	// Пустой хэндл снимает назначение.
	Assignee string `json:"assignee"`
}

func (s *Server) assignTask(c *gin.Context) {
	var req assignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	if err := s.auth.RequireWriteCircle(c.Request.Context(), identity(c), c.Param("taskKey"), "назначение"); err != nil {
		fail(c, s.log, err)
		return
	}
	task, err := s.engine.Assign(c.Request.Context(), c.Param("taskKey"), req.Assignee, actor(c).ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

type reportUsageRequest struct {
	Model            string          `json:"model" binding:"required"`
	InputTokens      *int64          `json:"input_tokens"`
	OutputTokens     *int64          `json:"output_tokens"`
	CacheReadTokens  *int64          `json:"cache_read_tokens"`
	CacheWriteTokens *int64          `json:"cache_write_tokens"`
	CostUSD          *float64        `json:"cost_usd"`
	DurationMS       *int64          `json:"duration_ms"`
	Details          json.RawMessage `json:"details"`
	// api или subscription; пусто — не указано (решение 0062). Значение
	// проверяет движок.
	Billing string `json:"billing"`
}

// reportUsage принимает отчёт о стоимости прогона. Сюда ходит раннер после
// того, как запущенный им агент закончил работу (решение 0026).
func (s *Server) reportUsage(c *gin.Context) {
	var req reportUsageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}

	in := workflow.ReportUsageInput{
		Model: req.Model, InputTokens: req.InputTokens, OutputTokens: req.OutputTokens,
		CacheReadTokens: req.CacheReadTokens, CacheWriteTokens: req.CacheWriteTokens,
		CostUSD: req.CostUSD, DurationMS: req.DurationMS, Details: req.Details,
		Billing: req.Billing,
	}
	if raw := c.Param("runID"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			fail(c, s.log, apperr.InvalidInput("идентификатор прогона не UUID"))
			return
		}
		in.StageRunID = &id
	}
	// Токен задачи отчитывается только о своём прогоне.
	runID, err := s.auth.OwnRun(identity(c), c.Param("taskKey"), in.StageRunID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	in.StageRunID = runID

	out, err := s.engine.ReportUsage(c.Request.Context(), c.Param("taskKey"), actor(c).ID, in)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, out)
}

func (s *Server) taskUsage(c *gin.Context) {
	usage, err := s.engine.TaskUsage(c.Request.Context(), c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, usage)
}

func (s *Server) listTransitions(c *gin.Context) {
	task, err := s.store.GetTaskByKey(c.Request.Context(), s.store.DB(), c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	transitions, err := s.engine.Preview(c.Request.Context(), task)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	limit, err := s.engine.StageLimit(c.Request.Context(), task)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"transitions": transitions, "stage_limit": limit})
}

type executeTransitionRequest struct {
	Comment string `json:"comment"`
}

// executeTransition двигает задачу. Заблокированный гейтами переход — это 409
// с разбором по каждому гейту, а не 500: для агента это штатный ответ
// "доделай вот это", и по нему он понимает, что именно.
func (s *Server) executeTransition(c *gin.Context) {
	var req executeTransitionRequest
	_ = c.ShouldBindJSON(&req)
	if err := s.ownTask(c); err != nil {
		fail(c, s.log, err)
		return
	}

	res, err := s.engine.ExecuteBound(c.Request.Context(),
		c.Param("taskKey"), c.Param("transitionKey"), actor(c).ID, s.auth.Bound(identity(c)), req.Comment)
	if err != nil {
		fail(c, s.log, err)
		return
	}

	if res.Blocked {
		fail(c, s.log, apperr.New(apperr.CodeGatesNotSatisfied,
			"переход %q заблокирован гейтами", c.Param("transitionKey")).
			WithDetails(gin.H{"gates": res.Gates}))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"task":         res.Task,
		"gates":        res.Gates,
		"stage_run":    res.NewRun,
		"previous_run": res.FromRun,
	})
}

type decideGateRequest struct {
	Status  string          `json:"status" binding:"required"`
	Details json.RawMessage `json:"details"`
}

func (s *Server) decideGate(c *gin.Context) {
	var req decideGateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	if err := workflow.ValidateDecision(req.Status); err != nil {
		fail(c, s.log, err)
		return
	}
	if len(req.Details) == 0 {
		req.Details = json.RawMessage(`{}`)
	}

	out, err := s.engine.DecideGate(c.Request.Context(),
		c.Param("taskKey"), c.Param("transitionKey"), c.Param("gateKey"),
		actor(c).ID, s.auth.Bound(identity(c)), req.Status, req.Details)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

type updateTaskRequest struct {
	// nil — поле не трогать; пустое описание очищает его.
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Priority    *string   `json:"priority"`
	Labels      *[]string `json:"labels"`
	// Ссылка на основной трекер целиком; {} снимает её.
	ExternalRef *json.RawMessage `json:"external_ref"`
	// Уровень модели задачи: "" снимает. Настройки этапов сливаются с
	// прежними по ключам статуса и полям этапа (решение 0060).
	ModelLevel    *string          `json:"model_level"`
	StageSettings *json.RawMessage `json:"stage_settings"`
}

// updateTask меняет постановку задачи. Право проверяет движок: у агента она
// та же, что у заведения задач — своя задача и её дети (решение 0024).
func (s *Server) updateTask(c *gin.Context) {
	var req updateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	if err := s.auth.RequireWriteCircle(c.Request.Context(), identity(c), c.Param("taskKey"), "правка постановки"); err != nil {
		fail(c, s.log, err)
		return
	}
	level, err := s.level(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}

	task, err := s.engine.UpdateTask(c.Request.Context(), c.Param("taskKey"), actor(c).ID,
		workflow.UpdateTaskInput{
			Title:         req.Title,
			Description:   req.Description,
			Priority:      req.Priority,
			Labels:        req.Labels,
			ExternalRef:   req.ExternalRef,
			ModelLevel:    req.ModelLevel,
			StageSettings: req.StageSettings,
			Level:         level,
		})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

type updateScopeRequest struct {
	AddRepositories    []string        `json:"add_repositories"`
	RemoveRepositories []string        `json:"remove_repositories"`
	Properties         json.RawMessage `json:"properties"`
	Note               string          `json:"note"`
}

// updateScope расширяет или сужает область задачи. Уровень ручки — agent:
// расширять область агент вправе, а сузить её может только сопровождающий —
// это правило движка, транспорт лишь передаёт уровень.
func (s *Server) updateScope(c *gin.Context) {
	var req updateScopeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	level, err := s.level(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	task, err := s.engine.UpdateScope(c.Request.Context(), c.Param("taskKey"), actor(c).ID,
		workflow.UpdateScopeInput{
			AddRepositories:    req.AddRepositories,
			RemoveRepositories: req.RemoveRepositories,
			Properties:         req.Properties,
			Note:               req.Note,
			Level:              level,
		})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (s *Server) listEvents(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()

	task, err := s.store.GetTaskByKey(ctx, q, c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	// ?status=<ключ статуса> — события прогонов этого статуса, включая
	// вложенные перепроверки; предел считается после фильтра, незнакомый
	// ключ даёт пустую ленту (CLAB-283).
	events, err := s.store.ListEventsByStatus(ctx, q, task.ID, c.Query("status"), limit)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}

// ── артефакты ───────────────────────────────────────────────────────────────

func (s *Server) listArtifacts(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()

	task, err := s.store.GetTaskByKey(ctx, q, c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	includeSuperseded := c.Query("include_superseded") == "true"
	artifacts, err := s.store.ListArtifacts(ctx, q, task.ID, includeSuperseded)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"artifacts": artifacts})
}

type submitArtifactRequest struct {
	Kind         string          `json:"kind" binding:"required"`
	Title        string          `json:"title" binding:"required"`
	Format       string          `json:"format"`
	Content      string          `json:"content" binding:"required"`
	MediaType    string          `json:"media_type"`
	SupersedesID *uuid.UUID      `json:"supersedes_id"`
	Meta         json.RawMessage `json:"meta"`
}

// submitArtifact принимает и форму из UI, и расширенную форму агента со
// ссылками и зоной в коде: ручка одна, потому что сдача результата одна.
func (s *Server) submitArtifact(c *gin.Context) {
	var req agentArtifactRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}

	in := agentSubmitInput(req)
	// Токен задачи сдаёт только в свой прогон.
	runID, err := s.auth.OwnRun(identity(c), c.Param("taskKey"), in.StageRunID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	in.StageRunID = runID
	artifact, err := s.engine.SubmitArtifact(c.Request.Context(), c.Param("taskKey"),
		actor(c).ID, in)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, artifact)
}

func (s *Server) getArtifactContent(c *gin.Context) {
	id, err := uuid.Parse(c.Param("artifactID"))
	if err != nil {
		fail(c, s.log, apperr.InvalidInput("идентификатор артефакта не UUID"))
		return
	}

	ctx, q := c.Request.Context(), s.store.DB()
	artifact, err := s.store.GetArtifact(ctx, q, id)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	content, mediaType, err := s.store.BlobContent(ctx, q, artifact.BlobSHA)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Data(http.StatusOK, mediaType, content)
}

func (s *Server) approveArtifact(c *gin.Context) {
	id, err := uuid.Parse(c.Param("artifactID"))
	if err != nil {
		fail(c, s.log, apperr.InvalidInput("идентификатор артефакта не UUID"))
		return
	}

	var out domain.Artifact
	err = s.store.InTx(c.Request.Context(), func(tx pgx.Tx) error {
		if err := s.store.ApproveArtifact(c.Request.Context(), tx, id, actor(c).ID); err != nil {
			return err
		}
		out, err = s.store.GetArtifact(c.Request.Context(), tx, id)
		return err
	})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, out)
}
