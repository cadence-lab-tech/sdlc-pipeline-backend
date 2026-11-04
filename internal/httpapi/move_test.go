package httpapi

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Перевод в статус по REST и MCP (CLAB-408): AC-9, AC-11, AC-14, AC-15,
// AC-17, AC-19, K-2, K-6. Цикл — демонстрационный Default SDLC.

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Сотрудник переводит задачу с живой арендой исполнителя: ответ — ровно
// task, stage_run, previous_run, closed_reviews; токен задачи закрытого
// прогона после перевода — 401 «токен задачи погас». Перевод в done и
// обратно снимает и ставит closed_at, задача снова в рабочих выборках.
func TestMoveREST(t *testing.T) {
	f := newAuditFixture(t)
	emp := f.memberToken("user:emp", domain.MemberAgent, []string{"developer"})
	const key = "SDLC-1"
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/claim", f.bot, map[string]any{}); code != http.StatusOK {
		t.Fatalf("захват: %d %v", code, body)
	}
	code, issued := f.json(http.MethodPost, "/v1/tasks/"+key+"/task-token", f.bot, nil)
	if code != http.StatusCreated {
		t.Fatalf("токен задачи: %d %v", code, issued)
	}
	tt := issued["token"].(string)

	// Токен задачи: маршрут вне белого списка, ничего не меняется.
	code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/move", tt, map[string]any{"to": "done", "reason": "так"})
	if msg, _ := errOf(body); code != http.StatusForbidden || !strings.HasPrefix(msg, workflow.ExecutorRefusal) {
		t.Fatalf("перевод токеном задачи: %d %v", code, body)
	}
	if f.status(key) != "analysis" {
		t.Fatal("отказ токену задачи сдвинул задачу")
	}

	// Сотрудник: analysis → qa через несколько статусов.
	code, body = f.json(http.MethodPost, "/v1/tasks/"+key+"/move", emp, map[string]any{"to": "qa", "reason": "проверить сразу"})
	if code != http.StatusOK {
		t.Fatalf("перевод сотрудником: %d %v", code, body)
	}
	if got := sortedKeys(body); !slices.Equal(got, []string{"closed_reviews", "previous_run", "stage_run", "task"}) {
		t.Fatalf("ключи ответа: %v", got)
	}
	run, prev := body["stage_run"].(map[string]any), body["previous_run"].(map[string]any)
	if run["state"] != "pending" || run["status_key"] != "qa" || prev["state"] != "abandoned" ||
		prev["outcome"] != "moved" || prev["status_key"] != "analysis" || body["closed_reviews"] != float64(0) ||
		body["task"].(map[string]any)["status_key"] != "qa" {
		t.Fatalf("ответ перевода: %v", body)
	}
	if _, has := prev["lease_expires_at"]; has {
		t.Fatalf("аренда не снята: %v", prev)
	}
	code, body = f.json(http.MethodPost, "/v1/tasks/"+key+"/heartbeat", tt, map[string]any{})
	if msg, _ := errOf(body); code != http.StatusUnauthorized || !strings.Contains(msg, "токен задачи погас") {
		t.Fatalf("токен задачи после перевода: %d %v", code, body)
	}

	// Лента: task.moved от сотрудника, вид токена api.
	var via, tokenKind, level, handle string
	if err := f.pool.QueryRow(t.Context(), `
		SELECT e.payload->>'via', e.payload->>'token_kind', e.payload->>'level', p.handle
		FROM task_events e JOIN tasks tk ON tk.id = e.task_id JOIN principals p ON p.id = e.actor_id
		WHERE tk.key = $1 AND e.type = 'task.moved'`, key).Scan(&via, &tokenKind, &level, &handle); err != nil ||
		via != "level" || tokenKind != "api" || level != "agent" || handle != "emp" {
		t.Fatalf("task.moved: %q %q %q %q %v", via, tokenKind, level, handle, err)
	}

	// AC-19, AC-11: в done без роли integrator и обратно; список задач
	// проекта без фильтра закрытых снова показывает её как открытую.
	if code, body = f.json(http.MethodPost, "/v1/tasks/"+key+"/move", emp, map[string]any{"to": "done", "reason": "готово"}); code != http.StatusOK ||
		body["task"].(map[string]any)["closed_at"] == nil {
		t.Fatalf("в done: %d %v", code, body)
	}
	if code, body = f.json(http.MethodPost, "/v1/tasks/"+key+"/move", emp, map[string]any{"to": "development", "reason": "рано закрыли"}); code != http.StatusOK ||
		body["task"].(map[string]any)["closed_at"] != nil {
		t.Fatalf("из done: %d %v", code, body)
	}
	_, list := f.json(http.MethodGet, "/v1/projects/SDLC/tasks", emp, nil)
	found := false
	for _, it := range list["tasks"].([]any) {
		row := it.(map[string]any)
		if row["key"] == key {
			found = row["status_key"] == "development" && row["closed_at"] == nil
		}
	}
	if !found {
		t.Fatalf("задачи нет среди открытых: %v", list)
	}

	// Карточка: прогоны analysis, qa, done закрыты переводом — outcome moved
	// и выход kind moved с причиной; открыт прогон development.
	_, card := f.json(http.MethodGet, "/v1/tasks/"+key, emp, nil)
	moved := 0
	for _, it := range card["stage_runs"].([]any) {
		r := it.(map[string]any)
		if r["outcome"] == "moved" {
			moved++
			if x, _ := r["exit"].(map[string]any); x == nil || x["kind"] != "moved" || x["comment"] == nil {
				t.Fatalf("выход прогона moved: %v", r["exit"])
			}
		}
	}
	if moved != 3 {
		t.Fatalf("прогонов moved %d", moved)
	}

	// Ошибки — коды контракта.
	for _, c := range []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"to": "qa", "reason": " "}, http.StatusBadRequest},
		{map[string]any{"to": "nope", "reason": "x"}, http.StatusBadRequest},
	} {
		code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/move", emp, c.body)
		if code != c.code {
			t.Fatalf("%v: %d %v", c.body, code, body)
		}
	}
	code, body = f.json(http.MethodPost, "/v1/tasks/"+key+"/move", emp, map[string]any{"to": "nope", "reason": "x"})
	if d, _ := body["error"].(map[string]any)["details"].(map[string]any); d == nil || len(d["statuses"].([]any)) != 8 {
		t.Fatalf("details.statuses: %v", body)
	}
	if code, _ := f.json(http.MethodPost, "/v1/tasks/SDLC-999/move", emp, map[string]any{"to": "qa", "reason": "x"}); code != http.StatusNotFound {
		t.Fatalf("нет задачи: %d", code)
	}
}

// AC-14, AC-15, AC-17: класс актора × перевод на двух транспортах — прогноз
// actor_rights.move (карточка, /state, sdlc_get_task) совпадает с исходом
// REST и MCP, отказы одинаковы.
func TestMoveRightsParity(t *testing.T) {
	f := newAuditFixture(t)
	emp := f.memberToken("user:emp", domain.MemberAgent, []string{"developer"})
	lead := f.memberToken("user:lead", domain.MemberMaintainer, nil)
	viewer := f.memberToken("user:watcher", domain.MemberViewer, nil)
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-2/transitions/start", f.root, nil); code != http.StatusOK {
		t.Fatalf("start: %d %v", code, body)
	}
	if code, body := f.json(http.MethodPost, "/v1/tasks/SDLC-2/claim", f.bot, map[string]any{}); code != http.StatusOK {
		t.Fatalf("захват: %d %v", code, body)
	}
	_, issued := f.json(http.MethodPost, "/v1/tasks/SDLC-2/task-token", f.bot, nil)
	tt := issued["token"].(string)

	// reason — причина прогноза; у viewer запрос отбивает порог маршрута
	// (agent) тем же текстом, но без details.reason.
	for _, c := range []struct {
		name, token, reason, reqReason string
		allow                          bool
	}{
		// Токен задачи — первым: перевод сотрудником его гасит.
		{"токен задачи", tt, "", "", false},
		{"бот", f.bot, workflow.DenyHumanAction, workflow.DenyHumanAction, false},
		{"viewer", viewer, workflow.DenyLevel, "", false},
		{"сотрудник", emp, "", "", true},
		{"сопровождающий", lead, "", "", true},
	} {
		const key = "SDLC-2"
		// Прогноз — три источника.
		_, card := f.json(http.MethodGet, "/v1/tasks/"+key, c.token, nil)
		_, state := f.json(http.MethodGet, "/v1/tasks/"+key+"/state", c.token, nil)
		got, _ := f.mcp(c.token, "sdlc_get_task", map[string]any{"task_key": key})
		for src, body := range map[string]map[string]any{"карточка": card, "/state": state, "sdlc_get_task": got} {
			rights, _ := body["actor_rights"].(map[string]any)
			if rights == nil || rights["move"] != c.allow {
				t.Fatalf("%s/%s: actor_rights %v", c.name, src, body["actor_rights"])
			}
			if !c.allow {
				d, _ := rights["actor_deny"].(map[string]any)["move"].(map[string]any)
				if d == nil || d["code"] != "forbidden" || (c.reason != "" && d["reason"] != c.reason) {
					t.Fatalf("%s/%s: actor_deny.move %v", c.name, src, rights["actor_deny"])
				}
			}
		}
		if c.allow {
			// Исход на двух транспортах: REST и MCP переводят по очереди.
			if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/move", c.token,
				map[string]any{"to": "analysis", "reason": "рестарт"}); code != http.StatusOK {
				t.Fatalf("%s REST: %d %v", c.name, code, body)
			}
			out, isErr := f.mcp(c.token, "sdlc_move_task", map[string]any{"task_key": key, "to": "analysis", "reason": "рестарт"})
			if isErr || out["previous_run"].(map[string]any)["outcome"] != "moved" {
				t.Fatalf("%s MCP: %v", c.name, out)
			}
			continue
		}
		code, rbody := f.json(http.MethodPost, "/v1/tasks/"+key+"/move", c.token, map[string]any{"to": "done", "reason": "так"})
		rmsg, rreason := errOf(rbody)
		out, isErr := f.mcp(c.token, "sdlc_move_task", map[string]any{"task_key": key, "to": "done", "reason": "так"})
		mmsg, mreason := errOf(out)
		// Токену задачи отказывает белый список транспорта: текст называет
		// маршрут или тул, начало — одно (ExecutorRefusal).
		same := rmsg == mmsg || (c.token == tt && strings.HasPrefix(rmsg, workflow.ExecutorRefusal))
		if code != http.StatusForbidden || !isErr || !same || rreason != mreason || rreason != c.reqReason {
			t.Fatalf("%s: REST %d %q %q / MCP %q %q", c.name, code, rmsg, rreason, mmsg, mreason)
		}
		if c.token == tt && !strings.HasPrefix(mmsg, workflow.ExecutorRefusal) {
			t.Fatalf("токен задачи по MCP: %q", mmsg)
		}
		if f.status(key) == "done" {
			t.Fatalf("%s: отказ перевёл задачу", c.name)
		}
	}

	// MCP: коды отказа те же, что у REST (неизвестный статус, пустая причина).
	for _, args := range []map[string]any{
		{"task_key": "SDLC-2", "to": "nope", "reason": "x"},
		{"task_key": "SDLC-2", "to": "qa", "reason": ""},
	} {
		code, rbody := f.json(http.MethodPost, "/v1/tasks/SDLC-2/move", emp, map[string]any{"to": args["to"], "reason": args["reason"]})
		out, isErr := f.mcp(emp, "sdlc_move_task", args)
		rmsg, _ := errOf(rbody)
		mmsg, _ := errOf(out)
		if code != http.StatusBadRequest || !isErr || out["error"].(map[string]any)["code"] != "invalid_input" || rmsg != mmsg {
			t.Fatalf("%v: REST %d %q / MCP %v", args, code, rmsg, out)
		}
	}
}
