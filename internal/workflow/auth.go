// Аутентификация по токенам и права на проект.
//
// Акторы заложены с первой миграции (решение 0010), поэтому включение
// аутентификации не тронуло ни одной строки истории: сюда добавились только
// токены, участие в проектах и проверка «достаточно ли прав». Проверка живёт
// здесь, а не в транспорте, потому что REST и MCP обязаны отвечать одинаково
// (решение 0011): тул, доступный агенту, и ручка, доступная CLI, — это одно и
// то же действие с одними и теми же правами.
package workflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
)

// Режимы аутентификации.
const (
	// AuthHeader — актор берётся из заголовка X-SDLC-Actor, как в фазах 1-3.
	// Права не проверяются: заголовку никто не мешает соврать, и проверять
	// права у того, кто сам себя назвал, бессмысленно. Режим отладки цикла.
	AuthHeader = "header"
	// AuthToken — только предъявленный токен; заголовок актора игнорируется,
	// права проверяются. Значение для продакшена.
	AuthToken = "token"
)

// TokenPrefix отличает токен системы в логах и конфигах от чужих секретов.
const TokenPrefix = "sdlc_"

// Identity — кто пришёл: принципал и, если предъявлен, токен.
type Identity struct {
	Principal domain.Principal
	// nil, если актор назван заголовком, а не доказан токеном.
	Token *domain.AccessToken
}

// Authenticated — доказал ли актор, кто он.
func (id Identity) Authenticated() bool { return id.Token != nil }

// SystemIdentity — движок или воркер: действует от имени системы и не
// ограничен участием в проектах.
func SystemIdentity(p domain.Principal) Identity {
	p.IsAdmin = true
	return Identity{Principal: p}
}

type Auth struct {
	store *postgres.Store
	log   *slog.Logger
	mode  string
}

func NewAuth(store *postgres.Store, log *slog.Logger, mode string) *Auth {
	if mode == "" {
		mode = AuthHeader
	}
	return &Auth{store: store, log: log, mode: mode}
}

// Mode — режим аутентификации, как его видит клиент.
func (a *Auth) Mode() string { return a.mode }

// Enforced — проверяются ли права.
func (a *Auth) Enforced() bool { return a.mode == AuthToken }

// ── токены ──────────────────────────────────────────────────────────────────

// NewToken генерирует токен, его хеш и префикс для списка.
func NewToken() (token, hash, prefix string, err error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", apperr.Internal(err, "генерация токена")
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), token[:12], nil
}

// HashToken — то, что лежит в базе вместо токена.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Authenticate резолвит предъявленный токен.
func (a *Auth) Authenticate(ctx context.Context, token string) (Identity, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Identity{}, apperr.Unauthenticated("нужен токен: заголовок Authorization: Bearer <token>")
	}
	q := a.store.DB()
	t, p, err := a.store.TokenByHash(ctx, q, HashToken(token))
	if err != nil {
		return Identity{}, err
	}
	if err := a.store.TouchToken(ctx, q, t.ID); err != nil {
		// Отметка использования — удобство, а не безопасность; её сбой не
		// должен превращаться в отказ.
		a.log.Warn("отметка использования токена не записана", "token", t.Prefix, "error", err)
	}
	return Identity{Principal: p, Token: &t}, nil
}

// Bootstrap заводит первого администратора из окружения: в пустой базе
// некому выдать первый токен. Тот же токен при рестарте не плодит записи.
func (a *Auth) Bootstrap(ctx context.Context, handle, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	if len(token) < 16 {
		return apperr.InvalidInput("SDLC_ADMIN_TOKEN короче 16 символов — это не секрет")
	}
	return a.store.InTx(ctx, func(tx pgx.Tx) error {
		p, err := a.store.ResolveActor(ctx, tx, handle, domain.PrincipalUser)
		if err != nil {
			return err
		}
		if !p.IsAdmin {
			if err := a.store.SetAdmin(ctx, tx, p.ID, true); err != nil {
				return err
			}
		}
		prefix := token
		if len(prefix) > 12 {
			prefix = prefix[:12]
		}
		return a.store.UpsertBootstrapToken(ctx, tx, p.ID, HashToken(token), prefix)
	})
}

// IssueTokenInput — кому и на что выдаётся токен.
type IssueTokenInput struct {
	// Кому. Пусто — себе. Формат "agent:bot" или "user:alice"; без префикса —
	// пользователь.
	Principal string
	// Ограничить токен проектом. Обязательно, если токен не себе и выдаёт
	// не глобальный администратор.
	ProjectKey string
	Name       string
	// Роли специалистов, которые агент сможет забирать. Пусто — любые.
	RoleKeys  []string
	ExpiresIn time.Duration
	// Уровень участия, который получит принципал в проекте, если ещё не
	// участник. По умолчанию agent для агента и viewer для человека.
	MemberRole domain.MemberRole
}

// IssuedToken — ответ на выдачу: единственный момент, когда токен виден целиком.
type IssuedToken struct {
	Token  string             `json:"token"`
	Record domain.AccessToken `json:"record"`
}

func (a *Auth) IssueToken(ctx context.Context, id Identity, in IssueTokenInput) (IssuedToken, error) {
	if strings.TrimSpace(in.Name) == "" {
		return IssuedToken{}, apperr.InvalidInput("у токена должно быть имя: по нему его потом отзывают")
	}
	var out IssuedToken

	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		var project *domain.Project
		if in.ProjectKey != "" {
			p, err := a.store.GetProjectByKey(ctx, tx, in.ProjectKey)
			if err != nil {
				return err
			}
			project = &p
		}

		// Кому выдаём. Себе — можно всегда; другому — администратор проекта
		// (токен тогда обязан быть привязан к проекту) или глобальный.
		target := id.Principal
		forSelf := in.Principal == "" || in.Principal == id.Principal.Handle ||
			in.Principal == string(id.Principal.Kind)+":"+id.Principal.Handle
		if !forSelf {
			kind, handle := SplitHandle(in.Principal, domain.PrincipalAgent)
			if project == nil {
				if err := a.RequireAdmin(id); err != nil {
					return apperr.Forbidden("токен другому принципалу без привязки к проекту выдаёт только администратор системы")
				}
			} else if err := a.Require(ctx, id, project.ID, domain.MemberAdmin); err != nil {
				return err
			}
			p, err := a.store.ResolveActor(ctx, tx, handle, kind)
			if err != nil {
				return err
			}
			target = p
		} else if project != nil {
			// Свой токен, ограниченный проектом, не должен давать больше,
			// чем есть у владельца: достаточно быть участником.
			if err := a.Require(ctx, id, project.ID, domain.MemberViewer); err != nil {
				return err
			}
		}

		// Токен, привязанный к проекту, бесполезен без участия в нём.
		if project != nil && !forSelf {
			role := in.MemberRole
			if role == "" {
				role = domain.MemberViewer
				if target.Kind == domain.PrincipalAgent {
					role = domain.MemberAgent
				}
			}
			if !role.Valid() {
				return apperr.InvalidInput("уровень участия %q неизвестен", role)
			}
			current, err := a.store.MemberRole(ctx, tx, project.ID, target.ID)
			if err != nil {
				return err
			}
			if current == "" || !current.Covers(role) {
				if _, err := a.store.SetMember(ctx, tx, project.ID, target.ID, role, id.Principal.ID); err != nil {
					return err
				}
			}
		}

		if len(in.RoleKeys) > 0 && project == nil {
			return apperr.InvalidInput("ограничение по ролям имеет смысл только у токена, привязанного к проекту")
		}
		for _, key := range in.RoleKeys {
			if _, err := a.store.GetRoleByKey(ctx, tx, project.ID, key); err != nil {
				return err
			}
		}

		token, hash, prefix, err := NewToken()
		if err != nil {
			return err
		}
		var expires *time.Time
		if in.ExpiresIn > 0 {
			t := time.Now().Add(in.ExpiresIn)
			expires = &t
		}
		var projectID *uuid.UUID
		if project != nil {
			projectID = &project.ID
		}
		record, err := a.store.CreateToken(ctx, tx, postgres.CreateTokenInput{
			PrincipalID: target.ID, ProjectID: projectID, Name: in.Name,
			TokenHash: hash, Prefix: prefix, RoleKeys: in.RoleKeys,
			ExpiresAt: expires, CreatedBy: id.Principal.ID,
		})
		if err != nil {
			return err
		}
		out = IssuedToken{Token: token, Record: record}
		return nil
	})

	return out, err
}

// ListTokens — свои токены, либо токены проекта для его администратора.
func (a *Auth) ListTokens(ctx context.Context, id Identity, projectKey string) ([]domain.AccessToken, error) {
	q := a.store.DB()
	if projectKey == "" {
		return a.store.ListTokens(ctx, q, &id.Principal.ID, nil)
	}
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return nil, err
	}
	if err := a.Require(ctx, id, project.ID, domain.MemberAdmin); err != nil {
		return nil, err
	}
	return a.store.ListTokens(ctx, q, nil, &project.ID)
}

// RevokeToken — свой токен, токен своего проекта (администратор) или любой
// (администратор системы).
func (a *Auth) RevokeToken(ctx context.Context, id Identity, tokenID uuid.UUID) error {
	q := a.store.DB()
	t, err := a.store.GetToken(ctx, q, tokenID)
	if err != nil {
		return err
	}
	switch {
	case t.PrincipalID == id.Principal.ID, id.Principal.IsAdmin:
	case t.ProjectID != nil:
		if err := a.Require(ctx, id, *t.ProjectID, domain.MemberAdmin); err != nil {
			return err
		}
	default:
		return apperr.Forbidden("чужой токен отзывает только его владелец или администратор")
	}
	return a.store.RevokeToken(ctx, q, tokenID)
}

// ── права ───────────────────────────────────────────────────────────────────

// RequireAdmin — действие для администратора системы: завести проект,
// выдать токен вне проекта.
func (a *Auth) RequireAdmin(id Identity) error {
	if !a.Enforced() || id.Principal.IsAdmin {
		return nil
	}
	return apperr.Forbidden("нужны права администратора системы")
}

// Require проверяет, что актору хватает уровня need в проекте.
//
// Токен, привязанный к другому проекту, отбивается даже у администратора:
// смысл привязки — ограничить ущерб от утечки, а не отражать права владельца.
func (a *Auth) Require(ctx context.Context, id Identity, projectID uuid.UUID, need domain.MemberRole) error {
	if !a.Enforced() {
		return nil
	}
	if id.Token != nil && id.Token.ProjectID != nil && *id.Token.ProjectID != projectID {
		return apperr.Forbidden("токен привязан к другому проекту")
	}
	role, err := a.ProjectRole(ctx, id, projectID)
	if err != nil {
		return err
	}
	if !role.Covers(need) {
		if role == "" {
			return apperr.Forbidden("нет доступа к проекту")
		}
		return apperr.Forbidden("нужен уровень %s, а есть %s", need, role)
	}
	return nil
}

// ProjectRole — действующий уровень прав в проекте. Администратор системы
// везде администратор; пустая строка — не участник.
// Level — действующий уровень актора в проекте для правил, которые движок
// применяет сам (например, «агент заводит только детей своей задачи»).
// В режиме заголовка прав нет вовсе, и уровень максимальный: правило прав
// выключено целиком, а не наполовину.
func (a *Auth) Level(ctx context.Context, id Identity, projectID uuid.UUID) (domain.MemberRole, error) {
	if !a.Enforced() {
		return domain.MemberAdmin, nil
	}
	return a.ProjectRole(ctx, id, projectID)
}

func (a *Auth) ProjectRole(ctx context.Context, id Identity, projectID uuid.UUID) (domain.MemberRole, error) {
	if id.Principal.IsAdmin {
		return domain.MemberAdmin, nil
	}
	// Максимум из прямого участия, участия через команды и владения командой
	// (решение 0027) — одним запросом: проверка прав идёт на каждый запрос.
	return a.store.EffectiveRole(ctx, a.store.DB(), projectID, id.Principal.ID)
}

// AllowedRoles — какие роли специалистов агент может забирать. nil — любые.
func (id Identity) AllowedRoles() []string {
	if id.Token == nil || len(id.Token.RoleKeys) == 0 {
		return nil
	}
	return id.Token.RoleKeys
}

// VisibleProjects — проекты, доступные актору. Без проверки прав — все.
func (a *Auth) VisibleProjects(ctx context.Context, id Identity) ([]domain.Project, error) {
	if !a.Enforced() {
		return a.store.ListProjects(ctx, a.store.DB())
	}
	return a.store.ListProjectsFor(ctx, a.store.DB(), id.Principal)
}

// ── участники ───────────────────────────────────────────────────────────────

func (a *Auth) ListMembers(ctx context.Context, id Identity, projectKey string) ([]domain.ProjectMember, error) {
	q := a.store.DB()
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return nil, err
	}
	if err := a.Require(ctx, id, project.ID, domain.MemberViewer); err != nil {
		return nil, err
	}
	return a.store.ListMembers(ctx, q, project.ID)
}

// SetMember добавляет участника или меняет его уровень. Принципал заводится,
// если его ещё нет: администратор добавляет «user:alice» до того, как та
// впервые придёт с токеном.
func (a *Auth) SetMember(ctx context.Context, id Identity, projectKey, principal string, role domain.MemberRole) (domain.ProjectMember, error) {
	if !role.Valid() {
		return domain.ProjectMember{}, apperr.InvalidInput(
			"уровень участия: viewer, agent, maintainer или admin, а не %q", role)
	}
	var out domain.ProjectMember
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := a.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		if err := a.Require(ctx, id, project.ID, domain.MemberAdmin); err != nil {
			return err
		}
		kind, handle := SplitHandle(principal, domain.PrincipalUser)
		p, err := a.store.ResolveActor(ctx, tx, handle, kind)
		if err != nil {
			return err
		}
		if role != domain.MemberAdmin && p.ID == id.Principal.ID {
			if err := a.guardLastAdmin(ctx, tx, project.ID, p.ID); err != nil {
				return err
			}
		}
		out, err = a.store.SetMember(ctx, tx, project.ID, p.ID, role, id.Principal.ID)
		return err
	})
	return out, err
}

func (a *Auth) RemoveMember(ctx context.Context, id Identity, projectKey, principal string) error {
	return a.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := a.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		if err := a.Require(ctx, id, project.ID, domain.MemberAdmin); err != nil {
			return err
		}
		_, handle := SplitHandle(principal, domain.PrincipalUser)
		p, err := a.store.GetPrincipalByHandle(ctx, tx, handle)
		if err != nil {
			return err
		}
		if err := a.guardLastAdmin(ctx, tx, project.ID, p.ID); err != nil {
			return err
		}
		return a.store.RemoveMember(ctx, tx, project.ID, p.ID)
	})
}

// guardLastAdmin не даёт оставить проект без администратора.
func (a *Auth) guardLastAdmin(ctx context.Context, q postgres.Querier, projectID, principalID uuid.UUID) error {
	role, err := a.store.MemberRole(ctx, q, projectID, principalID)
	if err != nil {
		return err
	}
	if role != domain.MemberAdmin {
		return nil
	}
	n, err := a.store.CountAdmins(ctx, q, projectID)
	if err != nil {
		return err
	}
	if n <= 1 {
		return apperr.Conflict("это последний администратор проекта; сначала назначьте другого")
	}
	return nil
}

// Me — ответ /auth/me: кто я, где состою, чем доказал.
type Me struct {
	Mode          string                 `json:"mode"`
	Authenticated bool                   `json:"authenticated"`
	Principal     domain.Principal       `json:"principal"`
	Token         *domain.AccessToken    `json:"token,omitempty"`
	Memberships   []domain.ProjectMember `json:"memberships"`
	// Команды показываются отдельно от участия в проектах: права из них
	// приходят в проекты, которых в memberships нет.
	Teams []domain.TeamMember `json:"teams"`
}

func (a *Auth) Me(ctx context.Context, id Identity) (Me, error) {
	q := a.store.DB()
	memberships, err := a.store.Memberships(ctx, q, id.Principal.ID)
	if err != nil {
		return Me{}, err
	}
	teams, err := a.store.TeamsOf(ctx, q, id.Principal.ID)
	if err != nil {
		return Me{}, err
	}
	return Me{
		Mode: a.mode, Authenticated: id.Authenticated(),
		Principal: id.Principal, Token: id.Token, Memberships: memberships, Teams: teams,
	}, nil
}

// SplitHandle разбирает "agent:bot" на вид и хэндл. Без префикса — вид по
// умолчанию; неизвестный префикс остаётся частью хэндла ("human:ci").
func SplitHandle(raw string, defaultKind domain.PrincipalKind) (domain.PrincipalKind, string) {
	raw = strings.TrimSpace(raw)
	if before, after, found := strings.Cut(raw, ":"); found {
		switch domain.PrincipalKind(before) {
		case domain.PrincipalAgent, domain.PrincipalUser, domain.PrincipalSystem:
			return domain.PrincipalKind(before), after
		}
	}
	return defaultKind, raw
}
