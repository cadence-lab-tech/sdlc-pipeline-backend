package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// auditBodyCap — сколько тела запроса журнал держит в памяти, чтобы разобрать
// параметры. Тело длиннее учитывается размером и sha256: импорт связки или
// крупный артефакт в журнал целиком не идёт всё равно.
const auditBodyCap = 4 << 20

// auditSkip — изменяющие маршруты, которые журнал не пишет: продление аренды
// (шум раз в пять минут) и чтения, идущие POST'ом. Отказ 401/403 на них
// пишется всё равно.
var auditSkip = map[string]bool{
	"POST /v1/tasks/:taskKey/heartbeat":    true,
	"POST /v1/projects/:projectKey/search": true,
	"POST /v1/invites/lookup":              true,
}

// auditREST пишет в журнал изменяющие вызовы REST и отказы 401/403 на любых
// (CLAB-210). Стоит до authenticate: 401 отбивается раньше, чем появляется
// актор. Пишет после обработчика — исход уже известен.
func (s *Server) auditREST() gin.HandlerFunc {
	return func(c *gin.Context) {
		action := c.Request.Method + " " + c.FullPath()
		mutating := isMutating(c.Request.Method) && !auditSkip[action]

		var tap *bodyTap
		if isMutating(c.Request.Method) && c.Request.Body != nil && c.Request.Body != http.NoBody {
			tap = &bodyTap{r: c.Request.Body, h: sha256.New()}
			c.Request.Body = tap
		}

		c.Next()

		status := c.Writer.Status()
		if !mutating && !denied(status) {
			return
		}
		if status == http.StatusUnauthorized && !hasBearer(c) {
			return
		}

		params := map[string]any{}
		if p := pathParams(c); len(p) > 0 {
			params["path"] = p
		}
		if q := c.Request.URL.Query(); len(q) > 0 {
			query := map[string]any{}
			for k, v := range q {
				if len(v) == 1 {
					query[k] = v[0]
				} else {
					query[k] = v
				}
			}
			params["query"] = query
		}
		if change, ok := c.Get(ctxAuditChange); ok {
			params["change"] = change
		}
		var body map[string]any
		if tap != nil {
			b := tap.finish()
			if b != nil {
				params["body"] = b
				body, _ = b.(map[string]any)
			}
		}

		s.audit.Record(c.Request.Context(), workflow.AuditEntry{
			Target:     restTarget(c, body),
			Identity:   s.auditIdentity(c),
			Channel:    workflow.AuditREST,
			Action:     action,
			Params:     params,
			Outcome:    restOutcome(c, status),
			HTTPStatus: status,
			RequestID:  c.GetString(ctxRequestID),
		})
	}
}

// auditMCPDenials пишет отказ 401 на подключении MCP: до тула такой вызов не
// доходит, и обёртка тулов его не видит. Тул в теле не разбирается —
// действие просто mcp.
func (s *Server) auditMCPDenials() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		status := c.Writer.Status()
		if !denied(status) || (status == http.StatusUnauthorized && !hasBearer(c)) {
			return
		}
		s.audit.Record(c.Request.Context(), workflow.AuditEntry{
			Target:     postgres.AuditTarget{ProjectKey: c.Param("projectKey")},
			Identity:   s.auditIdentity(c),
			Channel:    workflow.AuditMCP,
			Action:     workflow.AuditMCP,
			Params:     map[string]any{"path": map[string]any{"projectKey": c.Param("projectKey")}},
			Outcome:    restOutcome(c, status),
			HTTPStatus: status,
			RequestID:  c.GetString(ctxRequestID),
		})
	}
}

func denied(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func (s *Server) auditIdentity(c *gin.Context) workflow.Identity {
	if id := identity(c); id.Principal.ID != uuid.Nil {
		return id
	}
	if v, ok := c.Get(ctxRejected); ok {
		if id, ok := v.(workflow.Identity); ok {
			return id
		}
	}
	return workflow.Identity{}
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func hasBearer(c *gin.Context) bool {
	return strings.HasPrefix(c.GetHeader("Authorization"), "Bearer ")
}

func pathParams(c *gin.Context) map[string]any {
	out := map[string]any{}
	for _, p := range c.Params {
		out[p.Key] = p.Value
	}
	return out
}

// restTarget сводит путь (и поле project тела, у ручек без проекта в пути —
// выдача токена) к цели записи; ключи в идентификаторы переводит вставка.
func restTarget(c *gin.Context, body map[string]any) postgres.AuditTarget {
	t := postgres.AuditTarget{
		ProjectKey: c.Param("projectKey"),
		TaskKey:    c.Param("taskKey"),
		ArtifactID: uuidParam(c, "artifactID"),
		SignalID:   uuidParam(c, "signalID"),
		StageRunID: uuidParam(c, "runID"),
		WorkflowID: uuidParam(c, "workflowID"),
		TokenID:    uuidParam(c, "tokenID"),
		InviteID:   uuidParam(c, "inviteID"),
	}
	if t.ProjectKey == "" && body != nil {
		for _, k := range []string{"project", "project_key"} {
			if v, ok := body[k].(string); ok && v != "" {
				t.ProjectKey = v
				break
			}
		}
	}
	return t
}

func uuidParam(c *gin.Context, name string) *uuid.UUID {
	v := c.Param(name)
	if v == "" {
		return nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil
	}
	return &id
}

// restOutcome — код apperr, которым ответил fail; ответ без него (отбит до
// fail или ошибкой gin) сводится к коду по статусу.
func restOutcome(c *gin.Context, status int) string {
	if status < 400 {
		return workflow.AuditOK
	}
	if code := c.GetString(ctxErrorCode); code != "" {
		return code
	}
	switch status {
	case http.StatusUnauthorized:
		return string(apperr.CodeUnauthenticated)
	case http.StatusForbidden:
		return string(apperr.CodeForbidden)
	case http.StatusNotFound:
		return string(apperr.CodeNotFound)
	case http.StatusBadRequest:
		return string(apperr.CodeInvalidInput)
	case http.StatusConflict:
		return string(apperr.CodeConflict)
	}
	return string(apperr.CodeInternal)
}

// bodyTap пропускает тело запроса к обработчику, по дороге считая размер и
// sha256 и держа в памяти первые auditBodyCap байт для разбора параметров.
type bodyTap struct {
	r    io.ReadCloser
	h    hash.Hash
	n    int64
	buf  bytes.Buffer
	over bool
	eof  bool
}

func (t *bodyTap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.h.Write(p[:n])
		t.n += int64(n)
		if room := auditBodyCap - t.buf.Len(); room > 0 {
			t.buf.Write(p[:min(n, room)])
		}
		if t.n > auditBodyCap {
			t.over = true
		}
	}
	if err == io.EOF {
		t.eof = true
	}
	return n, err
}

func (t *bodyTap) Close() error { return t.r.Close() }

// finish дочитывает то, что обработчик не прочёл (запрос, отбитый guard'ом,
// тела не читает), и отдаёт тело для параметров: разобранный JSON, а иначе —
// размер и sha256.
func (t *bodyTap) finish() any {
	if !t.eof {
		_, _ = io.Copy(io.Discard, io.LimitReader(t, auditBodyCap+1))
	}
	if t.n == 0 {
		return nil
	}
	if !t.eof {
		return map[string]any{"_partial": true, "size_read": t.n}
	}
	sum := hex.EncodeToString(t.h.Sum(nil))
	if t.over {
		return map[string]any{"_truncated": true, "size": t.n, "sha256": sum}
	}
	dec := json.NewDecoder(bytes.NewReader(t.buf.Bytes()))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return map[string]any{"size": t.n, "sha256": sum}
	}
	return v
}

// ── чтение журнала ──────────────────────────────────────────────────────────

func (s *Server) taskAudit(c *gin.Context) {
	f, ok := s.auditFilter(c)
	if !ok {
		return
	}
	t, err := s.store.GetTaskByKey(c.Request.Context(), s.store.DB(), c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	f.TaskID = &t.ID
	f.TaskKey = ""
	s.respondAudit(c, f)
}

func (s *Server) projectAudit(c *gin.Context) {
	f, ok := s.auditFilter(c)
	if !ok {
		return
	}
	p, err := s.store.GetProjectByKey(c.Request.Context(), s.store.DB(), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	f.ProjectID = &p.ID
	s.respondAudit(c, f)
}

func (s *Server) instanceAudit(c *gin.Context) {
	f, ok := s.auditFilter(c)
	if !ok {
		return
	}
	f.Instance = true
	f.TaskKey = ""
	s.respondAudit(c, f)
}

func (s *Server) respondAudit(c *gin.Context, f postgres.AuditFilter) {
	page, err := s.audit.List(c.Request.Context(), f)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// auditFilter разбирает фильтры журнала из строки запроса.
func (s *Server) auditFilter(c *gin.Context) (postgres.AuditFilter, bool) {
	f := postgres.AuditFilter{
		TaskKey: c.Query("task"),
		Action:  c.Query("action"),
		Outcome: c.Query("outcome"),
		Cursor:  c.Query("cursor"),
	}
	bad := func(format string, args ...any) (postgres.AuditFilter, bool) {
		fail(c, s.log, apperr.InvalidInput(format, args...))
		return f, false
	}
	if p := strings.TrimSpace(c.Query("principal")); p != "" {
		if kind, handle, found := strings.Cut(p, ":"); found {
			f.PrincipalKind, f.PrincipalHandle = kind, handle
		} else {
			f.PrincipalHandle = p
		}
	}
	if tok := strings.TrimSpace(c.Query("token")); tok != "" {
		if id, err := uuid.Parse(tok); err == nil {
			f.TokenID = &id
		} else {
			f.TokenPrefix = tok
		}
	}
	switch ch := c.Query("channel"); ch {
	case "", workflow.AuditMCP, workflow.AuditREST:
		f.Channel = ch
	default:
		return bad("channel: %q — нужен mcp или rest", ch)
	}
	for _, p := range []struct {
		name string
		dst  **time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		v := c.Query(p.name)
		if v == "" {
			continue
		}
		at, err := parseAuditTime(v)
		if err != nil {
			return bad("%s: %q — нужна дата 2026-10-21 или время RFC 3339", p.name, v)
		}
		*p.dst = &at
	}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return bad("limit: %q — нужно целое больше нуля", v)
		}
		f.Limit = n
	}
	return f, true
}

func parseAuditTime(v string) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339, v); err == nil {
		return at, nil
	}
	return time.Parse(time.DateOnly, v)
}
