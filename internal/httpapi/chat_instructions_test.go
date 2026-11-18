package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// MCP для хостового чата (CLAB-484). Номера — критерии приёмки анализа
// (AC-18.N). Проект заводится со своим циклом: ключи статусов, роль и гейт
// не из нашей настройки.

// AC-18.1: initialize на инстансовом адресе и на адресе проекта отдаёт один и
// тот же непустой текст в пределе длины; версия сервера прежняя.
func TestInitializeInstructionsOnBothAddresses(t *testing.T) {
	f := newInstanceFixture(t)
	h := f.srv.Handler()
	initParams := map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "t", "version": "1"}}
	texts := map[string]string{}
	for _, path := range []string{"/mcp", "/mcp/projects/ACME"} {
		rec, out := rpc(h, path, f.alice, "initialize", initParams)
		res, _ := out["result"].(map[string]any)
		if rec.Code != http.StatusOK || res == nil {
			t.Fatalf("initialize %s: %d %s", path, rec.Code, rec.Body)
		}
		texts[path], _ = res["instructions"].(string)
		if info, _ := res["serverInfo"].(map[string]any); info["version"] != "test" {
			t.Errorf("%s: serverInfo %v", path, info)
		}
	}
	if texts["/mcp"] == "" || texts["/mcp"] != texts["/mcp/projects/ACME"] {
		t.Fatalf("instructions на двух адресах различаются или пусты:\n%q\n%q", texts["/mcp"], texts["/mcp/projects/ACME"])
	}
	if texts["/mcp"] != mcp.Instructions {
		t.Error("instructions в ответе — не mcp.Instructions")
	}
	if n := utf8.RuneCountInString(texts["/mcp"]); n > mcp.InstructionsMaxRunes {
		t.Errorf("instructions %d знаков, предел %d", n, mcp.InstructionsMaxRunes)
	}
}

// AC-18.6: tools/list по HTTP отдаёт аннотации под ключом annotations, с
// явными readOnlyHint и destructiveHint у каждого тула, на обоих адресах.
func TestToolsListCarriesAnnotationsOverHTTP(t *testing.T) {
	f := newInstanceFixture(t)
	for _, path := range []string{"/mcp", "/mcp/projects/ACME"} {
		rec, out := rpc(f.srv.Handler(), path, f.alice, "tools/list", map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("tools/list %s: %d", path, rec.Code)
		}
		tools, _ := out["result"].(map[string]any)["tools"].([]any)
		if len(tools) < 40 {
			t.Fatalf("%s: тулов %d", path, len(tools))
		}
		for _, raw := range tools {
			tool := raw.(map[string]any)
			a, _ := tool["annotations"].(map[string]any)
			if a == nil || a["title"] == "" {
				t.Errorf("%s %v: нет annotations.title: %v", path, tool["name"], a)
				continue
			}
			for _, hint := range []string{"readOnlyHint", "destructiveHint"} {
				if _, ok := a[hint].(bool); !ok {
					t.Errorf("%s %v: %s не задан явно: %v", path, tool["name"], hint, a)
				}
			}
		}
	}
}

// approvalProject — проект с циклом из трёх статусов и гейтом human_approval
// на переходе в конец; роль исполнителя — builder.
func (f *instanceFixture) approvalProject(key string) {
	f.t.Helper()
	f.ok("POST", "/v1/projects", f.root, map[string]any{"key": key, "name": "Проект " + key})
	report := f.ok("POST", "/v1/projects/"+key+"/import", f.root, map[string]any{
		"version": 1,
		"roles": []any{
			map[string]any{"key": "builder", "name": "Сборщик", "instructions": "собирать"},
			map[string]any{"key": "auditor", "name": "Аудитор", "instructions": "проверять"},
		},
		"workflows": []any{map[string]any{
			"name": "Подпись", "task_types": []string{"task"},
			"statuses": []any{
				map[string]any{"key": "inbox", "name": "Входящие", "category": "backlog", "is_initial": true},
				map[string]any{"key": "build", "name": "Сборка", "category": "active", "role": "builder"},
				map[string]any{"key": "shipped", "name": "Отгружено", "category": "done", "is_terminal": true},
			},
			"transitions": []any{
				map[string]any{"key": "go", "name": "go", "from": "inbox", "to": "build", "kind": "advance"},
				map[string]any{"key": "ship", "name": "ship", "from": "build", "to": "shipped", "kind": "advance",
					"gates": []any{map[string]any{"key": "sign", "name": "Подпись", "type": "human_approval",
						"config": map[string]any{"min_approvals": 1}}}},
			},
		}},
	})
	f.ok("POST", "/v1/workflows/"+report["workflows"].([]any)[0].(map[string]any)["id"].(string)+"/activate", f.root, nil)
}

// chatMember — участник проекта с личным токеном (человек или агент-принципал).
func (f *instanceFixture) chatMember(project, handle string, level domain.MemberRole, roles []string) string {
	f.t.Helper()
	store := postgres.New(f.pool)
	kind := domain.PrincipalUser
	if strings.HasPrefix(handle, "agent:") {
		kind = domain.PrincipalAgent
	}
	name := strings.TrimPrefix(strings.TrimPrefix(handle, "user:"), "agent:")
	if _, err := store.ResolveActor(f.ctx, store.DB(), name, kind); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.auth.SetMember(f.ctx, f.admin, project, handle, level, roles); err != nil {
		f.t.Fatal(err)
	}
	issued, err := f.auth.IssueToken(f.ctx, f.admin, workflow.IssueTokenInput{Name: handle, Principal: handle, ProjectKey: project})
	if err != nil {
		f.t.Fatal(err)
	}
	return issued.Token
}

func latestArtifactID(f *instanceFixture, key string) string {
	f.t.Helper()
	card := f.ok("GET", "/v1/tasks/"+key+"/artifacts", f.root, nil)
	arts, _ := card["artifacts"].([]any)
	if len(arts) == 0 {
		f.t.Fatalf("у %s нет артефактов: %v", key, card)
	}
	return arts[len(arts)-1].(map[string]any)["id"].(string)
}

// AC-18.7: сценарий апрува, как его ведёт чат по instructions, на проекте со
// своим циклом: найти → прочитать → гейты → решение с актуальным
// seen_artifact_id → переход отдельно. Сервер не обещает больше, чем сказано в
// тексте: устаревший артефакт — conflict без записи решения, участник ниже
// права и бот чужой роли — forbidden, токен задачи — отказ тула.
func TestChatApprovalScenario(t *testing.T) {
	f := newInstanceFixture(t)
	f.approvalProject("CHAT")
	human := f.chatMember("CHAT", "user:dana", domain.MemberAgent, nil)
	watcher := f.chatMember("CHAT", "user:eve", domain.MemberViewer, nil)
	stranger := f.chatMember("CHAT", "agent:other", domain.MemberAgent, []string{"auditor"})
	owner := f.chatMember("CHAT", "agent:owner", domain.MemberAgent, []string{"builder"})

	// Общий адрес: проект — из ключа задачи, аргумента project не нужно.
	call := func(token, tool string, args map[string]any) (map[string]any, bool) {
		return f.call("/mcp", token, tool, args)
	}
	prepare := func(title string) string {
		key := f.task("CHAT", title)
		f.ok("POST", "/v1/tasks/"+key+"/transitions/go", f.root, nil)
		f.ok("POST", "/v1/tasks/"+key+"/artifacts", f.root, map[string]any{"kind": "work.result", "title": "итог", "content": "# итог"})
		return key
	}
	key := prepare("На подпись")

	// Найти и прочитать: очередь, состояние, гейты.
	list, isErr := call(human, "sdlc_list_tasks", map[string]any{"project": "CHAT"})
	if isErr || !strings.Contains(jsonText(list), key) {
		t.Fatalf("sdlc_list_tasks: %v", list)
	}
	task, isErr := call(human, "sdlc_get_task", map[string]any{"task_key": key})
	if isErr || task["task"] == nil {
		t.Fatalf("sdlc_get_task: %v", task)
	}
	gates, isErr := call(human, "sdlc_check_gates", map[string]any{"task_key": key, "transition": "ship"})
	if isErr || !strings.Contains(jsonText(gates), "human_approval") || !strings.Contains(jsonText(gates), "actor_decisions") {
		t.Fatalf("sdlc_check_gates: %v", gates)
	}

	// Отказы по праву: наблюдатель и бот чужой роли.
	seen := latestArtifactID(f, key)
	decide := func(token, key, seen string) (map[string]any, bool) {
		return call(token, "sdlc_decide_gate", map[string]any{"task_key": key, "transition": "ship", "gate": "sign",
			"status": "passed", "seen_artifact_id": seen})
	}
	for name, token := range map[string]string{"наблюдатель": watcher, "бот чужой роли": stranger} {
		out, isErr := decide(token, key, seen)
		if msg, _ := errOf(out); !isErr || msg == "" {
			t.Fatalf("%s: апрув не отказан: %v", name, out)
		}
		if code, _ := out["error"].(map[string]any)["code"].(string); code != "forbidden" {
			t.Fatalf("%s: код %q: %v", name, code, out)
		}
	}
	if st := f.statusOf(key); st != "build" {
		t.Fatalf("после отказов статус %q", st)
	}

	// Устаревший seen_artifact_id: после новой сдачи — conflict, решение не записано.
	f.ok("POST", "/v1/tasks/"+key+"/artifacts", f.root, map[string]any{"kind": "work.result", "title": "итог 2", "content": "# итог 2"})
	out, isErr := decide(human, key, seen)
	if _, reason := errOf(out); !isErr || reason != workflow.SeenConflict {
		t.Fatalf("устаревший seen: %v", out)
	}
	if code, _ := out["error"].(map[string]any)["code"].(string); code != "conflict" {
		t.Fatalf("код устаревшего seen: %v", out)
	}
	if n := f.decisions(key); n != 0 {
		t.Fatalf("решений записано %d после отказов", n)
	}

	// Актуальный seen_artifact_id — решение записано; задача на месте, пока
	// человек не попросил переход.
	seen = latestArtifactID(f, key)
	out, isErr = decide(human, key, seen)
	if isErr || out["status"] != "passed" || out["gate_key"] != "sign" {
		t.Fatalf("апрув: %v", out)
	}
	if st := f.statusOf(key); st != "build" {
		t.Fatalf("апрув сам перевёл задачу: статус %q", st)
	}
	out, isErr = call(human, "sdlc_request_transition", map[string]any{"task_key": key, "transition": "ship"})
	if isErr {
		t.Fatalf("переход: %v", out)
	}
	if st := f.statusOf(key); st != "shipped" {
		t.Fatalf("после перехода статус %q", st)
	}

	// Токен задачи: тул закрыт белым списком, даже когда право этапа есть.
	second := prepare("Вторая")
	if code, body := f.json("POST", "/v1/tasks/"+second+"/claim", owner, map[string]any{}); code != http.StatusOK {
		t.Fatalf("захват: %d %v", code, body)
	}
	code, issued := f.json("POST", "/v1/tasks/"+second+"/task-token", owner, nil)
	if code != http.StatusCreated {
		t.Fatalf("токен задачи: %d %v", code, issued)
	}
	out, isErr = decide(issued["token"].(string), second, latestArtifactID(f, second))
	if msg, _ := errOf(out); !isErr || !strings.HasPrefix(msg, workflow.ExecutorRefusal) {
		t.Fatalf("sdlc_decide_gate токеном задачи: %v", out)
	}
}

func jsonText(v map[string]any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch y := x.(type) {
		case string:
			b.WriteString(y + " ")
		case map[string]any:
			for k, e := range y {
				b.WriteString(k + " ")
				walk(e)
			}
		case []any:
			for _, e := range y {
				walk(e)
			}
		}
	}
	walk(v)
	return b.String()
}

func (f *instanceFixture) statusOf(key string) string {
	f.t.Helper()
	task, err := postgres.New(f.pool).GetTaskByKey(context.Background(), postgres.New(f.pool).DB(), key)
	if err != nil {
		f.t.Fatal(err)
	}
	return task.StatusKey
}

// decisions — число записанных решений по гейтам задачи.
func (f *instanceFixture) decisions(key string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM task_events e JOIN tasks tk ON tk.id = e.task_id
		WHERE tk.key = $1 AND e.type = 'gate.decided'`, key).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
