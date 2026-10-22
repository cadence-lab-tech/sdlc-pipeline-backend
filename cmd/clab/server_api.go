package main

// Клиент API своей установки (CLAB-199). Единственное место, где clab server
// ходит в API Cadence Lab: шаг стартового проекта install --with-starter и
// server starter. Остальные подкоманды работают с файлами и Docker, как
// скрипты пакета (решение 0045, п. 1).
//
// Адрес и токен — только из установки: Caddy этой машины по loopback и токен
// администратора из .env или secrets/. Конфиг clab человека (~/.config/clab),
// SDLC_API_URL и SDLC_TOKEN не читаются: иначе токен администратора одной
// установки ушёл бы на чужой хост, а шаг применился бы не к той установке.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultTLSDial — куда соединяется шаг в режиме tls: Caddy этой машины.
// Имя в сертификате и SNI — домен установки, проверка сертификата обычная:
// loopback обходит DNS и NAT, но не доверие.
const defaultTLSDial = "127.0.0.1:443"

// installAPI — узкий клиент REST установки: те же ручки, что у clab project
// create, clab import и clab workflow activate, без конфига и проверки версии
// клиента — версию здесь сверяет сам шаг.
type installAPI struct {
	base  string // http://127.0.0.1:8080 или https://<домен>, без /v1
	token string
	http  *http.Client
}

// installAPIFor — клиент API установки i с токеном администратора.
func installAPIFor(i *installation, token string) (*installAPI, error) {
	if token == "" {
		return nil, errors.New("no administrator token")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		// Без прокси из окружения: токен идёт только в Caddy этой машины.
		Proxy:               nil,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	var base string
	switch mode := i.getDefault("CADENCE_MODE", "tls"); mode {
	case "proxy":
		host, port, err := parseHTTPAddr(i.getDefault("CADENCE_HTTP_ADDR", "127.0.0.1:8080"))
		if err != nil {
			return nil, err
		}
		base = "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	case "tls":
		domain := i.get("SDLC_DOMAIN")
		if domain == "" {
			return nil, errors.New("CADENCE_MODE=tls needs SDLC_DOMAIN in .env")
		}
		addr := i.e.apiTLSDial
		if addr == "" {
			addr = defaultTLSDial
		}
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
		tr.TLSClientConfig = &tls.Config{ServerName: domain, RootCAs: i.e.apiRoots, MinVersion: tls.VersionTLS12}
		base = "https://" + domain
	default:
		return nil, fmt.Errorf("CADENCE_MODE in .env is '%s', expected tls or proxy", mode)
	}
	return &installAPI{
		base:  base,
		token: token,
		http: &http.Client{
			Transport: tr,
			Timeout:   60 * time.Second,
			// Переадресация увела бы запрос с токеном с loopback: ответ 3xx —
			// ошибка, а не повод идти дальше.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// apiFailure — ответ API не 2xx. Текст сервера (по-русски) — вторым планом.
type apiFailure struct {
	method, path string
	status       int
	code, msg    string
}

func (f *apiFailure) Error() string {
	s := fmt.Sprintf("%s /v1%s answered HTTP %d", f.method, f.path, f.status)
	if f.code != "" {
		s += " " + f.code
	}
	if f.msg != "" {
		s += " (" + f.msg + ")"
	}
	return s
}

func apiStatus(err error) int {
	var f *apiFailure
	if errors.As(err, &f) {
		return f.status
	}
	return 0
}

func (a *installAPI) do(ctx context.Context, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+"/v1"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		f := &apiFailure{method: method, path: path, status: resp.StatusCode}
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			f.code, f.msg = envelope.Error.Code, envelope.Error.Message
		}
		return f
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s /v1%s: the answer is not JSON: %v", method, path, err)
	}
	return nil
}

func (a *installAPI) post(ctx context.Context, path string, body any, out any) error {
	raw, ok := body.([]byte)
	if !ok {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	return a.do(ctx, http.MethodPost, path, raw, out)
}

type apiInstance struct {
	Version string `json:"version"`
}

type apiProject struct {
	Key          string  `json:"key"`
	Name         string  `json:"name"`
	OwnerTeamKey *string `json:"owner_team_key"`
}

type apiWorkflow struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Version   int      `json:"version"`
	State     string   `json:"state"`
	TaskTypes []string `json:"task_types"`
}

type apiImportEntry struct {
	Key     string `json:"key"`
	Outcome string `json:"outcome"`
	ID      string `json:"id"`
}

type apiImportReport struct {
	Roles     []apiImportEntry `json:"roles"`
	Workflows []apiImportEntry `json:"workflows"`
}

func (a *installAPI) instance(ctx context.Context) (apiInstance, error) {
	var out apiInstance
	err := a.do(ctx, http.MethodGet, "/instance", nil, &out)
	return out, err
}

// project — проект по ключу; нет такого — nil без ошибки.
func (a *installAPI) project(ctx context.Context, key string) (*apiProject, error) {
	var out struct {
		Project apiProject `json:"project"`
	}
	err := a.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(key), nil, &out)
	if apiStatus(err) == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out.Project, nil
}

func (a *installAPI) createProject(ctx context.Context, key, name, team string) (*apiProject, error) {
	var out apiProject
	err := a.post(ctx, "/projects", map[string]string{"key": key, "name": name, "team": team}, &out)
	return &out, err
}

func (a *installAPI) importBundle(ctx context.Context, key string, bundle []byte, dryRun bool) (apiImportReport, error) {
	path := "/projects/" + url.PathEscape(key) + "/import"
	if dryRun {
		path += "?dry_run=true"
	}
	var out apiImportReport
	err := a.post(ctx, path, bundle, &out)
	return out, err
}

func (a *installAPI) workflows(ctx context.Context, key string) ([]apiWorkflow, error) {
	var out struct {
		Workflows []apiWorkflow `json:"workflows"`
	}
	err := a.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(key)+"/workflows", nil, &out)
	return out.Workflows, err
}

func (a *installAPI) activate(ctx context.Context, id string) error {
	return a.post(ctx, "/workflows/"+url.PathEscape(id)+"/activate", map[string]any{}, nil)
}

// waitInstance — /v1/instance через Caddy с повтором шагами по 3 секунды в
// пределах timeout: в режиме tls Caddy получает сертификат после старта, и
// до этого TLS не сходится. Отказ API по правам или 4xx не ждут.
func (a *installAPI) waitInstance(ctx context.Context, e *serverEnv, timeout int) (apiInstance, error) {
	for waited := 0; ; waited += 3 {
		inst, err := a.instance(ctx)
		if err == nil {
			return inst, nil
		}
		if s := apiStatus(err); s != 0 && s != http.StatusBadGateway && s != http.StatusServiceUnavailable && s != http.StatusGatewayTimeout {
			return inst, err
		}
		if ctx.Err() != nil || waited+3 > timeout {
			return inst, err
		}
		e.sleep(3 * time.Second)
	}
}

// redactToken — на всякий случай: текст ошибки транспорта токена не несёт,
// но сообщение сервера могло бы его процитировать.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[token]")
}
