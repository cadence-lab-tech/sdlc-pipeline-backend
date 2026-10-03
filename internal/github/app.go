package github

// GitHub App: доступ к репозиториям организации через установку приложения
// (решение 0028).
//
// Секрет один на инстанс — приватный ключ приложения. Проект ссылается на
// установку (`projects.github_installation_id`), а это идентификатор, а не
// секрет: его можно хранить, показывать в настройках и переносить связкой.
// Токен по установке выписывается на час и живёт в памяти процесса.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type App struct {
	appID   string
	key     *rsa.PrivateKey
	baseURL string
	http    *http.Client

	mu     sync.Mutex
	tokens map[int64]installationToken
	// offset — поправка к местным часам, снятая с ответа провайдера.
	offset time.Duration
}

type installationToken struct {
	value   string
	expires time.Time
}

// tokenSkew — насколько раньше срока токен считается просроченным. Прогон
// индексации длится минуты, и токен, годный ещё десять секунд, протухнет
// посреди него.
const tokenSkew = 5 * time.Minute

// clockBackdate — насколько iat утверждения отодвигается в прошлое, и
// clockTolerance — расхождение часов, которое это закрывает само.
//
// Часы машины уезжают: на VPS без NTP набежало три минуты за три недели, и
// GitHub стал отбивать утверждение, выписанное «в будущем», с 401 Bad
// credentials — неотличимо от отозванного ключа. Расхождение больше
// допуска поэтому измеряется по заголовку Date ответа, а не угадывается
// запасом в iat: запас пришлось бы брать на любой мыслимый дрейф, а срок
// жизни утверждения ограничен десятью минутами.
const (
	clockBackdate  = time.Minute
	clockTolerance = 30 * time.Second
)

// NewApp читает приватный ключ приложения в формате PEM (PKCS#1 или PKCS#8).
func NewApp(baseURL, appID string, keyPEM []byte) (*App, error) {
	if appID == "" {
		return nil, errors.New("github: не задан идентификатор приложения")
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("github: ключ приложения не в формате PEM")
	}
	var key *rsa.PrivateKey
	switch parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); {
	case err == nil:
		key = parsed
	default:
		any8, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("github: ключ приложения не разбирается: %w", err)
		}
		rsaKey, ok := any8.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github: ключ приложения не RSA")
		}
		key = rsaKey
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &App{
		appID: appID, key: key, baseURL: baseURL,
		http:   &http.Client{Timeout: 30 * time.Second},
		tokens: map[int64]installationToken{},
	}, nil
}

// InstallationToken отдаёт токен установки, выписывая новый, когда прежний
// подходит к концу. Кэш в памяти, а не в базе: токен — секрет, и место ему
// там, где он и так живёт всё время работы процесса.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	a.mu.Lock()
	cached, ok := a.tokens[installationID]
	offset := a.offset
	a.mu.Unlock()
	// Срок приходит от провайдера, значит и сравнивать его надо с временем
	// провайдера: по уехавшим часам токен либо выбрасывается живым, либо
	// переживает свой срок.
	if ok && time.Now().Add(offset).Before(cached.expires.Add(-tokenSkew)) {
		return cached.value, nil
	}

	// Вторая попытка бывает ровно одна и только после измеренной поправки
	// часов: отбитое по времени утверждение перевыписывается, а неверный
	// ключ не крутится по кругу.
	for attempt := 0; ; attempt++ {
		resp, body, err := a.postAccessToken(ctx, installationID)
		if err != nil {
			return "", err
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return "", fmt.Errorf("%w: установка %d", ErrNotFound, installationID)
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0 &&
			a.observeClock(resp.Header.Get("Date")):
			continue
		case resp.StatusCode != http.StatusCreated:
			return "", fmt.Errorf("github %d на токен установки %d: %s%s",
				resp.StatusCode, installationID, snippet(body), a.clockNote())
		}
		var out struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return "", fmt.Errorf("github: ответ на токен установки не разбирается: %w", err)
		}

		a.mu.Lock()
		a.tokens[installationID] = installationToken{value: out.Token, expires: out.ExpiresAt}
		a.mu.Unlock()
		return out.Token, nil
	}
}

// postAccessToken — один поход за токеном установки. Тело прочитано целиком,
// у ответа остаются статус и заголовки: по Date снимается поправка часов.
func (a *App) postAccessToken(ctx context.Context, installationID int64) (*http.Response, []byte, error) {
	appJWT, err := a.jwt()
	if err != nil {
		return nil, nil, err
	}
	url := a.baseURL + "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Authorization", "Bearer "+appJWT)

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("github: токен установки %d: %w", installationID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

// observeClock снимает поправку часов с заголовка Date. true — расхождение
// за допуском и поправка изменилась, то есть утверждение стоит перевыписать.
func (a *App) observeClock(date string) bool {
	at, err := http.ParseTime(date)
	if err != nil {
		return false
	}
	offset := time.Until(at)
	if offset.Abs() < clockTolerance {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.offset == offset {
		return false
	}
	a.offset = offset
	return true
}

// providerNow — время провайдера: местные часы с измеренной поправкой.
func (a *App) providerNow() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Add(a.offset)
}

// clockNote — приписка к отказу, когда часы разошлись с провайдером. Без неё
// 401 на утверждение приложения читают как отозванный ключ и ищут поломку
// не там: на этом однажды потеряли восемнадцать дней индексации.
func (a *App) clockNote() string {
	a.mu.Lock()
	offset := a.offset
	a.mu.Unlock()
	if offset.Abs() < clockTolerance {
		return ""
	}
	direction := "впереди"
	if offset > 0 {
		direction = "позади"
	}
	return fmt.Sprintf(" (часы машины %s времени провайдера на %s — проверьте синхронизацию времени)",
		direction, offset.Abs().Round(time.Second))
}

// jwt подписывает утверждение приложения его ключом. Живёт минуты: им только
// обменивают на токен установки.
func (a *App) jwt() (string, error) {
	now := a.providerNow()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		// iat в прошлое: GitHub отбивает токен, выписанный «в будущем», а
		// часы сервиса и провайдера расходятся. Мелкое расхождение закрывает
		// этот запас, крупное — поправка в providerNow.
		"iat": now.Add(-clockBackdate).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.appID,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github: подпись утверждения приложения: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// LoadApp читает приложение из настроек. nil без ошибки — приложение не
// настроено, и остаётся запасной путь: именованный секрет или общий токен.
func LoadApp(baseURL, appID, keyFile string) (*App, error) {
	if appID == "" || keyFile == "" {
		return nil, nil
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("github: ключ приложения %s: %w", keyFile, err)
	}
	return NewApp(baseURL, appID, key)
}

// ── выбор учётных данных ────────────────────────────────────────────────────

// Credentials — чем ходить в провайдера за конкретным репозиторием.
// Выбирает их CredentialRefs.Resolve; пустые означают общий токен инстанса.
type Credentials struct {
	// Установка приложения — репозитория или проекта.
	InstallationID *int64
	// Имя секрета в окружении: запасной путь для GitHub Enterprise без
	// приложения. В базе лежит имя, значение приезжает из окружения.
	SecretRef string
}

// CredentialSource — откуда взяты учётные данные репозитория. Значения —
// часть протокола: их отдаёт проверка доступа и показывает интерфейс.
type CredentialSource string

const (
	SourceRepositorySecret       CredentialSource = "repository_secret"
	SourceRepositoryInstallation CredentialSource = "repository_installation"
	SourceProjectInstallation    CredentialSource = "project_installation"
	SourceProjectSecret          CredentialSource = "project_secret"
	SourceInstance               CredentialSource = "instance"
)

// CredentialRefs — всё, что репозиторий и проект объявили о доступе к
// провайдеру: имена секретов и номера установок, без значений.
type CredentialRefs struct {
	RepositorySecret       string
	RepositoryInstallation *int64
	ProjectInstallation    *int64
	ProjectSecret          string
}

// Resolve выбирает учётные данные в порядке решения 0034, от частного к
// общему: секрет репозитория, установка репозитория, установка проекта,
// секрет проекта, токен инстанса. Частное побеждает, потому что его задают
// ради исключения из общего правила.
//
// Берётся первый заданный источник, и в Credentials заполнено не больше
// одного поля. Отказ выбранного источника — не повод пробовать следующий:
// читают репозиторий им, и доступ, которым никто не пользуется, ничего не
// доказывает.
func (r CredentialRefs) Resolve() (Credentials, CredentialSource) {
	switch {
	case r.RepositorySecret != "":
		return Credentials{SecretRef: r.RepositorySecret}, SourceRepositorySecret
	case r.RepositoryInstallation != nil:
		return Credentials{InstallationID: r.RepositoryInstallation}, SourceRepositoryInstallation
	case r.ProjectInstallation != nil:
		return Credentials{InstallationID: r.ProjectInstallation}, SourceProjectInstallation
	case r.ProjectSecret != "":
		return Credentials{SecretRef: r.ProjectSecret}, SourceProjectSecret
	}
	return Credentials{}, SourceInstance
}

// Provider выдаёт клиента по уже выбранным учётным данным. Порядок выбора
// живёт в CredentialRefs.Resolve; если в Credentials заполнены оба поля,
// секрет берётся раньше установки.
type Provider struct {
	base   *Client
	app    *App
	secret func(ref string) string
}

func NewProvider(base *Client, app *App, secret func(ref string) string) *Provider {
	if secret == nil {
		secret = func(string) string { return "" }
	}
	return &Provider{base: base, app: app, secret: secret}
}

// HasApp — настроено ли приложение. Без него остаётся запасной путь.
func (p *Provider) HasApp() bool { return p != nil && p.app != nil }

// SecretFromEnv переводит имя секрета в токен. В базе лежит имя, а не сам
// токен: секретов в базе нет. Основное имя — SDLC_GITHUB_TOKEN_<NAME>;
// голое имя переменной остаётся ради конфигураций, заведённых до приложения.
func SecretFromEnv(ref string) string {
	name := strings.Map(func(r rune) rune {
		if r == '-' || r == '.' || r == '/' {
			return '_'
		}
		return r
	}, strings.ToUpper(ref))
	if token := os.Getenv("SDLC_GITHUB_TOKEN_" + name); token != "" {
		return token
	}
	return os.Getenv(name)
}

func (p *Provider) Client(ctx context.Context, creds Credentials) (*Client, error) {
	if creds.SecretRef != "" {
		if token := p.secret(creds.SecretRef); token != "" {
			return p.base.WithToken(token), nil
		}
		// Имя есть, а секрета нет — это ошибка конфигурации, и молча ходить
		// общим токеном значило бы дать доступ, которого не хотели.
		return nil, fmt.Errorf("github: секрет %q не найден в окружении", creds.SecretRef)
	}
	if creds.InstallationID != nil && p.app != nil {
		token, err := p.app.InstallationToken(ctx, *creds.InstallationID)
		if err != nil {
			return nil, err
		}
		return p.base.WithToken(token), nil
	}
	return p.base, nil
}
