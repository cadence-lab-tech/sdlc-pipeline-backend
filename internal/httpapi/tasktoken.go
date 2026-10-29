package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
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

// readableTask — чтение токеном задачи для любого маршрута про задачу:
// задача своего проекта (решение 0065). Зовётся из guard и потому — общее
// условие и для ручек записи; запись сужают ownTask, OwnRun,
// RequireWriteCircle и RequireLink в самих ручках.
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
