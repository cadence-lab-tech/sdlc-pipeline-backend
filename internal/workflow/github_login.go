// Вход людей через GitHub (решение 0030).
//
// Тот же путь, что у корпоративного провайдера: проверенный вход превращается
// в принципала по постоянному ключу и в обычный короткоживущий токен. Разница
// только в том, чем доказан вход — кодом авторизации, который бэкенд меняет на
// токен пользователя через секрет клиента. Сам токен пользователя живёт ровно
// в запросе: репозитории система читает установкой приложения, а не глазами
// вошедшего (решение 0028).
package workflow

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// UseGitHub подключает вход через GitHub. Срок сессии общий с провайдером
// OIDC: сессия — одна сущность, чем бы она ни была получена.
func (a *Auth) UseGitHub(o *github.OAuth, sessionTTL time.Duration) *Auth {
	if a.sessionTTL <= 0 {
		a.sessionTTL = sessionTTL
	}
	if a.sessionTTL <= 0 {
		a.sessionTTL = 12 * time.Hour
	}
	a.github = o
	return a
}

// GitHubEnabled — настроен ли вход через GitHub.
func (a *Auth) GitHubEnabled() bool { return a.github != nil && a.github.Enabled() }

// GitHubSettings — что нужно странице входа, чтобы увести человека к GitHub.
// Секрета здесь нет: он остаётся на бэкенде, и обмен кода делает он.
type GitHubSettings struct {
	Enabled               bool   `json:"enabled"`
	ClientID              string `json:"client_id,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	// Права, которые просит вход. У GitHub App они заданы самим приложением
	// и параметр игнорируется; у обычного OAuth App нужны явно.
	Scopes string `json:"scopes,omitempty"`
}

func (a *Auth) GitHubSettings() GitHubSettings {
	if !a.GitHubEnabled() {
		return GitHubSettings{Enabled: false}
	}
	return GitHubSettings{
		Enabled: true, ClientID: a.github.ClientID(),
		AuthorizationEndpoint: a.github.AuthorizeURL(),
		Scopes:                "read:user user:email",
	}
}

// LoginGitHub меняет код авторизации на сессию. Команды здесь не
// синхронизируются: организации GitHub не группы провайдера, и состав
// команд ведут руками либо через корпоративный IdP.
func (a *Auth) LoginGitHub(ctx context.Context, code, redirectURI string) (Session, error) {
	if !a.GitHubEnabled() {
		return Session{}, apperr.InvalidInput("вход через GitHub не настроен")
	}
	token, err := a.github.Exchange(ctx, code, redirectURI)
	if err != nil {
		if errors.Is(err, github.ErrOAuthDenied) {
			return Session{}, apperr.Unauthenticated("%s", err.Error())
		}
		return Session{}, apperr.Internal(err, "GitHub не отвечает")
	}
	user, err := a.github.User(ctx, token)
	if err != nil {
		return Session{}, apperr.Internal(err, "GitHub не отдал профиль")
	}

	name := user.Name
	if name == "" {
		name = user.Login
	}
	// Хэндл — логин GitHub: так человека и зовут коллеги. Занятый хэндл
	// разрешает resolveIdP, как и у провайдера.
	handle := sanitizeHandle(user.Login)
	if handle == "" {
		handle = "gh-" + shortSubject(user.Subject())
	}
	p, err := a.resolveIdP(ctx, postgres.IdPPrincipalInput{
		Subject: user.Subject(), Handle: handle, DisplayName: name, Email: user.Email,
	})
	if err != nil {
		return Session{}, err
	}

	raw, hash, prefix, err := NewToken()
	if err != nil {
		return Session{}, err
	}
	expires := time.Now().Add(a.sessionTTL)
	var out Session
	err = a.store.InTx(ctx, func(tx pgx.Tx) error {
		record, err := a.store.CreateToken(ctx, tx, postgres.CreateTokenInput{
			PrincipalID: p.ID, Name: "вход через GitHub",
			TokenHash: hash, Prefix: prefix, ExpiresAt: &expires,
			CreatedBy: p.ID, Kind: domain.TokenSession,
		})
		if err != nil {
			return err
		}
		out = Session{Token: raw, ExpiresAt: expires, Principal: p, Record: record}
		return nil
	})
	return out, err
}
