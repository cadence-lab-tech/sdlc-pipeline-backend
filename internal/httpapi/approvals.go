package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// listApprovals — «Ждут меня» (CLAB-326): гейты проекта, ждущие решения
// человека, объектом. Читать её может любой участник, решать — с уровня
// maintainer (как guardDecision): ниже список пуст и can_decide=false, чтобы
// клиент сказал словами, почему пусто. Ручка только REST: MCP и clab её не
// требуют.
func (s *Server) listApprovals(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()
	project, err := s.store.GetProjectByKey(ctx, q, c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	level, err := s.level(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	out, err := s.engine.Approvals(ctx, q, project.ID, level.Covers(domain.MemberMaintainer), limit)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, out)
}
