package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Предложения версий ролей (CLAB-381, решение 0072 п. 5). Маршрут ставит
// уровень-минимум, точное право — роль из набора, человек уровня
// maintainer, автор или сопровождающий — считает workflow.Auth: те же
// методы зовёт MCP.

func (s *Server) proposeRoleVersion(c *gin.Context) {
	var req roleVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	p, err := s.authoring.ProposeRoleVersion(c.Request.Context(), s.auth, identity(c),
		c.Param("projectKey"), c.Param("roleKey"), req.toInput())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, p)
}

func (s *Server) listRoleProposals(c *gin.Context) {
	list, err := s.authoring.ListRoleProposals(c.Request.Context(),
		c.Param("projectKey"), c.Param("roleKey"), c.Query("state"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"proposals": list})
}

func proposalID(c *gin.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("proposalID"))
	if err != nil {
		return uuid.Nil, apperr.InvalidInput("идентификатор предложения не UUID")
	}
	return id, nil
}

func (s *Server) getRoleProposal(c *gin.Context) {
	id, err := proposalID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	p, err := s.authoring.GetRoleProposal(c.Request.Context(), c.Param("projectKey"), c.Param("roleKey"), id)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

func (s *Server) approveRoleProposal(c *gin.Context) {
	id, err := proposalID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	p, err := s.authoring.ApproveRoleProposal(c.Request.Context(), s.auth, identity(c),
		c.Param("projectKey"), c.Param("roleKey"), id)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

type proposalDecisionRequest struct {
	Note string `json:"note"`
}

func (s *Server) rejectRoleProposal(c *gin.Context) { s.closeRoleProposal(c, domain.ProposalRejected) }
func (s *Server) withdrawRoleProposal(c *gin.Context) {
	s.closeRoleProposal(c, domain.ProposalWithdrawn)
}

func (s *Server) closeRoleProposal(c *gin.Context, state string) {
	id, err := proposalID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	var req proposalDecisionRequest
	// Тело необязательно: отзыв без заметки — пустой POST. Отклонение без
	// заметки отбивает workflow своим текстом.
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		badRequest(c, s.log, err)
		return
	}
	ctx, project, role := c.Request.Context(), c.Param("projectKey"), c.Param("roleKey")
	var p domain.RoleProposal
	if state == domain.ProposalRejected {
		p, err = s.authoring.RejectRoleProposal(ctx, s.auth, identity(c), project, role, id, req.Note)
	} else {
		p, err = s.authoring.WithdrawRoleProposal(ctx, s.auth, identity(c), project, role, id, req.Note)
	}
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// guardRoleVersion — guard уровня maintainer на прямую версию роли с
// подсказкой участнику уровня agent: версию он предлагает (решение 0072,
// п. 5). Уровень и сам отказ — те же, что у guard.
func (s *Server) guardRoleVersion() gin.HandlerFunc {
	maintainer := s.guard(domain.MemberMaintainer)
	return func(c *gin.Context) {
		if !s.auth.Enforced() {
			c.Next()
			return
		}
		projectID, err := s.projectOf(c)
		if err != nil {
			fail(c, s.log, err)
			return
		}
		if err := s.auth.Require(c.Request.Context(), identity(c), projectID, domain.MemberMaintainer); err != nil {
			fail(c, s.log, s.auth.RoleVersionRefusal(c.Request.Context(), identity(c), projectID, err))
			return
		}
		maintainer(c)
	}
}
