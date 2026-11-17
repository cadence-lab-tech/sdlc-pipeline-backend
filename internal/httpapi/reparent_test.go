package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// liftToTopLevel снимает задачи сида с родителя прямой записью: SDLC-1..3 в
// демонстрационном проекте — дети эпика SDLC-4, а с CLAB-196 вложенность —
// один уровень, и завести ребёнка у ребёнка нельзя. Тестам, которым нужна
// своя задача с детьми, она нужна на верхнем уровне.
func liftToTopLevel(t *testing.T, db postgres.Querier, keys ...string) {
	t.Helper()
	if _, err := db.Exec(context.Background(),
		`UPDATE tasks SET parent_id = NULL WHERE key = ANY($1)`, keys); err != nil {
		t.Fatal(err)
	}
}

func (f *auditFixture) newTop(title string, extra map[string]any) string {
	f.t.Helper()
	body := map[string]any{"title": title}
	for k, v := range extra {
		body[k] = v
	}
	code, task := f.json(http.MethodPost, "/v1/projects/SDLC/tasks", f.root, body)
	if code != http.StatusCreated {
		f.t.Fatalf("%s: %d %v", title, code, task)
	}
	return task["key"].(string)
}

func (f *auditFixture) parentOf(key string) any {
	f.t.Helper()
	_, card := f.json(http.MethodGet, "/v1/tasks/"+key, f.root, nil)
	return card["task"].(map[string]any)["parent_key"]
}

func (f *auditFixture) reparentedIn(key string) []map[string]any {
	f.t.Helper()
	_, body := f.json(http.MethodGet, "/v1/tasks/"+key+"/timeline", f.root, nil)
	var out []map[string]any
	for _, e := range body["events"].([]any) {
		ev := e.(map[string]any)
		if ev["type"] == domain.EventTaskReparented {
			out = append(out, ev["payload"].(map[string]any))
		}
	}
	return out
}

// AC-1, AC-2, AC-8, AC-9 по REST: ответ — task, from, to, changed; карточка,
// выборка группы в порядке бэклога, лента обеих сторон и журнал вызова;
// снятие — parent_key: null; поле обязательно.
func TestReparentREST(t *testing.T) {
	f := newAuditFixture(t)
	emp := f.memberToken("user:emp", domain.MemberAgent, nil)
	target := f.newTop("Новый родитель", nil)
	f.newTop("Сосед в группе", map[string]any{"parent_key": target})

	code, body := f.json(http.MethodPut, "/v1/tasks/SDLC-2/parent", emp, map[string]any{"parent_key": target})
	if code != http.StatusOK || body["changed"] != true || body["from"] != "SDLC-4" || body["to"] != target ||
		len(body) != 4 || body["task"].(map[string]any)["parent_key"] != target {
		t.Fatalf("перенос: %d %v", code, body)
	}
	if got := f.parentOf("SDLC-2"); got != target {
		t.Fatalf("карточка: %v", got)
	}
	_, list := f.json(http.MethodGet, "/v1/projects/SDLC/tasks?parent="+target+"&order=backlog", f.root, nil)
	tasks := list["tasks"].([]any)
	if last := tasks[len(tasks)-1].(map[string]any)["key"]; len(tasks) != 2 || last != "SDLC-2" {
		t.Fatalf("группа нового родителя: %v", tasks)
	}
	for _, key := range []string{"SDLC-2", "SDLC-4", target} {
		if ev := f.reparentedIn(key); len(ev) != 1 || ev[0]["task"] != "SDLC-2" || ev[0]["from"] != "SDLC-4" || ev[0]["to"] != target {
			t.Fatalf("лента %s: %v", key, ev)
		}
	}
	if rows := f.rows("a.action = 'PUT /v1/tasks/:taskKey/parent' AND a.outcome = 'ok'"); len(rows) != 1 || *rows[0].Task != "SDLC-2" {
		t.Fatalf("журнал: %+v", rows)
	}

	// Повтор — 200 без события.
	if code, body := f.json(http.MethodPut, "/v1/tasks/SDLC-2/parent", emp, map[string]any{"parent_key": target}); code != http.StatusOK ||
		body["changed"] != false || len(f.reparentedIn("SDLC-2")) != 1 {
		t.Fatalf("повтор: %d %v", code, body)
	}
	// Снятие: null и пустая строка одинаковы.
	if code, body := f.json(http.MethodPut, "/v1/tasks/SDLC-2/parent", emp, map[string]any{"parent_key": nil}); code != http.StatusOK ||
		body["to"] != nil || body["changed"] != true || f.parentOf("SDLC-2") != nil {
		t.Fatalf("снятие: %d %v", code, body)
	}
	_, top := f.json(http.MethodGet, "/v1/projects/SDLC/tasks?top_level=true&order=backlog&limit=200", f.root, nil)
	topTasks := top["tasks"].([]any)
	if last := topTasks[len(topTasks)-1].(map[string]any)["key"]; last != "SDLC-2" {
		t.Fatalf("верхний уровень: последняя %v", last)
	}
	if code, body := f.json(http.MethodPut, "/v1/tasks/SDLC-3/parent", emp, map[string]any{"parent_key": ""}); code != http.StatusOK ||
		f.parentOf("SDLC-3") != nil {
		t.Fatalf("снятие пустой строкой: %d %v", code, body)
	}
	// Тело без поля и не JSON — 400, ничего не меняется.
	for _, b := range []any{map[string]any{}, map[string]any{"parent": target}, map[string]any{"parent_key": 5}} {
		if code, body := f.json(http.MethodPut, "/v1/tasks/SDLC-1/parent", emp, b); code != http.StatusBadRequest || errCode(body) != "invalid_input" {
			t.Fatalf("тело %v: %d %v", b, code, body)
		}
	}
	if f.parentOf("SDLC-1") != "SDLC-4" {
		t.Fatal("плохое тело сменило родителя")
	}
	// Вложенность при переносе и при заведении — 400 с ключами.
	child := f.newTop("Ребёнок цели", map[string]any{"parent_key": target})
	code, body = f.json(http.MethodPut, "/v1/tasks/SDLC-2/parent", emp, map[string]any{"parent_key": child})
	if msg, _ := errOf(body); code != http.StatusBadRequest || !strings.Contains(msg, child+" сама вложена в "+target) {
		t.Fatalf("под вложенную: %d %v", code, body)
	}
	code, body = f.json(http.MethodPut, "/v1/tasks/"+target+"/parent", emp, map[string]any{"parent_key": "SDLC-2"})
	if msg, _ := errOf(body); code != http.StatusBadRequest || !strings.Contains(msg, "у "+target+" есть дочерние задачи (2)") {
		t.Fatalf("задача с детьми: %d %v", code, body)
	}
	code, body = f.json(http.MethodPost, "/v1/projects/SDLC/tasks", emp, map[string]any{"title": "Внук", "parent_key": child})
	if msg, _ := errOf(body); code != http.StatusBadRequest || !strings.Contains(msg, child+" сама вложена в "+target) {
		t.Fatalf("заведение у вложенной: %d %v", code, body)
	}
	if out, isErr := f.mcp(emp, "sdlc_create_task", map[string]any{"title": "Внук", "parent": child}); !isErr ||
		!strings.Contains(fmt.Sprint(out), child+" сама вложена в "+target) {
		t.Fatalf("sdlc_create_task у вложенной: %v", out)
	}
	if code, body := f.json(http.MethodPut, "/v1/tasks/SDLC-999/parent", emp, map[string]any{"parent_key": target}); code != http.StatusNotFound {
		t.Fatalf("задачи нет: %d %v", code, body)
	}
}

// AC-10, AC-11, AC-12: прогноз actor_rights.reparent (карточка, /state,
// sdlc_get_task) совпадает с исходом REST и MCP для каждого класса; отказы
// одного кода и причины на обоих транспортах и ничего не меняют.
func TestReparentRightsParity(t *testing.T) {
	f := newAuditFixture(t)
	emp := f.memberToken("user:emp", domain.MemberAgent, nil)
	lead := f.memberToken("user:lead", domain.MemberMaintainer, nil)
	viewer := f.memberToken("user:watcher", domain.MemberViewer, nil)
	target := f.newTop("Цель", nil)
	for _, key := range []string{"SDLC-4", "SDLC-1"} {
		if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/start", f.root, nil); code != http.StatusOK {
			t.Fatalf("start %s: %d %v", key, code, body)
		}
		if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/claim", f.bot, map[string]any{}); code != http.StatusOK {
			t.Fatalf("захват %s: %d %v", key, code, body)
		}
	}
	_, issued := f.json(http.MethodPost, "/v1/tasks/SDLC-1/task-token", f.bot, nil)
	tt := issued["token"].(string)

	for _, c := range []struct {
		name, token, key, reason string
		allow                    bool
	}{
		{"токен своей задачи", tt, "SDLC-1", workflow.DenyHumanAction, false},
		{"токен о чужой", tt, "SDLC-2", workflow.DenyHumanAction, false},
		{"бот с арендой", f.bot, "SDLC-1", workflow.DenyHumanAction, false},
		{"бот с арендой родителя", f.bot, "SDLC-3", workflow.DenyHumanAction, false},
		{"viewer", viewer, "SDLC-1", workflow.DenyLevel, false},
		{"сотрудник", emp, "SDLC-3", "", true},
		{"сопровождающий", lead, "SDLC-2", "", true},
	} {
		_, card := f.json(http.MethodGet, "/v1/tasks/"+c.key, c.token, nil)
		_, state := f.json(http.MethodGet, "/v1/tasks/"+c.key+"/state", c.token, nil)
		got, _ := f.mcp(c.token, "sdlc_get_task", map[string]any{"task_key": c.key})
		for src, body := range map[string]map[string]any{"карточка": card, "/state": state, "sdlc_get_task": got} {
			rights, _ := body["actor_rights"].(map[string]any)
			if rights == nil || rights["reparent"] != c.allow {
				t.Fatalf("%s/%s: actor_rights %v", c.name, src, body["actor_rights"])
			}
			if !c.allow {
				d, _ := rights["actor_deny"].(map[string]any)["reparent"].(map[string]any)
				if d == nil || d["code"] != "forbidden" || d["reason"] != c.reason {
					t.Fatalf("%s/%s: actor_deny.reparent %v", c.name, src, rights["actor_deny"])
				}
			}
		}
		if c.allow {
			if code, body := f.json(http.MethodPut, "/v1/tasks/"+c.key+"/parent", c.token, map[string]any{"parent_key": target}); code != http.StatusOK {
				t.Fatalf("%s REST: %d %v", c.name, code, body)
			}
			out, isErr := f.mcp(c.token, "sdlc_set_task_parent", map[string]any{"task_key": c.key, "parent": ""})
			if isErr || out["changed"] != true || out["from"] != target || out["to"] != nil {
				t.Fatalf("%s MCP: %v", c.name, out)
			}
			continue
		}
		before := f.parentOf(c.key)
		code, rbody := f.json(http.MethodPut, "/v1/tasks/"+c.key+"/parent", c.token, map[string]any{"parent_key": target})
		_, rreason := errOf(rbody)
		out, isErr := f.mcp(c.token, "sdlc_set_task_parent", map[string]any{"task_key": c.key, "parent": target})
		_, mreason := errOf(out)
		// viewer отбивает порог маршрута (agent) — без details.reason, как у
		// перевода; прогноз называет причину level.
		want := c.reason
		if c.token == viewer {
			want = ""
		}
		if code != http.StatusForbidden || !isErr || rreason != want || mreason != want {
			t.Fatalf("%s: REST %d %v / MCP %v", c.name, code, rbody, out)
		}
		if c.token == tt && (!executorRefused(rbody) || !strings.HasPrefix(fmt.Sprint(out["error"].(map[string]any)["message"]), workflow.ExecutorRefusal)) {
			t.Fatalf("токен задачи: %v / %v", rbody, out)
		}
		if f.parentOf(c.key) != before {
			t.Fatalf("%s: отказ сменил родителя", c.name)
		}
	}
	// Отказы ручки видны в журнале с видом токена.
	if rows := f.rows("a.action = 'PUT /v1/tasks/:taskKey/parent' AND a.outcome = 'forbidden' AND a.token_kind = 'task'"); len(rows) != 2 {
		t.Fatalf("журнал отказов токену задачи: %+v", rows)
	}
	// Описания тулов (AC-11).
	if slices.Contains(workflow.ExecutorActions(), workflow.RESTAction("PUT", "/v1/tasks/:taskKey/parent")) ||
		slices.Contains(workflow.ExecutorActions(), "sdlc_set_task_parent") {
		t.Fatal("перенос в белом списке токена задачи")
	}
}
