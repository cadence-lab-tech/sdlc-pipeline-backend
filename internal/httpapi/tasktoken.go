package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Токен задачи в REST (решение 0049). Правило «что видит и куда пишет
// исполнитель этапа» живёт в workflow.Auth; здесь — только откуда взять
// задачу запроса. Те же функции зовут MCP-тулы.

// issueTaskToken выпускает токен задачи держателю живой аренды. Ручки нет у
// сервера прежней версии — раннер по 404 работает по-старому (CLAB-213).
func (s *Server) issueTaskToken(c *gin.Context) {
	out, err := s.auth.IssueTaskToken(c.Request.Context(), identity(c), c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, out)
}

// taskOfPath — задача, к которой относится запрос: по ключу, артефакту или
// сигналу дрейфа. uuid.Nil — маршрут не про задачу.
func (s *Server) taskOfPath(c *gin.Context) (uuid.UUID, error) {
	ctx, q := c.Request.Context(), s.store.DB()
	switch {
	case c.Param("taskKey") != "":
		t, err := s.store.GetTaskByKey(ctx, q, c.Param("taskKey"))
		return t.ID, err
	case c.Param("artifactID") != "":
		id, err := uuid.Parse(c.Param("artifactID"))
		if err != nil {
			return uuid.Nil, apperr.InvalidInput("идентификатор артефакта не UUID")
		}
		a, err := s.store.GetArtifact(ctx, q, id)
		return a.TaskID, err
	case c.Param("signalID") != "":
		id, err := uuid.Parse(c.Param("signalID"))
		if err != nil {
			return uuid.Nil, apperr.InvalidInput("идентификатор сигнала не UUID")
		}
		sig, err := s.store.GetDriftSignal(ctx, q, id)
		return sig.TaskID, err
	}
	return uuid.Nil, nil
}

// readableTask — круг чтения токена задачи для любого маршрута про задачу.
// Зовётся из guard: читать надо, чтобы что-то делать, поэтому круг чтения —
// общее условие и для ручек записи; запись сверх того сужают ownTask и
// ownRun в самих ручках.
func (s *Server) readableTask(c *gin.Context) error {
	id := identity(c)
	if s.auth.Bound(id) == nil {
		return nil
	}
	taskID, err := s.taskOfPath(c)
	if err != nil || taskID == uuid.Nil {
		return err
	}
	return s.auth.RequireReadable(c.Request.Context(), id, taskID)
}

// ownTask — запись токеном задачи только в своей задаче.
func (s *Server) ownTask(c *gin.Context) error {
	id := identity(c)
	if s.auth.Bound(id) == nil {
		return nil
	}
	taskID, err := s.taskOfPath(c)
	if err != nil {
		return err
	}
	return s.auth.RequireOwnTask(id, taskID)
}

// taskSummary — задача в списке, как её видит токен задачи: без описания,
// свойств и прочего, что есть только в карточке.
func taskSummary(t domain.Task) gin.H {
	repos := make([]string, 0, len(t.Repositories))
	for _, r := range t.Repositories {
		repos = append(repos, r.Owner+"/"+r.Name)
	}
	return gin.H{
		"key": t.Key, "title": t.Title, "status": t.StatusKey, "status_name": t.StatusName,
		"type": t.Type(), "parent": t.ParentKey, "assignee": t.AssigneeHandle,
		"repositories": repos,
	}
}
