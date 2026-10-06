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
	// Откуда взялись адрес и токен — для подсказок в ошибках.
	source string
	// Версию сервера сверяем по первому ответу: одно предупреждение на
	// команду, сколько бы запросов она ни сделала.
	versionOnce sync.Once
}

// NewClient собирает клиент из окружения и конфига (решение 0032):
// SDLC_API_URL и SDLC_TOKEN перекрывают конфиг; без них — хост из --host
// или хост по умолчанию с его токеном; совсем без всего — локальная сборка.
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
	switch {
	case origin != "":
		c.origin = origin
		c.source = host
	default:
		c.origin = "http://localhost:8088"
		c.source = "localhost (адрес не задан)"
	}
	c.base = c.origin + "/v1"
	if c.token == "" && found {
		c.token = hc.Token
	}
	return c, nil
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
	defer resp.Body.Close()
	c.checkServerVersion(resp)

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 300 {
		var envelope struct {
			Error APIError `json:"error"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Error.Code == "" {
			return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		}
		envelope.Error.Status = resp.StatusCode
		return &envelope.Error
	}

	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	c.checkServerVersion(resp)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
	}
	return raw, nil
}

// project — какой проект имеется в виду: флаг, SDLC_PROJECT, иначе проект,
// к которому привязан репозиторий текущего каталога.
func (c *Client) project(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if key := os.Getenv("SDLC_PROJECT"); key != "" {
		return key, nil
	}
	key, err := c.projectByRemote(ctx)
	if err != nil {
		return "", err
	}
	if key != "" {
		return key, nil
	}
	return "", fmt.Errorf("не указан проект: --project, SDLC_PROJECT или каталог репозитория, привязанного к проекту")
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
