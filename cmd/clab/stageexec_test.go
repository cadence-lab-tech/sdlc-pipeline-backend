package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLAB-222: исполнитель этапа /clab-run работает без MCP, только clab с
// токеном задачи. Ему нужны чтение входных артефактов (clab artifact) и
// конфиг токена задачи в своём каталоге (clab token task --config-dir).

const artifactID = "fa81dd6b-bd98-4a90-afdd-1559cc45e1e8"

// artifactServer отвечает содержимым артефакта по /v1/artifacts/<id>/content
// и конвертом ошибки на остальное.
func artifactServer(t *testing.T, status int, mediaType, body string) (*Client, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.EscapedPath())
		w.Header().Set("Content-Type", mediaType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: srv.URL, token: "sdlc_task", http: srv.Client()}, &calls
}

// Критерий 1: содержимое как есть, --json — {id, media_type, content}.
func TestArtifactPrintsContent(t *testing.T) {
	content := "# Требования\n\nСтрока с `кодом` и «кавычками»."

	withJSON(t, false)
	c, calls := artifactServer(t, http.StatusOK, "text/markdown", content)
	out, _, err := capture(t, func() error { return cmdArtifact(context.Background(), c, []string{artifactID}) })
	if err != nil {
		t.Fatal(err)
	}
	if want := "GET /v1/artifacts/" + artifactID + "/content"; len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("запросы %v, ждали %s", *calls, want)
	}
	if out != content+"\n" {
		t.Fatalf("содержимое не как есть: %q", out)
	}

	withJSON(t, true)
	c, _ = artifactServer(t, http.StatusOK, "text/markdown", content)
	out, _, err = capture(t, func() error { return cmdArtifact(context.Background(), c, []string{artifactID}) })
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("--json не JSON: %q", out)
	}
	if v["id"] != artifactID || v["media_type"] != "text/markdown" || v["content"] != content {
		t.Fatalf("--json: %v", v)
	}
}

// Критерий 1: вне круга видимости токена задачи — код сервера forbidden,
// неизвестный id — not_found; без id — подсказка, запроса нет.
func TestArtifactErrors(t *testing.T) {
	withJSON(t, false)
	for status, code := range map[int]string{http.StatusForbidden: "forbidden", http.StatusNotFound: "not_found"} {
		c, _ := artifactServer(t, status, "application/json",
			`{"error":{"code":"`+code+`","message":"артефакт вне круга задачи"}}`)
		out, _, err := capture(t, func() error { return cmdArtifact(context.Background(), c, []string{artifactID}) })
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != code || apiErr.Status != status {
			t.Fatalf("HTTP %d: ошибка %v", status, err)
		}
		if out != "" {
			t.Fatalf("HTTP %d: в stdout %q", status, out)
		}
	}

	c, calls := artifactServer(t, http.StatusOK, "text/plain", "x")
	if _, _, err := capture(t, func() error { return cmdArtifact(context.Background(), c, nil) }); err == nil ||
		!strings.Contains(err.Error(), "clab artifact <id>") {
		t.Fatalf("без id: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("без id ушёл запрос: %v", *calls)
	}
}

// Критерий 4: справка называет artifact и --config-dir.
func TestHelpNamesArtifactAndConfigDir(t *testing.T) {
	var artifact, token string
	for _, cmd := range commands() {
		switch cmd.name {
		case "artifact":
			artifact = cmd.usage
		case "token":
			token = cmd.usage
		}
	}
	if !strings.Contains(artifact, "clab artifact <id>") {
		t.Fatalf("нет команды artifact: %q", artifact)
	}
	if !strings.Contains(token, "--config-dir") {
		t.Fatalf("справка token без --config-dir: %q", token)
	}
}

// Критерий 2: --config-dir пишет конфиг clab с хостом вызвавшего и токеном
// задачи, каталог 0700, файл 0600; токен не печатается.
func TestTokenTaskConfigDir(t *testing.T) {
	t.Setenv("CLAB_CONFIG_DIR", filepath.Join(t.TempDir(), "human"))
	dir := filepath.Join(t.TempDir(), "CLAB-12-developer-1", "clab")

	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusCreated, taskTokenReply)
	c.host = "sdlc.example.com"
	out, _, err := capture(t, func() error {
		return cmdToken(context.Background(), c, []string{"task", "CLAB-12", "--config-dir", dir})
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/tasks/CLAB-12/task-token" {
		t.Fatalf("запрос %s %s", rec.method, rec.path)
	}
	if strings.Contains(out, "sdlc_tasktokensecret") {
		t.Fatalf("токен напечатан:\n%s", out)
	}
	for _, want := range []string{"7d0a6c0e-0000-4000-8000-000000000001", filepath.Join(dir, "config.json"), "CLAB_CONFIG_DIR=" + dir} {
		if !strings.Contains(out, want) {
			t.Fatalf("в выводе нет %q:\n%s", want, out)
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	hc := cfg.Hosts["sdlc.example.com"]
	if cfg.DefaultHost != "sdlc.example.com" || hc.URL != "https://ui.example" ||
		hc.Token != "sdlc_tasktokensecret" || hc.TokenID != "7d0a6c0e-0000-4000-8000-000000000001" {
		t.Fatalf("конфиг: %s", raw)
	}
	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, "config.json"), 0o600)

	// Конфиг человека не тронут.
	if _, err := os.Stat(filepath.Join(os.Getenv("CLAB_CONFIG_DIR"), "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("конфиг человека появился: %v", err)
	}

	// --json: без токена, с путём и id; существующий каталог сужается до 0700.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	withJSON(t, true)
	c, _ = newInviteClient(t, http.StatusCreated, taskTokenReply)
	c.host = "sdlc.example.com"
	out, _, err = capture(t, func() error {
		return cmdToken(context.Background(), c, []string{"task", "CLAB-12", "--config-dir", dir})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "sdlc_tasktokensecret") {
		t.Fatalf("--json напечатал токен:\n%s", out)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("--json не JSON: %q", out)
	}
	if v["token_id"] != "7d0a6c0e-0000-4000-8000-000000000001" || v["config_dir"] != dir || v["task_key"] != "CLAB-12" {
		t.Fatalf("--json: %s", out)
	}
	assertMode(t, dir, 0o700)
}

// Свой каталог конфига человека под токен задачи не отдаётся: запись затёрла
// бы его вход. Отказ — до запроса, токен не выпускается.
func TestTokenTaskConfigDirRefusesOwnConfig(t *testing.T) {
	own := t.TempDir()
	t.Setenv("CLAB_CONFIG_DIR", own)
	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusCreated, taskTokenReply)
	_, _, err := capture(t, func() error {
		return cmdToken(context.Background(), c, []string{"task", "CLAB-12", "--config-dir", own})
	})
	if err == nil || !strings.Contains(err.Error(), "собственного конфига") {
		t.Fatalf("ждали отказ: %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("токен выпущен при отказе: %d запросов", rec.calls)
	}
}

// Критерий 3: под префиксом исполнителя (env -u SDLC_TOKEN -u SDLC_API_URL
// CLAB_CONFIG_DIR=<каталог>) clab ходит токеном задачи из каталога, а не
// конфигом человека. Без -u переменные человека перекрыли бы каталог —
// поэтому префикс их и снимает.
func TestExecutorPrefixUsesTaskConfig(t *testing.T) {
	human := t.TempDir()
	t.Setenv("CLAB_CONFIG_DIR", human)
	if err := saveConfig(Config{DefaultHost: "sdlc.example.com", Hosts: map[string]HostConfig{
		"sdlc.example.com": {URL: "https://sdlc.example.com", Token: "sdlc_human"}}}); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(t.TempDir(), "clab")
	if err := saveConfigAt(task, Config{DefaultHost: "sdlc.example.com", Hosts: map[string]HostConfig{
		"sdlc.example.com": {URL: "https://sdlc.example.com", Token: "sdlc_task"}}}); err != nil {
		t.Fatal(err)
	}

	// Сессия человека: SDLC_TOKEN и SDLC_API_URL выставлены.
	t.Setenv("SDLC_TOKEN", "sdlc_human_env")
	t.Setenv("SDLC_API_URL", "https://sdlc.example.com/v1")
	t.Setenv("CLAB_CONFIG_DIR", task)
	c, err := NewClient(globals{})
	if err != nil {
		t.Fatal(err)
	}
	if c.token != "sdlc_human_env" {
		t.Fatalf("без -u ждали перекрытие окружением, получили %q", c.token)
	}

	// Префикс исполнителя: env -u снимает переменные человека.
	unsetenv(t, "SDLC_TOKEN")
	unsetenv(t, "SDLC_API_URL")
	c, err = NewClient(globals{})
	if err != nil {
		t.Fatal(err)
	}
	if c.token != "sdlc_task" || c.base != "https://sdlc.example.com/v1" {
		t.Fatalf("под префиксом: токен %q, адрес %q", c.token, c.base)
	}
}

func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "") // восстановит прежнее значение после теста
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != want {
		t.Fatalf("%s: права %o, ждали %o", path, got, want)
	}
}
