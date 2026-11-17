// Сервер авторизации OAuth 2.1 для удалённых MCP-клиентов (CLAB-481,
// решение 0079).
//
// Та же схема, что у входа CLI (cli_login.go, решение 0032): authorize
// заводит запрос, страница согласия интерфейса решает его от имени
// вошедшего, клиент меняет код на токены. Выдаётся личный токен человека —
// одна строка agent_tokens вида api на всё подключение; на refresh у неё
// меняются хеш, префикс и срок. Нового вида токена и ветвления по нему в
// правах нет: токен выдачи — это человек.
//
// OAuth действует только при включённом выключателе в режиме токенов. В
// остальных случаях ручки отвечают 404, а токен выдачи — 401 (Authenticate):
// выключатель отключает подключённых клиентов, а не только новые
// подключения, и режим заголовка не даёт токену выдачи силы без проверки
// прав.
package workflow

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/oauth"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Сроки и пределы (решение 0079, п. 1, 3, 5). Пределы DCR — константы кода,
// а не переменные.
const (
	oauthRequestTTL      = 10 * time.Minute
	oauthCodeTTL         = 2 * time.Minute
	oauthRefreshRace     = 30 * time.Second
	oauthRequestPurge    = time.Hour
	oauthIdleClientAge   = 7 * 24 * time.Hour
	oauthMaxIdleClients  = 1000
	oauthMaxRegsPerHour  = 60
	oauthRegWindow       = time.Hour
	oauthIdleRetryAfter  = time.Hour
	oauthDefaultAccess   = time.Hour
	oauthDefaultRefresh  = 720 * time.Hour
	oauthMaxStateLen     = 1024
	oauthMaxClientIDLen  = 2000
	oauthMaxSoftwareLen  = 200
	oauthRefreshPrefix   = "sdlc_rt_"
	oauthResourceMCP     = "/mcp"
	oauthResourceProject = "/mcp/projects/"
)

// Действия журнала (решение 0079, «Последствия»): обычный refresh записей
// не создаёт.
const (
	AuditOAuthGrant    = "oauth.grant"
	AuditOAuthRevoke   = "oauth.revoke"
	AuditOAuthRegister = "oauth.register"
)

// OAuthOptions — выключатели и сроки из конфигурации.
type OAuthOptions struct {
	Enabled    bool
	DCR        bool
	CIMD       bool
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	// Загрузчик документов клиентов; nil — обычный, с системным резолвером.
	Fetcher *oauth.Fetcher
	// Часы; nil — time.Now. Тесты подменяют их, база сроков не считает.
	Now func() time.Time
}

// OAuth — сервис выдач. Один на процесс api; у воркера — свой, ради Sweep.
type OAuth struct {
	store   *postgres.Store
	auth    *Auth
	audit   *Audit
	log     *slog.Logger
	opts    OAuthOptions
	fetcher *oauth.Fetcher
}

// NewOAuth собирает сервис и сообщает Auth выключатели: правило «OAuth
// действует» нужно Authenticate на каждом запросе, а сведения об инстансе
// отдают выключатели DCR и CIMD.
func NewOAuth(store *postgres.Store, auth *Auth, audit *Audit, log *slog.Logger, opts OAuthOptions) *OAuth {
	if opts.AccessTTL <= 0 {
		opts.AccessTTL = oauthDefaultAccess
	}
	if opts.RefreshTTL <= 0 {
		opts.RefreshTTL = oauthDefaultRefresh
	}
	f := opts.Fetcher
	if f == nil {
		f = oauth.NewFetcher()
	}
	o := &OAuth{store: store, auth: auth, audit: audit, log: log, opts: opts, fetcher: f}
	if f.Now == nil {
		f.Now = o.now
	}
	if auth != nil {
		auth.oauthEnabled, auth.oauthDCR, auth.oauthCIMD, auth.oauth = opts.Enabled, opts.DCR, opts.CIMD, o
	}
	return o
}

func (o *OAuth) now() time.Time {
	if o.opts.Now != nil {
		return o.opts.Now()
	}
	return time.Now()
}

// Active — OAuth действует: выключатель включён и режим токенов.
func (o *OAuth) Active() bool { return o.auth.OAuthActive() }

// DCR и CIMD — выключатели способов регистрации.
func (o *OAuth) DCR() bool  { return o.opts.DCR }
func (o *OAuth) CIMD() bool { return o.opts.CIMD }

// OAuthActive — правило «OAuth действует» (решение 0079, п. 9 и 11, CLAB-481):
// выключатель включён и режим токенов. Режим заголовка равен выключенному
// OAuth при любом значении выключателя.
func (a *Auth) OAuthActive() bool { return a.oauthEnabled && a.Enforced() }

// ── ошибки протокола ────────────────────────────────────────────────────────

// OAuthError — ошибка ручек протокола в формате RFC 6749 (§5.2): у ручек
// token, register и revoke свой формат ответа, его разбирают клиенты OAuth,
// а не clab. Описание — без секретов и без текста сетевых ошибок.
type OAuthError struct {
	Status      int
	Code        string
	Description string
	RetryAfter  time.Duration
}

func (e *OAuthError) Error() string { return e.Code + ": " + e.Description }

func oauthErr(status int, code, desc string) *OAuthError {
	return &OAuthError{Status: status, Code: code, Description: desc}
}

func invalidGrant(desc string) *OAuthError { return oauthErr(400, "invalid_grant", desc) }

// OAuthPageError — отказ authorize до сверки redirect_uri: страница ошибки
// инстанса без редиректа (защита от открытого редиректа).
type OAuthPageError struct{ Message string }

func (e *OAuthPageError) Error() string { return e.Message }

// ── authorize ───────────────────────────────────────────────────────────────

// AuthorizeInput — параметры authorize и публичный адрес, от которого
// считаются iss и resource этого запроса.
type AuthorizeInput struct {
	Issuer              string
	ClientID            string
	RedirectURI         string
	ResponseType        string
	CodeChallenge       string
	CodeChallengeMethod string
	State               string
	Scope               string
	Resource            string
	// Параметр пришёл несколько раз — запрос неоднозначен (RFC 6749, §3.1).
	Repeated []string
}

type oauthClient struct {
	ID           string
	Kind         string
	Name         string
	RedirectURIs []string
}

// Authorize сверяет запрос и отвечает адресом, куда уйдёт браузер: страница
// согласия или redirect_uri клиента с error. Ошибка — *OAuthPageError, до
// сверки redirect_uri ни одна ошибка не редиректит.
func (o *OAuth) Authorize(ctx context.Context, in AuthorizeInput) (string, error) {
	for _, p := range in.Repeated {
		if p == "client_id" || p == "redirect_uri" {
			return "", &OAuthPageError{Message: "параметр " + p + " передан несколько раз"}
		}
	}
	client, err := o.resolveClient(ctx, in.ClientID)
	if err != nil {
		return "", err
	}
	redirect := in.RedirectURI
	if redirect == "" && len(client.RedirectURIs) == 1 {
		redirect = client.RedirectURIs[0]
	}
	if redirect == "" || !oauth.MatchRedirect(client.RedirectURIs, redirect) {
		return "", &OAuthPageError{Message: "redirect_uri не зарегистрирован у клиента"}
	}

	// Дальше — ошибки редиректом на сверенный адрес.
	fail := func(code, desc string) (string, error) {
		return redirectWith(redirect, url.Values{
			"error": {code}, "error_description": {desc}, "state": {in.State}, "iss": {in.Issuer},
		}, in.State != ""), nil
	}
	if len(in.Repeated) > 0 {
		return fail("invalid_request", "параметр "+in.Repeated[0]+" передан несколько раз")
	}
	if in.ResponseType != "code" {
		return fail("unsupported_response_type", "поддерживается только response_type=code")
	}
	if in.CodeChallengeMethod != "S256" || !oauth.ValidChallenge(in.CodeChallenge) {
		return fail("invalid_request", "нужен PKCE: code_challenge и code_challenge_method=S256")
	}
	if len(in.State) > oauthMaxStateLen {
		return fail("invalid_request", "state слишком длинный")
	}
	scope, ok := oauth.NormalizeScope(in.Scope)
	if !ok {
		return fail("invalid_scope", "поддерживаются скоупы cadence и offline_access")
	}
	resource, projectKey, ok := o.parseResource(in.Issuer, in.Resource)
	if !ok {
		return fail("invalid_target", "resource — адрес MCP этого инстанса")
	}

	now := o.now()
	var state *string
	if in.State != "" {
		state = &in.State
	}
	req, err := o.store.CreateOAuthRequest(ctx, o.store.DB(), domain.OAuthAuthRequest{
		ClientID: client.ID, ClientKind: client.Kind, ClientName: client.Name,
		RedirectURI: redirect, State: state, CodeChallenge: in.CodeChallenge,
		Scope: scope, Resource: resource, ProjectKey: projectKey,
		CreatedAt: now, ExpiresAt: now.Add(oauthRequestTTL),
	})
	if err != nil {
		return "", err
	}
	return oauth.ConsentPath + req.ID.String(), nil
}

// resolveClient находит клиента: URL — документ CIMD (если включён), иначе
// регистрация DCR.
func (o *OAuth) resolveClient(ctx context.Context, clientID string) (oauthClient, error) {
	if clientID == "" || len(clientID) > oauthMaxClientIDLen {
		return oauthClient{}, &OAuthPageError{Message: "нет client_id"}
	}
	if oauth.LooksLikeURL(clientID) {
		if !o.opts.CIMD {
			return oauthClient{}, &OAuthPageError{Message: "клиенты по адресу документа (CIMD) на этом инстансе выключены"}
		}
		doc, err := o.fetcher.Fetch(ctx, clientID)
		if err != nil {
			var ce *oauth.CIMDError
			if errors.As(err, &ce) {
				// Причина сети — в лог сервера; клиенту и человеку — категория.
				o.log.Warn("документ клиента OAuth не принят", "client_id", clientID,
					"category", ce.Category, "error", ce.Cause)
				return oauthClient{}, &OAuthPageError{Message: ce.Category}
			}
			return oauthClient{}, &OAuthPageError{Message: oauth.CIMDUnreachable}
		}
		return oauthClient{ID: clientID, Kind: domain.OAuthClientCIMD, Name: doc.Name, RedirectURIs: doc.RedirectURIs}, nil
	}
	c, err := o.store.GetOAuthClient(ctx, o.store.DB(), clientID)
	if err != nil {
		if apperr.As(err).Code == apperr.CodeNotFound {
			return oauthClient{}, &OAuthPageError{Message: "клиент не зарегистрирован"}
		}
		return oauthClient{}, err
	}
	return oauthClient{ID: c.ID, Kind: domain.OAuthClientDCR, Name: c.Name, RedirectURIs: c.RedirectURIs}, nil
}

// parseResource — resource — адрес MCP этого инстанса: инстансовый или
// проекта. Пусто — инстансовый без привязки.
func (o *OAuth) parseResource(issuer, resource string) (string, *string, bool) {
	if resource == "" {
		return issuer + oauthResourceMCP, nil, true
	}
	if resource == issuer+oauthResourceMCP {
		return resource, nil, true
	}
	rest, ok := strings.CutPrefix(resource, issuer+oauthResourceProject)
	if !ok || rest == "" || strings.ContainsAny(rest, "/?#") {
		return "", nil, false
	}
	key, err := url.PathUnescape(rest)
	if err != nil || key == "" {
		return "", nil, false
	}
	return resource, &key, true
}

// redirectWith дописывает параметры к адресу клиента, сохраняя его запрос.
// state без значения не пишется: пустой state клиент не посылал.
func redirectWith(redirect string, params url.Values, keepState bool) string {
	u, _ := url.Parse(redirect)
	q := u.Query()
	for k, vs := range params {
		if k == "state" && !keepState {
			continue
		}
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// ── согласие ────────────────────────────────────────────────────────────────

// OAuthConsent — всё, что показывает страница согласия (решение 0079,
// п. 12). Хеша кода нет.
type OAuthConsent struct {
	ID         uuid.UUID `json:"id"`
	ClientID   string    `json:"client_id"`
	ClientKind string    `json:"client_kind"`
	// Имя — как его заявил клиент: экран помечает его заявленным.
	ClientName   string `json:"client_name"`
	RedirectHost string `json:"redirect_host"`
	// Хост документа клиента (CIMD); у DCR пусто.
	DocumentHost string           `json:"document_host,omitempty"`
	Loopback     bool             `json:"loopback"`
	Principal    domain.Principal `json:"principal"`
	Resource     string           `json:"resource"`
	ProjectKey   *string          `json:"project_key,omitempty"`
	ProjectName  *string          `json:"project_name,omitempty"`
	Scope        string           `json:"scope"`
	ExpiresAt    time.Time        `json:"expires_at"`
	CanGrant     bool             `json:"can_grant"`
	// Почему «Разрешить» недоступна: код и текст для экрана.
	Reason        string `json:"reason,omitempty"`
	ReasonMessage string `json:"reason_message,omitempty"`
}

// Причины, по которым «Разрешить» недоступна.
const (
	ConsentTaskToken  = "task_token"
	ConsentNotPerson  = "not_a_person"
	ConsentNoProject  = "project_not_found"
	ConsentNotMember  = "not_a_member"
	ConsentTokenBound = "token_bound_elsewhere"
)

// Consent — запрос для страницы согласия. Требует вошедшего.
func (o *OAuth) Consent(ctx context.Context, id Identity, reqID uuid.UUID) (OAuthConsent, error) {
	if err := o.requirePerson(id); err != nil {
		return OAuthConsent{}, err
	}
	req, err := o.store.GetOAuthRequest(ctx, o.store.DB(), reqID, false)
	if err != nil {
		return OAuthConsent{}, err
	}
	if err := o.open(req); err != nil {
		return OAuthConsent{}, err
	}
	return o.consentView(ctx, o.store.DB(), id, req)
}

func (o *OAuth) consentView(ctx context.Context, q postgres.Querier, id Identity, req domain.OAuthAuthRequest) (OAuthConsent, error) {
	out := OAuthConsent{
		ID: req.ID, ClientID: req.ClientID, ClientKind: req.ClientKind, ClientName: req.ClientName,
		RedirectHost: oauth.HostOf(req.RedirectURI), Loopback: oauth.IsLoopbackRedirect(req.RedirectURI),
		Principal: id.Principal, Resource: req.Resource, ProjectKey: req.ProjectKey,
		Scope: req.Scope, ExpiresAt: req.ExpiresAt, CanGrant: true,
	}
	if req.ClientKind == domain.OAuthClientCIMD {
		out.DocumentHost = oauth.HostOf(req.ClientID)
	}
	deny := func(code, msg string) (OAuthConsent, error) {
		out.CanGrant, out.Reason, out.ReasonMessage = false, code, msg
		return out, nil
	}
	if o.auth.Bound(id) != nil {
		return deny(ConsentTaskToken, "токеном задачи доступ клиенту не выдаётся")
	}
	if id.Principal.Kind != domain.PrincipalUser {
		return deny(ConsentNotPerson, "доступ клиенту выдаёт только человек")
	}
	if req.ProjectKey == nil {
		return out, nil
	}
	project, err := o.store.GetProjectByKey(ctx, q, *req.ProjectKey)
	if err != nil {
		if apperr.As(err).Code == apperr.CodeNotFound {
			return deny(ConsentNoProject, "проекта "+*req.ProjectKey+" нет или у вас нет к нему доступа")
		}
		return out, err
	}
	if id.Token != nil && id.Token.ProjectID != nil && *id.Token.ProjectID != project.ID {
		return deny(ConsentTokenBound, "вы вошли токеном другого проекта")
	}
	role, err := o.auth.ProjectRole(ctx, id, project.ID)
	if err != nil {
		return out, err
	}
	if role == "" {
		// Существование чужого проекта не раскрывается: тот же текст.
		return deny(ConsentNotMember, "проекта "+*req.ProjectKey+" нет или у вас нет к нему доступа")
	}
	out.ProjectName = &project.Name
	return out, nil
}

// open — запрос ещё ждёт решения: не просрочен и не решён.
func (o *OAuth) open(req domain.OAuthAuthRequest) error {
	if req.Decision != nil {
		return apperr.Conflict("запрос авторизации уже решён: начните подключение в клиенте заново").
			WithDetails(map[string]any{"reason": "decided"})
	}
	if !o.now().Before(req.ExpiresAt) {
		return apperr.Conflict("запрос авторизации устарел: начните подключение в клиенте заново").
			WithDetails(map[string]any{"reason": "expired"})
	}
	return nil
}

func (o *OAuth) requirePerson(id Identity) error {
	if !id.Authenticated() {
		return apperr.Unauthenticated("войдите, чтобы подключить клиента")
	}
	return nil
}

// Decide — решение человека. Ответ — адрес, куда уходит браузер: с code при
// allow, с error=access_denied при deny. Токеном задачи — 403 (как
// ApproveCLILogin, решение 0049).
func (o *OAuth) Decide(ctx context.Context, id Identity, reqID uuid.UUID, allow bool, issuer string) (string, error) {
	if err := o.requirePerson(id); err != nil {
		return "", err
	}
	if err := o.auth.refuseTaskToken(id); err != nil {
		return "", apperr.Forbidden("токеном задачи доступ клиенту не выдаётся")
	}
	var location string
	err := o.store.InTx(ctx, func(tx pgx.Tx) error {
		req, err := o.store.GetOAuthRequest(ctx, tx, reqID, true)
		if err != nil {
			return err
		}
		if err := o.open(req); err != nil {
			return err
		}
		state := ""
		if req.State != nil {
			state = *req.State
		}
		now := o.now()
		if !allow {
			if err := o.store.DecideOAuthRequest(ctx, tx, req.ID, "deny", id.Principal.ID, now, nil, nil); err != nil {
				return err
			}
			location = redirectWith(req.RedirectURI, url.Values{
				"error": {"access_denied"}, "state": {state}, "iss": {issuer},
			}, req.State != nil)
			return nil
		}
		view, err := o.consentView(ctx, tx, id, req)
		if err != nil {
			return err
		}
		if !view.CanGrant {
			return apperr.Forbidden("%s", view.ReasonMessage).WithDetails(map[string]any{"reason": view.Reason})
		}
		code, err := randomCode()
		if err != nil {
			return err
		}
		hash, expires := HashToken(code), now.Add(oauthCodeTTL)
		if err := o.store.DecideOAuthRequest(ctx, tx, req.ID, "allow", id.Principal.ID, now, &hash, &expires); err != nil {
			return err
		}
		location = redirectWith(req.RedirectURI, url.Values{
			"code": {code}, "state": {state}, "iss": {issuer},
		}, req.State != nil)
		return nil
	})
	return location, err
}

// randomCode — 32 случайных байта в base64url.
func randomCode() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", apperr.Internal(err, "генерация кода")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ── token ───────────────────────────────────────────────────────────────────

// TokenResponse — ответ ручки token (RFC 6749, §5.1).
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// TokenInput — поля формы ручки token.
type TokenInput struct {
	GrantType    string
	Code         string
	CodeVerifier string
	ClientID     string
	RedirectURI  string
	Resource     string
	RefreshToken string
}

// Token — обмен кода или refresh. Исходящих запросов нет: документ клиента
// загружен на authorize, ответ не ждёт чужой сети.
func (o *OAuth) Token(ctx context.Context, in TokenInput) (TokenResponse, error) {
	switch in.GrantType {
	case "authorization_code":
		return o.exchangeCode(ctx, in)
	case "refresh_token":
		return o.refresh(ctx, in)
	case "":
		return TokenResponse{}, oauthErr(400, "invalid_request", "нет grant_type")
	}
	return TokenResponse{}, oauthErr(400, "unsupported_grant_type", "поддерживаются authorization_code и refresh_token")
}

func (o *OAuth) exchangeCode(ctx context.Context, in TokenInput) (TokenResponse, error) {
	if in.Code == "" || in.CodeVerifier == "" || in.ClientID == "" {
		return TokenResponse{}, oauthErr(400, "invalid_request", "нужны code, code_verifier и client_id")
	}
	var out TokenResponse
	var reused *uuid.UUID
	var issued *domain.OAuthGrant
	err := o.store.InTx(ctx, func(tx pgx.Tx) error {
		req, err := o.store.OAuthRequestByCode(ctx, tx, HashToken(in.Code))
		if err != nil {
			if apperr.As(err).Code == apperr.CodeNotFound {
				return invalidGrant("код недействителен")
			}
			return err
		}
		now := o.now()
		if req.CodeUsedAt != nil {
			// Повтор погашенного кода: код утёк, выданное по нему отзывается.
			if req.GrantID != nil {
				revoked, err := o.store.RevokeOAuthGrant(ctx, tx, *req.GrantID, domain.OAuthRevokedCodeReuse, now)
				if err != nil {
					return err
				}
				if revoked {
					reused = req.GrantID
				}
			}
			return nil
		}
		switch {
		case req.CodeExpiresAt == nil || !now.Before(*req.CodeExpiresAt),
			in.ClientID != req.ClientID,
			in.RedirectURI != req.RedirectURI,
			in.Resource != "" && in.Resource != req.Resource,
			!oauth.VerifyS256(in.CodeVerifier, req.CodeChallenge),
			req.DecidedBy == nil:
			return invalidGrant("код недействителен")
		}
		principal, err := o.store.GetPrincipal(ctx, tx, *req.DecidedBy)
		if err != nil {
			return err
		}
		var projectID *uuid.UUID
		if req.ProjectKey != nil {
			p, err := o.store.GetProjectByKey(ctx, tx, *req.ProjectKey)
			if err != nil {
				if apperr.As(err).Code == apperr.CodeNotFound {
					return invalidGrant("проекта выдачи больше нет")
				}
				return err
			}
			projectID = &p.ID
		}
		redirectHost := oauth.HostOf(req.RedirectURI)
		access, hash, prefix, err := NewToken()
		if err != nil {
			return err
		}
		expires := now.Add(o.opts.AccessTTL)
		token, err := o.store.CreateToken(ctx, tx, postgres.CreateTokenInput{
			PrincipalID: principal.ID, ProjectID: projectID,
			Name:      grantLabel(req.ClientName, redirectHost),
			TokenHash: hash, Prefix: prefix, ExpiresAt: &expires, CreatedBy: principal.ID,
		})
		if err != nil {
			return err
		}
		grant, err := o.store.CreateOAuthGrant(ctx, tx, postgres.CreateOAuthGrantInput{
			TokenID: token.ID, PrincipalID: principal.ID, ClientID: req.ClientID, ClientKind: req.ClientKind,
			ClientName: req.ClientName, RedirectHost: redirectHost, Resource: req.Resource,
			ProjectID: projectID, Scope: req.Scope, CreatedAt: now,
		})
		if err != nil {
			return err
		}
		refresh, err := o.issueRefresh(ctx, tx, grant.ID, nil, now)
		if err != nil {
			return err
		}
		if err := o.store.UseOAuthCode(ctx, tx, req.ID, now, grant.ID); err != nil {
			return err
		}
		out = TokenResponse{
			AccessToken: access, TokenType: "Bearer", ExpiresIn: int(o.opts.AccessTTL / time.Second),
			RefreshToken: refresh, Scope: grant.Scope,
		}
		issued = &grant
		return nil
	})
	if err != nil {
		return TokenResponse{}, err
	}
	if reused != nil {
		o.recordGrant(ctx, AuditOAuthRevoke, *reused, domain.OAuthRevokedCodeReuse)
		return TokenResponse{}, invalidGrant("код уже использован")
	}
	if issued == nil {
		return TokenResponse{}, invalidGrant("код уже использован")
	}
	o.recordGrant(ctx, AuditOAuthGrant, issued.ID, "")
	return out, nil
}

// grantLabel — имя строки токена выдачи: «<имя клиента> · <хост redirect>».
func grantLabel(name, host string) string {
	return oauth.CleanName(name) + " · " + host
}

// issueRefresh выпускает refresh выдачи; в базе — только его sha256.
func (o *OAuth) issueRefresh(ctx context.Context, q postgres.Querier, grantID uuid.UUID, parent *uuid.UUID, now time.Time) (string, error) {
	raw, err := newSecret(oauthRefreshPrefix)
	if err != nil {
		return "", err
	}
	if err := o.store.CreateOAuthRefresh(ctx, q, grantID, HashToken(raw), parent, now, now.Add(o.opts.RefreshTTL)); err != nil {
		return "", err
	}
	return raw, nil
}

func (o *OAuth) refresh(ctx context.Context, in TokenInput) (TokenResponse, error) {
	if in.RefreshToken == "" || in.ClientID == "" {
		return TokenResponse{}, oauthErr(400, "invalid_request", "нужны refresh_token и client_id")
	}
	var out TokenResponse
	var reused *uuid.UUID
	err := o.store.InTx(ctx, func(tx pgx.Tx) error {
		r, err := o.store.OAuthRefreshByHash(ctx, tx, HashToken(in.RefreshToken))
		if err != nil {
			if apperr.As(err).Code == apperr.CodeNotFound {
				return invalidGrant("refresh недействителен")
			}
			return err
		}
		// Выдача под блокировкой: два параллельных refresh одной выдачи идут
		// по очереди, второй видит погашенный.
		grant, err := o.store.GetOAuthGrant(ctx, tx, r.GrantID, true)
		if err != nil {
			return err
		}
		if grant.RevokedAt != nil || grant.ClientID != in.ClientID {
			return invalidGrant("refresh недействителен")
		}
		if in.Resource != "" && in.Resource != grant.Resource {
			return invalidGrant("refresh выдан другому resource")
		}
		now := o.now()
		// Повторное чтение под блокировкой выдачи: между первым чтением и
		// блокировкой refresh мог погасить параллельный запрос.
		r, err = o.store.OAuthRefreshByHash(ctx, tx, HashToken(in.RefreshToken))
		if err != nil {
			return err
		}
		if r.UsedAt != nil {
			live, ok, err := o.store.LiveOAuthRefresh(ctx, tx, grant.ID)
			if err != nil {
				return err
			}
			// Гонка параллельных запросов одного клиента: непосредственно
			// предыдущий refresh в окне после ротации. Новых токенов нет,
			// поэтому окно укравшему ничего не даёт.
			if ok && live.ParentID != nil && *live.ParentID == r.ID && now.Sub(*r.UsedAt) <= oauthRefreshRace {
				return invalidGrant("refresh уже обновлён: возьмите новый")
			}
			revoked, err := o.store.RevokeOAuthGrant(ctx, tx, grant.ID, domain.OAuthRevokedRefreshReuse, now)
			if err != nil {
				return err
			}
			if revoked {
				reused = &grant.ID
			}
			return nil
		}
		if !now.Before(r.ExpiresAt) {
			return invalidGrant("refresh истёк: подключите клиента заново")
		}
		used, err := o.store.UseOAuthRefresh(ctx, tx, r.ID, now)
		if err != nil {
			return err
		}
		if !used {
			return invalidGrant("refresh уже обновлён: возьмите новый")
		}
		access, hash, prefix, err := NewToken()
		if err != nil {
			return err
		}
		if err := o.store.RotateGrantToken(ctx, tx, grant.ID, hash, prefix, now.Add(o.opts.AccessTTL), now); err != nil {
			return err
		}
		refresh, err := o.issueRefresh(ctx, tx, grant.ID, &r.ID, now)
		if err != nil {
			return err
		}
		out = TokenResponse{
			AccessToken: access, TokenType: "Bearer", ExpiresIn: int(o.opts.AccessTTL / time.Second),
			RefreshToken: refresh, Scope: grant.Scope,
		}
		return nil
	})
	if err != nil {
		return TokenResponse{}, err
	}
	if reused != nil {
		o.recordGrant(ctx, AuditOAuthRevoke, *reused, domain.OAuthRevokedRefreshReuse)
		return TokenResponse{}, invalidGrant("refresh уже использован")
	}
	if out.AccessToken == "" {
		return TokenResponse{}, invalidGrant("refresh уже использован")
	}
	return out, nil
}

// ── отзыв и список ──────────────────────────────────────────────────────────

// RevokeByClient — отзыв по RFC 7009: access или refresh выдачи своего
// клиента гасят выдачу целиком. Неизвестный токен и токен чужого клиента —
// тот же успех без отзыва: ответ не говорит, что токен существует.
func (o *OAuth) RevokeByClient(ctx context.Context, token, clientID string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return oauthErr(400, "invalid_request", "нет token")
	}
	if clientID == "" {
		return nil
	}
	q := o.store.DB()
	hash := HashToken(token)
	var grantID uuid.UUID
	if r, err := o.store.OAuthRefreshByHash(ctx, q, hash); err == nil {
		grantID = r.GrantID
	} else if g, err := o.store.OAuthGrantByAccessHash(ctx, q, hash); err == nil {
		grantID = g.ID
	} else {
		return nil
	}
	g, err := o.store.GetOAuthGrant(ctx, q, grantID, false)
	if err != nil || g.ClientID != clientID {
		return nil
	}
	revoked, err := o.store.RevokeOAuthGrant(ctx, q, g.ID, domain.OAuthRevokedByUser, o.now())
	if err != nil {
		return err
	}
	if revoked {
		o.recordGrant(ctx, AuditOAuthRevoke, g.ID, domain.OAuthRevokedByUser)
	}
	return nil
}

// ListGrants — подключённые клиенты вошедшего; чужих не видно никому.
func (o *OAuth) ListGrants(ctx context.Context, id Identity) ([]domain.OAuthGrant, error) {
	if err := o.requirePerson(id); err != nil {
		return nil, err
	}
	return o.store.ListOAuthGrants(ctx, o.store.DB(), id.Principal.ID)
}

// RevokeGrant — «Отключить» своего клиента. Чужая выдача — 404, как
// несуществующая; повторный отзыв — без ошибки.
func (o *OAuth) RevokeGrant(ctx context.Context, id Identity, grantID uuid.UUID) error {
	if err := o.requirePerson(id); err != nil {
		return err
	}
	q := o.store.DB()
	g, err := o.store.GetOAuthGrant(ctx, q, grantID, false)
	if err != nil {
		return err
	}
	if g.PrincipalID != id.Principal.ID {
		return apperr.NotFound("выдача не найдена")
	}
	revoked, err := o.store.RevokeOAuthGrant(ctx, q, g.ID, domain.OAuthRevokedByUser, o.now())
	if err != nil {
		return err
	}
	if revoked {
		o.recordGrant(ctx, AuditOAuthRevoke, g.ID, domain.OAuthRevokedByUser)
	}
	return nil
}

// ── DCR ─────────────────────────────────────────────────────────────────────

// RegisterInput — метаданные клиента из тела регистрации (RFC 7591, §2).
type RegisterInput struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	Scope                   string   `json:"scope"`
	SoftwareID              string   `json:"software_id"`
	SoftwareVersion         string   `json:"software_version"`
}

// RegisterResponse — принятые метаданные (RFC 7591, §3.2.1). Способ
// аутентификации клиента всегда none: сервер вправе заменить запрошенный.
type RegisterResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	Scope                   string   `json:"scope"`
	SoftwareID              string   `json:"software_id,omitempty"`
	SoftwareVersion         string   `json:"software_version,omitempty"`
}

// Register регистрирует публичного клиента. Пределы — по базе под
// advisory-блокировкой: 1000 клиентов без живой выдачи, 60 за скользящий час.
func (o *OAuth) Register(ctx context.Context, in RegisterInput) (RegisterResponse, error) {
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > oauth.MaxRedirectURIs {
		return RegisterResponse{}, oauthErr(400, "invalid_redirect_uri", "нужны redirect_uris: от 1 до 20 адресов")
	}
	for _, r := range in.RedirectURIs {
		if err := oauth.CheckRedirectURI(r); err != nil {
			return RegisterResponse{}, oauthErr(400, "invalid_redirect_uri", err.Error())
		}
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return RegisterResponse{}, oauthErr(400, "invalid_client_metadata", "grant_types: authorization_code и refresh_token")
		}
	}
	for _, r := range in.ResponseTypes {
		if r != "code" {
			return RegisterResponse{}, oauthErr(400, "invalid_client_metadata", "response_types: code")
		}
	}
	scope := oauth.ScopeCadence + " " + oauth.ScopeOffline
	if in.Scope != "" {
		s, ok := oauth.NormalizeScope(in.Scope)
		if !ok {
			return RegisterResponse{}, oauthErr(400, "invalid_client_metadata", "scope: cadence и offline_access")
		}
		scope = s
	}
	if len(in.SoftwareID) > oauthMaxSoftwareLen || len(in.SoftwareVersion) > oauthMaxSoftwareLen {
		return RegisterResponse{}, oauthErr(400, "invalid_client_metadata", "software_id и software_version — до 200 символов")
	}
	name := oauth.CleanName(in.ClientName)
	if name == "" {
		name = oauth.HostOf(in.RedirectURIs[0])
	}
	idRaw, err := randomCode()
	if err != nil {
		return RegisterResponse{}, err
	}
	now := o.now()
	c := domain.OAuthClient{ID: idRaw, Name: name, RedirectURIs: in.RedirectURIs, CreatedAt: now}
	if in.SoftwareID != "" {
		c.SoftwareID = &in.SoftwareID
	}
	if in.SoftwareVersion != "" {
		c.SoftwareVersion = &in.SoftwareVersion
	}
	var created domain.OAuthClient
	err = o.store.InTx(ctx, func(tx pgx.Tx) error {
		if err := o.store.LockOAuthRegistrations(ctx, tx); err != nil {
			return err
		}
		n, oldest, err := o.store.OAuthRegistrationsSince(ctx, tx, now.Add(-oauthRegWindow))
		if err != nil {
			return err
		}
		if n >= oauthMaxRegsPerHour {
			retry := oauthRegWindow
			if oldest != nil {
				retry = max(oldest.Add(oauthRegWindow).Sub(now), time.Second)
			}
			return &OAuthError{Status: 429, Code: "temporarily_unavailable",
				Description: "слишком много регистраций за час: повторите позже", RetryAfter: retry}
		}
		idle, err := o.store.CountIdleOAuthClients(ctx, tx)
		if err != nil {
			return err
		}
		if idle >= oauthMaxIdleClients {
			return &OAuthError{Status: 429, Code: "temporarily_unavailable",
				Description: "слишком много клиентов без подключения: повторите позже", RetryAfter: oauthIdleRetryAfter}
		}
		created, err = o.store.CreateOAuthClient(ctx, tx, c)
		return err
	})
	if err != nil {
		return RegisterResponse{}, err
	}
	o.audit.Record(ctx, AuditEntry{
		Channel: AuditREST, Action: AuditOAuthRegister, HTTPStatus: 201, RequestID: requestIDFrom(ctx),
		Params: map[string]any{"client_id": created.ID, "client_name": created.Name, "redirect_uris": created.RedirectURIs},
	})
	out := RegisterResponse{
		ClientID: created.ID, ClientIDIssuedAt: created.CreatedAt.Unix(), ClientName: created.Name,
		RedirectURIs: created.RedirectURIs, TokenEndpointAuthMethod: "none",
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		Scope: scope, SoftwareID: in.SoftwareID, SoftwareVersion: in.SoftwareVersion,
	}
	return out, nil
}

// ── воркер ──────────────────────────────────────────────────────────────────

// OAuthSweepStats — итог прохода задания.
type OAuthSweepStats struct {
	Requests int64
	Expired  int
	Refresh  int64
	Clients  int64
}

// Sweep — проход задания воркера: просроченные запросы (через час после
// срока), выдачи с истёкшим refresh — отозваны с причиной «истекла», refresh
// погасших выдач, клиенты DCR без живой выдачи старше 7 суток.
func (o *OAuth) Sweep(ctx context.Context) (OAuthSweepStats, error) {
	var st OAuthSweepStats
	q, now := o.store.DB(), o.now()
	var err error
	if st.Requests, err = o.store.PurgeOAuthRequests(ctx, q, now.Add(-oauthRequestPurge)); err != nil {
		return st, err
	}
	expired, err := o.store.ExpiredOAuthGrants(ctx, q, now)
	if err != nil {
		return st, err
	}
	for _, g := range expired {
		revoked, err := o.store.RevokeOAuthGrant(ctx, q, g.ID, domain.OAuthRevokedExpired, now)
		if err != nil {
			return st, err
		}
		if revoked {
			st.Expired++
			o.recordGrant(ctx, AuditOAuthRevoke, g.ID, domain.OAuthRevokedExpired)
		}
	}
	if st.Refresh, err = o.store.PurgeDeadGrantRefresh(ctx, q); err != nil {
		return st, err
	}
	if st.Clients, err = o.store.PurgeIdleOAuthClients(ctx, q, now.Add(-oauthIdleClientAge)); err != nil {
		return st, err
	}
	return st, nil
}

// ── журнал ──────────────────────────────────────────────────────────────────

// recordGrant пишет выдачу или отзыв со ссылкой на принципала и token_id
// выдачи. Ни кода, ни verifier, ни токенов в параметрах нет.
func (o *OAuth) recordGrant(ctx context.Context, action string, grantID uuid.UUID, reason string) {
	if o.audit == nil {
		return
	}
	q := o.store.DB()
	g, err := o.store.GetOAuthGrant(ctx, q, grantID, false)
	if err != nil {
		o.log.Error("журнал OAuth: выдача не прочитана", "grant_id", grantID, "error", err)
		return
	}
	token, err := o.store.GetToken(ctx, q, g.TokenID)
	if err != nil {
		o.log.Error("журнал OAuth: токен выдачи не прочитан", "grant_id", grantID, "error", err)
		return
	}
	p, err := o.store.GetPrincipal(ctx, q, g.PrincipalID)
	if err != nil {
		o.log.Error("журнал OAuth: принципал выдачи не прочитан", "grant_id", grantID, "error", err)
		return
	}
	params := map[string]any{
		"grant_id": g.ID, "client_id": g.ClientID, "client_kind": g.ClientKind,
		"client_name": g.ClientName, "redirect_host": g.RedirectHost, "resource": g.Resource,
	}
	if reason != "" {
		params["reason"] = reason
	}
	tokenID := g.TokenID
	o.audit.Record(ctx, AuditEntry{
		Target:   postgres.AuditTarget{ProjectID: g.ProjectID, TokenID: &tokenID},
		Identity: Identity{Principal: p, Token: &token},
		Channel:  AuditREST, Action: action, Params: params, RequestID: requestIDFrom(ctx),
	})
}

// RecordTokenRevoked — отзыв выдачи общим путём токенов (DELETE
// /v1/auth/tokens/:id): тот же след в журнале, что у «Отключить».
func (o *OAuth) recordTokenRevoked(ctx context.Context, grantID uuid.UUID) {
	o.recordGrant(ctx, AuditOAuthRevoke, grantID, domain.OAuthRevokedByUser)
}

type requestIDKey struct{}

// WithRequestID кладёт X-Request-Id в контекст: сервисные записи журнала
// связываются с логом процесса так же, как записи транспорта.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
