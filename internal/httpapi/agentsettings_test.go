package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Настройка агентов проекта по REST и MCP (CLAB-425, решение 0077 п. 4.5–4.9):
// версии, права с причинами, секреты нигде не оседают, журнал «было → стало»,
// ответ захвата и карточка. Номера — критерии приёмки аналитики CLAB-425.

const agentSettingsPath = "/v1/projects/SDLC/agent-settings"

// syncBuffer — буфер лога сервера: секрет не должен попасть и туда.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newLoggedFixture — newAuditFixture с логом сервера в буфер.
func newLoggedFixture(t *testing.T) (*auditFixture, *syncBuffer) {
	t.Helper()
	ctx := context.Background()
	buf := &syncBuffer{}
	store, pool := testdb.Store(t)
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	authoring := workflow.NewAuthoring(store, log, engine)
	mcpServer := mcp.NewServer(log, store, engine, authoring, auth, "test")
	srv := NewServer(config.Config{Env: "test"}, log, store, engine, authoring, auth, mcpServer)
	const root = "bootstrap-token-0123456789"
	if err := auth.Bootstrap(ctx, "root", root); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.Authenticate(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SetMember(ctx, admin, "SDLC", "agent:bot", domain.MemberAgent, nil); err != nil {
		t.Fatal(err)
	}
	issued, err := auth.IssueToken(ctx, admin, workflow.IssueTokenInput{Name: "bot", Principal: "agent:bot", ProjectKey: "SDLC"})
	if err != nil {
		t.Fatal(err)
	}
	return &auditFixture{t: t, srv: srv, pool: pool, auth: auth, admin: admin,
		root: root, bot: issued.Token, botTok: issued.Record.ID}, buf
}

func agentDoc(high string) map[string]any {
	return map[string]any{
		"agents": map[string]any{
			"claude": map[string]any{
				"models": []string{"claude-haiku-4-5", high},
				"levels": map[string]any{
					"low":  map[string]any{"model": "claude-haiku-4-5"},
					"high": map[string]any{"model": high, "effort": "high"},
				},
			},
		},
		"pricing": map[string]any{
			"as_of":  "2026-11-01",
			"models": map[string]any{high: map[string]any{"input": 1.0, "output": 2.0}},
		},
	}
}

func detailPath(body map[string]any) (string, bool) {
	e, _ := body["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	p, ok := d["path"].(string)
	return p, ok
}

// sizedDoc — годный документ ровно size байт компактного JSON (тот же
// приём, что в тесте проверки движка).
func sizedDoc(size int) json.RawMessage {
	head, tail := `{"agents":{"claude":{"models":[`, `]}}}`
	room := size - len(head) - len(tail)
	var items []string
	for {
		sep := 0
		if len(items) > 0 {
			sep = 1
		}
		if last := room - sep - 2; last >= 1 && last <= 200 {
			items = append(items, strings.Repeat("m", last))
			break
		}
		items = append(items, strings.Repeat("m", 100))
		room -= sep + 102
	}
	return json.RawMessage(head + `"` + strings.Join(items, `","`) + `"` + tail)
}

// AC-3, AC-4, AC-6–AC-10, AC-14: версии по REST и MCP с журналом.
func TestAgentSettingsVersionsOverREST(t *testing.T) {
	f := newAuditFixture(t)
	lead := f.memberToken("lead", domain.MemberMaintainer, nil)

	// AC-9: настройки нет — {"version": null}, список пуст.
	if code, body := f.json("GET", agentSettingsPath, f.bot, nil); code != http.StatusOK || len(body) != 1 || body["version"] != nil {
		t.Fatalf("без настройки: %d %v", code, body)
	}
	if code, body := f.json("GET", agentSettingsPath+"/versions", f.bot, nil); code != http.StatusOK || len(body["versions"].([]any)) != 0 {
		t.Fatalf("пустой список: %d %v", code, body)
	}

	// AC-3: без тела и без активной — пустой документ, номер 1.
	code, v1 := f.json("POST", agentSettingsPath+"/draft", lead, nil)
	if code != http.StatusCreated || v1["version"] != float64(1) || v1["state"] != "draft" ||
		mustJSON(t, v1["document"]) != `{}` || v1["created_by"] != "lead" || v1["created_at"] == nil ||
		v1["activated_by"] != nil || v1["activated_at"] != nil {
		t.Fatalf("пустой черновик: %d %v", code, v1)
	}
	if code, body := f.json("POST", agentSettingsPath+"/draft", lead, map[string]any{"from": "empty"}); code != http.StatusConflict {
		t.Fatalf("второй черновик: %d %v", code, body)
	}

	// AC-4: замена целиком, changelog; без document — 400; агент вне набора — предупреждение.
	doc := agentDoc("claude-opus-5-5")
	doc["agents"].(map[string]any)["acme-cli"] = map[string]any{}
	code, put := f.json("PUT", agentSettingsPath+"/draft", lead, map[string]any{"document": doc, "changelog": "первая таблица"})
	if code != http.StatusOK || put["changelog"] != "первая таблица" || !strings.Contains(mustJSON(t, put["warnings"]), "acme-cli") ||
		!strings.Contains(mustJSON(t, put["document"]), "claude-opus-5-5") {
		t.Fatalf("замена: %d %v", code, put)
	}
	if code, body := f.json("PUT", agentSettingsPath+"/draft", lead, map[string]any{"changelog": "x"}); code != http.StatusBadRequest {
		t.Fatalf("PUT без document: %d %v", code, body)
	}
	if code, body := f.json("PUT", agentSettingsPath+"/draft", lead, map[string]any{"document": map[string]any{"agents": map[string]any{}}}); code != http.StatusOK ||
		mustJSON(t, body["document"]) != `{"agents":{}}` || body["changelog"] != "первая таблица" {
		t.Fatalf("замена целиком без changelog: %d %v", code, body)
	}
	if code, _ := f.json("PUT", agentSettingsPath+"/draft", lead, map[string]any{"document": doc}); code != http.StatusOK {
		t.Fatal("возврат документа")
	}
	if code, body := f.json("POST", agentSettingsPath+"/draft", lead, map[string]any{"changelog": strings.Repeat("я", 2001)}); code != http.StatusBadRequest {
		t.Fatalf("длинный changelog: %d %v", code, body)
	} else if p, _ := detailPath(body); p != "changelog" {
		t.Fatalf("путь changelog: %v", body)
	}

	// AC-7: номер не тот — 409, верный — активация с retired null.
	if code, body := f.json("POST", agentSettingsPath+"/draft/activate", lead, map[string]any{"version": 2}); code != http.StatusConflict {
		t.Fatalf("чужой номер: %d %v", code, body)
	}
	code, act := f.json("POST", agentSettingsPath+"/draft/activate", lead, nil)
	activated, _ := act["activated"].(map[string]any)
	if code != http.StatusOK || act["retired"] != nil || activated["version"] != float64(1) || activated["state"] != "active" ||
		activated["activated_by"] != "lead" || activated["activated_at"] == nil {
		t.Fatalf("первая активация: %d %v", code, act)
	}
	if code, _ := f.json("POST", agentSettingsPath+"/draft/activate", lead, nil); code != http.StatusNotFound {
		t.Fatalf("активация без черновика: %d", code)
	}

	// AC-3: копия активной без тела; document важнее from.
	code, v2 := f.json("POST", agentSettingsPath+"/draft", lead, map[string]any{"changelog": "копия"})
	if code != http.StatusCreated || v2["version"] != float64(2) || v2["sha256"] != activated["sha256"] {
		t.Fatalf("копия активной: %d %v", code, v2)
	}
	// AC-8: удаление, повтор — 404, номер свободен.
	if code, _ := f.json("DELETE", agentSettingsPath+"/draft", lead, nil); code != http.StatusNoContent {
		t.Fatalf("удаление: %d", code)
	}
	if code, _ := f.json("DELETE", agentSettingsPath+"/draft", lead, nil); code != http.StatusNotFound {
		t.Fatalf("повторное удаление: %d", code)
	}
	if code, _ := f.json("GET", agentSettingsPath+"/versions/2", f.bot, nil); code != http.StatusNotFound {
		t.Fatalf("удалённая версия: %d", code)
	}
	if code, _ := f.json("PUT", agentSettingsPath+"/draft", lead, map[string]any{"document": doc}); code != http.StatusNotFound {
		t.Fatalf("PUT без черновика: %d", code)
	}
	code, v2 = f.json("POST", agentSettingsPath+"/draft", lead, map[string]any{"from": "empty", "document": agentDoc("claude-opus-6")})
	if code != http.StatusCreated || v2["version"] != float64(2) || !strings.Contains(mustJSON(t, v2["document"]), "claude-opus-6") {
		t.Fatalf("document важнее from: %d %v", code, v2)
	}
	if code, body := f.json("POST", agentSettingsPath+"/draft/activate", lead, map[string]any{"version": 2}); code != http.StatusOK || body["retired"] != float64(1) {
		t.Fatalf("вторая активация: %d %v", code, body)
	}
	if code, body := f.json("POST", agentSettingsPath+"/draft", lead, map[string]any{"from": "other"}); code != http.StatusBadRequest {
		t.Fatalf("from other: %d %v", code, body)
	} else if p, _ := detailPath(body); p != "from" {
		t.Fatalf("путь from: %v", body)
	}

	// AC-9: чтение — активная целиком, список без документов, черновик по номеру.
	code, active := f.json("GET", agentSettingsPath, f.bot, nil)
	for _, field := range []string{"project", "version", "state", "document", "changelog", "sha256", "created_by", "created_at", "activated_by", "activated_at", "warnings"} {
		if _, ok := active[field]; !ok {
			t.Errorf("в активной нет %s: %v", field, active)
		}
	}
	if code != http.StatusOK || active["version"] != float64(2) || active["project"] != "SDLC" {
		t.Fatalf("активная: %d %v", code, active)
	}
	if code, _ := f.json("POST", agentSettingsPath+"/draft", lead, map[string]any{"from": "empty"}); code != http.StatusCreated {
		t.Fatal("черновик 3")
	}
	_, list := f.json("GET", agentSettingsPath+"/versions", f.bot, nil)
	states := ""
	for _, raw := range list["versions"].([]any) {
		item := raw.(map[string]any)
		if _, ok := item["document"]; ok {
			t.Fatalf("документ в списке: %v", item)
		}
		if len(item["sha256"].(string)) != 64 {
			t.Fatalf("sha256 в списке: %v", item)
		}
		states += fmt.Sprintf("%v:%v ", item["version"], item["state"])
	}
	if states != "1:retired 2:active 3:draft " {
		t.Fatalf("список: %s", states)
	}
	if code, body := f.json("GET", agentSettingsPath+"/versions/3", f.bot, nil); code != http.StatusOK || body["state"] != "draft" {
		t.Fatalf("черновик по номеру: %d %v", code, body)
	}
	if code, _ := f.json("GET", agentSettingsPath+"/versions/99", f.bot, nil); code != http.StatusNotFound {
		t.Fatalf("нет версии: %d", code)
	}
	// AC-6: предупреждение видно и при чтении версии.
	if _, body := f.json("GET", agentSettingsPath+"/versions/1", f.bot, nil); !strings.Contains(mustJSON(t, body["warnings"]), "acme-cli") {
		t.Fatalf("warnings при чтении: %v", body)
	}

	// AC-10: MCP — те же данные.
	got, isErr := f.mcp(f.bot, "sdlc_get_agent_settings", map[string]any{})
	if isErr || got["sha256"] != active["sha256"] || got["version"] != float64(2) {
		t.Fatalf("MCP активная: %v", got)
	}
	got, isErr = f.mcp(f.bot, "sdlc_get_agent_settings", map[string]any{"version": 3})
	if isErr || got["state"] != "draft" {
		t.Fatalf("MCP по номеру: %v", got)
	}
	got, isErr = f.mcp(f.bot, "sdlc_list_agent_settings_versions", map[string]any{})
	if isErr || mustJSON(t, got["versions"]) != mustJSON(t, list["versions"]) {
		t.Fatalf("MCP список: %v", got)
	}

	// MCP запись: есть черновик — заменяет; удаление; нет — заводит; активация.
	if out, isErr := f.mcp(lead, "sdlc_put_agent_settings_draft", map[string]any{"document": agentDoc("claude-opus-7"), "changelog": "mcp"}); isErr || out["created"] != false {
		t.Fatalf("MCP замена: %v", out)
	}
	if out, isErr := f.mcp(lead, "sdlc_discard_agent_settings_draft", map[string]any{}); isErr || out["discarded"] != float64(3) {
		t.Fatalf("MCP удаление: %v", out)
	}
	if out, isErr := f.mcp(lead, "sdlc_discard_agent_settings_draft", map[string]any{}); !isErr || errCode(out) != "not_found" {
		t.Fatalf("MCP повторное удаление: %v", out)
	}
	if out, isErr := f.mcp(lead, "sdlc_put_agent_settings_draft", map[string]any{"from": "active"}); isErr || out["created"] != true {
		t.Fatalf("MCP заведение: %v", out)
	}
	if out, isErr := f.mcp(lead, "sdlc_activate_agent_settings", map[string]any{"version": 9}); !isErr || errCode(out) != "conflict" {
		t.Fatalf("MCP чужой номер: %v", out)
	}
	if out, isErr := f.mcp(lead, "sdlc_activate_agent_settings", map[string]any{"version": 3}); isErr || out["retired"] != float64(2) {
		t.Fatalf("MCP активация: %v", out)
	}

	// AC-14: каждая запись — строка журнала с «было → стало» обоих каналов.
	type change struct {
		Action  string `json:"action"`
		Version int    `json:"version"`
		Before  *struct {
			Version int    `json:"version"`
			SHA256  string `json:"sha256"`
			Size    int    `json:"size"`
		} `json:"before"`
		After *struct {
			Version int    `json:"version"`
			State   string `json:"state"`
			SHA256  string `json:"sha256"`
		} `json:"after"`
		Retired *int `json:"retired"`
	}
	var trail []string
	for _, row := range f.rows(`(a.action LIKE '%agent-settings%' OR a.action LIKE '%agent_settings%') AND a.outcome = 'ok'`) {
		if row.Action == "GET /v1/projects/:projectKey/agent-settings" || strings.Contains(row.Action, "sdlc_get_") || strings.Contains(row.Action, "sdlc_list_") {
			continue
		}
		var p struct {
			Change change         `json:"change"`
			Body   map[string]any `json:"body"`
		}
		if err := json.Unmarshal([]byte(row.Params), &p); err != nil {
			t.Fatal(err)
		}
		c := p.Change
		entry := fmt.Sprintf("%s %s %d", row.Channel, c.Action, c.Version)
		if c.Before != nil {
			entry += fmt.Sprintf(" before=%d", c.Before.Version)
		}
		if c.After != nil {
			entry += fmt.Sprintf(" after=%d/%s", c.After.Version, c.After.State)
		}
		if c.Retired != nil {
			entry += fmt.Sprintf(" retired=%d", *c.Retired)
		}
		trail = append(trail, entry)
		// Документ из тела — размером и sha256, не содержимым.
		if d, ok := p.Body["document"].(map[string]any); ok && (d["sha256"] == nil || d["agents"] != nil) {
			t.Fatalf("документ в журнале как есть: %s", row.Params)
		}
	}
	want := []string{
		"rest draft_created 1 after=1/draft",
		"rest draft_replaced 1 before=1 after=1/draft",
		"rest draft_replaced 1 before=1 after=1/draft",
		"rest draft_replaced 1 before=1 after=1/draft",
		"rest activated 1 after=1/active",
		"rest draft_created 2 after=2/draft",
		"rest draft_discarded 2 before=2",
		"rest draft_created 2 after=2/draft",
		"rest activated 2 before=1 after=2/active retired=1",
		"rest draft_created 3 after=3/draft",
		"mcp draft_replaced 3 before=3 after=3/draft",
		"mcp draft_discarded 3 before=3",
		"mcp draft_created 3 after=3/draft",
		"mcp activated 3 before=2 after=3/active retired=2",
	}
	if strings.Join(trail, "\n") != strings.Join(want, "\n") {
		t.Fatalf("журнал:\n%s\nждали\n%s", strings.Join(trail, "\n"), strings.Join(want, "\n"))
	}
	// Отказы 400 и 409 — строки с исходом своего кода.
	if rows := f.rows(`a.action = 'POST /v1/projects/:projectKey/agent-settings/draft' AND a.outcome = 'invalid_input'`); len(rows) != 2 {
		t.Fatalf("отказы 400 в журнале: %d", len(rows))
	}
	// Журнал проекта читает сопровождающий, viewer — нет.
	viewer := f.memberToken("watcher", domain.MemberViewer, nil)
	if code, _ := f.json("GET", "/v1/projects/SDLC/audit", viewer, nil); code != http.StatusForbidden {
		t.Fatalf("журнал viewer'у: %d", code)
	}
	if code, _ := f.json("GET", "/v1/projects/SDLC/audit?action=activated", lead, nil); code != http.StatusOK {
		t.Fatalf("журнал сопровождающему: %d", code)
	}
}

// AC-11, AC-12: права записи по классу актора в REST и MCP, чтение —
// участникам и токену задачи, не участнику — ничего.
func TestAgentSettingsRights(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()
	lead := f.memberToken("lead", domain.MemberMaintainer, nil)
	boss := f.memberToken("boss", domain.MemberAdmin, nil)
	viewer := f.memberToken("watcher", domain.MemberViewer, nil)
	employee := f.memberToken("emp", domain.MemberAgent, nil)
	botMaintainer := f.memberToken("agent:keeper", domain.MemberMaintainer, nil)
	botAdmin := f.memberToken("agent:chief", domain.MemberAdmin, nil)
	// Не участник: человек без участия с токеном без проекта.
	if _, err := postgres.New(f.pool).ResolveActor(ctx, f.pool, "stranger", domain.PrincipalUser); err != nil {
		t.Fatal(err)
	}
	issued, err := f.auth.IssueToken(ctx, f.admin, workflow.IssueTokenInput{Name: "stranger", Principal: "user:stranger"})
	if err != nil {
		t.Fatal(err)
	}
	stranger := issued.Token

	// Токен задачи: держатель — сопровождающий.
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/transitions/start", f.root, map[string]any{}); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	if code, body := f.json("POST", "/v1/tasks/SDLC-1/claim", lead, map[string]any{}); code != http.StatusOK {
		t.Fatalf("claim: %d %v", code, body)
	}
	code, tt := f.json("POST", "/v1/tasks/SDLC-1/task-token", lead, nil)
	if code != http.StatusCreated {
		t.Fatalf("токен задачи: %d %v", code, tt)
	}
	taskToken := tt["token"].(string)

	versions := func() int {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_settings_versions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	writes := []struct{ method, path, tool string }{
		{"POST", agentSettingsPath + "/draft", "sdlc_put_agent_settings_draft"},
		{"PUT", agentSettingsPath + "/draft", ""},
		{"DELETE", agentSettingsPath + "/draft", "sdlc_discard_agent_settings_draft"},
		{"POST", agentSettingsPath + "/draft/activate", "sdlc_activate_agent_settings"},
	}
	body := map[string]any{"document": map[string]any{}}
	denied := []struct {
		name, token, reason string
	}{
		{"viewer", viewer, workflow.DenyLevel},
		{"сотрудник", employee, workflow.DenyLevel},
		{"бот agent", f.bot, workflow.DenyHumanAction},
		{"бот maintainer", botMaintainer, workflow.DenyHumanAction},
		{"бот admin", botAdmin, workflow.DenyHumanAction},
		{"токен задачи", taskToken, "task"},
		{"не участник", stranger, "member"},
	}
	// Черновик есть, чтобы PUT/DELETE/activate отказывали правом, а не 404.
	if code, _ := f.json("POST", agentSettingsPath+"/draft", lead, nil); code != http.StatusCreated {
		t.Fatal("черновик")
	}
	before := versions()
	for _, d := range denied {
		for _, w := range writes {
			code, out := f.json(w.method, w.path, d.token, body)
			msg, reason := errOf(out)
			ok := code == http.StatusForbidden
			switch d.reason {
			case "task":
				ok = ok && strings.HasPrefix(msg, workflow.ExecutorRefusal)
			case "member":
			default:
				ok = ok && reason == d.reason
			}
			if !ok {
				t.Errorf("%s %s %s: %d %v", d.name, w.method, w.path, code, out)
			}
			// Не участнику MCP проекта отказывает подключение, до тула.
			if w.tool == "" || d.reason == "member" {
				continue
			}
			res, isErr := f.mcp(d.token, w.tool, map[string]any{})
			msg, reason = errOf(res)
			ok = isErr && errCode(res) == "forbidden"
			if d.reason == "task" {
				ok = ok && strings.HasPrefix(msg, workflow.ExecutorRefusal)
			} else {
				ok = ok && reason == d.reason
			}
			if !ok {
				t.Errorf("%s MCP %s: %v", d.name, w.tool, res)
			}
		}
		if got := versions(); got != before {
			t.Fatalf("%s: отказ изменил данные (%d → %d)", d.name, before, got)
		}
	}
	// Каждый отказ 403 — строка журнала с исходом forbidden.
	if rows := f.rows(`a.action IN ('POST /v1/projects/:projectKey/agent-settings/draft', 'PUT /v1/projects/:projectKey/agent-settings/draft',
		'DELETE /v1/projects/:projectKey/agent-settings/draft', 'POST /v1/projects/:projectKey/agent-settings/draft/activate')
		AND a.outcome = 'forbidden'`); len(rows) != len(denied)*len(writes) {
		t.Fatalf("отказы в журнале: %d, ожидали %d", len(rows), len(denied)*len(writes))
	}

	// Чтение: участники и токен задачи — 200, не участник — 403.
	for name, tok := range map[string]string{"viewer": viewer, "сотрудник": employee, "бот": f.bot, "токен задачи": taskToken, "admin": boss} {
		for _, p := range []string{agentSettingsPath, agentSettingsPath + "/versions", agentSettingsPath + "/versions/1"} {
			if code, out := f.json("GET", p, tok, nil); code != http.StatusOK {
				t.Errorf("%s читает %s: %d %v", name, p, code, out)
			}
		}
		for _, tool := range []string{"sdlc_get_agent_settings", "sdlc_list_agent_settings_versions"} {
			if out, isErr := f.mcp(tok, tool, map[string]any{}); isErr {
				t.Errorf("%s: %s %v", name, tool, out)
			}
		}
	}
	if code, _ := f.json("GET", agentSettingsPath, stranger, nil); code != http.StatusForbidden {
		t.Fatalf("не участник читает: %d", code)
	}

	// Сопровождающий-человек уровня admin правит.
	if code, out := f.json("PUT", agentSettingsPath+"/draft", boss, body); code != http.StatusOK {
		t.Fatalf("admin правит: %d %v", code, out)
	}
	if out, isErr := f.mcp(boss, "sdlc_activate_agent_settings", map[string]any{}); isErr {
		t.Fatalf("admin активирует по MCP: %v", out)
	}

	// AC-12: белый список — два тула чтения и три GET, ни одной записи.
	var listed []string
	for _, a := range workflow.ExecutorActions() {
		if strings.Contains(a, "agent-settings") || strings.Contains(a, "agent_settings") {
			listed = append(listed, a)
		}
	}
	if strings.Join(listed, " | ") != "GET /v1/projects/:projectKey/agent-settings | "+
		"GET /v1/projects/:projectKey/agent-settings/versions | GET /v1/projects/:projectKey/agent-settings/versions/:version | "+
		"sdlc_get_agent_settings | sdlc_list_agent_settings_versions" {
		t.Fatalf("белый список: %v", listed)
	}
}

// AC-11: в режиме заголовка правило вида принципала тоже действует.
func TestAgentSettingsHeaderMode(t *testing.T) {
	store, _ := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthHeader)
	srv := NewServer(config.Config{Env: "test", DefaultActor: "anonymous"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), auth, nil)
	call := func(actor string) (int, string) {
		req := httptest.NewRequest("POST", agentSettingsPath+"/draft", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(ActorHeader, actor)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := call("agent:somebot"); code != http.StatusForbidden || !strings.Contains(body, workflow.DenyHumanAction) {
		t.Fatalf("агент в режиме заголовка: %d %s", code, body)
	}
	if code, body := call("user:alice"); code != http.StatusCreated {
		t.Fatalf("человек в режиме заголовка: %d %s", code, body)
	}
}

// AC-13: секрет — 400 с путём; значения нет в ответе, журнале, логе и базе.
// AC-5: предел размера — 65536 проходит, 65537 нет, по REST и MCP.
func TestAgentSettingsSecretsAndSize(t *testing.T) {
	f, logs := newLoggedFixture(t)
	ctx := context.Background()
	lead := f.memberToken("lead", domain.MemberMaintainer, nil)
	if code, _ := f.json("POST", agentSettingsPath+"/draft", lead, nil); code != http.StatusCreated {
		t.Fatal("черновик")
	}

	n := 0
	secret := func() string {
		n++
		return fmt.Sprintf("sdlc_UniqueSecret%04dXyZ", n)
	}
	type sent struct {
		value string
		doc   map[string]any
		path  string
	}
	var cases []sent
	for _, mk := range []func(s string) (map[string]any, string){
		func(s string) (map[string]any, string) {
			return map[string]any{"agents": map[string]any{"claude": map[string]any{"levels": map[string]any{"high": map[string]any{"model": s}}}}},
				"agents.claude.levels.high.model"
		},
		func(s string) (map[string]any, string) {
			return map[string]any{"agents": map[string]any{"claude": map[string]any{"levels": map[string]any{"high": map[string]any{"model": "m", "effort": s}}}}},
				"agents.claude.levels.high.effort"
		},
		func(s string) (map[string]any, string) {
			return map[string]any{"agents": map[string]any{"claude": map[string]any{"models": []string{"m", s}}}}, "agents.claude.models[1]"
		},
		func(s string) (map[string]any, string) {
			return map[string]any{"pricing": map[string]any{"as_of": "2026-11-01", "models": map[string]any{s: map[string]any{"input": 1, "output": 1}}}},
				"pricing.models.[sdlc_…]"
		},
		func(s string) (map[string]any, string) {
			return map[string]any{"agents": map[string]any{"my-token-bot": map[string]any{"models": []string{s[5:]}}}}, "agents.my-token-bot"
		},
		func(s string) (map[string]any, string) {
			return map[string]any{"token": s[5:]}, "token"
		},
		func(s string) (map[string]any, string) {
			return map[string]any{"pricing": map[string]any{"Secret": s[5:]}}, "pricing.Secret"
		},
		func(s string) (map[string]any, string) {
			return map[string]any{"agents": map[string]any{"claude": map[string]any{"api_key": s[5:], "apikey": "x", "password": "y"}}}, "agents.claude.api_key"
		},
	} {
		for range 3 {
			s := secret()
			doc, path := mk(s)
			// У ключевых секретов значение без префикса — его тоже нигде быть не должно.
			value := s
			if !strings.Contains(mustJSON(t, doc), s) {
				value = s[5:]
			}
			cases = append(cases, sent{value: value, doc: doc, path: path})
		}
	}
	for i, c := range cases {
		var code int
		var out map[string]any
		var raw string
		switch i % 3 {
		case 0: // REST PUT
			rec := f.do("PUT", agentSettingsPath+"/draft", lead, map[string]any{"document": c.doc})
			code, raw = rec.Code, rec.Body.String()
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
		case 1: // REST POST при черновике — сначала удалить
			if code, _ := f.json("DELETE", agentSettingsPath+"/draft", lead, nil); code != http.StatusNoContent {
				t.Fatal("удаление")
			}
			rec := f.do("POST", agentSettingsPath+"/draft", lead, map[string]any{"document": c.doc})
			code, raw = rec.Code, rec.Body.String()
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if code != http.StatusBadRequest {
				break
			}
			if c, _ := f.json("POST", agentSettingsPath+"/draft", lead, nil); c != http.StatusCreated {
				t.Fatal("черновик после отказа")
			}
		case 2: // MCP
			res, isErr := f.mcp(lead, "sdlc_put_agent_settings_draft", map[string]any{"document": c.doc})
			out, raw = res, mustJSON(t, res)
			code = http.StatusOK
			if isErr && errCode(res) == "invalid_input" {
				code = http.StatusBadRequest
			}
		}
		if p, _ := detailPath(out); code != http.StatusBadRequest || p != c.path {
			t.Errorf("случай %d (%s): %d путь %q, ожидали %q: %s", i, c.value, code, p, c.path, raw)
		}
		if strings.Contains(raw, c.value) {
			t.Errorf("случай %d: значение в ответе: %s", i, raw)
		}
	}

	// Активация документа с секретом, записанного в обход сервиса.
	bypass := secret()
	if _, err := f.pool.Exec(ctx, `UPDATE agent_settings_versions SET document = jsonb_build_object('agents',
		jsonb_build_object('claude', jsonb_build_object('levels', jsonb_build_object('low', jsonb_build_object('model', $1::text)))))
		WHERE state = 'draft'`, bypass); err != nil {
		t.Fatal(err)
	}
	for _, via := range []string{"rest", "mcp"} {
		var raw string
		var p string
		if via == "rest" {
			rec := f.do("POST", agentSettingsPath+"/draft/activate", lead, nil)
			raw = rec.Body.String()
			var out map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			p, _ = detailPath(out)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("активация с секретом: %d %s", rec.Code, raw)
			}
		} else {
			res, isErr := f.mcp(lead, "sdlc_activate_agent_settings", map[string]any{})
			raw = mustJSON(t, res)
			p, _ = detailPath(res)
			if !isErr || errCode(res) != "invalid_input" {
				t.Fatalf("MCP активация с секретом: %s", raw)
			}
		}
		if p != "agents.claude.levels.low.model" || strings.Contains(raw, bypass) {
			t.Fatalf("%s активация: путь %q, ответ %s", via, p, raw)
		}
	}
	if _, err := f.pool.Exec(ctx, `UPDATE agent_settings_versions SET document = '{}' WHERE state = 'draft'`); err != nil {
		t.Fatal(err)
	}

	// Значение нигде: журнал целиком, лог сервера, документы версий.
	var audit, docs string
	if err := f.pool.QueryRow(ctx, `SELECT coalesce(string_agg(params::text, E'\n'), '') FROM audit_log`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT coalesce(string_agg(document::text, E'\n'), '') FROM agent_settings_versions`).Scan(&docs); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		for name, hay := range map[string]string{"журнал": audit, "лог": logs.String(), "база": docs} {
			if strings.Contains(hay, c.value) {
				t.Errorf("значение %s в: %s", c.value, name)
			}
		}
	}
	if strings.Contains(audit, bypass) || strings.Contains(logs.String(), bypass) {
		t.Errorf("секрет активации в журнале или логе")
	}
	if rows := f.rows(`a.outcome = 'invalid_input' AND (a.action LIKE '%agent-settings%' OR a.action LIKE '%agent_settings%')`); len(rows) < len(cases) {
		t.Errorf("отказы 400 в журнале: %d", len(rows))
	}

	// Похожее, но не секрет, проходит.
	if code, out := f.json("PUT", agentSettingsPath+"/draft", lead, map[string]any{"document": map[string]any{
		"agents": map[string]any{"codex": map[string]any{"levels": map[string]any{"high": map[string]any{"model": "gpt-6.1-sol"}}}}}}); code != http.StatusOK {
		t.Fatalf("обычная модель: %d %v", code, out)
	}

	// Размер: ровно 65536 байт JSON документа проходит, 65537 нет.
	at, over := sizedDoc(workflow.AgentSettingsMaxBytes), sizedDoc(workflow.AgentSettingsMaxBytes+1)
	if len(at) != 65536 || len(over) != 65537 {
		t.Fatalf("заготовки: %d %d", len(at), len(over))
	}
	if code, out := f.json("PUT", agentSettingsPath+"/draft", lead, map[string]any{"document": at}); code != http.StatusOK {
		t.Fatalf("65536 по REST: %d %v", code, out["error"])
	}
	if code, out := f.json("PUT", agentSettingsPath+"/draft", lead, map[string]any{"document": over}); code != http.StatusBadRequest {
		t.Fatalf("65537 по REST: %d", code)
	} else if p, ok := detailPath(out); !ok || p != "" {
		t.Fatalf("путь предела: %v", out)
	}
	if out, isErr := f.mcp(lead, "sdlc_put_agent_settings_draft", map[string]any{"document": at}); isErr {
		t.Fatalf("65536 по MCP: %v", out["error"])
	}
	if out, isErr := f.mcp(lead, "sdlc_put_agent_settings_draft", map[string]any{"document": over}); !isErr || errCode(out) != "invalid_input" {
		t.Fatalf("65537 по MCP: %v", out)
	} else if p, ok := detailPath(out); !ok || p != "" {
		t.Fatalf("путь предела MCP: %v", out)
	}
	// Документ на пределе пишется в журнал размером и sha256 — запись читаема.
	rows := f.rows(`a.action = 'PUT /v1/projects/:projectKey/agent-settings/draft' AND a.outcome = 'ok'`)
	last := rows[len(rows)-1]
	if len(last.Params) > 16<<10 || !strings.Contains(last.Params, `"sha256"`) || !strings.Contains(last.Params, `"change"`) {
		t.Fatalf("запись журнала на пределе: %d байт %.300s", len(last.Params), last.Params)
	}
}

// AC-18, AC-19: execution одинаковый у REST claim и state, MCP claim и
// get_task; stage_runs[] карточки несут номер снимка.
func TestAgentSettingsInClaimResponse(t *testing.T) {
	f := newAuditFixture(t)
	lead := f.memberToken("lead", domain.MemberMaintainer, nil)

	start := func(key string) {
		t.Helper()
		if code, body := f.json("POST", "/v1/tasks/"+key+"/transitions/start", f.root, map[string]any{}); code != http.StatusOK {
			t.Fatalf("start %s: %d %v", key, code, body)
		}
	}
	execution := func(body map[string]any) string {
		return mustJSON(t, body["execution"])
	}

	// Без настройки: version null, моделей нет — и прежние поля как были.
	start("SDLC-1")
	_, claimed := f.json("POST", "/v1/tasks/SDLC-1/claim", f.bot, map[string]any{})
	if got := execution(claimed); got != `{"agent_settings_version":null,"model_level":null,"model_level_source":"none"}` {
		t.Fatalf("execution без настройки: %s", got)
	}

	// Активная v1 с уровнем high у этапа задачи.
	if code, _ := f.json("POST", agentSettingsPath+"/draft", lead, map[string]any{"document": agentDoc("claude-opus-5-5")}); code != http.StatusCreated {
		t.Fatal("черновик")
	}
	if code, _ := f.json("POST", agentSettingsPath+"/draft/activate", lead, nil); code != http.StatusOK {
		t.Fatal("активация")
	}
	if code, body := f.json("PATCH", "/v1/tasks/SDLC-3", f.root, map[string]any{"model_level": "high"}); code != http.StatusOK {
		t.Fatalf("уровень задачи: %d %v", code, body)
	}
	start("SDLC-3")
	want := `{"agent_settings_version":1,"model_level":"high","model_level_source":"task","models":{"claude":{"effort":"high","model":"claude-opus-5-5"}}}`
	_, state := f.json("GET", "/v1/tasks/SDLC-3/state", f.bot, nil)
	if got := execution(state); got != want {
		t.Fatalf("state до захвата: %s", got)
	}
	mcpClaim, isErr := f.mcp(f.bot, "sdlc_claim_task", map[string]any{"task_key": "SDLC-3"})
	if isErr || execution(mcpClaim) != want {
		t.Fatalf("MCP claim: %v", mcpClaim["execution"])
	}
	// Активация v2 не трогает идущий прогон ни в одном из четырёх ответов.
	if code, _ := f.json("POST", agentSettingsPath+"/draft", lead, map[string]any{"document": agentDoc("claude-opus-6")}); code != http.StatusCreated {
		t.Fatal("черновик 2")
	}
	if code, _ := f.json("POST", agentSettingsPath+"/draft/activate", lead, nil); code != http.StatusOK {
		t.Fatal("активация 2")
	}
	_, restClaim := f.json("POST", "/v1/tasks/SDLC-3/claim", f.bot, map[string]any{})
	_, state = f.json("GET", "/v1/tasks/SDLC-3/state", f.bot, nil)
	mcpTask, _ := f.mcp(f.bot, "sdlc_get_task", map[string]any{"task_key": "SDLC-3"})
	for name, body := range map[string]map[string]any{"REST claim": restClaim, "REST state": state, "MCP get_task": mcpTask} {
		if got := execution(body); got != want {
			t.Errorf("%s: %s", name, got)
		}
	}
	// Прежний клиент: структура executionView CLI прежней версии декодирует ответ.
	var old struct {
		Execution *struct {
			ModelLevel       *string `json:"model_level"`
			ModelLevelSource string  `json:"model_level_source"`
		} `json:"execution"`
	}
	if err := json.Unmarshal([]byte(mustJSON(t, restClaim)), &old); err != nil || old.Execution == nil || *old.Execution.ModelLevel != "high" {
		t.Fatalf("прежний клиент: %v %+v", err, old)
	}

	// Карточка: у захваченного прогона SDLC-3 — 1, у прогона SDLC-1 (без
	// настройки) — null; поле есть у каждого прогона, у прогонов без
	// захвата (вход в начальный статус) — null.
	versionOf := func(card map[string]any) []string {
		var out []string
		for _, raw := range card["stage_runs"].([]any) {
			r := raw.(map[string]any)
			v, ok := r["agent_settings_version"]
			if !ok {
				out = append(out, "нет поля")
				continue
			}
			out = append(out, fmt.Sprint(v))
		}
		return out
	}
	// Последний прогон — текущий, захваченный; прежние — без снимка.
	check := func(name string, card map[string]any, last string) {
		t.Helper()
		got := versionOf(card)
		if len(got) == 0 || got[len(got)-1] != last {
			t.Fatalf("%s: %v, у текущего прогона ждали %s", name, got, last)
		}
		for _, v := range got[:len(got)-1] {
			if v != "<nil>" {
				t.Fatalf("%s: %v, у прежних прогонов ждали null", name, got)
			}
		}
	}
	_, card := f.json("GET", "/v1/tasks/SDLC-3", f.root, nil)
	check("карточка SDLC-3", card, "1")
	_, card = f.json("GET", "/v1/tasks/SDLC-1", f.root, nil)
	check("карточка SDLC-1", card, "<nil>")
	check("stage_runs MCP", mcpTask, "1")
}
