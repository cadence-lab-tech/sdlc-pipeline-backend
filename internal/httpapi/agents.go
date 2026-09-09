package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Ручки агентской части. Те же сервисы, что и у MCP-тулов: CLI и CI ходят
// сюда, локальный агент — в MCP, правила одни (решение 0011).

type claimRequest struct {
	SessionRef string `json:"session_ref"`
}

func (s *Server) claimTask(c *gin.Context) {
	var req claimRequest
	_ = c.ShouldBindJSON(&req)

	projectID, err := s.projectOf(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	allowed, err := s.auth.AllowedRoles(c.Request.Context(), identity(c), projectID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	res, err := s.engine.ClaimWith(c.Request.Context(), c.Param("taskKey"), actor(c).ID, req.SessionRef,
		workflow.ClaimOptions{AllowedRoles: allowed})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

type leaseRequest struct {
	StageRunID *uuid.UUID `json:"stage_run_id"`
	Note       string     `json:"note"`
}

// leaseTarget: прогон из тела запроса либо тот, который держит этот актор.
func (s *Server) leaseTarget(c *gin.Context, req leaseRequest) (uuid.UUID, error) {
	if req.StageRunID != nil {
		return *req.StageRunID, nil
	}
	run, err := s.engine.HeldRun(c.Request.Context(), c.Param("taskKey"), actor(c).ID)
	if err != nil {
		return uuid.Nil, err
	}
	return run.ID, nil
}

func (s *Server) heartbeat(c *gin.Context) {
	var req leaseRequest
	_ = c.ShouldBindJSON(&req)

	runID, err := s.leaseTarget(c, req)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	lease, err := s.engine.Heartbeat(c.Request.Context(), runID, actor(c).ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, lease)
}

func (s *Server) releaseTask(c *gin.Context) {
	var req leaseRequest
	_ = c.ShouldBindJSON(&req)

	runID, err := s.leaseTarget(c, req)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	if err := s.engine.Release(c.Request.Context(), runID, actor(c).ID, req.Note); err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"released": true, "stage_run_id": runID})
}

// getTaskState отдаёт задачу так, как её видит агент: инструкция роли, вход
// от предыдущих этапов, дрейф и требования гейтов впереди.
func (s *Server) getTaskState(c *gin.Context) {
	state, err := s.engine.State(c.Request.Context(), c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, state)
}

// listQueue — очередь работы проекта: незакрытые прогоны с указанием роли,
// которой они адресованы, и того, свободна ли аренда.
func (s *Server) listQueue(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()

	project, err := s.store.GetProjectByKey(ctx, q, c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	runs, err := s.store.ListOpenRuns(ctx, q, project.ID, postgres.OpenRunsFilter{
		StatusKey: c.Query("status"), RoleKey: c.Query("role"),
		Viewer: actor(c).ID, Limit: limit,
	})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"queue": runs})
}

type repoRefRequest struct {
	Repository string `json:"repository" binding:"required"`
	RefType    string `json:"ref_type"`
	Ref        string `json:"ref" binding:"required"`
	URL        string `json:"url"`
	State      string `json:"state"`
}

func (s *Server) linkRepoRef(c *gin.Context) {
	var req repoRefRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	ref, err := s.engine.LinkRepoRef(c.Request.Context(), c.Param("taskKey"), actor(c).ID,
		workflow.LinkRepoRefInput{
			Repository: req.Repository, RefType: req.RefType, Ref: req.Ref,
			URL: req.URL, State: req.State,
		})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, ref)
}

func (s *Server) listRepoRefs(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()

	task, err := s.store.GetTaskByKey(ctx, q, c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	refs, err := s.store.ListRepoRefs(ctx, q, task.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"repo_refs": refs})
}

func (s *Server) getDrift(c *gin.Context) {
	states := []string{"open", "acknowledged"}
	if raw := c.Query("state"); raw != "" {
		states = strings.Split(raw, ",")
	}
	drift, err := s.engine.Drift(c.Request.Context(), c.Param("taskKey"), states)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, drift)
}

type resolveDriftRequest struct {
	Resolution string `json:"resolution" binding:"required"`
	Note       string `json:"note"`
	Artifact   *struct {
		Kind    string `json:"kind"`
		Title   string `json:"title"`
		Format  string `json:"format"`
		Content string `json:"content"`
	} `json:"artifact"`
}

func (s *Server) resolveDrift(c *gin.Context) {
	id, err := uuid.Parse(c.Param("signalID"))
	if err != nil {
		fail(c, s.log, apperr.InvalidInput("идентификатор сигнала не UUID"))
		return
	}
	var req resolveDriftRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}

	in := workflow.ResolveDriftInput{
		SignalID: id, Resolution: req.Resolution, Note: req.Note,
	}
	if req.Artifact != nil {
		title := req.Artifact.Title
		if title == "" {
			title = "Разбор расхождения"
		}
		in.Artifact = &workflow.SubmitArtifactInput{
			Kind: req.Artifact.Kind, Title: title,
			Format: req.Artifact.Format, Content: []byte(req.Artifact.Content),
		}
	}

	signal, err := s.engine.ResolveDrift(c.Request.Context(), actor(c).ID, in)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, signal)
}

// submitArtifactAgent — сдача артефакта со ссылками и зоной в коде.
// Отличается от submitArtifact только тем, что принимает links и code_refs;
// разделены, чтобы форма UI не тащила поля, которых у неё нет.
type agentArtifactRequest struct {
	submitArtifactRequest
	StageRunID *uuid.UUID `json:"stage_run_id"`
	Links      []struct {
		Rel       string `json:"rel"`
		TargetURI string `json:"target_uri"`
		Note      string `json:"note"`
	} `json:"links"`
	CodeRefs []struct {
		Repository string `json:"repository"`
		Path       string `json:"path"`
		SymbolFQN  string `json:"symbol_fqn"`
		StartLine  *int   `json:"start_line"`
		EndLine    *int   `json:"end_line"`
		Commit     string `json:"commit"`
		RefKind    string `json:"ref_kind"`
	} `json:"code_refs"`
}

func agentSubmitInput(req agentArtifactRequest) workflow.SubmitArtifactInput {
	in := workflow.SubmitArtifactInput{
		Kind: req.Kind, Title: req.Title, Format: req.Format,
		Content: []byte(req.Content), MediaType: req.MediaType,
		SupersedesID: req.SupersedesID, Meta: json.RawMessage(req.Meta),
		StageRunID: req.StageRunID,
	}
	for _, l := range req.Links {
		in.Links = append(in.Links, postgres.ArtifactLink{
			Rel: l.Rel, TargetURI: l.TargetURI, Note: l.Note,
		})
	}
	for _, r := range req.CodeRefs {
		in.CodeRefs = append(in.CodeRefs, postgres.ArtifactCodeRef{
			Repository: r.Repository, Path: r.Path, SymbolFQN: r.SymbolFQN,
			StartLine: r.StartLine, EndLine: r.EndLine,
			CommitSHA: r.Commit, RefKind: r.RefKind,
		})
	}
	return in
}
