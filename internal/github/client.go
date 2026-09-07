// Package github — чтение репозиториев проекта.
//
// Только чтение: система не пишет в репозитории пользователя
// (repositories.access_mode). Клиент отдаёт ровно то, что нужно индексатору и
// гейту external_check, и ничего сверх: листинг дерева на коммите, содержимое
// файла, состояние PR и его проверок.
package github

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

// ErrNotFound отделяет «нет такой ветки/файла» от сбоя: удалённая ветка — это
// нормальный ход событий, а не повод валить прогон индексации.
var ErrNotFound = errors.New("github: не найдено")

// Repo — координаты репозитория у провайдера.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// MaxWait ограничивает ожидание сброса лимита: упереться в час простоя
	// внутри одного прогона хуже, чем перенести прогон на следующий тик.
	MaxWait time.Duration
}

const (
	defaultBaseURL = "https://api.github.com"
	apiVersion     = "2022-11-28"
	maxAttempts    = 4
)

func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		MaxWait: 2 * time.Minute,
	}
}

// WithToken отдаёт клиента с другим токеном: у репозитория может быть свой
// секрет (repositories.credential_ref), а транспорт у них общий.
func (c *Client) WithToken(token string) *Client {
	if token == "" || token == c.Token {
		return c
	}
	clone := *c
	clone.Token = token
	return &clone
}

// Head — коммит, на котором стоит ветка сейчас.
func (c *Client) Head(ctx context.Context, repo Repo, branch string) (string, error) {
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	path := fmt.Sprintf("/repos/%s/%s/branches/%s",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), url.PathEscape(branch))
	if err := c.get(ctx, path, "", &out); err != nil {
		return "", err
	}
	if out.Commit.SHA == "" {
		return "", fmt.Errorf("github: ветка %s/%s не вернула коммит", repo, branch)
	}
	return out.Commit.SHA, nil
}

// TreeEntry — файл в дереве коммита.
type TreeEntry struct {
	Path string `json:"path"`
	// SHA объекта у провайдера. Именно по нему индексатор понимает, изменился
	// ли файл: сравнение листинга с предыдущим прогоном стоит один запрос,
	// а выкачивание содержимого — по запросу на файл.
	SHA  string `json:"sha"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	Mode string `json:"mode"`
}

// Tree отдаёт полный листинг файлов коммита.
//
// truncated=true означает, что репозиторий больше, чем провайдер согласен
// отдать одним ответом. Индексатор обязан на это реагировать, а не молча
// проиндексировать половину: неполный листинг выглядит как массовое удаление
// файлов и завёл бы ложные сигналы дрейфа.
func (c *Client) Tree(ctx context.Context, repo Repo, commit string) (entries []TreeEntry, truncated bool, err error) {
	var out struct {
		Tree      []TreeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	path := fmt.Sprintf("/repos/%s/%s/git/trees/%s?recursive=1",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), url.PathEscape(commit))
	if err := c.get(ctx, path, "", &out); err != nil {
		return nil, false, err
	}

	files := out.Tree[:0]
	for _, e := range out.Tree {
		// blob — обычный файл. tree — каталог, commit — вложенный субмодуль:
		// содержимого у них здесь нет.
		if e.Type == "blob" {
			files = append(files, e)
		}
	}
	return files, out.Truncated, nil
}

// Blob — содержимое файла по sha объекта.
func (c *Client) Blob(ctx context.Context, repo Repo, sha string) ([]byte, error) {
	path := fmt.Sprintf("/repos/%s/%s/git/blobs/%s",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), url.PathEscape(sha))
	// raw вместо json+base64: содержимое приезжает как есть, без раздувания
	// на треть и без лишнего разбора.
	return c.raw(ctx, path, "application/vnd.github.raw")
}

// PullRequest — состояние PR, на которое смотрят гейты repo_ref_required и
// external_check.
type PullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Draft   bool   `json:"draft"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
}

func (c *Client) PullRequest(ctx context.Context, repo Repo, number int) (PullRequest, error) {
	var out PullRequest
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), number)
	err := c.get(ctx, path, "", &out)
	return out, err
}

// CheckRun — одна проверка CI на коммите.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

func (c *Client) CheckRuns(ctx context.Context, repo Repo, sha string) ([]CheckRun, error) {
	var out struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100",
		url.PathEscape(repo.Owner), url.PathEscape(repo.Name), url.PathEscape(sha))
	if err := c.get(ctx, path, "", &out); err != nil {
		return nil, err
	}
	return out.CheckRuns, nil
}

func (c *Client) get(ctx context.Context, path, accept string, out any) error {
	body, err := c.raw(ctx, path, accept)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("github: разбор ответа %s: %w", path, err)
	}
	return nil
}

func (c *Client) raw(ctx context.Context, path, accept string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		body, wait, err := c.do(ctx, path, accept)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if wait < 0 || attempt == maxAttempts {
			return nil, err
		}
		if wait == 0 {
			wait = time.Duration(1<<attempt) * time.Second
		}
		if wait > c.MaxWait {
			return nil, fmt.Errorf("github: лимит запросов до %s — прогон отложен: %w", wait, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

// do возвращает wait < 0, если повторять бессмысленно.
func (c *Client) do(ctx context.Context, path, accept string) ([]byte, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, -1, err
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("github: запрос %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return nil, 0, err
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		return body, 0, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, -1, fmt.Errorf("%w: %s", ErrNotFound, path)
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0",
		resp.StatusCode >= 500:
		return nil, rateLimitWait(resp.Header), fmt.Errorf("github %d на %s: %s",
			resp.StatusCode, path, snippet(body))
	default:
		return nil, -1, fmt.Errorf("github %d на %s: %s", resp.StatusCode, path, snippet(body))
	}
}

// rateLimitWait читает, сколько провайдер просит подождать. Retry-After —
// вторичный лимит, X-RateLimit-Reset — основной почасовой.
func rateLimitWait(h http.Header) time.Duration {
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	if s := h.Get("X-RateLimit-Reset"); s != "" {
		if ts, err := strconv.ParseInt(s, 10, 64); err == nil {
			if d := time.Until(time.Unix(ts, 0)); d > 0 {
				return d
			}
		}
	}
	return 0
}

func snippet(b []byte) string {
	const limit = 300
	if len(b) > limit {
		return string(b[:limit]) + "…"
	}
	return string(b)
}
