package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Client — тонкая обёртка над REST. CLI не знает правил цикла: он только
// перекладывает аргументы в запрос и печатает ответ (решение 0011).
type Client struct {
	base string
	// Адрес интерфейса без /v1: от него считаются MCP и страница
	// подтверждения входа.
	origin string
	actor  string
	// Токен доступа. С ним заголовок актора не нужен: кто пришёл, определяет
	// токен (решение 0021).
	token   string
	http    *http.Client
	globals globals
	// Хост, который имелся в виду: --host, SDLC_API_URL или хост по
	// умолчанию из конфига. Адреса у него может не быть — тогда подсказка
	// «хост не настроен» называет его.
	host string
	// Версию сервера сверяем по первому ответу: одно предупреждение на
	// команду, сколько бы запросов она ни сделала.
	versionOnce sync.Once
}

// NewClient собирает клиент из окружения и конфига (решение 0032):
// SDLC_API_URL и SDLC_TOKEN перекрывают конфиг; без них — хост из --host
// или хост по умолчанию с его токеном. Совсем без всего клиент остаётся без
// адреса и не падает: auth и version с ним работают, а запрос к серверу
// возвращает «хост не настроен» (CLAB-147). Подставлять адрес по умолчанию
// нельзя: у клиента self-hosted им оказался бы dev-стек разработчика.
func NewClient(g globals) (*Client, error) {
	c := &Client{
		actor:   env("SDLC_ACTOR", "agent:"+env("USER", "sdlc-cli")),
		token:   env("SDLC_TOKEN", ""),
		http:    &http.Client{Timeout: 60 * time.Second},
		globals: g,
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	host, origin, hc, found, err := resolveHost(cfg, g)
	if err != nil {
		return nil, err
	}
	c.host = host
	if origin != "" {
		c.origin = origin
		c.base = origin + "/v1"
	}
	if c.token == "" && found {
		c.token = hc.Token
	}
	return c, nil
}

// requireHost — есть ли куда ходить. Без адреса — подсказка, как его
// задать; запроса при этом нет, и адрес из пустого origin не собирается.
func (c *Client) requireHost() error {
	if c.base != "" {
		return nil
	}
	if c.host != "" {
		// default_host остался в конфиге без записи в hosts — поправили руками.
		return fmt.Errorf("хост не настроен: clab auth login --host %s (хоста по умолчанию %s нет в %s)", c.host, c.host, configPath())
	}
	return fmt.Errorf("хост не настроен: clab auth login --host https://sdlc.example.com")
}

// APIError — ошибка домена, пришедшая с сервера. Код совпадает с таблицей
// ошибок MCP_CONTRACT.md: CI различает «занято другим» и «не прошли гейты»
// по коду, а не по тексту.
type APIError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
	Status  int             `json:"-"`
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	if err := c.requireHost(); err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	c.authorize(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	return c.finish(resp, out)
}

// finish читает ответ до конца, сверяет версию сервера и раскладывает его:
// ошибка домена — конвертом {"error":…}, успех — в out. Тело закрывается
// здесь. Общий хвост do и файловой загрузки.
func (c *Client) finish(resp *http.Response, out any) error {
	defer resp.Body.Close()
	c.checkServerVersion(resp)

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 300 {
		return responseError(resp.StatusCode, raw)
	}

	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// responseError — ошибка по неуспешному ответу: конверт сервера, а если его
// нет, то тело как сообщение.
func responseError(status int, raw []byte) *APIError {
	var envelope struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Error.Code == "" {
		return &APIError{Status: status, Message: strings.TrimSpace(string(raw))}
	}
	envelope.Error.Status = status
	return &envelope.Error
}

func (c *Client) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		req.Header.Set("X-SDLC-Actor", c.actor)
	}
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// getRaw читает ответ как есть: содержимое документа приезжает сырым телом.
func (c *Client) getRaw(ctx context.Context, path string) ([]byte, error) {
	raw, _, err := c.getContent(ctx, path)
	return raw, err
}

// getContent — getRaw вместе с типом содержимого из ответа. Ошибка домена
// приходит конвертом {"error":{code,message}}, как у do: по коду CI и
// агент отличают «нет доступа» от «нет такого».
func (c *Client) getContent(ctx context.Context, path string) ([]byte, string, error) {
	if err := c.requireHost(); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, "", err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	c.checkServerVersion(resp)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 300 {
		var envelope struct {
			Error APIError `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code != "" {
			envelope.Error.Status = resp.StatusCode
			return nil, "", &envelope.Error
		}
		return nil, "", &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

// project — какой проект имеется в виду: флаг, SDLC_PROJECT, иначе проект,
// к которому привязан репозиторий текущего каталога. Проект нужен только
// командам с сервером, поэтому без хоста первой говорится подсказка про
// хост: иначе вне репозитория человек услышал бы «не указан проект», а с
// SDLC_PROJECT mcp собрал бы адрес из пустого origin.
func (c *Client) project(ctx context.Context, explicit string) (string, error) {
	key, err := c.optionalProject(ctx, explicit)
	if err != nil {
		return "", err
	}
	if key != "" {
		return key, nil
	}
	return "", fmt.Errorf("не указан проект: --project, SDLC_PROJECT или каталог репозитория, привязанного к проекту")
}

// optionalProject — тот же выбор проекта, но «не задан» здесь не ошибка, а
// пустой ключ: clab whoami отвечает и без проекта, а ошибка сервера при
// поиске по git remote остаётся ошибкой.
func (c *Client) optionalProject(ctx context.Context, explicit string) (string, error) {
	if err := c.requireHost(); err != nil {
		return "", err
	}
	if explicit != "" {
		return explicit, nil
	}
	if key := os.Getenv("SDLC_PROJECT"); key != "" {
		return key, nil
	}
	return c.projectByRemote(ctx)
}

func query(pairs ...string) url.Values {
	q := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			q.Set(pairs[i], pairs[i+1])
		}
	}
	return q
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
