package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Ручки библиотеки инструкций (решение 0069). Логики нет: правила — в
// workflow/instructions.go, их же зовёт MCP. В журнал действий кроме тела
// запроса (содержимое там — размер и sha256) уходит итог: ключ, номер
// версии, sha и размер, а у «обновить в ролях» — роли и их новые версии
// (ctxAuditChange).

func (s *Server) listInstructions(c *gin.Context) {
	list, err := s.authoring.ListInstructions(c.Request.Context(), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"instructions": list})
}

func (s *Server) getInstruction(c *gin.Context) {
	version := 0
	if raw := c.Query("version"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			fail(c, s.log, apperr.InvalidInput("version — номер версии инструкции, целое от 1"))
			return
		}
		version = n
	}
	out, err := s.authoring.GetInstruction(c.Request.Context(), c.Param("projectKey"), c.Param("instructionKey"), version)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) getInstructionVersion(c *gin.Context) {
	version, err := strconv.Atoi(c.Param("version"))
	if err != nil {
		fail(c, s.log, apperr.InvalidInput("версия инструкции — число"))
		return
	}
	v, err := s.authoring.GetInstructionVersion(c.Request.Context(), c.Param("projectKey"), c.Param("instructionKey"), version)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

type instructionRequest struct {
	Key       string  `json:"key"`
	Name      string  `json:"name"`
	Summary   *string `json:"summary"`
	Kind      string  `json:"kind"`
	Content   string  `json:"content"`
	Changelog *string `json:"changelog"`
}

func (r instructionRequest) toInput() workflow.InstructionInput {
	return workflow.InstructionInput{Key: r.Key, Name: r.Name, Summary: r.Summary,
		Kind: r.Kind, Content: r.Content, Changelog: r.Changelog}
}

func (s *Server) createInstruction(c *gin.Context) {
	var req instructionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	out, err := s.authoring.CreateInstruction(c.Request.Context(), c.Param("projectKey"), actor(c).ID, req.toInput())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Set(ctxAuditChange, instructionAudit(out))
	c.JSON(http.StatusCreated, out)
}

func (s *Server) createInstructionVersion(c *gin.Context) {
	var req instructionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	out, err := s.authoring.AddInstructionVersion(c.Request.Context(), c.Param("projectKey"),
		c.Param("instructionKey"), actor(c).ID, req.toInput())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Set(ctxAuditChange, instructionAudit(out))
	c.JSON(http.StatusCreated, out)
}

// instructionAudit — заведённая версия для журнала без содержимого.
func instructionAudit(out workflow.InstructionWrite) map[string]any {
	return map[string]any{
		"instruction": out.Instruction.Key, "version": out.Version.Version, "kind": out.Version.Kind,
		"sha256": out.Version.SHA256, "size_bytes": out.Version.SizeBytes, "created": out.Created,
	}
}

type updateInstructionRequest struct {
	Name    *string `json:"name"`
	Summary *string `json:"summary"`
}

func (s *Server) updateInstruction(c *gin.Context) {
	var req updateInstructionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	inst, err := s.authoring.UpdateInstruction(c.Request.Context(), c.Param("projectKey"),
		c.Param("instructionKey"), req.Name, req.Summary)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, inst)
}

type applyInstructionRequest struct {
	Version   int                  `json:"version"`
	Roles     []workflow.ApplyRole `json:"roles"`
	Changelog string               `json:"changelog"`
}

func (s *Server) applyInstruction(c *gin.Context) {
	var req applyInstructionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	out, err := s.authoring.ApplyInstruction(c.Request.Context(), c.Param("projectKey"), c.Param("instructionKey"),
		actor(c).ID, workflow.ApplyInput{Version: req.Version, Roles: req.Roles, Changelog: req.Changelog})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Set(ctxAuditChange, out)
	c.JSON(http.StatusOK, out)
}
