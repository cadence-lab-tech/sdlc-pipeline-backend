package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// listApprovals — «Ждут меня» (CLAB-326): гейты проекта, ждущие решения
// человека, объектом. Читать её может любой участник; что в списке и можно
// ли решать по элементу, решает движок по актору (решение 0072): maintainer
// и выше — весь проект, agent — этапы своих ролей, viewer — пустой список и
// can_decide=false, чтобы клиент сказал словами, почему пусто. Ручка только
// REST: MCP и clab её не требуют.
func (s *Server) listApprovals(c *gin.Context) {
	ctx, q := c.Request.Context(), s.store.DB()
	project, err := s.store.GetProjectByKey(ctx, q, c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	who, err := s.auth.Actor(ctx, identity(c), project.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	out, err := s.engine.Approvals(ctx, q, project.ID, who, limit)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, out)
}
