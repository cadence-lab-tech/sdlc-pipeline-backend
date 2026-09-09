package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Ручки редактирования ролей и цикла. Логики здесь нет: правила иммутабельности
// живут в internal/workflow/authoring.go, чтобы MCP-сервер в фазе 2 встал на те
// же сервисы.

// ── роли ────────────────────────────────────────────────────────────────────

func (s *Server) listRoles(c *gin.Context) {
	roles, err := s.authoring.ListRoles(c.Request.Context(), c.Param("projectKey"),
		c.Query("include_archived") == "true")
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"roles": roles})
}

type roleVersionRequest struct {
	Instructions   string          `json:"instructions" binding:"required"`
	InputKinds     []string        `json:"input_kinds"`
	OutputContract json.RawMessage `json:"output_contract"`
	ToolPolicy     json.RawMessage `json:"tool_policy"`
	ModelHint      *string         `json:"model_hint"`
	RepoScope      []string        `json:"repo_scope"`
	ContextBudget  json.RawMessage `json:"context_budget"`
	Changelog      *string         `json:"changelog"`
}

func (r roleVersionRequest) toInput() workflow.RoleVersionInput {
	return workflow.RoleVersionInput{
		Instructions:   r.Instructions,
		InputKinds:     r.InputKinds,
		OutputContract: r.OutputContract,
		ToolPolicy:     r.ToolPolicy,
		ModelHint:      r.ModelHint,
		RepoScope:      r.RepoScope,
		ContextBudget:  r.ContextBudget,
		Changelog:      r.Changelog,
	}
}

type createRoleRequest struct {
	Key     string              `json:"key" binding:"required"`
	Name    string              `json:"name" binding:"required"`
	Summary *string             `json:"summary"`
	Version *roleVersionRequest `json:"version"`
}

func (s *Server) createRole(c *gin.Context) {
	var req createRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}

	in := workflow.CreateRoleInput{Key: req.Key, Name: req.Name, Summary: req.Summary}
	if req.Version != nil {
		v := req.Version.toInput()
		in.Version = &v
	}

	role, err := s.authoring.CreateRole(c.Request.Context(), c.Param("projectKey"), actor(c).ID, in)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, role)
}

func (s *Server) getRole(c *gin.Context) {
	role, err := s.authoring.GetRole(c.Request.Context(),
		c.Param("projectKey"), c.Param("roleKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, role)
}

type updateRoleRequest struct {
	Name    *string `json:"name"`
	Summary *string `json:"summary"`
}

func (s *Server) updateRole(c *gin.Context) {
	var req updateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	role, err := s.authoring.UpdateRole(c.Request.Context(),
		c.Param("projectKey"), c.Param("roleKey"), req.Name, req.Summary)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, role)
}

// archiveRole и restoreRole: роль не удаляется никогда — на её версии
// ссылаются пройденные этапы, и удаление переписало бы историю.
func (s *Server) archiveRole(c *gin.Context) { s.setRoleArchived(c, true) }
func (s *Server) restoreRole(c *gin.Context) { s.setRoleArchived(c, false) }

func (s *Server) setRoleArchived(c *gin.Context, archived bool) {
	role, err := s.authoring.ArchiveRole(c.Request.Context(),
		c.Param("projectKey"), c.Param("roleKey"), archived)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, role)
}

func (s *Server) listRoleVersions(c *gin.Context) {
	role, err := s.authoring.GetRole(c.Request.Context(),
		c.Param("projectKey"), c.Param("roleKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"versions": role.Versions})
}

func (s *Server) createRoleVersion(c *gin.Context) {
	var req roleVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	rv, err := s.authoring.AddRoleVersion(c.Request.Context(),
		c.Param("projectKey"), c.Param("roleKey"), actor(c).ID, req.toInput())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, rv)
}

func (s *Server) getRoleVersion(c *gin.Context) {
	version, err := strconv.Atoi(c.Param("version"))
	if err != nil {
		fail(c, s.log, apperr.InvalidInput("версия роли — число"))
		return
	}
	role, err := s.authoring.GetRole(c.Request.Context(),
		c.Param("projectKey"), c.Param("roleKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	for _, rv := range role.Versions {
		if rv.Version == version {
			c.JSON(http.StatusOK, rv)
			return
		}
	}
	fail(c, s.log, apperr.NotFound("версия %d роли %q не найдена", version, c.Param("roleKey")))
}

// ── версии цикла ────────────────────────────────────────────────────────────

func (s *Server) listWorkflows(c *gin.Context) {
	workflows, err := s.authoring.ListWorkflows(c.Request.Context(), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"workflows": workflows})
}

type createWorkflowRequest struct {
	Name string `json:"name"`
	// "active" (по умолчанию) — копия работающего цикла тех же типов задач,
	// идентификатор версии — копия конкретной, "empty" — пустой черновик.
	From string `json:"from"`
	// Типы задач, которые поведёт цикл. Пусто — обычные задачи.
	TaskTypes []string `json:"task_types"`
}

func (s *Server) createWorkflow(c *gin.Context) {
	var req createWorkflowRequest
	_ = c.ShouldBindJSON(&req)

	wf, err := s.authoring.CreateDraft(c.Request.Context(),
		c.Param("projectKey"), actor(c).ID, workflow.DraftInput{
			Name: req.Name, From: req.From, TaskTypes: req.TaskTypes,
		})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, wf)
}

type updateWorkflowRequest struct {
	Name string `json:"name"`
	// Типы задач, которые поведёт цикл.
	TaskTypes []string `json:"task_types"`
}

func (s *Server) updateWorkflowVersion(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	var req updateWorkflowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	wf, err := s.authoring.UpdateDraft(c.Request.Context(), id, req.Name, req.TaskTypes)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, wf)
}

// workflowID разбирает идентификатор версии цикла из пути.
func workflowID(c *gin.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("workflowID"))
	if err != nil {
		return uuid.Nil, apperr.InvalidInput("идентификатор версии цикла не UUID")
	}
	return id, nil
}

func (s *Server) getWorkflowVersion(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	wf, err := s.authoring.GetWorkflow(c.Request.Context(), id)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, wf)
}

func (s *Server) deleteWorkflow(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	if err := s.authoring.DeleteWorkflow(c.Request.Context(), id); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// validateWorkflow отдаёт вердикт, ничего не меняя: редактор цикла показывает
// проблемы до того, как человек нажмёт «активировать».
func (s *Server) validateWorkflow(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	v, err := s.authoring.Validate(c.Request.Context(), id)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (s *Server) activateWorkflow(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	res, err := s.authoring.Activate(c.Request.Context(), id, actor(c).ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// ── статусы черновика ───────────────────────────────────────────────────────

type statusRequest struct {
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Category    string  `json:"category"`
	Position    *int    `json:"position"`
	IsInitial   *bool   `json:"is_initial"`
	IsTerminal  *bool   `json:"is_terminal"`
	WipLimit    *int    `json:"wip_limit"`
	Color       *string `json:"color"`
}

func (r statusRequest) toInput() postgres.StatusInput {
	return postgres.StatusInput{
		Key: r.Key, Name: r.Name, Description: r.Description, Category: r.Category,
		Position: r.Position, IsInitial: r.IsInitial, IsTerminal: r.IsTerminal,
		WipLimit: r.WipLimit, Color: r.Color,
	}
}

func (s *Server) createStatus(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	var req statusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	if req.Key == "" || req.Name == "" || req.Category == "" {
		fail(c, s.log, apperr.InvalidInput("у статуса обязательны key, name и category"))
		return
	}

	st, err := s.authoring.CreateStatus(c.Request.Context(), id, req.toInput())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, st)
}

func (s *Server) updateStatus(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	var req statusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	st, err := s.authoring.UpdateStatus(c.Request.Context(), id, c.Param("statusKey"), req.toInput())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, st)
}

func (s *Server) deleteStatus(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	if err := s.authoring.DeleteStatus(c.Request.Context(), id, c.Param("statusKey")); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type reorderStatusesRequest struct {
	Order []string `json:"order" binding:"required"`
}

// reorderStatuses переставляет колонки одним запросом: по одному статусу за
// раз переставить нельзя, уникальность позиции ломается на середине пути.
func (s *Server) reorderStatuses(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	var req reorderStatusesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	statuses, err := s.authoring.ReorderStatuses(c.Request.Context(), id, req.Order)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"statuses": statuses})
}

type statusRolesRequest struct {
	Roles []struct {
		Role      string `json:"role" binding:"required"`
		IsPrimary bool   `json:"is_primary"`
		Position  int    `json:"position"`
	} `json:"roles"`
}

func (s *Server) setStatusRoles(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	var req statusRolesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}

	roles := make([]postgres.StatusRoleInput, 0, len(req.Roles))
	for _, r := range req.Roles {
		roles = append(roles, postgres.StatusRoleInput{
			RoleKey: r.Role, IsPrimary: r.IsPrimary, Position: r.Position})
	}

	st, err := s.authoring.SetStatusRoles(c.Request.Context(), id, c.Param("statusKey"), roles)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, st)
}

// ── переходы и гейты черновика ──────────────────────────────────────────────

type transitionRequest struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// Указатель, потому что null здесь осмыслен: переход «из любого статуса».
	FromStatus *string `json:"from_status"`
	ToStatus   string  `json:"to_status"`
	Kind       string  `json:"kind"`
	Auto       *bool   `json:"auto"`
	Position   *int    `json:"position"`
	// Гейты можно завести сразу вместе с переходом.
	Gates []gateRequest `json:"gates"`
}

func (r transitionRequest) toInput() postgres.TransitionInput {
	return postgres.TransitionInput{
		Key: r.Key, Name: r.Name, FromStatus: r.FromStatus, ToStatus: r.ToStatus,
		Kind: r.Kind, Auto: r.Auto, Position: r.Position,
	}
}

func (s *Server) createTransition(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	var req transitionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	if req.Key == "" || req.Name == "" {
		fail(c, s.log, apperr.InvalidInput("у перехода обязательны key и name"))
		return
	}

	t, err := s.authoring.CreateTransition(c.Request.Context(), id, req.toInput())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	for _, g := range req.Gates {
		gate, err := s.authoring.CreateGate(c.Request.Context(), id, t.Key, g.toInput())
		if err != nil {
			fail(c, s.log, err)
			return
		}
		t.Gates = append(t.Gates, gate)
	}
	c.JSON(http.StatusCreated, t)
}

func (s *Server) updateTransition(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	var req transitionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	t, err := s.authoring.UpdateTransition(c.Request.Context(), id,
		c.Param("transitionKey"), req.toInput())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

func (s *Server) deleteTransition(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	if err := s.authoring.DeleteTransition(c.Request.Context(), id, c.Param("transitionKey")); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type gateRequest struct {
	Key      string          `json:"key"`
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Config   json.RawMessage `json:"config"`
	Blocking *bool           `json:"blocking"`
	Position *int            `json:"position"`
}

func (r gateRequest) toInput() postgres.GateInput {
	return postgres.GateInput{
		Key: r.Key, Name: r.Name, Type: r.Type,
		Config: r.Config, Blocking: r.Blocking, Position: r.Position,
	}
}

func (s *Server) createGate(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	var req gateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	if req.Key == "" || req.Name == "" || req.Type == "" {
		fail(c, s.log, apperr.InvalidInput("у гейта обязательны key, name и type"))
		return
	}

	gate, err := s.authoring.CreateGate(c.Request.Context(), id, c.Param("transitionKey"), req.toInput())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, gate)
}

func (s *Server) updateGate(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	var req gateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	gate, err := s.authoring.UpdateGate(c.Request.Context(), id,
		c.Param("transitionKey"), c.Param("gateKey"), req.toInput())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gate)
}

func (s *Server) deleteGate(c *gin.Context) {
	id, err := workflowID(c)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	if err := s.authoring.DeleteGate(c.Request.Context(), id,
		c.Param("transitionKey"), c.Param("gateKey")); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}
