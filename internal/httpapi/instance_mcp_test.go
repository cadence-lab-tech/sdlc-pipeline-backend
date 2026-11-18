package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Инстансовый адрес MCP (CLAB-483, решение 0079, п. 6): один адрес /mcp на
// все проекты участия, проект — на каждом вызове. Номера — критерии приёмки
// анализа CLAB-483. Проекты заводятся со своим циклом (AC-16): ключи
// статусов, роль и число этапов не наши.

// instanceFixture — три проекта со своим циклом и человек с разным участием:
// viewer в ACME, agent в BOLT, не участник ZETA.
type instanceFixture struct {
	*auditFixture
	ctx                context.Context
	alice, aliceBound  string
	aliceTok, boundTok uuid.UUID
	a1, b1, z1         string
}

func newInstanceFixture(t *testing.T) *instanceFixture {
	f := &instanceFixture{auditFixture: newAuditFixture(t), ctx: context.Background()}
	for _, key := range []string{"ACME", "BOLT", "ZETA"} {
		f.ownCycleProject(key)
	}
	store := postgres.New(f.pool)
	if _, err := store.ResolveActor(f.ctx, store.DB(), "alice", domain.PrincipalUser); err != nil {
		t.Fatal(err)
	}
	for key, level := range map[string]domain.MemberRole{"ACME": domain.MemberViewer, "BOLT": domain.MemberAgent} {
		if _, err := f.auth.SetMember(f.ctx, f.admin, key, "user:alice", level, nil); err != nil {
			t.Fatal(err)
		}
	}
	issued, err := f.auth.IssueToken(f.ctx, f.admin, workflow.IssueTokenInput{Name: "alice", Principal: "user:alice"})
	if err != nil {
		t.Fatal(err)
	}
	f.alice, f.aliceTok = issued.Token, issued.Record.ID
	bound, err := f.auth.IssueToken(f.ctx, f.admin, workflow.IssueTokenInput{Name: "alice acme", Principal: "user:alice", ProjectKey: "ACME"})
	if err != nil {
		t.Fatal(err)
	}
	f.aliceBound, f.boundTok = bound.Token, bound.Record.ID
	f.a1, f.b1, f.z1 = f.task("ACME", "Задача A"), f.task("BOLT", "Задача B"), f.task("ZETA", "Задача Z")
	return f
}

// ownCycleProject — проект с циклом из трёх статусов и ролью не из нашей
// настройки.
func (f *instanceFixture) ownCycleProject(key string) {
	f.t.Helper()
	f.ok("POST", "/v1/projects", f.root, map[string]any{"key": key, "name": "Проект " + key})
	report := f.ok("POST", "/v1/projects/"+key+"/import", f.root, map[string]any{
		"version": 1,
		"roles":   []any{map[string]any{"key": "builder", "name": "Сборщик", "instructions": "собирать"}},
		"workflows": []any{map[string]any{
			"name": "Свой", "task_types": []string{"task"},
			"statuses": []any{
				map[string]any{"key": "inbox", "name": "Входящие", "category": "backlog", "is_initial": true},
				map[string]any{"key": "build", "name": "Сборка", "category": "active", "role": "builder"},
				map[string]any{"key": "shipped", "name": "Отгружено", "category": "done", "is_terminal": true},
			},
			"transitions": []any{
				map[string]any{"key": "go", "name": "go", "from": "inbox", "to": "build", "kind": "advance"},
				map[string]any{"key": "ship", "name": "ship", "from": "build", "to": "shipped", "kind": "advance"},
			},
		}},
	})
	f.ok("POST", "/v1/workflows/"+report["workflows"].([]any)[0].(map[string]any)["id"].(string)+"/activate", f.root, nil)
}

func (f *instanceFixture) ok(method, path, token string, body any) map[string]any {
	f.t.Helper()
	code, out := f.json(method, path, token, body)
	if code != http.StatusOK && code != http.StatusCreated {
		f.t.Fatalf("%s %s: %d %v", method, path, code, out)
	}
	return out
}

func (f *instanceFixture) task(project, title string) string {
	f.t.Helper()
	return f.ok("POST", "/v1/projects/"+project+"/tasks", f.root, map[string]any{"title": title})["key"].(string)
}

// rpc — запрос JSON-RPC на адрес MCP: код HTTP и разобранный ответ.
func rpc(h http.Handler, path, token, method string, params any) (*httptest.ResponseRecorder, map[string]any) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req := httptest.NewRequest("POST", path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if strings.HasPrefix(token, "actor:") {
		req.Header.Set("X-SDLC-Actor", strings.TrimPrefix(token, "actor:"))
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	raw := rec.Body.String()
	if i := strings.LastIndex(raw, "data: "); i >= 0 {
		raw = strings.TrimSpace(strings.SplitN(raw[i+len("data: "):], "\n", 2)[0])
	}
	out := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &out)
	return rec, out
}

// call — тул на адресе path: разобранный текст результата и признак ошибки.
func (f *instanceFixture) call(path, token, tool string, args map[string]any) (map[string]any, bool) {
	f.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	rec, out := rpc(f.srv.Handler(), path, token, "tools/call", map[string]any{"name": tool, "arguments": args})
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if rec.Code != http.StatusOK || len(content) == 0 {
		f.t.Fatalf("%s %s: ответ MCP не разобран: %d %s", path, tool, rec.Code, rec.Body)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	body := map[string]any{}
	_ = json.Unmarshal([]byte(text), &body)
	isErr, _ := res["isError"].(bool)
	return body, isErr
}

func projectKeys(out map[string]any) map[string]string {
	got := map[string]string{}
	list, _ := out["projects"].([]any)
	for _, it := range list {
		p := it.(map[string]any)
		got[p["key"].(string)] = fmt.Sprint(p["level"])
	}
	return got
}

// AC-4, AC-7 (список): проекты участия с уровнем; глобальный администратор —
// все; тот же ответ на адресе проекта; токен с привязкой — только свой.
func TestInstanceMCPListProjects(t *testing.T) {
	f := newInstanceFixture(t)
	for _, path := range []string{"/mcp", "/mcp/projects/ACME", "/mcp/projects/BOLT"} {
		out, isErr := f.call(path, f.alice, "sdlc_list_projects", nil)
		want := map[string]string{"ACME": "viewer", "BOLT": "agent"}
		if isErr || fmt.Sprint(projectKeys(out)) != fmt.Sprint(want) {
			t.Fatalf("AC-4 %s: %v", path, out)
		}
		if actor := out["actor"].(map[string]any); actor["handle"] != "alice" {
			t.Fatalf("AC-4 actor: %v", actor)
		}
	}
	out, _ := f.call("/mcp", f.root, "sdlc_list_projects", nil)
	got := projectKeys(out)
	for _, key := range []string{"ACME", "BOLT", "ZETA", "SDLC"} {
		if got[key] != "admin" {
			t.Fatalf("AC-4 администратор: %v", got)
		}
	}
	// Название проекта — как у REST.
	list := out["projects"].([]any)
	for _, it := range list {
		if p := it.(map[string]any); p["key"] == "ACME" && p["name"] != "Проект ACME" {
			t.Fatalf("название: %v", p)
		}
	}
	out, _ = f.call("/mcp", f.aliceBound, "sdlc_list_projects", nil)
	if fmt.Sprint(projectKeys(out)) != "map[ACME:viewer]" {
		t.Fatalf("AC-7 привязка: %v", out)
	}
	// Аргумент project тулу вне проекта не нужен: в схеме его нет, схема
	// отбивает лишнее поле.
	if _, isErr := f.call("/mcp", f.alice, "sdlc_list_projects", map[string]any{"project": "ACME"}); !isErr {
		t.Fatal("sdlc_list_projects принял project")
	}
}

// AC-5: выбор проекта на инстансовом адресе.
func TestInstanceMCPProjectChoice(t *testing.T) {
	f := newInstanceFixture(t)

	// (а) ключ задачи без project.
	out, isErr := f.call("/mcp", f.alice, "sdlc_get_task", map[string]any{"task_key": f.a1})
	if task, _ := out["task"].(map[string]any); isErr || task["key"] != f.a1 {
		t.Fatalf("(а): %v", out)
	}
	// (б) project у тула без ключа.
	out, isErr = f.call("/mcp", f.alice, "sdlc_list_tasks", map[string]any{"project": "ACME"})
	if isErr || !strings.Contains(fmt.Sprint(out), f.a1) || strings.Contains(fmt.Sprint(out), f.b1) {
		t.Fatalf("(б): %v", out)
	}
	// (в) без project и без ключа.
	for _, tool := range []string{"sdlc_list_tasks", "sdlc_whoami", "sdlc_search_context", "sdlc_list_documents"} {
		args := map[string]any{}
		if tool == "sdlc_search_context" {
			args["query"] = "x"
		}
		out, isErr = f.call("/mcp", f.alice, tool, args)
		if code, msg := mcpErr(out); !isErr || code != "invalid_input" || !strings.Contains(msg, "project") ||
			!strings.Contains(msg, "sdlc_list_projects") {
			t.Fatalf("(в) %s: %v", tool, out)
		}
	}
	// (г) project и ключ задачи другого проекта — not_found, как inProject.
	out, isErr = f.call("/mcp", f.alice, "sdlc_get_task", map[string]any{"task_key": f.a1, "project": "BOLT"})
	if code, _ := mcpErr(out); !isErr || code != "not_found" {
		t.Fatalf("(г): %v", out)
	}
	// (д) ключ в других полях.
	out, isErr = f.call("/mcp", f.alice, "sdlc_rank_task", map[string]any{"task": f.b1, "position": "top"})
	if isErr {
		t.Fatalf("(д) rank: %v", out)
	}
	out, isErr = f.call("/mcp", f.alice, "sdlc_link_tasks", map[string]any{"from": f.b1, "to": f.a1})
	if code, _ := mcpErr(out); !isErr || code != "not_found" {
		t.Fatalf("(д) link из BOLT в ACME: %v", out)
	}
	out, isErr = f.call("/mcp", f.alice, "sdlc_create_task", map[string]any{"title": "Ребёнок", "parent": f.b1})
	if key, _ := out["key"].(string); isErr || !strings.HasPrefix(key, "BOLT-") {
		t.Fatalf("(д) create с parent: %v", out)
	}
	// (е) тул по id без ключа задачи требует project.
	out, isErr = f.call("/mcp", f.alice, "sdlc_get_artifact", map[string]any{"artifact_id": uuid.NewString()})
	if code, msg := mcpErr(out); !isErr || code != "invalid_input" || !strings.Contains(msg, "project") {
		t.Fatalf("(е): %v", out)
	}
	// Ключ неверного формата — invalid_input, как на адресе проекта.
	out, isErr = f.call("/mcp", f.alice, "sdlc_get_task", map[string]any{"task_key": "ACME"})
	if code, _ := mcpErr(out); !isErr || code != "invalid_input" {
		t.Fatalf("ключ без номера: %v", out)
	}
}

// AC-9: на адресе проекта прежние ответы; project, равный пути, принят;
// другой — invalid_input, тул не выполнен.
func TestProjectMCPProjectArg(t *testing.T) {
	f := newInstanceFixture(t)
	plain, _ := f.call("/mcp/projects/ACME", f.root, "sdlc_list_tasks", nil)
	same, isErr := f.call("/mcp/projects/ACME", f.root, "sdlc_list_tasks", map[string]any{"project": "ACME"})
	if isErr || fmt.Sprint(plain) != fmt.Sprint(same) {
		t.Fatalf("тот же project: %v\n%v", plain, same)
	}
	before := len(f.rows("a.action = 'sdlc_create_task' AND a.outcome = 'ok'"))
	out, isErr := f.call("/mcp/projects/ACME", f.root, "sdlc_create_task", map[string]any{"title": "Мимо", "project": "BOLT"})
	if code, msg := mcpErr(out); !isErr || code != "invalid_input" || !strings.Contains(msg, "BOLT") {
		t.Fatalf("другой project: %v", out)
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM tasks WHERE title = 'Мимо'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("тул выполнен: %d %v", n, err)
	}
	if after := len(f.rows("a.action = 'sdlc_create_task' AND a.outcome = 'ok'")); after != before {
		t.Fatal("запись успешного заведения")
	}
}

// AC-6, AC-16: коды и тексты на инстансовом адресе совпадают с адресом
// проекта того же проекта: таблица «тул × уровень × участие».
func TestInstanceMCPRightsParity(t *testing.T) {
	f := newInstanceFixture(t)
	maint := f.memberToken("user:mila", "BOLT", domain.MemberMaintainer)
	type call struct {
		tool  string
		args  func(project string) map[string]any
		byKey bool // проект из ключа задачи, без project
	}
	calls := []call{
		{"sdlc_whoami", func(string) map[string]any { return map[string]any{} }, false},
		{"sdlc_list_tasks", func(string) map[string]any { return map[string]any{} }, false},
		{"sdlc_list_documents", func(string) map[string]any { return map[string]any{} }, false},
		{"sdlc_get_task", func(p string) map[string]any { return map[string]any{"task_key": p + "-1"} }, true},
		{"sdlc_check_gates", func(p string) map[string]any {
			return map[string]any{"task_key": p + "-1", "transition": "go"}
		}, true},
		{"sdlc_update_task", func(p string) map[string]any {
			return map[string]any{"task_key": p + "-1", "title": "Правка " + p}
		}, true},
		// Запись: заведение и правка повторяются без смены исхода, поэтому
		// второй вызов того же набора сравним с первым.
		{"sdlc_create_task", func(string) map[string]any { return map[string]any{"title": "Паритет"} }, false},
	}
	tokens := map[string]string{"alice": f.alice, "alice-acme": f.aliceBound, "mila": maint}
	for name, tok := range tokens {
		for _, p := range []string{"ACME", "BOLT", "ZETA", "NOPE"} {
			for _, c := range calls {
				args := c.args(p)
				want, wantErr := f.call("/mcp/projects/"+p, tok, c.tool, args)
				inst := map[string]any{}
				for k, v := range args {
					inst[k] = v
				}
				if !c.byKey {
					inst["project"] = p
				}
				got, gotErr := f.call("/mcp", tok, c.tool, inst)
				wc, wm := mcpErr(want)
				gc, gm := mcpErr(got)
				if wantErr != gotErr || wc != gc || wm != gm {
					t.Errorf("%s %s %s: адрес проекта %v %q %q, инстанс %v %q %q",
						name, p, c.tool, wantErr, wc, wm, gotErr, gc, gm)
				}
			}
		}
	}
}

func (f *instanceFixture) memberToken(handle, project string, level domain.MemberRole) string {
	f.t.Helper()
	store := postgres.New(f.pool)
	if _, err := store.ResolveActor(f.ctx, store.DB(), strings.TrimPrefix(handle, "user:"), domain.PrincipalUser); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.auth.SetMember(f.ctx, f.admin, project, handle, level, nil); err != nil {
		f.t.Fatal(err)
	}
	issued, err := f.auth.IssueToken(f.ctx, f.admin, workflow.IssueTokenInput{Name: handle, Principal: handle})
	if err != nil {
		f.t.Fatal(err)
	}
	return issued.Token
}

// AC-7: токен с привязкой и токен задачи на инстансовом адресе.
func TestInstanceMCPBoundAndTaskToken(t *testing.T) {
	f := newInstanceFixture(t)

	// Привязка: проект по умолчанию — из токена, чужой — отказ, и у администратора.
	out, isErr := f.call("/mcp", f.aliceBound, "sdlc_whoami", nil)
	if p, _ := out["project"].(map[string]any); isErr || p["key"] != "ACME" {
		t.Fatalf("whoami по привязке: %v", out)
	}
	rootBound, err := f.auth.IssueToken(f.ctx, f.admin, workflow.IssueTokenInput{Name: "root acme", ProjectKey: "ACME"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{f.aliceBound, rootBound.Token} {
		for _, args := range []map[string]any{{"project": "BOLT"}, {"task_key": f.b1}} {
			tool := "sdlc_list_tasks"
			if args["task_key"] != nil {
				tool = "sdlc_get_task"
			}
			out, isErr := f.call("/mcp", tok, tool, args)
			if code, msg := mcpErr(out); !isErr || code != "forbidden" || msg != "токен привязан к другому проекту" {
				t.Fatalf("чужой проект %v: %v", args, out)
			}
		}
	}

	// Токен задачи: агент в BOLT забирает задачу своего цикла.
	bot := f.agentToken("agent:builder-bot", "BOLT")
	f.ok("POST", "/v1/tasks/"+f.b1+"/transitions/go", f.root, map[string]any{})
	f.ok("POST", "/v1/tasks/"+f.b1+"/claim", bot, map[string]any{})
	issued := f.ok("POST", "/v1/tasks/"+f.b1+"/task-token", bot, nil)
	taskTok := issued["token"].(string)

	out, isErr = f.call("/mcp", taskTok, "sdlc_whoami", nil)
	if p, _ := out["project"].(map[string]any); isErr || p["key"] != "BOLT" {
		t.Fatalf("токен задачи whoami: %v", out)
	}
	if out, isErr = f.call("/mcp", taskTok, "sdlc_get_task", map[string]any{"task_key": f.b1}); isErr {
		t.Fatalf("своя задача: %v", out)
	}
	// Тул без ключа и без project — прогон токена в проекте привязки.
	if out, isErr = f.call("/mcp", taskTok, "sdlc_heartbeat", nil); isErr {
		t.Fatalf("heartbeat по привязке: %v", out)
	}
	out, _ = f.call("/mcp", taskTok, "sdlc_list_projects", nil)
	if fmt.Sprint(projectKeys(out)) != "map[BOLT:agent]" {
		t.Fatalf("список токена задачи: %v", out)
	}
	out, isErr = f.call("/mcp", taskTok, "sdlc_move_task", map[string]any{"task_key": f.b1, "to": "inbox", "reason": "x"})
	if code, msg := mcpErr(out); !isErr || code != "forbidden" || !strings.Contains(msg, workflow.ExecutorRefusal) {
		t.Fatalf("вне белого списка: %v", out)
	}
	out, isErr = f.call("/mcp", taskTok, "sdlc_get_task", map[string]any{"task_key": f.a1})
	if code, _ := mcpErr(out); !isErr || code != "forbidden" {
		t.Fatalf("задача другого проекта: %v", out)
	}

	// AC-6: захват и переход через инстансовый адрес — то же право этапа.
	b2 := f.task("BOLT", "Задача B2")
	f.ok("POST", "/v1/tasks/"+b2+"/transitions/go", f.root, map[string]any{})
	if out, isErr = f.call("/mcp", bot, "sdlc_claim_task", map[string]any{"task_key": b2}); isErr {
		t.Fatalf("захват через /mcp: %v", out)
	}
	out, isErr = f.call("/mcp", bot, "sdlc_request_transition", map[string]any{"task_key": b2, "transition": "ship"})
	if task, _ := out["task"].(map[string]any); isErr || task["status"] != "shipped" {
		t.Fatalf("переход через /mcp: %v", out)
	}
}

func (f *instanceFixture) agentToken(handle, project string) string {
	f.t.Helper()
	if _, err := f.auth.SetMember(f.ctx, f.admin, project, handle, domain.MemberAgent, nil); err != nil {
		f.t.Fatal(err)
	}
	issued, err := f.auth.IssueToken(f.ctx, f.admin, workflow.IssueTokenInput{Name: handle, Principal: handle})
	if err != nil {
		f.t.Fatal(err)
	}
	return issued.Token
}

// AC-8: журнал вызовов инстансового адреса — проект и задача найденные на
// вызове; отказ до выбора проекта — без проекта; журнал проекта их видит.
func TestInstanceMCPAudit(t *testing.T) {
	f := newInstanceFixture(t)
	f.call("/mcp", f.alice, "sdlc_get_task", map[string]any{"task_key": f.a1})
	f.call("/mcp", f.alice, "sdlc_list_tasks", map[string]any{"project": "BOLT"})
	f.call("/mcp", f.alice, "sdlc_list_tasks", nil)
	f.call("/mcp", f.alice, "sdlc_whoami", map[string]any{"project": "ZETA"})

	rows := f.rows("a.channel = 'mcp' AND a.token_id = $1", f.aliceTok)
	type seen struct{ action, project, task, outcome string }
	var got []seen
	for _, r := range rows {
		s := seen{action: r.Action, outcome: r.Outcome}
		if r.Project != nil {
			s.project = *r.Project
		}
		if r.Task != nil {
			s.task = *r.Task
		}
		got = append(got, s)
	}
	want := []seen{
		{"sdlc_get_task", "ACME", f.a1, "ok"},
		{"sdlc_list_tasks", "BOLT", "", "ok"},
		{"sdlc_list_tasks", "", "", "invalid_input"},
		{"sdlc_whoami", "ZETA", "", "forbidden"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("журнал:\n%v\nждали\n%v", got, want)
	}
	code, out := f.json("GET", "/v1/projects/ACME/audit", f.root, nil)
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(out), "sdlc_get_task") {
		t.Fatalf("журнал проекта: %d %v", code, out)
	}
	// 401 с неверным токеном на /mcp — запись канала MCP без проекта.
	rec, _ := rpc(f.srv.Handler(), "/mcp", "sdlc_nope", "tools/list", map[string]any{})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("401: %d", rec.Code)
	}
	denials := f.rows("a.channel = 'mcp' AND a.action = 'mcp' AND a.outcome = 'unauthenticated'")
	if len(denials) != 1 || denials[0].Project != nil || denials[0].Params != `{"path": {}}` {
		t.Fatalf("отказ 401: %+v", denials)
	}
}

// AC-3: initialize и tools/list на инстансовом адресе — тулы адреса проекта и
// sdlc_list_projects; редирект /mcp/ на канонический адрес.
func TestInstanceMCPTransport(t *testing.T) {
	f := newInstanceFixture(t)
	h := f.srv.Handler()
	initParams := map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "t", "version": "1"}}
	if rec, out := rpc(h, "/mcp", f.alice, "initialize", initParams); rec.Code != http.StatusOK || out["result"] == nil {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	names := func(path string) []string {
		rec, out := rpc(h, path, f.alice, "tools/list", map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("tools/list %s: %d", path, rec.Code)
		}
		var list []string
		for _, it := range out["result"].(map[string]any)["tools"].([]any) {
			list = append(list, it.(map[string]any)["name"].(string))
		}
		slices.Sort(list)
		return list
	}
	inst, proj := names("/mcp"), names("/mcp/projects/ACME")
	if !slices.Equal(inst, proj) || !slices.Contains(inst, "sdlc_list_projects") || len(inst) < 40 {
		t.Fatalf("tools/list: %v\n%v", inst, proj)
	}
	rec, _ := rpc(h, "/mcp/", f.alice, "tools/list", map[string]any{})
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/mcp" {
		t.Fatalf("/mcp/: %d %v", rec.Code, rec.Header())
	}
}

// dummyArgs — минимальный вход по схеме тула: обязательные поля заполнены
// значениями нужного типа; ключи задач — key, идентификаторы — UUID. Нужен,
// чтобы вызов дошёл до проверки прав, а не упал на схеме.
func dummyArgs(schema map[string]any, key string) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	args := map[string]any{}
	required, _ := schema["required"].([]any)
	for _, r := range required {
		name := r.(string)
		args[name] = dummyValue(name, props[name].(map[string]any), key)
	}
	return args
}

func dummyValue(name string, sch map[string]any, key string) any {
	if enum, ok := sch["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	typ, _ := sch["type"].(string)
	if list, ok := sch["type"].([]any); ok {
		for _, it := range list {
			if s := it.(string); s != "null" {
				typ = s
			}
		}
	}
	switch typ {
	case "object":
		return dummyArgs(sch, key)
	case "array":
		return []any{}
	case "integer", "number":
		return 1
	case "boolean":
		return false
	}
	switch {
	case slices.Contains([]string{"task_key", "task", "from", "to", "parent", "before", "after"}, name):
		return key
	case name == "id" || strings.HasSuffix(name, "_id"):
		return uuid.NewString()
	}
	return "x"
}

// Граница проектов у каждого тула: человек, который не участник проекта, и
// токен с привязкой к другому проекту не получают ни одного результата ни
// по аргументу project, ни по ключу задачи чужого проекта. Перечень — из
// tools/list, поэтому новый тул без проверки прав на инстансовом адресе (где
// путь больше не ограничивает проект) валит тест.
func TestInstanceMCPEveryToolChecksProject(t *testing.T) {
	f := newInstanceFixture(t)
	_, out := rpc(f.srv.Handler(), "/mcp", f.alice, "tools/list", map[string]any{})
	tools, _ := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 40 {
		t.Fatalf("tools/list: %d тулов", len(tools))
	}
	cases := []struct{ who, token, project, key string }{
		{"не участник ZETA", f.alice, "ZETA", f.z1},
		{"токен привязан к ACME, проект BOLT", f.aliceBound, "BOLT", f.b1},
	}
	for _, it := range tools {
		tool := it.(map[string]any)
		name := tool["name"].(string)
		if name == "sdlc_list_projects" {
			continue
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		for _, c := range cases {
			variants := []map[string]any{}
			args := dummyArgs(schema, c.key)
			withProject := map[string]any{"project": c.project}
			for k, v := range args {
				withProject[k] = v
			}
			variants = append(variants, withProject)
			// Проект — только из ключа задачи, без аргумента project.
			if args["task_key"] != nil || args["task"] != nil || args["from"] != nil {
				variants = append(variants, args)
			}
			for _, in := range variants {
				got, isErr := f.call("/mcp", c.token, name, in)
				if code, msg := mcpErr(got); !isErr || code != "forbidden" {
					t.Errorf("%s: %s %v: ждали forbidden, получили %v %q %q", name, c.who, in, isErr, code, msg)
				}
			}
		}
	}
}
