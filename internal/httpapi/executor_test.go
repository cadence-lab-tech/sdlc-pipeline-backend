package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Исполнитель этапа по REST (CLAB-208, решение 0049 п. 3–4): белый список
// на каждом маршруте /v1, область, гейты, переходы, круг записи. Токен
// задачи — у человека уровня maintainer (путь /clab-run) и у администратора.

// publicV1 — маршруты /v1 вне группы с аутентификацией: у них нет актора, и
// токен задачи им не предъявляют. Новый открытый маршрут сюда вносят явно.
var publicV1 = map[string]bool{
	"GET /v1/auth/oidc": true, "POST /v1/auth/oidc/login": true,
	"GET /v1/auth/github": true, "POST /v1/auth/github/login": true,
	"POST /v1/auth/cli": true, "POST /v1/auth/cli/poll": true,
	"GET /v1/instance": true, "GET /v1/agent-kit": true,
}

func executorRefused(body map[string]any) bool {
	e, _ := body["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return e["code"] == "forbidden" && strings.HasPrefix(msg, workflow.ExecutorRefusal)
}

// samplePath подставляет в шаблон маршрута ключи демо-проекта: до ручки
// запрос токена задачи вне перечня не доходит, значения нужны только роутеру.
func samplePath(tpl string) string {
	r := strings.NewReplacer(
		":projectKey", "SDLC", ":taskKey", "SDLC-1", ":roleKey", "analyst",
		":teamKey", "acme", ":handle", "user:alice", ":owner", "acme", ":name", "api",
		":code", "ABCD-EFGH", ":version", "1", ":statusKey", "analysis",
		":transitionKey", "analysis_done", ":gateKey", "artifacts",
	)
	out := r.Replace(tpl)
	for _, p := range []string{":workflowID", ":tokenID", ":inviteID", ":documentID", ":artifactID", ":signalID", ":runID"} {
		out = strings.ReplaceAll(out, p, "00000000-0000-0000-0000-000000000001")
	}
	return out
}

func TestExecutorOverREST(t *testing.T) {
	ctx := context.Background()
	srv, store, call := newTaskTokenServer(t)
	root, err := srv.auth.Authenticate(ctx, rootToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveActor(ctx, store.DB(), "alice", domain.PrincipalUser); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.auth.SetMember(ctx, root, "SDLC", "user:alice", domain.MemberMaintainer, nil); err != nil {
		t.Fatal(err)
	}
	personal := func(principal string) string {
		t.Helper()
		issued, err := srv.auth.IssueToken(ctx, root, workflow.IssueTokenInput{
			Name: "personal", Principal: principal, ProjectKey: "SDLC",
		})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Token
	}
	alice, pool := personal("user:alice"), personal("agent:pool")
	// Второй проект, где алиса тоже состоит: токен задачи его не видит.
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO projects (key, name, owner_team_id)
		SELECT 'ALT', 'Другой', owner_team_id FROM projects WHERE key = 'SDLC'`); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.auth.SetMember(ctx, root, "ALT", "user:alice", domain.MemberMaintainer, nil); err != nil {
		t.Fatal(err)
	}
	// Чек-лист на analysis_done: в демо-цикле его там нет.
	if _, err := store.DB().Exec(ctx, `
		INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
		SELECT tr.id, 'selfhosted', 'Self-hosted учтён', 'checklist',
		       '{"items":[{"id":"selfhosted","text":"учтён","required":true}]}', true, 9
		FROM workflow_transitions tr JOIN tasks t ON t.workflow_id = tr.workflow_id
		WHERE t.key = 'SDLC-1' AND tr.key = 'analysis_done'`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, token string }{
		{"/v1/tasks/SDLC-1/transitions/start", rootToken},
		{"/v1/tasks/SDLC-3/transitions/start", rootToken},
		{"/v1/tasks/SDLC-1/claim", alice},
		{"/v1/tasks/SDLC-3/claim", rootToken},
	} {
		if code, body := call("POST", c.path, c.token, nil); code != http.StatusOK {
			t.Fatalf("%s: %d %v", c.path, code, body)
		}
	}
	issue := func(key, token string) string {
		t.Helper()
		code, body := call("POST", "/v1/tasks/"+key+"/task-token", token, nil)
		if code != http.StatusCreated {
			t.Fatalf("токен задачи %s: %d %v", key, code, body)
		}
		return body["token"].(string)
	}
	tm, tadm := issue("SDLC-1", alice), issue("SDLC-3", rootToken)

	// Критерий 7: каждый маршрут /v1 вне перечня отвечает токену задачи 403
	// до уровня и is_admin; каждая строка перечня — существующий маршрут.
	routes := map[string]bool{}
	for _, r := range srv.Handler().(*gin.Engine).Routes() {
		action := workflow.RESTAction(r.Method, r.Path)
		if !strings.HasPrefix(r.Path, "/v1/") || publicV1[action] {
			continue
		}
		routes[action] = true
	}
	allowed := map[string]bool{}
	for _, a := range workflow.ExecutorActions() {
		if strings.Contains(a, " /v1/") {
			allowed[a] = true
			if !routes[a] {
				t.Errorf("в перечне маршрут, которого нет: %s", a)
			}
		}
	}
	closed := 0
	for action := range routes {
		if allowed[action] {
			continue
		}
		method, tpl, _ := strings.Cut(action, " ")
		for _, tok := range []string{tm, tadm} {
			if code, body := call(method, samplePath(tpl), tok, map[string]any{}); code != http.StatusForbidden || !executorRefused(body) {
				t.Errorf("%s токеном задачи: %d %v", action, code, body)
			}
		}
		closed++
	}
	if closed < 60 {
		t.Fatalf("закрытых маршрутов подозрительно мало: %d", closed)
	}

	// Критерий 8: то, чем ходит clab, — открыто.
	for _, path := range []string{
		"/v1/auth/me", "/v1/projects/SDLC", "/v1/projects/SDLC/roles", "/v1/projects/SDLC/roles/analyst",
		"/v1/projects/SDLC/documents", "/v1/projects/SDLC/tasks", "/v1/projects/SDLC/queue",
		"/v1/tasks/SDLC-1", "/v1/tasks/SDLC-1/state", "/v1/tasks/SDLC-1/timeline",
		"/v1/tasks/SDLC-1/artifacts", "/v1/tasks/SDLC-1/drift", "/v1/tasks/SDLC-1/repo-refs",
		"/v1/tasks/SDLC-1/transitions", "/v1/tasks/SDLC-1/usage",
		// События долгим опросом (CLAB-323): рукопожатие без after.
		"/v1/tasks/SDLC-1/events", "/v1/projects/SDLC/events",
	} {
		if code, body := call("GET", path, tm, nil); code != http.StatusOK {
			t.Errorf("GET %s токеном задачи: %d %v", path, code, body)
		}
	}
	// Критерий 9: проекты — только проект токена; личный токен видит оба.
	_, body := call("GET", "/v1/projects", tm, nil)
	if ps, _ := body["projects"].([]any); len(ps) != 1 || ps[0].(map[string]any)["key"] != "SDLC" {
		t.Errorf("проекты токеном задачи: %v", body)
	}
	_, body = call("GET", "/v1/projects", alice, nil)
	if ps, _ := body["projects"].([]any); len(ps) != 2 {
		t.Errorf("проекты личным токеном: %v", body)
	}

	// Критерий 10: область и свойства — ни в какую сторону.
	for _, req := range []map[string]any{
		{"add_repositories": []string{"cadence-lab-tech/sdlc-pipeline-frontend"}},
		{"remove_repositories": []string{"cadence-lab-tech/sdlc-pipeline-backend"}},
		{"properties": map[string]any{"multi_repo": true}},
	} {
		if code, body := call("PATCH", "/v1/tasks/SDLC-1/scope", tm, req); code != http.StatusForbidden || !executorRefused(body) {
			t.Errorf("область токеном задачи %v: %d %v", req, code, body)
		}
	}
	task, _ := store.GetTaskByKey(ctx, store.DB(), "SDLC-1")
	if task.MultiRepo() || len(task.Repositories) != 1 {
		t.Errorf("область изменилась: %+v", task.Repositories)
	}

	// Критерии 11–13: чек-лист своего этапа — да, остальное — нет.
	decide := func(token, task, transition, gate, status string) (int, map[string]any) {
		return call("POST", "/v1/tasks/"+task+"/transitions/"+transition+"/gates/"+gate+"/decision", token,
			map[string]any{"status": status, "details": map[string]any{"checked": []string{"selfhosted"}}})
	}
	if code, body := decide(tm, "SDLC-1", "analysis_done", "selfhosted", "passed"); code != http.StatusOK {
		t.Fatalf("чек-лист токеном задачи: %d %v", code, body)
	}
	for _, c := range []struct{ token, task, transition, gate, status string }{
		{tm, "SDLC-1", "analysis_done", "selfhosted", "waived"},
		{tm, "SDLC-1", "analysis_done", "selfhosted", "failed"},
		{tm, "SDLC-1", "analysis_done", "artifacts", "passed"},
		{tm, "SDLC-1", "architecture_done", "traceability", "passed"},
		{tm, "SDLC-1", "analysis_done", "human_approval", "passed"},
		{tadm, "SDLC-3", "analysis_done", "human_approval", "passed"},
	} {
		if code, body := decide(c.token, c.task, c.transition, c.gate, c.status); code != http.StatusForbidden || !executorRefused(body) {
			t.Errorf("решение %s/%s %s: %d %v", c.transition, c.gate, c.status, code, body)
		}
	}
	// Чужая задача — отказ своей задачи в решении по гейту: читать её токен
	// может (CLAB-305), решать по ней — нет.
	if code, _ := decide(tm, "SDLC-3", "analysis_done", "selfhosted", "passed"); code != http.StatusForbidden {
		t.Errorf("чек-лист чужой задачи: %d", code)
	}
	// Тот же человек личным токеном — апрув проходит; агент — 403 по уровню, как раньше.
	if code, body := decide(alice, "SDLC-1", "analysis_done", "human_approval", "passed"); code != http.StatusOK {
		t.Errorf("апрув личным токеном: %d %v", code, body)
	}
	code, body := decide(pool, "SDLC-1", "analysis_done", "selfhosted", "passed")
	if e, _ := body["error"].(map[string]any); code != http.StatusForbidden || e["message"] != "нужен уровень maintainer, а есть agent" {
		t.Errorf("чек-лист личным токеном агента: %d %v", code, body)
	}

	// Критерий 14: снять задачу — нет.
	if code, body := call("POST", "/v1/tasks/SDLC-1/transitions/cancel", tm, nil); code != http.StatusForbidden || !executorRefused(body) {
		t.Errorf("cancel токеном задачи: %d %v", code, body)
	}
	if task, _ := store.GetTaskByKey(ctx, store.DB(), "SDLC-1"); task.StatusKey != "analysis" {
		t.Errorf("задача сдвинулась: %s", task.StatusKey)
	}

	// Критерии 15, 17: правка и назначение — своя задача; чужая — 403.
	if code, body := call("PATCH", "/v1/tasks/SDLC-1", tm, map[string]any{"priority": "high"}); code != http.StatusOK {
		t.Errorf("правка своей задачи: %d %v", code, body)
	}
	for _, c := range []struct {
		method, path string
		body         map[string]any
	}{
		{"PATCH", "/v1/tasks/SDLC-3", map[string]any{"priority": "high"}},
		{"PUT", "/v1/tasks/SDLC-3/assignee", map[string]any{"assignee": "agent:pool"}},
		// Чужая блокирует свою — порядок работ решает человек.
		{"POST", "/v1/tasks/SDLC-3/links", map[string]any{"task": "SDLC-1", "rel": "blocks"}},
	} {
		if code, body := call(c.method, c.path, tm, c.body); code != http.StatusForbidden {
			t.Errorf("%s %s токеном задачи SDLC-1: %d %v", c.method, c.path, code, body)
		}
	}
	// Своя связана с любой задачей своего проекта (CLAB-305, AC-6).
	if code, body := call("POST", "/v1/tasks/SDLC-1/links", tm, map[string]any{"task": "SDLC-3", "rel": "relates_to"}); code != http.StatusOK {
		t.Errorf("связь своей задачи с SDLC-3: %d %v", code, body)
	}
	if code, body := call("PUT", "/v1/tasks/SDLC-1/assignee", tm, map[string]any{"assignee": "agent:pool"}); code != http.StatusOK {
		t.Errorf("назначение своей задачи: %d %v", code, body)
	}
}
