package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Ручки приглашений (решение 0035). Токен ходит в теле, а не в пути: пути
// запросов оседают в логах прокси и сервиса, а ссылка — пропуск на
// предъявителя.

type teamInviteRequest struct {
	Role string `json:"role"`
	// Указатель: «не задан» — неделя по умолчанию, ноль — ошибка.
	Days *int `json:"days"`
}

func (s *Server) createTeamInvite(c *gin.Context) {
	var req teamInviteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	role := domain.TeamRole(req.Role)
	if role == "" {
		role = domain.TeamRoleMember
	}
	issued, err := s.auth.CreateTeamInvite(c.Request.Context(), identity(c), c.Param("teamKey"), role, req.Days)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, issued)
}

func (s *Server) listTeamInvites(c *gin.Context) {
	invites, err := s.auth.ListTeamInvites(c.Request.Context(), identity(c), c.Param("teamKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invites": invites})
}

type projectInviteRequest struct {
	Level string `json:"level" binding:"required"`
	// Роли специалистов; пусто — любые (решение 0030).
	RoleKeys []string `json:"role_keys"`
	Days     *int     `json:"days"`
}

func (s *Server) createProjectInvite(c *gin.Context) {
	var req projectInviteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	issued, err := s.auth.CreateProjectInvite(c.Request.Context(), identity(c), c.Param("projectKey"),
		domain.MemberRole(req.Level), roleKeysOf(req.RoleKeys), req.Days)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, issued)
}

func (s *Server) listProjectInvites(c *gin.Context) {
	invites, err := s.auth.ListProjectInvites(c.Request.Context(), identity(c), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"invites": invites})
}

func (s *Server) revokeInvite(c *gin.Context) {
	id, err := uuid.Parse(c.Param("inviteID"))
	if err != nil {
		fail(c, s.log, apperr.InvalidInput("идентификатор приглашения не UUID"))
		return
	}
	inv, err := s.auth.RevokeInvite(c.Request.Context(), identity(c), id)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, inv)
}

type inviteTokenRequest struct {
	Token string `json:"token" binding:"required"`
}

func (s *Server) lookupInvite(c *gin.Context) {
	var req inviteTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	inv, err := s.auth.LookupInvite(c.Request.Context(), identity(c), req.Token)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, inv)
}

func (s *Server) acceptInvite(c *gin.Context) {
	var req inviteTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	out, err := s.auth.AcceptInvite(c.Request.Context(), identity(c), req.Token)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, out)
}
