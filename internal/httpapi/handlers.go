package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/workflow"
)

// ── проекты ─────────────────────────────────────────────────────────────────

func (s *Server) listProjects(c *gin.Context) {
	projects, err := s.store.ListProjects(c.Request.Context(), s.store.DB())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

type createProjectRequest struct {
	Key         string  `json:"key" binding:"required"`
	Name        string  `json:"name" binding:"required"`
	Description *string `json:"description"`
}

func (s *Server) createProject(c *gin.Context) {
	var req createProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	project, err := s.store.CreateProject(c.Request.Context(), s.store.DB(),
		postgres.CreateProjectInput{
			Key: req.Key, Name: req.Name, Description: req.Description,
			CreatedBy: actor(c).ID,
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

	c.JSON(http.StatusOK, gin.H{
		"project":            project,
		"repositories":       repos,
		"task_property_defs": props,
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
	wf, err := s.store.GetActiveWorkflow(ctx, q, project.ID)
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
		EpicKey:   c.Query("epic"),
		Limit:     limit,
	})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

type createTaskRequest struct {
	Title        string          `json:"title" binding:"required"`
	Description  *string         `json:"description"`
	EpicKey      string          `json:"epic_key"`
	Priority     string          `json:"priority"`
	Labels       []string        `json:"labels"`
	Properties   json.RawMessage `json:"properties"`
	Repositories []string        `json:"repositories"`
}

func (s *Server) createTask(c *gin.Context) {
	var req createTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}

	task, err := s.engine.CreateTask(c.Request.Context(), c.Param("projectKey"), actor(c).ID,
		postgres.CreateTaskInput{
			EpicKey:      req.EpicKey,
			Title:        req.Title,
			Description:  req.Description,
			Priority:     req.Priority,
			Labels:       req.Labels,
			Properties:   req.Properties,
			Repositories: req.Repositories,
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
	runs, err := s.store.ListStageRuns(ctx, q, task.ID)
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

	c.JSON(http.StatusOK, gin.H{
		"task":        task,
		"stage_runs":  runs,
		"artifacts":   artifacts,
		"transitions": transitions,
	})
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
	c.JSON(http.StatusOK, gin.H{"transitions": transitions})
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

	res, err := s.engine.Execute(c.Request.Context(),
		c.Param("taskKey"), c.Param("transitionKey"), actor(c).ID, req.Comment)
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
		actor(c).ID, req.Status, req.Details)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

type updateScopeRequest struct {
	AddRepositories []string        `json:"add_repositories"`
	Properties      json.RawMessage `json:"properties"`
	Note            string          `json:"note"`
}

func (s *Server) updateScope(c *gin.Context) {
	var req updateScopeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	task, err := s.engine.UpdateScope(c.Request.Context(), c.Param("taskKey"), actor(c).ID,
		workflow.UpdateScopeInput{
			AddRepositories: req.AddRepositories,
			Properties:      req.Properties,
			Note:            req.Note,
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
	events, err := s.store.ListEvents(ctx, q, task.ID, limit)
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

	artifact, err := s.engine.SubmitArtifact(c.Request.Context(), c.Param("taskKey"),
		actor(c).ID, agentSubmitInput(req))
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
