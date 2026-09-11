package github

// Вход человека через GitHub (решение 0030) и его установки (решение 0034).
//
// GitHub не OIDC-провайдер: ID-токена у него нет, а код авторизации меняется
// на токен с секретом клиента. Секрет живёт на бэкенде, поэтому обмен делает
// он, а не браузер. Токен пользователя живёт ровно в запросе и нигде не
// сохраняется: на входе им спрашивают, кто вошёл; при подключении GitHub —
// какие установки приложения ему видны и какие репозитории в них он видит
// сам. Читает репозитории всё равно установка приложения, а не вошедший
// (решение 0028).
//
// Регистрация одна: GitHub App умеет быть и OAuth-клиентом, так что вход и
// доступ к репозиториям идут через одно приложение.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrOAuthDenied — код авторизации не принят: просрочен, уже использован или
// выписан другому клиенту. Клиенту в любом случае нужно войти заново.
var ErrOAuthDenied = errors.New("github: код авторизации не принят")

type OAuth struct {
	clientID     string
	clientSecret string
	// Адрес сайта — там живут /login/oauth/*; у GitHub Enterprise он
	// отличается от адреса API.
	webURL string
	apiURL string
	http   *http.Client
}

const defaultWebURL = "https://github.com"

// NewOAuth собирает клиента входа. Пустые идентификатор или секрет означают,
// что вход через GitHub выключен.
func NewOAuth(webURL, apiURL, clientID, clientSecret string) *OAuth {
	if webURL == "" {
		webURL = defaultWebURL
	}
	if apiURL == "" {
		apiURL = defaultBaseURL
	}
	return &OAuth{
		clientID: clientID, clientSecret: clientSecret,
		webURL: strings.TrimRight(webURL, "/"), apiURL: strings.TrimRight(apiURL, "/"),
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled — настроен ли вход через GitHub.
func (o *OAuth) Enabled() bool { return o != nil && o.clientID != "" && o.clientSecret != "" }

func (o *OAuth) ClientID() string { return o.clientID }

// AuthorizeURL — куда браузер уводит человека за кодом.
func (o *OAuth) AuthorizeURL() string { return o.webURL + "/login/oauth/authorize" }

// InstallURL — страница установки приложения на аккаунт: GitHub сам спросит,
// на какой аккаунт и на какие репозитории.
func (o *OAuth) InstallURL(slug string) string {
	return o.webURL + "/apps/" + slug + "/installations/new"
}

// Exchange меняет код авторизации на токен пользователя.
func (o *OAuth) Exchange(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {o.clientID},
		"client_secret": {o.clientSecret},
		"code":          {code},
	}
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.webURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: обмен кода: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github %d на обмен кода: %s", resp.StatusCode, snippet(body))
	}
	// Отказ GitHub отдаёт со статусом 200 и полем error в теле.
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("github: ответ на обмен кода не разбирается: %w", err)
	}
	if out.Error != "" || out.AccessToken == "" {
		reason := out.Description
		if reason == "" {
			reason = out.Error
		}
		return "", fmt.Errorf("%w: %s", ErrOAuthDenied, reason)
	}
	return out.AccessToken, nil
}

// User — кто вошёл. ID постоянен, логин у GitHub можно сменить, поэтому
// принципал ключуется идентификатором.
type User struct {
	ID    int64
	Login string
	Name  string
	Email string
}

// Subject — ключ принципала в principals.idp_subject: с префиксом, чтобы не
// столкнуться с `sub` корпоративного провайдера.
func (u User) Subject() string { return "github:" + strconv.FormatInt(u.ID, 10) }

// User спрашивает GitHub, кто предъявил токен. Почта у профиля часто скрыта;
// тогда берётся основная подтверждённая из списка почт, а если приложению не
// выдали и этого — человек остаётся без почты, входу это не мешает.
func (o *OAuth) User(ctx context.Context, token string) (User, error) {
	var profile struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := o.get(ctx, token, "/user", &profile); err != nil {
		return User{}, err
	}
	if profile.ID == 0 || profile.Login == "" {
		return User{}, errors.New("github: профиль без идентификатора")
	}
	u := User{ID: profile.ID, Login: profile.Login, Name: profile.Name, Email: profile.Email}
	if u.Email == "" {
		var emails []struct {
			Email    string `json:"email"`
			Primary  bool   `json:"primary"`
			Verified bool   `json:"verified"`
		}
		if err := o.get(ctx, token, "/user/emails", &emails); err == nil {
			for _, e := range emails {
				if e.Primary && e.Verified {
					u.Email = e.Email
					break
				}
			}
		}
	}
	return u, nil
}

func (o *OAuth) get(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github %d на %s: %s", resp.StatusCode, path, snippet(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("github: разбор ответа %s: %w", path, err)
	}
	return nil
}

// ── установки, видимые вошедшему ────────────────────────────────────────────

// Installation — установка приложения, к которой у вошедшего есть доступ.
type Installation struct {
	ID           int64
	AccountLogin string
	AccountID    int64
	AccountType  string
}

// UserInstallations — установки приложения, видимые предъявителю токена.
// Это и есть доказательство права на установку: идентификаторы установок
// идут подряд, и без этого списка любой мог бы приписать себе чужую.
func (o *OAuth) UserInstallations(ctx context.Context, token string) ([]Installation, error) {
	var out []Installation
	for page := 1; page <= maxPages; page++ {
		var resp struct {
			Installations []struct {
				ID      int64 `json:"id"`
				Account struct {
					Login string `json:"login"`
					ID    int64  `json:"id"`
					Type  string `json:"type"`
				} `json:"account"`
			} `json:"installations"`
		}
		if err := o.get(ctx, token, "/user/installations?per_page=100&page="+strconv.Itoa(page), &resp); err != nil {
			return nil, err
		}
		for _, i := range resp.Installations {
			out = append(out, Installation{
				ID: i.ID, AccountLogin: i.Account.Login, AccountID: i.Account.ID, AccountType: i.Account.Type,
			})
		}
		if len(resp.Installations) < 100 {
			break
		}
	}
	return out, nil
}

// UserRepository — репозиторий установки, который вошедший видит и сам.
type UserRepository struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	CloneURL      string `json:"clone_url"`
	Description   string `json:"description,omitempty"`
}

// InstallationRepositories — пересечение репозиториев установки с личным
// доступом вошедшего. Участник арендатора может привязать только то, что
// видит сам на GitHub, а не всё, что отдала приложению установка.
func (o *OAuth) InstallationRepositories(ctx context.Context, token string, installationID int64) ([]UserRepository, error) {
	var out []UserRepository
	path := "/user/installations/" + strconv.FormatInt(installationID, 10) + "/repositories"
	for page := 1; page <= maxPages; page++ {
		var resp struct {
			Repositories []struct {
				Name          string `json:"name"`
				DefaultBranch string `json:"default_branch"`
				Private       bool   `json:"private"`
				CloneURL      string `json:"clone_url"`
				Description   string `json:"description"`
				Owner         struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		if err := o.get(ctx, token, path+"?per_page=100&page="+strconv.Itoa(page), &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Repositories {
			out = append(out, UserRepository{
				Owner: r.Owner.Login, Name: r.Name, DefaultBranch: r.DefaultBranch,
				Private: r.Private, CloneURL: r.CloneURL, Description: r.Description,
			})
		}
		if len(resp.Repositories) < 100 {
			break
		}
	}
	return out, nil
}

// maxPages — предел листания: тысяча репозиториев на установку. Больше в
// выпадающий список всё равно не помещается, а организации на десять тысяч
// репозиториев подключают их не пачкой из интерфейса.
const maxPages = 10
