package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Настройка агентов проекта (CLAB-425, решение 0077 п. 4.6). Порог маршрутов
// — viewer и на чтение, и на запись: кто правит (человек уровня maintainer,
// не бот даже с admin), решает движок по классу актора, с причиной level или
// human_action в details.reason. Те же методы зовёт MCP. В журнал действий
// документ из тела идёт размером и sha256 (workflow.auditDigestActions), а
// «было → стало» — из принятых версий (ctxAuditChange).

func (s *Server) agentSettings(c *gin.Context) {
	view, err := s.engine.AgentSettings(c.Request.Context(), c.Param("projectKey"), nil)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	if view == nil {
		c.JSON(http.StatusOK, gin.H{"version": nil})
		return
	}
	c.JSON(http.StatusOK, view)
}

func (s *Server) agentSettingsVersions(c *gin.Context) {
	list, err := s.engine.AgentSettingsVersions(c.Request.Context(), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"versions": list})
}

func (s *Server) agentSettingsVersion(c *gin.Context) {
	n, err := strconv.Atoi(c.Param("version"))
	if err != nil || n < 1 {
		fail(c, s.log, apperr.InvalidInput("номер версии — целое больше нуля"))
		return
	}
	view, err := s.engine.AgentSettings(c.Request.Context(), c.Param("projectKey"), &n)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// agentSettingsDraftRequest — тело заведения и замены черновика. document
// сырым JSON: размер считается по нему, а null и отсутствие поля — разное.
type agentSettingsDraftRequest struct {
	From      string          `json:"from"`
	Document  json.RawMessage `json:"document"`
	Changelog *string         `json:"changelog"`
}

// bindOptional разбирает необязательное тело: пустое — пустой запрос.
func bindOptional(c *gin.Context, dst any) error {
	if c.Request.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return apperr.InvalidInput("тело запроса — JSON-объект с полями запроса")
	}
	return nil
}

func (s *Server) createAgentSettingsDraft(c *gin.Context) {
	var req agentSettingsDraftRequest
	if err := bindOptional(c, &req); err != nil {
		fail(c, s.log, err)
		return
	}
	who, err := s.stageActor(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	view, change, err := s.engine.CreateAgentSettingsDraft(c.Request.Context(), c.Param("projectKey"), who,
		workflow.AgentSettingsDraftInput{From: req.From, Document: req.Document, Changelog: req.Changelog})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Set(ctxAuditChange, change)
	c.JSON(http.StatusCreated, view)
}

func (s *Server) putAgentSettingsDraft(c *gin.Context) {
	var req agentSettingsDraftRequest
	if err := bindOptional(c, &req); err != nil {
		fail(c, s.log, err)
		return
	}
	if req.From != "" {
		fail(c, s.log, apperr.InvalidInput("from — только для заведения черновика (POST …/draft)").
			WithDetails(map[string]any{"path": "from"}))
		return
	}
	who, err := s.stageActor(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	view, change, err := s.engine.PutAgentSettingsDraft(c.Request.Context(), c.Param("projectKey"), who,
		workflow.AgentSettingsPutInput{Document: req.Document, Changelog: req.Changelog})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Set(ctxAuditChange, change)
	c.JSON(http.StatusOK, view)
}

func (s *Server) discardAgentSettingsDraft(c *gin.Context) {
	who, err := s.stageActor(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	change, err := s.engine.DiscardAgentSettingsDraft(c.Request.Context(), c.Param("projectKey"), who)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Set(ctxAuditChange, change)
	c.Status(http.StatusNoContent)
}

func (s *Server) activateAgentSettings(c *gin.Context) {
	var req struct {
		Version *int `json:"version"`
	}
	if err := bindOptional(c, &req); err != nil {
		fail(c, s.log, err)
		return
	}
	who, err := s.stageActor(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	out, change, err := s.engine.ActivateAgentSettings(c.Request.Context(), c.Param("projectKey"), who, req.Version)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Set(ctxAuditChange, change)
	c.JSON(http.StatusOK, out)
}
