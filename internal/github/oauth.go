package github

// Вход человека через GitHub (решение 0030).
//
// GitHub не OIDC-провайдер: ID-токена у него нет, а код авторизации меняется
// на токен с секретом клиента. Секрет живёт на бэкенде, поэтому обмен делает
// он, а не браузер. Токен пользователя нужен ровно на два запроса — кто он и
// его почта — и нигде не сохраняется: репозитории система читает установкой
// приложения, а не глазами вошедшего (решение 0028).
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
