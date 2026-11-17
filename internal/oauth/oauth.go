// Package oauth — протокольная часть сервера авторизации OAuth 2.1 для
// удалённых MCP-клиентов (CLAB-481, решение 0079): метаданные (RFC 8414,
// RFC 9728), публичный адрес инстанса, сверка redirect_uri, PKCE и загрузка
// документа клиента (CIMD) с защитой от SSRF.
//
// Пакет не знает ни базы, ни прав: состояние выдач и правила «кто может»
// живут в workflow.OAuth, транспорт — в httpapi. Здесь только то, что
// проверяется по самим строкам протокола.
package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/url"
	"strings"
	"unicode"
)

// Скоупы (решение 0079, п. 7): права человека целиком и refresh.
const (
	ScopeCadence = "cadence"
	ScopeOffline = "offline_access"
)

// SupportedScopes — scopes_supported обоих документов метаданных.
var SupportedScopes = []string{ScopeCadence, ScopeOffline}

// Пути метаданных и ручек. Ручки — под /v1/oauth: прокси пакета отдают
// /v1/* в api без нового маршрута; метаданные — /.well-known, где их ищут
// клиенты по RFC.
const (
	PathAuthServerMetadata = "/.well-known/oauth-authorization-server"
	PathResourceMetadata   = "/.well-known/oauth-protected-resource"
	PathAuthorize          = "/v1/oauth/authorize"
	PathToken              = "/v1/oauth/token"
	PathRegister           = "/v1/oauth/register"
	PathRevoke             = "/v1/oauth/revoke"
	// ConsentPath — страница согласия интерфейса; id запроса дописывается.
	ConsentPath = "/login/oauth/"
)

// ResourceMetadata — метаданные защищённого ресурса (RFC 9728).
type ResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// ResourceFor — метаданные ресурса по пути MCP: resource — публичный адрес
// плюс путь ровно в том виде, в каком он пришёл. Одна функция на адрес
// проекта и инстансовый адрес: инстансовый (CLAB-483) подключает её, не
// копируя правила.
func ResourceFor(issuer, mcpPath string) ResourceMetadata {
	return ResourceMetadata{
		Resource:               issuer + mcpPath,
		AuthorizationServers:   []string{issuer},
		ScopesSupported:        SupportedScopes,
		BearerMethodsSupported: []string{"header"},
	}
}

// ResourceMetadataURL — адрес метаданных ресурса для пути MCP (RFC 9728,
// §3.1: путь ресурса дописывается за well-known).
func ResourceMetadataURL(issuer, mcpPath string) string {
	return issuer + PathResourceMetadata + mcpPath
}

// ServerMetadata — метаданные сервера авторизации (RFC 8414).
type ServerMetadata struct {
	Issuer                                 string   `json:"issuer"`
	AuthorizationEndpoint                  string   `json:"authorization_endpoint"`
	TokenEndpoint                          string   `json:"token_endpoint"`
	RevocationEndpoint                     string   `json:"revocation_endpoint"`
	RegistrationEndpoint                   string   `json:"registration_endpoint,omitempty"`
	ResponseTypesSupported                 []string `json:"response_types_supported"`
	GrantTypesSupported                    []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported          []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported      []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethodsSupported []string `json:"revocation_endpoint_auth_methods_supported"`
	ScopesSupported                        []string `json:"scopes_supported"`
	ClientIDMetadataDocumentSupported      bool     `json:"client_id_metadata_document_supported"`
	AuthorizationResponseISSParameter      bool     `json:"authorization_response_iss_parameter_supported"`
}

// ServerFor — метаданные сервера авторизации: registration_endpoint — только
// при включённом DCR, признак CIMD — по его выключателю.
func ServerFor(issuer string, dcr, cimd bool) ServerMetadata {
	m := ServerMetadata{
		Issuer:                                 issuer,
		AuthorizationEndpoint:                  issuer + PathAuthorize,
		TokenEndpoint:                          issuer + PathToken,
		RevocationEndpoint:                     issuer + PathRevoke,
		ResponseTypesSupported:                 []string{"code"},
		GrantTypesSupported:                    []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:          []string{"S256"},
		TokenEndpointAuthMethodsSupported:      []string{"none"},
		RevocationEndpointAuthMethodsSupported: []string{"none"},
		ScopesSupported:                        SupportedScopes,
		ClientIDMetadataDocumentSupported:      cimd,
		AuthorizationResponseISSParameter:      true,
	}
	if dcr {
		m.RegistrationEndpoint = issuer + PathRegister
	}
	return m
}

// NormalizeScope разбирает scope запроса: пусто — оба скоупа; чужой скоуп —
// ok=false. cadence есть всегда: это единственное право выдачи.
func NormalizeScope(raw string) (string, bool) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ScopeCadence + " " + ScopeOffline, true
	}
	offline := false
	for _, f := range fields {
		switch f {
		case ScopeCadence:
		case ScopeOffline:
			offline = true
		default:
			return "", false
		}
	}
	if offline {
		return ScopeCadence + " " + ScopeOffline, true
	}
	return ScopeCadence, true
}

// VerifyS256 сверяет code_verifier с code_challenge (RFC 7636, S256).
func VerifyS256(verifier, challenge string) bool {
	if !validPKCE(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

// ValidChallenge — code_challenge S256: 43 символа base64url без
// выравнивания (32 байта sha256).
func ValidChallenge(challenge string) bool {
	if len(challenge) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(challenge)
	return err == nil
}

// validPKCE — code_verifier по RFC 7636, §4.1: 43–128 символов
// [A-Za-z0-9-._~].
func validPKCE(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '-', r == '.', r == '_', r == '~':
		default:
			return false
		}
	}
	return true
}

// MaxClientName — предел имени клиента в метке токена.
const MaxClientName = 100

// CleanName — имя клиента для метки и экрана: управляющие символы
// вырезаны, пробелы по краям сняты, не длиннее 100 символов.
func CleanName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if runes := []rune(out); len(runes) > MaxClientName {
		out = strings.TrimSpace(string(runes[:MaxClientName]))
	}
	return out
}

// IsLoopbackHost — имя хоста петли: localhost, 127.0.0.0/8 и ::1.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HostOf — хост адреса без порта; пусто — не разбирается.
func HostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
