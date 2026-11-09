package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
)

// clab prompt work, clab guide и clab decide --seen-artifact (CLAB-416,
// решение 0077 пп. 1–2): тесты на фейковом сервере, AC-N — из анализа.

const promptReply = `{"name":"work_on_task","text":"Возьми задачу CLAB-12 в работу ролью developer.\n\nПорядок: …"}`

// AC-6: текст и перевод строки; --role и --usage уходят в запрос; --json —
// ответ ручки целиком.
func TestPromptWorkPrintsText(t *testing.T) {
	withJSON(t, false)
	c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, http.StatusOK, promptReply) })
	out, _, err := capture(t, func() error {
		return cmdPrompt(context.Background(), c, []string{"work", "CLAB-12", "--role", "developer", "--usage", "runner"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != "GET /v1/tasks/CLAB-12/prompts/work?role=developer&usage=runner" {
		t.Fatalf("запросы %v", f.calls)
	}
	if out != "Возьми задачу CLAB-12 в работу ролью developer.\n\nПорядок: …\n" {
		t.Fatalf("вывод %q", out)
	}

	c, f = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, http.StatusOK, promptReply) })
	if _, _, err := capture(t, func() error { return cmdPrompt(context.Background(), c, []string{"work", "CLAB-12"}) }); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "GET /v1/tasks/CLAB-12/prompts/work" {
		t.Fatalf("без флагов в запросе лишнее: %v", f.calls)
	}

	withJSON(t, true)
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, http.StatusOK, promptReply) })
	out, _, err = capture(t, func() error { return cmdPrompt(context.Background(), c, []string{"work", "CLAB-12"}) })
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err := json.Unmarshal([]byte(out), &v); err != nil || v["name"] != "work_on_task" || !strings.HasPrefix(v["text"], "Возьми задачу") {
		t.Fatalf("--json: %q", out)
	}
}

// AC-6: без ключа — подсказка формы команды, без подкоманды и с неизвестной —
// справка; запроса нет.
func TestPromptUsageErrors(t *testing.T) {
	withJSON(t, false)
	c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, http.StatusOK, promptReply) })
	for _, args := range [][]string{{"work"}, {"work", "--role", "developer"}} {
		_, stderr, err := capture(t, func() error { return cmdPrompt(context.Background(), c, args) })
		if err == nil || !strings.Contains(err.Error(), "clab prompt work <ключ>") || !strings.Contains(stderr, "clab prompt work SDLC-42") {
			t.Errorf("%v: %v / %q", args, err, stderr)
		}
	}
	for _, args := range [][]string{nil, {"run"}} {
		_, stderr, err := capture(t, func() error { return cmdPrompt(context.Background(), c, args) })
		if err == nil || !strings.Contains(err.Error(), "work") || !strings.Contains(stderr, "clab prompt work") {
			t.Errorf("%v: %v / %q", args, err, stderr)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("ушли запросы: %v", f.calls)
	}
}

// AC-6: под CLAB_CONFIG_DIR каталога токена задачи — токен задачи в
// заголовке, адрес хоста из этого конфига.
func TestPromptWorkUnderTaskTokenConfig(t *testing.T) {
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		reply(w, http.StatusOK, promptReply)
	}))
	t.Cleanup(srv.Close)
	dir := filepath.Join(t.TempDir(), "CLAB-12-developer-1", "clab")
	if err := saveConfigAt(dir, Config{DefaultHost: "sdlc.example.com", Hosts: map[string]HostConfig{
		"sdlc.example.com": {URL: srv.URL, Token: "sdlc_tasktokensecret"},
	}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAB_CONFIG_DIR", dir)
	t.Setenv("SDLC_TOKEN", "")
	t.Setenv("SDLC_API_URL", "")
	c, err := NewClient(globals{})
	if err != nil {
		t.Fatal(err)
	}
	withJSON(t, false)
	if _, _, err := capture(t, func() error { return cmdPrompt(context.Background(), c, []string{"work", "CLAB-12"}) }); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sdlc_tasktokensecret" || path != "/v1/tasks/CLAB-12/prompts/work" {
		t.Fatalf("запрос %q %q", auth, path)
	}
}

// oldServerCase — ответ сервера, который команды обязаны распознать как
// «прежний сервер» или как ошибку домена.
type oldServerCase struct {
	name        string
	header      string // X-Clab-Version
	instance    string // ответ /v1/instance, "" — 404
	status      int
	contentType string
	body        string
	wantVersion string
}

var oldServerCases = []oldServerCase{
	{"404 без кода", "v0.27.0", "", http.StatusNotFound, "text/plain", "404 page not found", "v0.27.0"},
	{"200 HTML", "", `{"version":"v0.26.1"}`, http.StatusOK, "text/html",
		"<!doctype html><html><body><div id=\"root\"></div></body></html>", "v0.26.1"},
	{"HTML и версии нет", "", "", http.StatusOK, "text/html", "<html><body>Cadence Lab</body></html>", "(версия неизвестна)"},
}

func oldServer(t *testing.T, oc oldServerCase, route string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if oc.header != "" {
			w.Header().Set(headerServerVersion, oc.header)
		}
		if r.URL.Path == "/v1/instance" {
			if oc.instance == "" {
				http.NotFound(w, r)
				return
			}
			reply(w, http.StatusOK, oc.instance)
			return
		}
		if r.URL.Path != route {
			t.Errorf("неожиданный запрос %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", oc.contentType)
		w.WriteHeader(oc.status)
		_, _ = w.Write([]byte(oc.body))
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: srv.URL, actor: "user:alice", http: srv.Client()}
}

func checkOldServerRefusal(t *testing.T, name, out string, err error, version, claim string) {
	t.Helper()
	if !errors.Is(err, errOldServer) {
		t.Fatalf("%s: ждали отказ «прежний сервер», получили %v", name, err)
	}
	if out != "" {
		t.Errorf("%s: в stdout %q", name, out)
	}
	msg := err.Error()
	for _, want := range []string{version, "команде нужен сервер " + agentTextMinServer + " или новее", claim} {
		if !strings.Contains(msg, want) {
			t.Errorf("%s: в отказе нет %q:\n%s", name, want, msg)
		}
	}
	for _, banned := range []string{"<html", "<!doctype", "<body", "<div", "page not found", "panic", "goroutine"} {
		if strings.Contains(msg, banned) {
			t.Errorf("%s: в отказе %q:\n%s", name, banned, msg)
		}
	}
}

// AC-8: prompt work против сервера прежней версии — понятный отказ без HTML;
// 404 с кодом not_found — ошибка домена, а не «старый сервер».
func TestPromptWorkOldServer(t *testing.T) {
	withJSON(t, false)
	for _, oc := range oldServerCases {
		c := oldServer(t, oc, "/v1/tasks/CLAB-12/prompts/work")
		out, _, err := capture(t, func() error { return cmdPrompt(context.Background(), c, []string{"work", "CLAB-12"}) })
		checkOldServerRefusal(t, oc.name, out, err, oc.wantVersion, "clab claim CLAB-12")
	}

	c := oldServer(t, oldServerCase{status: http.StatusNotFound, contentType: "application/json",
		body: `{"error":{"code":"not_found","message":"задача CLAB-12 не найдена"}}`}, "/v1/tasks/CLAB-12/prompts/work")
	out, _, err := capture(t, func() error { return cmdPrompt(context.Background(), c, []string{"work", "CLAB-12"}) })
	var apiErr *APIError
	if errors.Is(err, errOldServer) || !errors.As(err, &apiErr) || apiErr.Code != "not_found" || out != "" {
		t.Fatalf("задачи нет: %v / %q", err, out)
	}
}

// kitServer отдаёт набор агента как /v1/agent-kit.
func kitServer(t *testing.T, kit any) (*Client, *[]string) {
	t.Helper()
	body, _ := json.Marshal(kit)
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if r.URL.Path != "/v1/agent-kit" {
			t.Errorf("неожиданный запрос %s", r.URL.Path)
		}
		reply(w, http.StatusOK, string(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: srv.URL, actor: "user:alice", http: srv.Client()}, &auth
}

// AC-7: clab guide печатает справку настоящего набора без шапки; --json —
// {version, name, text}; токен не нужен. С CLAB-417 (AC-10) текст — core.guide
// нового сервера: нейтральное ядро без заметок привязки claude.
func TestGuidePrintsKitGuide(t *testing.T) {
	kit, err := agentkit.Load("v0.28.0")
	if err != nil {
		t.Fatal(err)
	}
	body := kit.Core.Guide.Content
	if !strings.HasPrefix(body, "# ") {
		t.Fatalf("core.guide не справка: %.60q", body)
	}
	for _, s := range []string{"Claude", "claude", ".claude", "mcp__", "haiku", "opus", "Codex"} {
		if strings.Contains(body, s) {
			t.Errorf("core.guide называет провайдера: %q", s)
		}
	}

	withJSON(t, false)
	c, auth := kitServer(t, kit)
	out, _, err := capture(t, func() error { return cmdGuide(context.Background(), c, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if out != body {
		t.Fatalf("вывод не равен телу clab-guide:\n%.200q\n---\n%.200q", out, body)
	}
	if strings.HasPrefix(out, "---") || strings.Contains(out, "description: Reference on Cadence Lab") {
		t.Fatal("шапка скила в выводе")
	}
	if (*auth)[0] != "" {
		t.Fatalf("без токена ушёл Authorization: %q", (*auth)[0])
	}

	withJSON(t, true)
	c, _ = kitServer(t, kit)
	out, _, err = capture(t, func() error { return cmdGuide(context.Background(), c, nil) })
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err := json.Unmarshal([]byte(out), &v); err != nil || v["version"] != "v0.28.0" || v["name"] != "clab-guide" || v["text"] != body {
		t.Fatalf("--json: %.300q", out)
	}
}

// AC-10 (CLAB-417): сервер до ядра и привязок отдаёт только files — clab
// guide печатает тело скила clab-guide оттуда, без шапки, как раньше.
func TestGuideServerWithoutCore(t *testing.T) {
	old := map[string]any{
		"version": "v0.28.0",
		"files": []map[string]string{
			{"path": "skills/clab-work/SKILL.md", "content": "---\nname: clab-work\n---\nwork"},
			{"path": guideKitPath, "content": "---\nname: clab-guide\ndescription: d\n---\n\n# Old guide\n\ntext\n"},
		},
	}
	withJSON(t, false)
	c, _ := kitServer(t, old)
	out, _, err := capture(t, func() error { return cmdGuide(context.Background(), c, nil) })
	if err != nil || out != "# Old guide\n\ntext\n" {
		t.Fatalf("прежний сервер: %v / %q", err, out)
	}
}

// AC-11 (CLAB-417): clab последнего релиза ставит из files настоящего набора
// — каждый путь проходит kitPathValid, суммы сходятся с содержимым, а новые
// поля core и bindings ему не мешают (тип agentKit их не знает).
func TestKitFilesInstallableByOldClab(t *testing.T) {
	kit, err := agentkit.Load("v0.28.0")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	var got agentKit
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 7 {
		t.Fatalf("files: %d", len(got.Files))
	}
	for _, f := range got.Files {
		if err := kitPathValid(f.Path); err != nil {
			t.Error(err)
		}
		sum := sha256.Sum256([]byte(f.Content))
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			t.Errorf("%s: sha256 не сходится", f.Path)
		}
	}
	if len(kit.Bindings) == 0 || kit.Core.Guide.Content == "" {
		t.Fatal("в наборе нет core и bindings")
	}
}

// AC-8: clab guide против сервера прежней версии: ручки нет, вместо JSON —
// страница, в наборе нет clab-guide.
func TestGuideOldServer(t *testing.T) {
	withJSON(t, false)
	for _, oc := range oldServerCases {
		c := oldServer(t, oc, "/v1/agent-kit")
		out, _, err := capture(t, func() error { return cmdGuide(context.Background(), c, nil) })
		checkOldServerRefusal(t, oc.name, out, err, oc.wantVersion, "clab claim <ключ>")
	}
	c := oldServer(t, oldServerCase{header: "v0.21.0", status: http.StatusOK, contentType: "application/json",
		body: `{"version":"v0.21.0","files":[{"path":"skills/clab-work/SKILL.md","content":"---\nname: clab-work\n---\nwork"}]}`},
		"/v1/agent-kit")
	out, _, err := capture(t, func() error { return cmdGuide(context.Background(), c, nil) })
	checkOldServerRefusal(t, "набор без clab-guide", out, err, "v0.21.0", "clab claim <ключ>")
}

// AC-9: --seen-artifact кладёт seen_artifact_id в тело, пустая строка —
// пустой строкой, без флага ключа нет; 409 доходит кодом и причиной.
func TestDecideSeenArtifact(t *testing.T) {
	withJSON(t, true)
	const decision = `{"gate":"human_approval","status":"passed"}`
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"id", []string{"--seen-artifact", "a1b2"}, `{"seen_artifact_id":"a1b2","status":"passed"}`},
		{"пусто", []string{"--seen-artifact", ""}, `{"seen_artifact_id":"","status":"passed"}`},
		{"пусто через =", []string{"--seen-artifact="}, `{"seen_artifact_id":"","status":"passed"}`},
		{"без флага", nil, `{"status":"passed"}`},
	} {
		cl, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, http.StatusOK, decision) })
		args := append([]string{"CLAB-12", "--transition", "analysis_done", "--gate", "human_approval"}, c.args...)
		if _, _, err := capture(t, func() error { return cmdDecide(context.Background(), cl, args) }); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(f.bodies) != 1 || f.bodies[0] != c.want {
			t.Errorf("%s: тело %v, ждали %s", c.name, f.bodies, c.want)
		}
	}

	cl, _ := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusConflict, `{"error":{"code":"conflict","message":"последний артефакт этапа другой","details":{"reason":"seen_artifact_stale","latest_artifact_id":"c3d4"}}}`)
	})
	_, _, err := capture(t, func() error {
		return cmdDecide(context.Background(), cl, []string{"CLAB-12", "--transition", "analysis_done", "--gate", "human_approval", "--seen-artifact", "a1b2"})
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Code != "conflict" ||
		!strings.Contains(string(apiErr.Details), "seen_artifact_stale") {
		t.Fatalf("409: %v", err)
	}

	usage := commandUsage(t, "decide")
	if !strings.Contains(usage, "--seen-artifact") {
		t.Fatalf("справка decide без --seen-artifact: %s", usage)
	}
}

// AC-5: каждая команда clab, названная в стартовой инструкции, есть в CLI.
func TestWorkPromptCommandsExist(t *testing.T) {
	known := map[string]bool{}
	for _, c := range commands() {
		known[c.name] = true
	}
	ref := regexp.MustCompile(`clab ([a-z][a-z-]*)`)
	for _, usage := range []string{"", "runner"} {
		text := mcp.WorkPrompt("CLAB-12", "developer", usage)
		seen := 0
		for _, m := range ref.FindAllStringSubmatch(text, -1) {
			seen++
			if !known[m[1]] {
				t.Errorf("usage=%q: в инструкции clab %s, а такой команды нет", usage, m[1])
			}
		}
		if seen < 10 {
			t.Errorf("usage=%q: команд clab в инструкции подозрительно мало: %d", usage, seen)
		}
	}
}

// AC-6, AC-10: prompt и guide в справке; справка не называет моделей
// провайдера, пример usage — полным id модели без провайдера.
func TestHelpProviderNeutral(t *testing.T) {
	_, stderr, _ := capture(t, func() error { usage(); return nil })
	for _, want := range []string{"prompt", "guide"} {
		if !strings.Contains(stderr, "  "+want+" ") {
			t.Errorf("clab --help без %s", want)
		}
	}
	var all strings.Builder
	all.WriteString(stderr)
	for _, c := range commands() {
		all.WriteString(c.summary + "\n" + c.usage + "\n")
	}
	low := strings.ToLower(all.String())
	for _, banned := range []string{"haiku", "sonnet", "opus", "claude-"} {
		if strings.Contains(low, banned) {
			t.Errorf("справка называет %q", banned)
		}
	}
	if u := commandUsage(t, "usage"); !strings.Contains(u, "--model <полный id модели из сессии>") {
		t.Errorf("пример usage: %s", u)
	}
	// «Claude» остаётся только у команд, которые с ним и работают (до
	// CLAB-418): плагин и подключение MCP в setup.
	for _, c := range commands() {
		if strings.Contains(c.summary+c.usage, "Claude") && c.name != "setup" {
			t.Errorf("команда %s называет Claude", c.name)
		}
	}
}
