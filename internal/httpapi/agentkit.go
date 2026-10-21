package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
)

// agentKit отдаёт набор агента той же версии, что сборка сервера (CLAB-232,
// решение 0050). Ручка открытая, как /v1/instance: секретов в наборе нет, а
// clab setup и страница /docs спрашивают её до любых прав в проекте.
func (s *Server) agentKit(c *gin.Context) {
	kit, err := agentkit.Load(s.buildVersion())
	if err != nil {
		fail(c, s.log, apperr.Internal(err, "набор агента не собрался"))
		return
	}
	c.JSON(http.StatusOK, kit)
}
