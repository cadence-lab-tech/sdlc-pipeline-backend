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
	"time"
)

// Client — тонкая обёртка над REST. CLI не знает правил цикла: он только
// перекладывает аргументы в запрос и печатает ответ (решение 0011).
type Client struct {
	base  string
	actor string
	http  *http.Client
}

func NewClient() *Client {
	base := env("SDLC_API_URL", "http://localhost:8088/v1")
	return &Client{
		base:  strings.TrimRight(base, "/"),
		actor: env("SDLC_ACTOR", "agent:"+env("USER", "sdlc-cli")),
		http:  &http.Client{Timeout: 60 * time.Second},
	}
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
	req.Header.Set("X-SDLC-Actor", c.actor)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

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

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
