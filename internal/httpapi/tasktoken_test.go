package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Токен задачи по REST (CLAB-212, решение 0049; чтение — CLAB-305, решение
// 0065): ручка выпуска, работа своей задачей, отказы записи по чужой задаче,
// чтение всего проекта и граница проекта, полные списки, поиск, /auth/me.
// Те же правила, что у MCP, — их держит workflow.Auth.

type restCall func(method, path, token string, body any) (int, map[string]any)

func newTaskTokenServer(t *testing.T) (*Server, *postgres.Store, restCall) {
	t.Helper()
	store, _ := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthToken)
	srv := NewServer(config.Config{Env: "test"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), auth, nil)
	if err := auth.Bootstrap(context.Background(), "root", rootToken); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token string, body any) (int, map[string]any) {
		t.Helper()
		var reader *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		} else {
			reader = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		out := map[string]any{}
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
		} else {
			out["raw"] = rec.Body.String()
		}
		return rec.Code, out
	}
	return srv, store, call
}

const rootToken = "bootstrap-token-0123456789"

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestTaskTokenOverREST(t *testing.T) {
	ctx := context.Background()
	_, store, call := newTaskTokenServer(t)

	personal := func(handle string) string {
		t.Helper()
		code, body := call("POST", "/v1/auth/tokens", rootToken, map[string]any{
			"name": "runner", "principal": "agent:" + handle, "project": "SDLC",
		})
		if code != http.StatusCreated {
			t.Fatalf("личный токен %s: %d %v", handle, code, body)
		}
		return body["token"].(string)
	}
	pool, other := personal("pool"), personal("other")
	for _, key := range []string{"SDLC-1", "SDLC-3"} {
		if code, body := call("POST", "/v1/tasks/"+key+"/transitions/start", rootToken, nil); code != http.StatusOK {
			t.Fatalf("старт %s: %d %v", key, code, body)
		}
		if code, body := call("POST", "/v1/tasks/"+key+"/claim", pool, nil); code != http.StatusOK {
			t.Fatalf("захват %s пулом: %d %v", key, code, body)
		}
	}

	// Выпуск (критерии 4–6).
	code, issued := call("POST", "/v1/tasks/SDLC-1/task-token", pool, nil)
	if code != http.StatusCreated {
		t.Fatalf("выпуск: %d %v", code, issued)
	}
	tok, _ := issued["token"].(string)
	rec, _ := issued["record"].(map[string]any)
	if tok == "" || rec["token_kind"] != "task" || rec["task_key"] != "SDLC-1" || rec["stage_run_id"] == nil ||
		rec["expires_at"] != nil || issued["role"] != "analyst" || issued["project_key"] != "SDLC" ||
		issued["lease_expires_at"] == nil {
		t.Fatalf("ответ выпуска: %v", issued)
	}
	runID := issued["stage_run_id"].(string)
	for _, c := range []struct{ path, token, want string }{
		{"/v1/tasks/SDLC-2/task-token", pool, "lease_expired"},
		{"/v1/tasks/SDLC-1/task-token", other, "lease_held_by_other"},
		{"/v1/tasks/SDLC-1/task-token", tok, "forbidden"},
	} {
		if _, body := call("POST", c.path, c.token, nil); errCode(body) != c.want {
			t.Errorf("POST %s: %v, ожидали %s", c.path, body, c.want)
		}
	}
	if code, body := call("POST", "/v1/auth/tokens", tok, map[string]any{"name": "escape"}); code != http.StatusForbidden {
		t.Errorf("личный токен токеном задачи: %d %v", code, body)
	}

	// Своя задача (критерий 8).
	if code, body := call("POST", "/v1/tasks/SDLC-1/claim", tok, nil); code != http.StatusOK || body["stage_run_id"] != runID {
		t.Fatalf("подтверждение аренды: %d %v", code, body)
	}
	if code, body := call("POST", "/v1/tasks/SDLC-1/heartbeat", tok, nil); code != http.StatusOK || body["stage_run_id"] != runID {
		t.Fatalf("heartbeat: %d %v", code, body)
	}
	if code, body := call("POST", "/v1/tasks/SDLC-1/artifacts", tok, map[string]any{
		"kind": "analysis.brief", "title": "Разбор", "content": "# Разбор",
	}); code != http.StatusCreated {
		t.Fatalf("сдача: %d %v", code, body)
	}
	if code, body := call("POST", "/v1/tasks/SDLC-1/repo-refs", tok, map[string]any{
		"repository": "cadence-lab-tech/sdlc-pipeline-backend", "ref_type": "pull_request", "ref": "12",
	}); code != http.StatusCreated {
		t.Fatalf("PR: %d %v", code, body)
	}
	if code, body := call("POST", "/v1/tasks/SDLC-1/usage", tok, map[string]any{"model": "m", "cost_usd": 0.1}); code != http.StatusCreated {
		t.Fatalf("стоимость: %d %v", code, body)
	}
	code, child := call("POST", "/v1/projects/SDLC/tasks", tok, map[string]any{"title": "D", "parent_key": "SDLC-1"})
	if code != http.StatusCreated {
		t.Fatalf("дочерняя задача: %d %v", code, child)
	}
	if code, body := call("GET", "/v1/tasks/SDLC-1/transitions", tok, nil); code != http.StatusOK {
		t.Fatalf("гейты: %d %v", code, body)
	}
	task1, _ := store.GetTaskByKey(ctx, store.DB(), "SDLC-1")
	task3, _ := store.GetTaskByKey(ctx, store.DB(), "SDLC-3")
	var ownSignal, otherSignal uuid.UUID
	for _, s := range []struct {
		task uuid.UUID
		out  *uuid.UUID
	}{{task1.ID, &ownSignal}, {task3.ID, &otherSignal}} {
		if err := store.DB().QueryRow(ctx, `
			INSERT INTO task_drift_signals (task_id, kind, severity, to_commit)
			VALUES ($1, 'baseline_drift', 'low', 'abc') RETURNING id`, s.task).Scan(s.out); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := call("POST", "/v1/drift-signals/"+ownSignal.String()+"/resolve", tok,
		map[string]any{"resolution": "dismissed"}); code != http.StatusOK {
		t.Fatalf("свой дрейф: %d %v", code, body)
	}

	// Чужая задача пула (критерий 9): та же аренда того же принципала, но 403.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/tasks/SDLC-3/claim", nil},
		{"POST", "/v1/tasks/SDLC-3/heartbeat", nil},
		{"POST", "/v1/tasks/SDLC-3/release", nil},
		{"POST", "/v1/tasks/SDLC-3/artifacts", map[string]any{"kind": "analysis.brief", "title": "x", "content": "x"}},
		{"POST", "/v1/tasks/SDLC-3/transitions/analysis_done", nil},
		{"POST", "/v1/tasks/SDLC-3/usage", map[string]any{"model": "m"}},
		{"POST", "/v1/tasks/SDLC-3/repo-refs", map[string]any{"repository": "cadence-lab-tech/sdlc-pipeline-backend", "ref": "13"}},
		{"POST", "/v1/projects/SDLC/tasks", map[string]any{"title": "x", "parent_key": "SDLC-3"}},
		{"POST", "/v1/projects/SDLC/tasks", map[string]any{"title": "x"}},
		{"POST", "/v1/drift-signals/" + otherSignal.String() + "/resolve", map[string]any{"resolution": "dismissed"}},
	} {
		if code, body := call(c.method, c.path, tok, c.body); code != http.StatusForbidden {
			t.Errorf("%s %s токеном задачи SDLC-1: %d %v", c.method, c.path, code, body)
		}
	}
	held, _ := store.CurrentStageRun(ctx, store.DB(), task3.ID, task3.StatusID)
	arts, _ := store.ListArtifacts(ctx, store.DB(), task3.ID, true)
	if held.ClaimedBy == nil || len(arts) != 0 {
		t.Fatalf("SDLC-3 тронут: аренда %v, артефактов %d", held.ClaimedBy, len(arts))
	}

	// Чтение — весь проект (CLAB-305, AC-1, AC-2): прежний круг (родитель
	// SDLC-4, ребёнок D, blocks →SDLC-2, relates_to Y→SDLC-1) и задачи вне
	// него (Z, SDLC-3 пула) отвечают 200 и тем же телом, что личному токену.
	_, y := call("POST", "/v1/projects/SDLC/tasks", rootToken, map[string]any{"title": "Y"})
	_, z := call("POST", "/v1/projects/SDLC/tasks", rootToken, map[string]any{
		"title": "Z", "description": "описание Z",
	})
	yKey, zKey := y["key"].(string), z["key"].(string)
	if code, body := call("POST", "/v1/tasks/"+yKey+"/links", rootToken, map[string]any{
		"task": "SDLC-1", "rel": "relates_to",
	}); code != http.StatusOK {
		t.Fatalf("связь: %d %v", code, body)
	}
	readable := []string{"SDLC-1", "SDLC-4", child["key"].(string), "SDLC-2", yKey, zKey, "SDLC-3"}
	paths := []string{"", "/state", "/timeline", "/artifacts", "/attachments", "/drift", "/repo-refs", "/usage", "/transitions"}
	for _, key := range readable {
		for _, p := range paths {
			code, body := call("GET", "/v1/tasks/"+key+p, tok, nil)
			if code != http.StatusOK {
				t.Errorf("GET %s%s токеном задачи: %d %v", key, p, code, body)
				continue
			}
			if p == "/state" {
				// Состояние несёт поля зрителя (кто держит аренду), и
				// принципал у обоих токенов один — тело совпадает целиком.
				if _, same := call("GET", "/v1/tasks/"+key+p, pool, nil); mustJSON(t, same) != mustJSON(t, body) {
					t.Errorf("GET %s%s: токену задачи не то, что личному", key, p)
				}
			}
		}
	}
	_, zCard := call("GET", "/v1/tasks/"+zKey, tok, nil)
	if got, _ := zCard["task"].(map[string]any); got == nil || got["description"] != "описание Z" {
		t.Errorf("карточка Z без описания: %v", zCard)
	}
	code, art3 := call("POST", "/v1/tasks/SDLC-3/artifacts", rootToken, map[string]any{
		"kind": "analysis.brief", "title": "чужое", "content": "секрет SDLC-3",
	})
	if code != http.StatusCreated {
		t.Fatalf("артефакт SDLC-3: %d %v", code, art3)
	}
	_, ownArts := call("GET", "/v1/tasks/SDLC-1/artifacts", tok, nil)
	list, _ := ownArts["artifacts"].([]any)
	if len(list) == 0 {
		t.Fatal("своих артефактов не видно")
	}
	ownArt := list[0].(map[string]any)["id"].(string)
	if code, _ := call("GET", "/v1/artifacts/"+ownArt+"/content", tok, nil); code != http.StatusOK {
		t.Errorf("содержимое своего артефакта: %d", code)
	}
	if code, body := call("GET", "/v1/artifacts/"+art3["id"].(string)+"/content", tok, nil); code != http.StatusOK ||
		!strings.Contains(body["raw"].(string), "секрет SDLC-3") {
		t.Errorf("содержимое артефакта чужой задачи проекта: %d %v", code, body)
	}

	// Запись по Z, которую токен теперь читает, — 403, Z не тронута (AC-5).
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PATCH", "/v1/tasks/" + zKey, map[string]any{"priority": "high"}},
		{"PUT", "/v1/tasks/" + zKey + "/assignee", map[string]any{"assignee": "agent:pool"}},
		{"POST", "/v1/tasks/" + zKey + "/attachments", nil},
		{"POST", "/v1/tasks/" + zKey + "/artifacts", map[string]any{"kind": "analysis.brief", "title": "x", "content": "x"}},
		{"POST", "/v1/tasks/" + zKey + "/repo-refs", map[string]any{"repository": "cadence-lab-tech/sdlc-pipeline-backend", "ref": "14"}},
		{"POST", "/v1/tasks/" + zKey + "/usage", map[string]any{"model": "m"}},
		{"POST", "/v1/tasks/" + zKey + "/transitions/start", nil},
		{"POST", "/v1/tasks/" + zKey + "/transitions/analysis_done/gates/artifacts/decision", map[string]any{"status": "passed"}},
		{"POST", "/v1/tasks/" + zKey + "/links", map[string]any{"task": "SDLC-1", "rel": "blocks"}},
		{"POST", "/v1/projects/SDLC/tasks", map[string]any{"title": "x", "parent_key": zKey}},
	} {
		if code, body := call(c.method, c.path, tok, c.body); code != http.StatusForbidden {
			t.Errorf("%s %s токеном задачи SDLC-1: %d %v", c.method, c.path, code, body)
		}
	}
	zTask, _ := store.GetTaskByKey(ctx, store.DB(), zKey)
	zArts, _ := store.ListArtifacts(ctx, store.DB(), zTask.ID, true)
	zAttach, _ := store.ListAttachments(ctx, store.DB(), zTask.ID, true)
	if zTask.StatusKey != "new" || zTask.Priority != "normal" || zTask.AssigneeID != nil ||
		len(zArts) != 0 || len(zAttach) != 0 {
		t.Fatalf("Z тронута отказами: %+v, артефактов %d, вложений %d", zTask, len(zArts), len(zAttach))
	}
	var refusals int
	if err := store.DB().QueryRow(ctx, `
		SELECT count(*) FROM audit_log
		WHERE token_kind = 'task' AND outcome = 'forbidden' AND task_id = $1`, zTask.ID).Scan(&refusals); err != nil {
		t.Fatal(err)
	}
	if refusals == 0 {
		t.Error("отказы записи по Z не в журнале")
	}

	// Граница проекта (AC-7): задача, артефакт и списки другого проекта — 403.
	foreign, foreignArt := testdb.ForeignTask(t, store)
	for _, p := range paths {
		code, body := call("GET", "/v1/tasks/"+foreign.Key+p, tok, nil)
		if code != http.StatusForbidden || !strings.Contains(mustJSON(t, body), "другому проекту") {
			t.Errorf("GET %s%s другого проекта: %d %v", foreign.Key, p, code, body)
		}
	}
	for _, path := range []string{
		"/v1/artifacts/" + foreignArt.ID.String() + "/content",
		"/v1/projects/OTHER/tasks", "/v1/projects/OTHER/queue", "/v1/projects/OTHER/search?q=x",
	} {
		if code, body := call("GET", path, tok, nil); code != http.StatusForbidden {
			t.Errorf("GET %s другого проекта: %d %v", path, code, body)
		}
	}
	_, projects := call("GET", "/v1/projects", tok, nil)
	if ps, _ := projects["projects"].([]any); len(ps) != 1 || ps[0].(map[string]any)["key"] != "SDLC" {
		t.Errorf("проекты токена задачи: %v", projects)
	}
	// Личный токен того же принципала читает Z, как раньше (эпик 1–3).
	if code, _ := call("GET", "/v1/tasks/"+zKey, pool, nil); code != http.StatusOK {
		t.Errorf("личный токен не читает Z: %d", code)
	}

	// Списки — полные строки, как личному токену (AC-3).
	_, own := call("GET", "/v1/projects/SDLC/tasks", tok, nil)
	_, full := call("GET", "/v1/projects/SDLC/tasks", pool, nil)
	if mustJSON(t, own) != mustJSON(t, full) {
		t.Fatalf("список токену задачи не тот, что личному:\n%s\n%s", mustJSON(t, own), mustJSON(t, full))
	}
	items, _ := own["tasks"].([]any)
	described := false
	for _, it := range items {
		m := it.(map[string]any)
		for _, f := range []string{"key", "title", "status_key", "properties", "updated_at"} {
			if _, has := m[f]; !has {
				t.Fatalf("в строке списка нет %s: %v", f, m)
			}
		}
		if m["key"] == child["key"] && m["parent_key"] != "SDLC-1" {
			t.Fatalf("у ребёнка нет parent_key: %v", m)
		}
		if m["key"] == zKey && m["description"] == "описание Z" {
			described = true
		}
	}
	if !described {
		t.Fatal("в списке токену задачи нет описаний")
	}
	_, queue := call("GET", "/v1/projects/SDLC/queue", tok, nil)
	_, fullQueue := call("GET", "/v1/projects/SDLC/queue", pool, nil)
	if qs, _ := queue["queue"].([]any); len(qs) == 0 || mustJSON(t, queue) != mustJSON(t, fullQueue) {
		t.Fatalf("очередь токену задачи не та, что личному:\n%s\n%s", mustJSON(t, queue), mustJSON(t, fullQueue))
	}

	// Кто я (критерий 14).
	_, me := call("GET", "/v1/auth/me", tok, nil)
	meTok, _ := me["token"].(map[string]any)
	teams, _ := me["teams"].([]any)
	if meTok["token_kind"] != "task" || meTok["task_key"] != "SDLC-1" || len(teams) != 0 {
		t.Fatalf("me токеном задачи: %v", me)
	}
	for _, m := range me["memberships"].([]any) {
		if m.(map[string]any)["project_key"] != "SDLC" {
			t.Fatalf("участие вне проекта токена: %v", m)
		}
	}

	// Переход гасит токен: дальше любой вызов — 401 (критерий 18). Снять
	// задачу токеном задачи нельзя (CLAB-208) — снимает человек.
	if code, body := call("POST", "/v1/tasks/SDLC-1/transitions/cancel", tok, nil); code != http.StatusForbidden {
		t.Fatalf("cancel токеном задачи: %d %v", code, body)
	}
	if code, body := call("POST", "/v1/tasks/SDLC-1/transitions/cancel", rootToken, nil); code != http.StatusOK {
		t.Fatalf("переход человеком: %d %v", code, body)
	}
	// Погасший токен не читает ни свою, ни чужую задачу проекта (AC-9).
	for _, key := range []string{"SDLC-1", zKey} {
		code, body := call("GET", "/v1/tasks/"+key, tok, nil)
		if code != http.StatusUnauthorized || !strings.Contains(body["error"].(map[string]any)["message"].(string), "токен задачи погас") {
			t.Fatalf("%s после перехода: %d %v", key, code, body)
		}
	}
}

// AC-4 CLAB-305: поиск токеном задачи — та же выдача, что личному токену
// того же принципала: код всех репозиториев проекта, артефакты любых задач;
// репозиторий вне области задачи в фильтре сужает выдачу, а не
// отбрасывается.
func TestTaskTokenSearchOverREST(t *testing.T) {
	_, store, call := newTaskTokenServer(t)
	code, issuedPool := call("POST", "/v1/auth/tokens", rootToken, map[string]any{
		"name": "runner", "principal": "agent:pool", "project": "SDLC",
	})
	if code != http.StatusCreated {
		t.Fatalf("личный токен: %d %v", code, issuedPool)
	}
	pool := issuedPool["token"].(string)
	if code, body := call("POST", "/v1/tasks/SDLC-1/transitions/start", rootToken, nil); code != http.StatusOK {
		t.Fatalf("старт: %d %v", code, body)
	}
	if code, body := call("POST", "/v1/tasks/SDLC-1/claim", pool, nil); code != http.StatusOK {
		t.Fatalf("захват: %d %v", code, body)
	}
	_, issued := call("POST", "/v1/tasks/SDLC-1/task-token", pool, nil)
	tok := issued["token"].(string)
	// Область SDLC-1 — только backend; SDLC-3 вне прежнего круга.
	testdb.SearchCorpus(t, store, "quasar", "SDLC-1", "SDLC-3")

	search := func(token string, repos ...string) (code []string, tasks []string) {
		t.Helper()
		status, body := call("POST", "/v1/projects/SDLC/search", token, map[string]any{
			"query": "quasar", "repositories": repos, "top_k": 50,
		})
		if status != http.StatusOK {
			t.Fatalf("поиск: %d %v", status, body)
		}
		hits, _ := body["results"].([]any)
		for _, h := range hits {
			m := h.(map[string]any)
			switch m["scope"] {
			case "code":
				code = append(code, m["repository"].(string))
			case "artifacts":
				tasks = append(tasks, m["task"].(string))
			}
		}
		// Равные оценки выдача упорядочивает как придётся — сравниваем
		// состав, а не порядок.
		sort.Strings(code)
		sort.Strings(tasks)
		return
	}
	codeAll, tasksAll := search(pool)
	if len(codeAll) != 3 || len(tasksAll) != 2 {
		t.Fatalf("личный токен: код %v, артефакты %v", codeAll, tasksAll)
	}
	code1, tasks1 := search(tok)
	if mustJSON(t, code1) != mustJSON(t, codeAll) || mustJSON(t, tasks1) != mustJSON(t, tasksAll) {
		t.Fatalf("токен задачи: код %v, артефакты %v; личный: %v, %v", code1, tasks1, codeAll, tasksAll)
	}
	code2, _ := search(tok, "cadence-lab-tech/sdlc-pipeline-frontend")
	if len(code2) != 1 || code2[0] != "cadence-lab-tech/sdlc-pipeline-frontend" {
		t.Fatalf("репозиторий вне области задачи: %v", code2)
	}
}
