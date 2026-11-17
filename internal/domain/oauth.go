package domain

import (
	"time"

	"github.com/google/uuid"
)

// Сервер авторизации OAuth 2.1 для удалённых MCP-клиентов (решение 0079).

// Способ, которым клиент известен инстансу.
const (
	// OAuthClientDCR — зарегистрирован по RFC 7591, строка oauth_clients.
	OAuthClientDCR = "dcr"
	// OAuthClientCIMD — client_id — https-адрес документа клиента; строки нет.
	OAuthClientCIMD = "cimd"
)

// Причины отзыва выдачи (oauth_grants.revoke_reason).
const (
	OAuthRevokedByUser       = "user"
	OAuthRevokedRefreshReuse = "refresh_reuse"
	OAuthRevokedCodeReuse    = "code_reuse"
	OAuthRevokedExpired      = "expired"
)

// TokenOAuthGrant — ссылка строки токена на выдачу в списке токенов.
type TokenOAuthGrant struct {
	ID           uuid.UUID `json:"id"`
	ClientName   string    `json:"client_name"`
	RedirectHost string    `json:"redirect_host"`
}

// OAuthClient — клиент DCR. Секретов нет: клиенты только публичные.
type OAuthClient struct {
	ID              string    `json:"client_id"`
	Name            string    `json:"client_name"`
	RedirectURIs    []string  `json:"redirect_uris"`
	SoftwareID      *string   `json:"software_id,omitempty"`
	SoftwareVersion *string   `json:"software_version,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// OAuthGrant — подключённый клиент человека. Отзыв — revoked_at его токена.
type OAuthGrant struct {
	ID           uuid.UUID  `json:"id"`
	TokenID      uuid.UUID  `json:"token_id"`
	PrincipalID  uuid.UUID  `json:"principal_id"`
	ClientID     string     `json:"client_id"`
	ClientKind   string     `json:"client_kind"`
	ClientName   string     `json:"client_name"`
	RedirectHost string     `json:"redirect_host"`
	Resource     string     `json:"resource"`
	ProjectID    *uuid.UUID `json:"project_id,omitempty"`
	ProjectKey   *string    `json:"project_key,omitempty"`
	Scope        string     `json:"scope"`
	CreatedAt    time.Time  `json:"created_at"`
	RefreshedAt  *time.Time `json:"refreshed_at,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokeReason *string    `json:"revoke_reason,omitempty"`
}

// OAuthAuthRequest — запрос авторизации от authorize до обмена кода. Хеш
// кода наружу не уходит: у полей хеша нет JSON-имени.
type OAuthAuthRequest struct {
	ID            uuid.UUID
	ClientID      string
	ClientKind    string
	ClientName    string
	RedirectURI   string
	State         *string
	CodeChallenge string
	Scope         string
	Resource      string
	ProjectKey    *string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	Decision      *string
	DecidedAt     *time.Time
	DecidedBy     *uuid.UUID
	CodeHash      *string
	CodeExpiresAt *time.Time
	CodeUsedAt    *time.Time
	GrantID       *uuid.UUID
}

// OAuthRefresh — выпущенный refresh выдачи (хранится только хеш).
type OAuthRefresh struct {
	ID        uuid.UUID
	GrantID   uuid.UUID
	ParentID  *uuid.UUID
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
}
