package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/workflow"
)

// Поиск по знанию проекта и состояние индекса. Те же сервисы, что у MCP-тула
// sdlc_search_context: REST нужен интерфейсу и CI.

type searchRequest struct {
	Query        string   `json:"query"`
	Scope        []string `json:"scope"`
	Repositories []string `json:"repositories"`
	TopK         int      `json:"top_k"`
}

// search принимает и POST с телом, и GET с параметрами: из UI удобнее первое,
// из curl и CI — второе.
func (s *Server) search(c *gin.Context) {
	var req searchRequest
	if c.Request.Method == http.MethodPost {
		_ = c.ShouldBindJSON(&req)
	}
	if req.Query == "" {
		req.Query = c.Query("q")
	}
	if len(req.Scope) == 0 {
		req.Scope = splitList(c.Query("scope"))
	}
	if len(req.Repositories) == 0 {
		req.Repositories = splitList(c.Query("repositories"))
	}
	if req.TopK == 0 {
		req.TopK, _ = strconv.Atoi(c.Query("top_k"))
	}

	res, err := s.engine.SearchContext(c.Request.Context(), c.Param("projectKey"), workflow.SearchInput{
		Query:        req.Query,
		Scopes:       req.Scope,
		Repositories: req.Repositories,
		TopK:         req.TopK,
	})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (s *Server) indexStatus(c *gin.Context) {
	ctx := c.Request.Context()
	project, err := s.store.GetProjectByKey(ctx, s.store.DB(), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	rows, err := s.store.IndexStatus(ctx, s.store.DB(), project.ID)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"repositories": rows})
}

func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
