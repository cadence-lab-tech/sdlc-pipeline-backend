package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/workflow"
)

// Ручки фазы 4: кто я, токены, участники, перенос правил, документы и
// репозитории, метрики, вебхуки. Логики здесь нет: правила живут в
// workflow.Auth, workflow.Authoring и движке.

// ── кто я и токены ──────────────────────────────────────────────────────────

func (s *Server) me(c *gin.Context) {
	me, err := s.auth.Me(c.Request.Context(), identity(c))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, me)
}

type issueTokenRequest struct {
	Name          string   `json:"name" binding:"required"`
	Principal     string   `json:"principal"`
	Project       string   `json:"project"`
	RoleKeys      []string `json:"role_keys"`
	ExpiresInDays int      `json:"expires_in_days"`
	MemberRole    string   `json:"member_role"`
}

func (s *Server) issueToken(c *gin.Context) {
	var req issueTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	issued, err := s.auth.IssueToken(c.Request.Context(), identity(c), workflow.IssueTokenInput{
		Name: req.Name, Principal: req.Principal, ProjectKey: req.Project,
		RoleKeys: req.RoleKeys, ExpiresIn: time.Duration(req.ExpiresInDays) * 24 * time.Hour,
		MemberRole: domain.MemberRole(req.MemberRole),
	})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusCreated, issued)
}

func (s *Server) listTokens(c *gin.Context) {
	tokens, err := s.auth.ListTokens(c.Request.Context(), identity(c), c.Query("project"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": tokens})
}

func (s *Server) revokeToken(c *gin.Context) {
	id, err := uuid.Parse(c.Param("tokenID"))
	if err != nil {
		fail(c, s.log, apperr.InvalidInput("идентификатор токена не UUID"))
		return
	}
	if err := s.auth.RevokeToken(c.Request.Context(), identity(c), id); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── вход через корпоративный провайдер ──────────────────────────────────────

// oidcSettings отдаёт фронтенду адреса провайдера. Ручка открытая: её
// спрашивает страница входа, у которой токена ещё нет.
func (s *Server) oidcSettings(c *gin.Context) {
	settings, err := s.auth.OIDCSettings(c.Request.Context())
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, settings)
}

type oidcLoginRequest struct {
	IDToken string `json:"id_token" binding:"required"`
}

// oidcLogin принимает ID-токен, полученный фронтендом у провайдера, и меняет
// его на сессию — обычный токен с коротким сроком.
func (s *Server) oidcLogin(c *gin.Context) {
	var req oidcLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	session, err := s.auth.Login(c.Request.Context(), req.IDToken)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, session)
}

// ── вход через GitHub ───────────────────────────────────────────────────────

// githubSettings отдаёт странице входа адрес авторизации и идентификатор
// клиента. Ручка открытая, как и у провайдера.
func (s *Server) githubSettings(c *gin.Context) {
	c.JSON(http.StatusOK, s.auth.GitHubSettings())
}

type githubLoginRequest struct {
	Code string `json:"code" binding:"required"`
	// Адрес возврата, с которым код был запрошен: GitHub сверяет его при
	// обмене.
	RedirectURI string `json:"redirect_uri"`
}

// githubLogin меняет код авторизации на сессию.
func (s *Server) githubLogin(c *gin.Context) {
	var req githubLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	session, err := s.auth.LoginGitHub(c.Request.Context(), req.Code, req.RedirectURI)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, session)
}

func (s *Server) logout(c *gin.Context) {
	if err := s.auth.Logout(c.Request.Context(), identity(c)); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// roleKeysOf приводит список из запроса к договорённости хранилища: пустой
// список значит «любые роли», то есть NULL.
func roleKeysOf(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	return keys
}

// ── доступ к GitHub у проекта ───────────────────────────────────────────────

type setProjectGitHubRequest struct {
	// Идентификатор установки приложения в организации: ссылка, а не секрет
	// (решение 0028).
	InstallationID *int64 `json:"installation_id"`
	// Имя секрета для запасного пути; значение живёт в окружении.
	CredentialRef *string `json:"credential_ref"`
}

func (s *Server) setProjectGitHub(c *gin.Context) {
	var req setProjectGitHubRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	project, err := s.engine.SetProjectGitHub(c.Request.Context(), c.Param("projectKey"),
		workflow.SetGitHubInput{InstallationID: req.InstallationID, CredentialRef: req.CredentialRef})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, project)
}

// checkProjectGitHub ходит в провайдера по каждому репозиторию проекта.
// Ручка по кнопке из настроек: это единственное место, где система спрашивает
// провайдера о доступе не по делу, а чтобы показать человеку результат.
func (s *Server) checkProjectGitHub(c *gin.Context) {
	status, err := s.engine.CheckGitHubAccess(c.Request.Context(), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, status)
}

// ── участники ───────────────────────────────────────────────────────────────

func (s *Server) listMembers(c *gin.Context) {
	members, err := s.auth.ListMembers(c.Request.Context(), identity(c), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"members": members})
}

type setMemberRequest struct {
	Principal string `json:"principal" binding:"required"`
	Role      string `json:"role" binding:"required"`
	// Роли специалистов, которые участник может забирать; пусто — любые
	// (решение 0030).
	RoleKeys []string `json:"role_keys"`
}

func (s *Server) setMember(c *gin.Context) {
	var req setMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	m, err := s.auth.SetMember(c.Request.Context(), identity(c), c.Param("projectKey"),
		req.Principal, domain.MemberRole(req.Role), roleKeysOf(req.RoleKeys))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

func (s *Server) removeMember(c *gin.Context) {
	if err := s.auth.RemoveMember(c.Request.Context(), identity(c), c.Param("projectKey"),
		c.Param("handle")); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── перенос правил ──────────────────────────────────────────────────────────

func (s *Server) exportBundle(c *gin.Context) {
	bundle, err := s.authoring.Export(c.Request.Context(), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, bundle)
}

// importBundle принимает связку в теле; ?dry_run=true — только отчёт.
func (s *Server) importBundle(c *gin.Context) {
	var bundle workflow.Bundle
	if err := c.ShouldBindJSON(&bundle); err != nil {
		badRequest(c, s.log, err)
		return
	}
	dryRun := c.Query("dry_run") == "true"
	report, err := s.authoring.Import(c.Request.Context(), c.Param("projectKey"), actor(c).ID, bundle, dryRun)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, report)
}

// ── репозитории и документы ─────────────────────────────────────────────────

type addRepositoryRequest struct {
	Repository    string   `json:"repository" binding:"required"`
	DefaultBranch string   `json:"default_branch"`
	CloneURL      string   `json:"clone_url"`
	Visibility    string   `json:"visibility"`
	RoleHint      string   `json:"role_hint"`
	CredentialRef string   `json:"credential_ref"`
	WatchBranches []string `json:"watch_branches"`
	ExcludeGlobs  []string `json:"exclude_globs"`
	IndexEnabled  *bool    `json:"index_enabled"`
}

func (s *Server) addRepository(c *gin.Context) {
	var req addRepositoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	repo, created, err := s.engine.AddRepository(c.Request.Context(), c.Param("projectKey"),
		workflow.AddRepositoryInput{
			Repository: req.Repository, DefaultBranch: req.DefaultBranch, CloneURL: req.CloneURL,
			Visibility: req.Visibility, RoleHint: req.RoleHint, CredentialRef: req.CredentialRef,
			WatchBranches: req.WatchBranches, ExcludeGlobs: req.ExcludeGlobs,
			IndexEnabled: req.IndexEnabled,
		})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, repo)
}

func (s *Server) listDocuments(c *gin.Context) {
	docs, err := s.engine.ListDocuments(c.Request.Context(), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"documents": docs})
}

type putDocumentRequest struct {
	Kind       string          `json:"kind" binding:"required"`
	Title      string          `json:"title" binding:"required"`
	Format     string          `json:"format"`
	Content    string          `json:"content"`
	URL        string          `json:"url"`
	Repository string          `json:"repository"`
	RepoPath   string          `json:"repo_path"`
	Meta       json.RawMessage `json:"meta"`
}

func (s *Server) putDocument(c *gin.Context) {
	var req putDocumentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, s.log, err)
		return
	}
	doc, outcome, err := s.engine.PutDocument(c.Request.Context(), c.Param("projectKey"), actor(c).ID,
		workflow.PutDocumentInput{
			Kind: req.Kind, Title: req.Title, Format: req.Format, Content: []byte(req.Content),
			URL: req.URL, Repository: req.Repository, RepoPath: req.RepoPath, Meta: req.Meta,
		})
	if err != nil {
		fail(c, s.log, err)
		return
	}
	status := http.StatusOK
	if outcome == "created" {
		status = http.StatusCreated
	}
	c.JSON(status, gin.H{"document": doc, "outcome": outcome})
}

func (s *Server) getDocument(c *gin.Context) {
	id, err := uuid.Parse(c.Param("documentID"))
	if err != nil {
		fail(c, s.log, apperr.InvalidInput("идентификатор документа не UUID"))
		return
	}
	doc, content, mediaType, err := s.engine.DocumentContent(c.Request.Context(), c.Param("projectKey"), id)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	if strings.HasSuffix(c.FullPath(), "/content") {
		if content == nil {
			fail(c, s.log, apperr.NotFound("у документа нет содержимого: он лежит по ссылке или в репозитории"))
			return
		}
		c.Data(http.StatusOK, mediaType, content)
		return
	}
	c.JSON(http.StatusOK, gin.H{"document": doc, "content": string(content), "media_type": mediaType})
}

func (s *Server) deleteDocument(c *gin.Context) {
	id, err := uuid.Parse(c.Param("documentID"))
	if err != nil {
		fail(c, s.log, apperr.InvalidInput("идентификатор документа не UUID"))
		return
	}
	if err := s.engine.DeleteDocument(c.Request.Context(), c.Param("projectKey"), id); err != nil {
		fail(c, s.log, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ── метрики ─────────────────────────────────────────────────────────────────

func (s *Server) metrics(c *gin.Context) {
	var period time.Duration
	if raw := c.Query("period"); raw != "" {
		d, err := parsePeriod(raw)
		if err != nil {
			fail(c, s.log, err)
			return
		}
		period = d
	}
	m, err := s.engine.ProjectMetrics(c.Request.Context(), c.Param("projectKey"), period)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

// parsePeriod понимает «30d», «12h» и то, что понимает time.ParseDuration.
func parsePeriod(raw string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		d, err := time.ParseDuration(days + "h")
		if err != nil {
			return 0, apperr.InvalidInput("период %q не разбирается: нужно 30d или 12h", raw)
		}
		return d * 24, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, apperr.InvalidInput("период %q не разбирается: нужно 30d или 12h", raw)
	}
	return d, nil
}

// ── вебхуки ─────────────────────────────────────────────────────────────────

// githubWebhook принимает события GitHub. Без секрета ручка выключена:
// принимать неподписанные события значит позволить кому угодно ставить
// прогоны в очередь.
func (s *Server) githubWebhook(c *gin.Context) {
	if s.cfg.GitHubWebhookSecret == "" {
		fail(c, s.log, apperr.New(apperr.CodeConflict, "вебхуки выключены: не задан SDLC_GITHUB_WEBHOOK_SECRET"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
	if err != nil {
		badRequest(c, s.log, err)
		return
	}
	if !workflow.VerifyGitHubSignature(s.cfg.GitHubWebhookSecret, body, c.GetHeader("X-Hub-Signature-256")) {
		fail(c, s.log, apperr.Unauthenticated("подпись вебхука не сошлась"))
		return
	}
	out, err := s.engine.HandleGitHubEvent(c.Request.Context(),
		c.GetHeader("X-GitHub-Event"), c.GetHeader("X-GitHub-Delivery"), body)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	s.log.Info("вебхук GitHub обработан", "event", out.Event, "repository", out.Repository,
		"queued", out.Queued, "touched", out.Touched, "duplicate", out.Duplicate, "ignored", out.Ignored)
	c.JSON(http.StatusOK, out)
}
