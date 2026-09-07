// Package oidc — проверка ID-токена корпоративного провайдера.
//
// Обмен кода на токены делает фронтенд (authorization code flow с PKCE,
// решение 0027): бэкенд не хранит секрет клиента и не участвует в редиректах,
// ему предъявляют готовый ID-токен. Здесь — ровно то, что нужно, чтобы этому
// токену поверить: дискавери провайдера, ключи подписи и проверка утверждений.
//
// Библиотеки ради этого не берутся по той же причине, что и у GitHub с Voyage:
// нужны две ручки провайдера и разбор JWT, а зависимость пришлось бы тащить в
// статическую сборку и обновлять.
package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrInvalidToken — токену верить нельзя: подпись, срок, издатель или
// аудитория не сошлись. Причина в тексте, но код один: клиенту в любом случае
// нужно войти заново.
var ErrInvalidToken = errors.New("oidc: ID-токен недействителен")

// Claims — то, что система берёт из ID-токена. Остальные утверждения
// провайдера игнорируются: чем меньше система знает о человеке, тем меньше ей
// хранить.
type Claims struct {
	Subject string
	Email   string
	Name    string
	// Группы провайдера — по ним синхронизируется состав команд.
	Groups []string
}

// Config — настройки провайдера. Пустой Issuer означает, что вход через
// провайдера выключен.
type Config struct {
	Issuer   string
	ClientID string
	// Клейм с группами: у провайдеров он называется по-разному (groups,
	// roles, cognito:groups).
	GroupsClaim string
	HTTP        *http.Client
}

// Discovery — то, что провайдер рассказывает о себе. Фронтенду нужен адрес
// авторизации и токенов, бэкенду — адрес ключей.
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	EndSessionEndpoint    string `json:"end_session_endpoint,omitempty"`
}

type Verifier struct {
	cfg  Config
	http *http.Client

	mu        sync.Mutex
	discovery *Discovery
	keys      map[string]*rsa.PublicKey
	// Ключи провайдер меняет, и перечитывать их на каждый вход — лишний
	// поход в сеть на каждый вход. Перечитываются по расписанию и при
	// неизвестном kid.
	keysFetched time.Time
}

const (
	keysTTL     = time.Hour
	httpTimeout = 10 * time.Second
	// Допуск на расхождение часов: без него вход ломается от секунды разницы
	// между провайдером и сервисом. Минуты хватает — это про рассинхрон NTP,
	// а не про продление срока токена.
	clockSkew = time.Minute
)

func New(cfg Config) *Verifier {
	cfg.Issuer = strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	client := cfg.HTTP
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	return &Verifier{cfg: cfg, http: client, keys: map[string]*rsa.PublicKey{}}
}

// Enabled — настроен ли вход через провайдера.
func (v *Verifier) Enabled() bool { return v.cfg.Issuer != "" && v.cfg.ClientID != "" }

func (v *Verifier) ClientID() string { return v.cfg.ClientID }
func (v *Verifier) Issuer() string   { return v.cfg.Issuer }

// Discovery отдаёт конфигурацию провайдера; результат кэшируется вместе с
// ключами.
func (v *Verifier) Discovery(ctx context.Context) (Discovery, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.loadLocked(ctx, false); err != nil {
		return Discovery{}, err
	}
	return *v.discovery, nil
}

// Verify проверяет подпись и утверждения ID-токена и отдаёт то, что из него
// нужно системе.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	if !v.Enabled() {
		return Claims{}, fmt.Errorf("%w: провайдер не настроен", ErrInvalidToken)
	}
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: это не JWT", ErrInvalidToken)
	}
	headerJSON, err := decodeSegment(parts[0])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: заголовок не читается", ErrInvalidToken)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return Claims{}, fmt.Errorf("%w: заголовок не читается", ErrInvalidToken)
	}
	hash, err := hashFor(header.Alg)
	if err != nil {
		return Claims{}, err
	}

	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return Claims{}, err
	}
	signature, err := decodeSegment(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: подпись не читается", ErrInvalidToken)
	}
	digest := sum(hash, parts[0]+"."+parts[1])
	if err := rsa.VerifyPKCS1v15(key, hash, digest, signature); err != nil {
		return Claims{}, fmt.Errorf("%w: подпись не сходится", ErrInvalidToken)
	}

	payload, err := decodeSegment(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: тело не читается", ErrInvalidToken)
	}
	return v.claims(payload)
}

// claims разбирает тело токена и проверяет издателя, аудиторию и срок.
func (v *Verifier) claims(payload []byte) (Claims, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		return Claims{}, fmt.Errorf("%w: тело не читается", ErrInvalidToken)
	}
	issuer := strings.TrimRight(stringClaim(body["iss"]), "/")
	if issuer != v.cfg.Issuer {
		return Claims{}, fmt.Errorf("%w: издатель %q, а ожидался %q", ErrInvalidToken, issuer, v.cfg.Issuer)
	}
	// Токен, выписанный другому клиенту, не годится: иначе токеном соседнего
	// приложения того же провайдера можно было бы войти сюда.
	if !contains(stringsClaim(body["aud"]), v.cfg.ClientID) {
		return Claims{}, fmt.Errorf("%w: токен выписан другому клиенту", ErrInvalidToken)
	}
	exp, ok := timeClaim(body["exp"])
	if !ok || time.Now().After(exp.Add(clockSkew)) {
		return Claims{}, fmt.Errorf("%w: срок действия истёк", ErrInvalidToken)
	}
	if nbf, ok := timeClaim(body["nbf"]); ok && time.Now().Add(clockSkew).Before(nbf) {
		return Claims{}, fmt.Errorf("%w: токен ещё не действует", ErrInvalidToken)
	}
	subject := stringClaim(body["sub"])
	if subject == "" {
		return Claims{}, fmt.Errorf("%w: в токене нет sub", ErrInvalidToken)
	}
	name := stringClaim(body["name"])
	if name == "" {
		name = stringClaim(body["preferred_username"])
	}
	return Claims{
		Subject: subject,
		Email:   stringClaim(body["email"]),
		Name:    name,
		Groups:  stringsClaim(body[v.cfg.GroupsClaim]),
	}, nil
}

// key отдаёт ключ подписи по kid, перечитывая набор, если такого ключа нет:
// провайдер меняет ключи без предупреждения, и вход не должен ломаться до
// перезапуска сервиса.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if err := v.loadLocked(ctx, false); err != nil {
		return nil, err
	}
	if key, ok := v.lookupLocked(kid); ok {
		return key, nil
	}
	if err := v.loadLocked(ctx, true); err != nil {
		return nil, err
	}
	if key, ok := v.lookupLocked(kid); ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: ключ подписи неизвестен", ErrInvalidToken)
}

func (v *Verifier) lookupLocked(kid string) (*rsa.PublicKey, bool) {
	if kid == "" && len(v.keys) == 1 {
		// Провайдер вправе не проставлять kid, если ключ один.
		for _, key := range v.keys {
			return key, true
		}
	}
	key, ok := v.keys[kid]
	return key, ok
}

func (v *Verifier) loadLocked(ctx context.Context, force bool) error {
	if !force && v.discovery != nil && time.Since(v.keysFetched) < keysTTL {
		return nil
	}
	if v.discovery == nil || force {
		var d Discovery
		if err := v.fetch(ctx, v.cfg.Issuer+"/.well-known/openid-configuration", &d); err != nil {
			return err
		}
		if strings.TrimRight(d.Issuer, "/") != v.cfg.Issuer {
			return fmt.Errorf("oidc: провайдер представился как %q, а настроен %q", d.Issuer, v.cfg.Issuer)
		}
		v.discovery = &d
	}
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := v.fetch(ctx, v.discovery.JWKSURI, &jwks); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err := decodeSegment(k.N)
		if err != nil {
			continue
		}
		e, err := decodeSegment(k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent(e))}
	}
	if len(keys) == 0 {
		return errors.New("oidc: у провайдера нет ни одного RSA-ключа подписи")
	}
	v.keys = keys
	v.keysFetched = time.Now()
	return nil
}

func (v *Verifier) fetch(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return fmt.Errorf("oidc: %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: %s ответил %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("oidc: %s: ответ не разбирается: %w", url, err)
	}
	return nil
}

// ── разбор JWT ──────────────────────────────────────────────────────────────

// hashFor допускает только RSA с SHA-2. HS256 не поддерживается сознательно:
// симметричный алгоритм в ID-токене означал бы общий секрет с провайдером, а
// «alg: none» — известная дыра.
func hashFor(alg string) (crypto.Hash, error) {
	switch alg {
	case "RS256":
		return crypto.SHA256, nil
	case "RS384":
		return crypto.SHA384, nil
	case "RS512":
		return crypto.SHA512, nil
	}
	return 0, fmt.Errorf("%w: алгоритм подписи %q не поддерживается", ErrInvalidToken, alg)
}

func sum(hash crypto.Hash, data string) []byte {
	switch hash {
	case crypto.SHA384:
		d := sha512.Sum384([]byte(data))
		return d[:]
	case crypto.SHA512:
		d := sha512.Sum512([]byte(data))
		return d[:]
	default:
		d := sha256.Sum256([]byte(data))
		return d[:]
	}
}

func decodeSegment(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// exponent разворачивает показатель из JWK: он приезжает big-endian числом
// произвольной длины, а обычно это три байта.
func exponent(b []byte) uint64 {
	padded := make([]byte, 8)
	if len(b) > 8 {
		b = b[len(b)-8:]
	}
	copy(padded[8-len(b):], b)
	return binary.BigEndian.Uint64(padded)
}

func stringClaim(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// stringsClaim читает клейм, который бывает и строкой, и списком: aud и groups
// провайдеры отдают по-разному.
func stringsClaim(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	if s := stringClaim(raw); s != "" {
		return []string{s}
	}
	return nil
}

func timeClaim(raw json.RawMessage) (time.Time, bool) {
	var seconds float64
	if len(raw) == 0 || json.Unmarshal(raw, &seconds) != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0), true
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
