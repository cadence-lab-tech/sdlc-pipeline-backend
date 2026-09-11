package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Клиент REST: раннеру нужны пять ручек, и клиентская библиотека ради них не
// окупается. Ошибки домена приезжают кодом — по нему раннер отличает «занято
// другим» от «нет прав».
type client struct {
	base  string
	token string
	actor string
	http  *http.Client
}

func newClient(cfg Config) *client {
	return &client{
		base: cfg.APIURL, token: cfg.Token, actor: cfg.Actor,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
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
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		req.Header.Set("X-SDLC-Actor", c.actor)
	}
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
	if resp.StatusCode >= 400 {
		var wrapper struct {
			Error apiError `json:"error"`
		}
		if json.Unmarshal(raw, &wrapper) == nil && wrapper.Error.Code != "" {
			return &wrapper.Error
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// queueItem — строка очереди: прогон, роль, которой он адресован, и можно ли
// его забрать прямо сейчас.
type queueItem struct {
	Task struct {
		Key        string  `json:"key"`
		Title      string  `json:"title"`
		StatusKey  string  `json:"status_key"`
		AssigneeID *string `json:"assignee_id"`
	} `json:"task"`
	Run struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	} `json:"run"`
	RoleKey   string `json:"role_key"`
	Claimable bool   `json:"claimable"`
}

func (c *client) queue(ctx context.Context, project, role string) ([]queueItem, error) {
	q := url.Values{}
	if role != "" {
		q.Set("role", role)
	}
	var out struct {
		Queue []queueItem `json:"queue"`
	}
	path := "/projects/" + project + "/queue"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return out.Queue, c.do(ctx, http.MethodGet, path, nil, &out)
}

// claimResult — то, что нужно раннеру из ответа захвата: модель и инструменты
// берутся из версии роли, а не из настроек раннера (решение 0025).
type claimResult struct {
	StageRunID string `json:"stage_run_id"`
	Role       *struct {
		Key        string  `json:"key"`
		ModelHint  *string `json:"model_hint"`
		ToolPolicy struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"tool_policy"`
	} `json:"role"`
	Lease *struct {
		ExpiresAt                time.Time `json:"expires_at"`
		HeartbeatIntervalSeconds int       `json:"heartbeat_interval_seconds"`
	} `json:"lease"`
	Context claimContext `json:"context"`
}

// claimContext — область задачи из ответа захвата. Рабочие копии заводятся
// ровно для этих репозиториев: это та область, на которую выдана аренда, и
// отдельный запрос за ней мог бы разойтись с захватом.
type claimContext struct {
	Repositories []claimRepo `json:"repositories"`
}

type claimRepo struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
}

func (r claimRepo) fullName() string { return r.Owner + "/" + r.Name }

func (c *client) claim(ctx context.Context, task, session string) (claimResult, error) {
	var out claimResult
	err := c.do(ctx, http.MethodPost, "/tasks/"+task+"/claim",
		map[string]any{"session_ref": session}, &out)
	return out, err
}

func (c *client) heartbeat(ctx context.Context, task string) error {
	return c.do(ctx, http.MethodPost, "/tasks/"+task+"/heartbeat", map[string]any{}, nil)
}

func (c *client) release(ctx context.Context, task, note string) error {
	return c.do(ctx, http.MethodPost, "/tasks/"+task+"/release",
		map[string]any{"note": note}, nil)
}

func (c *client) reportUsage(ctx context.Context, task, runID string, body map[string]any) error {
	path := "/tasks/" + task + "/usage"
	if runID != "" {
		path = "/tasks/" + task + "/runs/" + runID + "/usage"
	}
	return c.do(ctx, http.MethodPost, path, body, nil)
}
