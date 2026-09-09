// Вход людей через корпоративный провайдер (решение 0027).
//
// Обмен кода на токены делает фронтенд (authorization code flow с PKCE), сюда
// приезжает готовый ID-токен. Проверив его, система выдаёт обычный токен
// `agent_tokens` с коротким сроком: второй транспорт (cookie, JWT в заголовке)
// означал бы вторую ветку в проверке прав, а короткоживущий токен той же
// таблицы — ту же ветку.
package workflow

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/oidc"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// UseOIDC подключает провайдера. Вызывается при старте: без него вход по
// токенам работает как в фазе 4.
func (a *Auth) UseOIDC(v *oidc.Verifier, sessionTTL time.Duration) *Auth {
	if sessionTTL <= 0 {
		sessionTTL = 12 * time.Hour
	}
	a.oidc = v
	a.sessionTTL = sessionTTL
	return a
}

// OIDCEnabled — настроен ли вход через провайдера.
func (a *Auth) OIDCEnabled() bool { return a.oidc != nil && a.oidc.Enabled() }

// OIDCSettings — что нужно фронтенду, чтобы начать authorization code flow.
// Адреса берутся из дискавери провайдера, а не из настроек сервиса: держать
// их в двух местах — значит однажды разъехаться.
type OIDCSettings struct {
	Enabled               bool   `json:"enabled"`
	Issuer                string `json:"issuer,omitempty"`
	ClientID              string `json:"client_id,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint,omitempty"`
	EndSessionEndpoint    string `json:"end_session_endpoint,omitempty"`
	Scopes                string `json:"scopes,omitempty"`
}

func (a *Auth) OIDCSettings(ctx context.Context) (OIDCSettings, error) {
	if !a.OIDCEnabled() {
		return OIDCSettings{Enabled: false}, nil
	}
	d, err := a.oidc.Discovery(ctx)
	if err != nil {
		return OIDCSettings{}, apperr.Internal(err, "провайдер не отвечает")
	}
	return OIDCSettings{
		Enabled: true, Issuer: a.oidc.Issuer(), ClientID: a.oidc.ClientID(),
		AuthorizationEndpoint: d.AuthorizationEndpoint, TokenEndpoint: d.TokenEndpoint,
		EndSessionEndpoint: d.EndSessionEndpoint,
		Scopes:             "openid profile email",
	}, nil
}

// Session — результат входа. Токен показывается один раз, как и любой другой.
type Session struct {
	Token     string             `json:"token"`
	ExpiresAt time.Time          `json:"expires_at"`
	Principal domain.Principal   `json:"principal"`
	Record    domain.AccessToken `json:"record"`
}

// Login проверяет ID-токен, находит или заводит принципала, синхронизирует его
// команды по группам провайдера и выдаёт сессию.
func (a *Auth) Login(ctx context.Context, idToken string) (Session, error) {
	if !a.OIDCEnabled() {
		return Session{}, apperr.InvalidInput("вход через провайдера не настроен")
	}
	claims, err := a.oidc.Verify(ctx, idToken)
	if err != nil {
		return Session{}, apperr.Unauthenticated("%s", err.Error())
	}

	token, hash, prefix, err := NewToken()
	if err != nil {
		return Session{}, err
	}
	expires := time.Now().Add(a.sessionTTL)

	// Принципал резолвится до транзакции: занятый хэндл — это отбитый уникальный
	// индекс, после которого транзакция уже не принимает вторую попытку.
	p, err := a.resolveIdP(ctx, postgres.IdPPrincipalInput{
		Subject: claims.Subject, Handle: handleFromClaims(claims),
		DisplayName: claims.Name, Email: claims.Email,
	})
	if err != nil {
		return Session{}, err
	}

	var out Session
	err = a.store.InTx(ctx, func(tx pgx.Tx) error {
		if err := a.syncTeams(ctx, tx, p.ID, claims.Groups); err != nil {
			return err
		}
		if _, err := a.ensurePersonalTeam(ctx, tx, p); err != nil {
			return err
		}
		record, err := a.store.CreateToken(ctx, tx, postgres.CreateTokenInput{
			PrincipalID: p.ID, Name: "вход через " + a.oidc.Issuer(),
			TokenHash: hash, Prefix: prefix, ExpiresAt: &expires,
			CreatedBy: p.ID, Kind: domain.TokenSession,
		})
		if err != nil {
			return err
		}
		out = Session{Token: token, ExpiresAt: expires, Principal: p, Record: record}
		return nil
	})
	return out, err
}

// Logout отзывает предъявленную сессию. Токен агента так не отзывается: у него
// другой срок жизни и другой владелец — его отзывают по имени в списке.
func (a *Auth) Logout(ctx context.Context, id Identity) error {
	if id.Token == nil {
		return nil
	}
	if id.Token.TokenKind != domain.TokenSession {
		return apperr.InvalidInput("это не сессия, а выданный токен: отзывается из списка токенов")
	}
	return a.store.RevokeToken(ctx, a.store.DB(), id.Token.ID)
}

// resolveIdP находит принципала по ключу у провайдера или заводит его.
// Занятый хэндл не повод отказать во входе: человек в этом не виноват, и
// различить его позволяет ключ. Общий для OIDC и GitHub: провайдеры разные,
// принципал один и тот же.
func (a *Auth) resolveIdP(ctx context.Context, in postgres.IdPPrincipalInput) (domain.Principal, error) {
	q := a.store.DB()
	p, err := a.store.UpsertIdPPrincipal(ctx, q, in)
	if err == nil {
		return p, nil
	}
	if apperr.As(err).Code != apperr.CodeConflict {
		return p, err
	}
	in.Handle = in.Handle + "-" + shortSubject(in.Subject)
	return a.store.UpsertIdPPrincipal(ctx, q, in)
}

// syncTeams приводит состав команд к тому, что сказал провайдер.
//
// Трогаются только команды с настроенным сопоставлением групп: набранную
// руками синхронизация не разбирает. Лид не понижается и не выводится — роль
// в команде назначил человек, а провайдер про неё ничего не знает.
func (a *Auth) syncTeams(ctx context.Context, tx pgx.Tx, principalID uuid.UUID, groups []string) error {
	if groups == nil {
		groups = []string{}
	}
	want, err := a.store.TeamsForIdPGroups(ctx, tx, groups)
	if err != nil {
		return err
	}
	have, err := a.store.SyncedTeamsOf(ctx, tx, principalID)
	if err != nil {
		return err
	}
	wanted := map[uuid.UUID]bool{}
	for _, id := range want {
		wanted[id] = true
	}
	held := map[uuid.UUID]bool{}
	for _, id := range have {
		held[id] = true
	}
	for id := range wanted {
		if held[id] {
			continue
		}
		if _, err := a.store.SetTeamMember(ctx, tx, id, principalID, domain.TeamRoleMember, principalID); err != nil {
			return err
		}
	}
	for id := range held {
		if wanted[id] {
			continue
		}
		role, err := a.store.TeamRoleOf(ctx, tx, id, principalID)
		if err != nil {
			return err
		}
		if role == domain.TeamRoleLead {
			a.log.Warn("лид остался в команде, хотя группа провайдера ушла",
				"team", id, "principal", principalID)
			continue
		}
		if err := a.store.RemoveTeamMember(ctx, tx, id, principalID); err != nil {
			return err
		}
	}
	return nil
}

// handleFromClaims выводит хэндл из того, что дал провайдер: локальная часть
// почты, иначе имя, иначе `sub`. Хэндл — то, чем человека зовут в задачах и
// аудите, поэтому «u-8f3a91» здесь запасной вариант, а не первый.
func handleFromClaims(claims oidc.Claims) string {
	candidates := []string{claims.Email, claims.Name}
	for _, raw := range candidates {
		if before, _, found := strings.Cut(raw, "@"); found {
			raw = before
		}
		if h := sanitizeHandle(raw); h != "" {
			return h
		}
	}
	return "u-" + shortSubject(claims.Subject)
}

func sanitizeHandle(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('.')
		}
	}
	return strings.Trim(b.String(), ".-_")
}

func shortSubject(subject string) string {
	h := HashToken(subject)
	return h[:8]
}

// ── сопоставление групп командам ────────────────────────────────────────────

// TeamIdPGroups — группы провайдера, ведущие в команду. Читает участник
// команды: видеть, откуда берётся её состав, важно и лиду, и обычному
// участнику; правит только администратор системы.
func (a *Auth) TeamIdPGroups(ctx context.Context, id Identity, key string) ([]string, error) {
	q := a.store.DB()
	team, err := a.store.GetTeamByKey(ctx, q, key)
	if err != nil {
		return nil, err
	}
	if _, err := a.teamRole(ctx, q, id, team.ID, false); err != nil {
		return nil, err
	}
	return a.store.ListTeamIdPGroups(ctx, q, team.ID)
}

// SetTeamIdPGroups задаёт группы провайдера, ведущие в команду. Это настройка
// доступа целой группы людей, поэтому право — у администратора системы.
func (a *Auth) SetTeamIdPGroups(ctx context.Context, id Identity, key string, groups []string) ([]string, error) {
	if err := a.RequireAdmin(id); err != nil {
		return nil, err
	}
	var out []string
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		team, err := a.store.GetTeamByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		clean := make([]string, 0, len(groups))
		for _, g := range groups {
			if g = strings.TrimSpace(g); g != "" {
				clean = append(clean, g)
			}
		}
		if err := a.store.SetTeamIdPGroups(ctx, tx, team.ID, clean, id.Principal.ID); err != nil {
			return err
		}
		out, err = a.store.ListTeamIdPGroups(ctx, tx, team.ID)
		return err
	})
	return out, err
}
