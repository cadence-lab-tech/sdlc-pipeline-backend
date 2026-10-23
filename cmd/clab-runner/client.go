package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// Клиент REST: раннеру нужны семь ручек, и клиентская библиотека ради них не
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
	// Статус ответа: код домена есть не всегда, а 401 и 404 раннер различает
	// и без него.
	Status int `json:"-"`
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// noRoute — ответ 404 без кода домена: такой ручки у сервера нет. Сервер
// отвечает на неизвестный маршрут штатным 404 gin текстом, а на «задачи нет»
// — JSON с code: not_found; по этой разнице раннер узнаёт сервер прежней
// версии.
func noRoute(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code == ""
}

// errCode — что написать в ленту задачи об отказе: код домена, иначе статус,
// иначе текст ошибки сети.
func errCode(err error) string {
	var e *apiError
	switch {
	case errors.As(err, &e) && e.Code != "":
		return e.Code
	case errors.As(err, &e):
		return http.StatusText(e.Status)
	default:
		return err.Error()
	}
}

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
			wrapper.Error.Status = resp.StatusCode
			return &wrapper.Error
		}
		return &apiError{Status: resp.StatusCode,
			Message: fmt.Sprintf("%s %s: %s", method, path, resp.Status)}
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

// claimResult — то, что нужно раннеру из ответа захвата: инструменты — из
// версии роли (решение 0025), рекомендованный уровень модели — из execution
// (решение 0060), устаревшая строка модели model_hint — запасной путь.
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
	// Сервер прежней версии execution не отдаёт: nil, и раннер ведёт себя
	// как раньше — модель из model_hint.
	Execution *claimExecution `json:"execution"`
}

// claimExecution — рекомендация прогона: уровень и откуда он (этап задачи,
// задача, роль или none).
type claimExecution struct {
	ModelLevel       *string `json:"model_level"`
	ModelLevelSource string  `json:"model_level_source"`
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

// taskToken — токен задачи на захваченный прогон (решение 0049, CLAB-212).
// Значение не печатается: ни в журнал, ни в argv — у него свой LogValue.
type taskToken struct {
	Token  string `json:"token"`
	Record struct {
		ID string `json:"id"`
	} `json:"record"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

func (t taskToken) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", t.Record.ID), slog.String("token", "<скрыт>"))
}

// issueTaskToken выпускает токен задачи держателю аренды — раннеру, своим
// токеном. Сервер прежней версии ручки не знает: это noRoute.
func (c *client) issueTaskToken(ctx context.Context, task string) (taskToken, error) {
	var out taskToken
	if err := c.do(ctx, http.MethodPost, "/tasks/"+task+"/task-token", map[string]any{}, &out); err != nil {
		return out, err
	}
	if out.Token == "" || out.Record.ID == "" {
		return out, fmt.Errorf("сервер выпустил токен задачи без значения или идентификатора")
	}
	return out, nil
}

// revokeToken отзывает токен по идентификатору записи. Свой токен задачи
// раннер отзывает как его владелец.
func (c *client) revokeToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/auth/tokens/"+url.PathEscape(id), nil, nil)
}
