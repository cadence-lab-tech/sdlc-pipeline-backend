package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/workflow"
)

// Ручки команд (решение 0027). Права у каждой свои — их проверяет
// workflow.Auth, а не guard по уровню в проекте: команда живёт над проектами.

func (s *Server) listTeams(c *gin.Context) {
	teams, err := s.auth.ListTeams(c.Request.Context(), identity(c))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"teams": teams})
}

func (s *Server) getTeam(c *gin.Context) {
	detail, err := s.auth.GetTeam(c.Request.Context(), identity(c), c.Param("teamKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

type createTeamRequest struct {
	Key               string  `json:"key" binding:"required"`
	Name              string  `json:"name" binding:"required"`
	Description       *string `json:"description"`
	CanCreateProjects bool    `json:"can_create_projects"`
}

func (s *Server) createTeam(c *gin.Context) {
	var req createTeamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	team, err := s.auth.CreateTeam(c.Request.Context(), identity(c), workflow.CreateTeamInput{
		Key: req.Key, Name: req.Name, Description: req.Description,
		CanCreateProjects: req.CanCreateProjects,
	})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, team)
}

// updateTeamRequest — поля указателями: незаполненное поле не меняется, и
// «снять право» отличается от «не трогать».
type updateTeamRequest struct {
	Name              *string `json:"name"`
	Description       *string `json:"description"`
	CanCreateProjects *bool   `json:"can_create_projects"`
}

func (s *Server) updateTeam(c *gin.Context) {
	var req updateTeamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	team, err := s.auth.UpdateTeam(c.Request.Context(), identity(c), c.Param("teamKey"),
		workflow.UpdateTeamInput{
			Name: req.Name, Description: req.Description,
			CanCreateProjects: req.CanCreateProjects,
		})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, team)
}

func (s *Server) archiveTeam(c *gin.Context) {
	if err := s.auth.ArchiveTeam(c.Request.Context(), identity(c), c.Param("teamKey")); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type setTeamMemberRequest struct {
	Principal string `json:"principal" binding:"required"`
	Role      string `json:"role"`
}

func (s *Server) setTeamMember(c *gin.Context) {
	var req setTeamMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	role := domain.TeamRole(req.Role)
	if role == "" {
		role = domain.TeamRoleMember
	}
	m, err := s.auth.SetTeamMember(c.Request.Context(), identity(c), c.Param("teamKey"),
		req.Principal, role)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

func (s *Server) removeTeamMember(c *gin.Context) {
	if err := s.auth.RemoveTeamMember(c.Request.Context(), identity(c), c.Param("teamKey"),
		c.Param("handle")); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── команды в проекте ───────────────────────────────────────────────────────

func (s *Server) listProjectTeams(c *gin.Context) {
	teams, err := s.auth.ListProjectTeams(c.Request.Context(), identity(c), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"teams": teams})
}

type setProjectTeamRequest struct {
	Team  string `json:"team" binding:"required"`
	Level string `json:"level" binding:"required"`
}

func (s *Server) setProjectTeam(c *gin.Context) {
	var req setProjectTeamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	pt, err := s.auth.SetProjectTeam(c.Request.Context(), identity(c), c.Param("projectKey"),
		req.Team, domain.MemberRole(req.Level))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, pt)
}

func (s *Server) removeProjectTeam(c *gin.Context) {
	if err := s.auth.RemoveProjectTeam(c.Request.Context(), identity(c), c.Param("projectKey"),
		c.Param("teamKey")); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}
