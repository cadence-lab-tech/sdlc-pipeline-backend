package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// usagePolicy — политика учёта проекта (CLAB-219): режим, окно и момент
// включения. Читают все участники.
func (s *Server) usagePolicy(c *gin.Context) {
	out, err := s.engine.UsagePolicy(c.Request.Context(), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// setUsagePolicyRequest — правка политики: меняется то, что пришло.
// grace_minutes сырым JSON: отсутствие поля, null и число значат разное.
type setUsagePolicyRequest struct {
	Mode         *string         `json:"mode"`
	GraceMinutes json.RawMessage `json:"grace_minutes"`
}

// setUsagePolicy меняет политику. Гард — администратор проекта или системы;
// вид принципала проверяет движок. В журнал действий кроме тела запроса
// уходит «было → стало» (ctxAuditChange).
func (s *Server) setUsagePolicy(c *gin.Context) {
	var req setUsagePolicyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	in := workflow.UsagePolicyInput{Mode: req.Mode}
	if req.GraceMinutes != nil {
		grace, err := workflow.ParseUsageGrace(req.GraceMinutes)
		if err != nil {
			fail(c, s.log, err)
			return
		}
		in.SetGrace, in.Grace = true, grace
	}
	change, err := s.engine.SetUsagePolicy(c.Request.Context(), c.Param("projectKey"), actor(c).ID, in)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Set(ctxAuditChange, change)
	c.JSON(http.StatusOK, change.After)
}
