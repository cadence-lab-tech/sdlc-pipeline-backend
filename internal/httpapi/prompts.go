package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
)

// workPromptResponse — стартовая инструкция этапа по REST (решение 0077
// п. 2): имя — как у промпта MCP, текст — тот же, побайтно.
type workPromptResponse struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// workPrompt — канал CLI стартовой инструкции (clab prompt work). Текст
// строит mcp.WorkPrompt, та же функция, что у промпта work_on_task, с теми
// же аргументами: агент без MCP-промптов получает ту же инструкцию.
//
// Задачи нет — 404 (guard в режиме токенов отвечает раньше, а в режиме
// заголовка задачу ищем здесь). Токен задачи читает только инструкцию своей
// задачи: чтение проекта (решение 0065) открывает ему карточки соседей, но
// не их инструкции — по чужой инструкции исполнитель пошёл бы работать
// чужой этап.
func (s *Server) workPrompt(c *gin.Context) {
	ctx := c.Request.Context()
	task, err := s.store.GetTaskByKey(ctx, s.store.DB(), c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	if err := s.auth.RequireOwnTaskKey(identity(c), task.Key); err != nil {
		fail(c, s.log, err)
		return
	}
	role := c.Query("role")
	if err := mcp.CheckPromptRole(role); err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, workPromptResponse{
		Name: mcp.WorkPromptName,
		Text: mcp.WorkPrompt(task.Key, role, c.Query("usage")),
	})
}
