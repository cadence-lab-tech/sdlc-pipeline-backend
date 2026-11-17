package httpapi

import (
	"encoding/json"
	"errors"
	"html"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/oauth"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Сервер авторизации OAuth 2.1 (CLAB-481, решение 0079). Ручки только
// разбирают запрос и сериализуют ответ; правила — workflow.OAuth, протокол
// — internal/oauth.

// oauthFormLimit — предел тела ручек token, register и revoke: в них
// несколько коротких полей, а открыты они всему интернету.
const oauthFormLimit = 64 << 10

// WithOAuth подменяет сервис OAuth — тесты подставляют часы и загрузчик
// документов клиентов.
func (s *Server) WithOAuth(o *workflow.OAuth) *Server {
	s.oauth = o
	return s
}

// registerOAuth — открытые ручки: метаданные и протокол вне группы /v1 с
// журналом REST (в теле token — код, verifier и refresh), страница согласия
// и список выдач — своей группой под токеном вошедшего. Выключатель — первым
// слоем: при недействующем OAuth все пути отвечают 404, как будто их нет.
func (s *Server) registerOAuth(r *gin.Engine) {
	open := r.Group("", s.oauthOnly(), oauthCORS())
	open.GET(oauth.PathAuthServerMetadata, s.authServerMetadata)
	open.OPTIONS(oauth.PathAuthServerMetadata, preflight)
	open.GET(oauth.PathResourceMetadata, s.resourceMetadata)
	open.GET(oauth.PathResourceMetadata+"/*mcpPath", s.resourceMetadata)
	open.OPTIONS(oauth.PathResourceMetadata+"/*mcpPath", preflight)
	open.POST(oauth.PathToken, s.oauthToken)
	open.OPTIONS(oauth.PathToken, preflight)
	open.POST(oauth.PathRegister, s.oauthRegister)
	open.OPTIONS(oauth.PathRegister, preflight)
	open.POST(oauth.PathRevoke, s.oauthRevoke)
	open.OPTIONS(oauth.PathRevoke, preflight)
	// authorize открывает браузер человека, а не скрипт: CORS ему не нужен.
	r.GET(oauth.PathAuthorize, s.oauthOnly(), s.oauthAuthorize)

	// Белый список токена задачи этих маршрутов не знает — токен задачи
	// получает 403 до ручки (решение 0049), в том числе при недействующем
	// OAuth: отказ по белому списку не зависит от настроек, как у прочих
	// маршрутов /v1. Остальным при недействующем OAuth — 404.
	consent := r.Group("/v1/oauth", s.auditREST(), s.rejectTooLarge(),
		s.authenticate(domain.PrincipalUser, false), s.executorOnly(), s.oauthOnly())
	consent.GET("/requests/:requestID", s.oauthConsent)
	consent.POST("/requests/:requestID", s.oauthDecide)
	consent.GET("/grants", s.oauthGrants)
	consent.DELETE("/grants/:grantID", s.oauthRevokeGrant)
}

// oauthOnly — пока OAuth не действует (выключатель или режим заголовка),
// его пути не существуют.
func (s *Server) oauthOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.oauth == nil || !s.oauth.Active() {
			fail(c, s.log, apperr.NotFound("маршрут не найден"))
			return
		}
		c.Next()
	}
}

// oauthCORS — клиенты в браузере читают метаданные и меняют код сами.
// Учётных данных у инстанса в cookie нет, поэтому «*» без credentials
// ничего не открывает.
func oauthCORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Next()
	}
}

func preflight(c *gin.Context) {
	c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
	c.Header("Access-Control-Max-Age", "600")
	c.Status(http.StatusNoContent)
}

// issuer — публичный адрес инстанса для этого запроса: SDLC_PUBLIC_URL или
// Host и X-Forwarded-Proto. Адрес по http не на петле — предупреждение в
// логе один раз на процесс: клиенты требуют https.
func (s *Server) issuer(c *gin.Context) string {
	base := s.cfg.PublicURL
	if base == "" {
		base = oauth.RequestBase(c.Request)
	}
	if oauth.InsecureBase(base) {
		s.insecureOnce.Do(func() {
			s.log.Warn("публичный адрес OAuth — http не на петле: клиенты требуют https; "+
				"задайте SDLC_PUBLIC_URL или передайте X-Forwarded-Proto", "issuer", base)
		})
	}
	return base
}

func (s *Server) authServerMetadata(c *gin.Context) {
	// Адрес зависит от запроса — ответ не кэшируется ни сервером, ни прокси.
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, oauth.ServerFor(s.issuer(c), s.oauth.DCR(), s.oauth.CIMD()))
}

// resourceMetadata — метаданные ресурса адреса проекта. Путь берётся в том
// виде, в каком пришёл, — resource побайтно равен адресу, который ввёл
// человек. Существование проекта не проверяется: ручка открыта и проекты не
// перечисляет. Инстансовый адрес (/mcp и корень) — до CLAB-483 404.
func (s *Server) resourceMetadata(c *gin.Context) {
	mcpPath := strings.TrimPrefix(c.Request.URL.EscapedPath(), oauth.PathResourceMetadata)
	key, ok := strings.CutPrefix(mcpPath, "/mcp/projects/")
	if !ok || key == "" || strings.Contains(key, "/") {
		fail(c, s.log, apperr.NotFound("метаданные ресурса для этого пути не отдаются"))
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, oauth.ResourceFor(s.issuer(c), mcpPath))
}

// mcpChallenge — WWW-Authenticate у 401 на MCP (RFC 9728, §5.1): где искать
// метаданные ресурса этого пути. Только пока OAuth действует: иначе клиенту
// незачем идти туда, где 404.
func (s *Server) mcpChallenge(c *gin.Context, tokenPresented bool) {
	if s.oauth == nil || !s.oauth.Active() {
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(c.Request.URL.EscapedPath(), "/"), "/", 4)
	if len(parts) < 3 {
		return
	}
	mcpPath := "/" + parts[0] + "/" + parts[1] + "/" + parts[2]
	value := `Bearer resource_metadata="` + oauth.ResourceMetadataURL(s.issuer(c), mcpPath) + `"`
	if tokenPresented {
		value += `, error="invalid_token"`
	}
	c.Header("WWW-Authenticate", value)
}

// oauthAuthorize — начало подключения в браузере человека. Ошибка до сверки
// redirect_uri — страница инстанса, после — редирект с error.
func (s *Server) oauthAuthorize(c *gin.Context) {
	q := c.Request.URL.Query()
	in := workflow.AuthorizeInput{
		Issuer: s.issuer(c), ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"),
		ResponseType: q.Get("response_type"), CodeChallenge: q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"), State: q.Get("state"),
		Scope: q.Get("scope"), Resource: q.Get("resource"),
	}
	for k, v := range q {
		if len(v) > 1 {
			in.Repeated = append(in.Repeated, k)
		}
	}
	c.Header("Cache-Control", "no-store")
	location, err := s.oauth.Authorize(c.Request.Context(), in)
	if err != nil {
		var page *workflow.OAuthPageError
		if errors.As(err, &page) {
			oauthErrorPage(c, page.Message)
			return
		}
		s.log.Error("authorize: внутренняя ошибка", "error", err, "request_id", c.GetString(ctxRequestID))
		oauthErrorPage(c, "внутренняя ошибка сервера")
		return
	}
	c.Redirect(http.StatusFound, location)
}

// oauthErrorPage — страница ошибки без редиректа: человек видит, что не
// так, а адрес клиента, который не сверен, браузер не получает.
func oauthErrorPage(c *gin.Context, msg string) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'")
	c.String(http.StatusBadRequest, "<!doctype html><html lang=\"ru\"><meta charset=\"utf-8\">"+
		"<title>Подключение клиента не удалось</title><h1>Подключение клиента не удалось</h1><p>%s</p>"+
		"<p>Вернитесь в приложение и начните подключение заново.</p></html>", html.EscapeString(msg))
}

// oauthFail — ответ ошибкой протокола (RFC 6749, §5.2).
func (s *Server) oauthFail(c *gin.Context, err error) {
	var oe *workflow.OAuthError
	if !errors.As(err, &oe) {
		s.log.Error("ручка OAuth: внутренняя ошибка", "path", c.FullPath(), "error", err,
			"request_id", c.GetString(ctxRequestID))
		oe = &workflow.OAuthError{Status: http.StatusInternalServerError, Code: "server_error", Description: "внутренняя ошибка"}
	}
	if oe.RetryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(int((oe.RetryAfter+time.Second-1)/time.Second)))
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(oe.Status, gin.H{"error": oe.Code, "error_description": oe.Description})
}

// readForm — тело application/x-www-form-urlencoded; иной тип — ошибка
// протокола, а не разбор наугад.
func readForm(c *gin.Context) error {
	media, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if media != "application/x-www-form-urlencoded" {
		return &workflow.OAuthError{Status: 400, Code: "invalid_request",
			Description: "тело — application/x-www-form-urlencoded"}
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, oauthFormLimit)
	if err := c.Request.ParseForm(); err != nil {
		return &workflow.OAuthError{Status: 400, Code: "invalid_request", Description: "тело формы не разбирается"}
	}
	return nil
}

func (s *Server) oauthToken(c *gin.Context) {
	if err := readForm(c); err != nil {
		s.oauthFail(c, err)
		return
	}
	f := c.Request.PostForm
	ctx := workflow.WithRequestID(c.Request.Context(), c.GetString(ctxRequestID))
	out, err := s.oauth.Token(ctx, workflow.TokenInput{
		GrantType: f.Get("grant_type"), Code: f.Get("code"), CodeVerifier: f.Get("code_verifier"),
		ClientID: f.Get("client_id"), RedirectURI: f.Get("redirect_uri"), Resource: f.Get("resource"),
		RefreshToken: f.Get("refresh_token"),
	})
	if err != nil {
		s.oauthFail(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.JSON(http.StatusOK, out)
}

func (s *Server) oauthRevoke(c *gin.Context) {
	if err := readForm(c); err != nil {
		s.oauthFail(c, err)
		return
	}
	f := c.Request.PostForm
	ctx := workflow.WithRequestID(c.Request.Context(), c.GetString(ctxRequestID))
	if err := s.oauth.RevokeByClient(ctx, f.Get("token"), f.Get("client_id")); err != nil {
		s.oauthFail(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
}

func (s *Server) oauthRegister(c *gin.Context) {
	if !s.oauth.DCR() {
		fail(c, s.log, apperr.NotFound("регистрация клиентов на этом инстансе выключена"))
		return
	}
	var in workflow.RegisterInput
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, oauthFormLimit))
	if err := dec.Decode(&in); err != nil {
		s.oauthFail(c, &workflow.OAuthError{Status: 400, Code: "invalid_client_metadata",
			Description: "тело — JSON с метаданными клиента"})
		return
	}
	ctx := workflow.WithRequestID(c.Request.Context(), c.GetString(ctxRequestID))
	out, err := s.oauth.Register(ctx, in)
	if err != nil {
		s.oauthFail(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, out)
}

func (s *Server) oauthConsent(c *gin.Context) {
	id, err := uuid.Parse(c.Param("requestID"))
	if err != nil {
		fail(c, s.log, apperr.NotFound("запрос авторизации не найден"))
		return
	}
	out, err := s.oauth.Consent(c.Request.Context(), identity(c), id)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, out)
}

type oauthDecideRequest struct {
	Decision string `json:"decision" binding:"required"`
}

func (s *Server) oauthDecide(c *gin.Context) {
	id, err := uuid.Parse(c.Param("requestID"))
	if err != nil {
		fail(c, s.log, apperr.NotFound("запрос авторизации не найден"))
		return
	}
	var req oauthDecideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	if req.Decision != "allow" && req.Decision != "deny" {
		fail(c, s.log, apperr.InvalidInput("decision: allow или deny"))
		return
	}
	ctx := workflow.WithRequestID(c.Request.Context(), c.GetString(ctxRequestID))
	location, err := s.oauth.Decide(ctx, identity(c), id, req.Decision == "allow", s.issuer(c))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"redirect_uri": location})
}

func (s *Server) oauthGrants(c *gin.Context) {
	out, err := s.oauth.ListGrants(c.Request.Context(), identity(c))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"grants": out})
}

func (s *Server) oauthRevokeGrant(c *gin.Context) {
	id, err := uuid.Parse(c.Param("grantID"))
	if err != nil {
		fail(c, s.log, apperr.NotFound("выдача не найдена"))
		return
	}
	ctx := workflow.WithRequestID(c.Request.Context(), c.GetString(ctxRequestID))
	if err := s.oauth.RevokeGrant(ctx, identity(c), id); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}
