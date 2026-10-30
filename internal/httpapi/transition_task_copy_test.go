package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// Копия задачи в ответе перехода (CLAB-331): task.closed_at и task.updated_at
// совпадают с базой и с карточкой — раньше ответ нёс значения, прочитанные до
// SetTaskStatus.

// closeBundle — цикл с необязательным docs перед терминальным done: переход в
// терминальный статус бывает прямым (docs включён), цепочкой пропуска (docs
// выключен), отменой и заблокированным гейтом.
func closeBundle() map[string]any {
	st := func(key, category string, extra map[string]any) map[string]any {
		out := map[string]any{"key": key, "name": key, "category": category}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	tr := func(key, from, to, kind string, extra map[string]any) map[string]any {
		out := map[string]any{"key": key, "name": key, "to": to, "kind": kind}
		if from != "" {
			out["from"] = from
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	return map[string]any{
		"version": 1,
		"workflows": []any{map[string]any{
			"name": "Закрытие", "task_types": []string{"task"},
			"statuses": []any{
				st("new", "backlog", map[string]any{"is_initial": true}),
				st("analysis", "active", nil),
				st("docs", "active", map[string]any{"is_optional": true}),
				st("done", "done", map[string]any{"is_terminal": true}),
				st("cancelled", "cancelled", map[string]any{"is_terminal": true}),
			},
			"transitions": []any{
				tr("start", "new", "analysis", "advance", nil),
				tr("analysis_done", "analysis", "docs", "advance", nil),
				tr("docs_done", "docs", "done", "advance", nil),
				tr("gated_finish", "analysis", "done", "advance", map[string]any{
					"gates": []any{map[string]any{"key": "ok", "name": "Апрув", "type": "human_approval"}}}),
				tr("cancel", "", "cancelled", "cancel", nil),
			},
		}},
	}
}

type taskTimes struct {
	updated time.Time
	closed  *time.Time
}

// dbTimes — updated_at и closed_at строки tasks.
func (f *auditFixture) dbTimes(key string) taskTimes {
	f.t.Helper()
	var tt taskTimes
	if err := f.pool.QueryRow(context.Background(),
		`SELECT updated_at, closed_at FROM tasks WHERE key = $1`, key).Scan(&tt.updated, &tt.closed); err != nil {
		f.t.Fatal(err)
	}
	return tt
}

func timeField(t *testing.T, task map[string]any, field string) *time.Time {
	t.Helper()
	raw, ok := task[field].(string)
	if !ok {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("%s: %v", field, err)
	}
	return &parsed
}

// wantTaskMatchesDB сверяет task из ответа перехода с базой и с карточкой.
func (f *auditFixture) wantTaskMatchesDB(key string, task map[string]any, terminal bool, before time.Time) {
	f.t.Helper()
	db := f.dbTimes(key)
	_, card := f.json("GET", "/v1/tasks/"+key, f.root, nil)
	cardTask := card["task"].(map[string]any)

	closed := timeField(f.t, task, "closed_at")
	if terminal {
		if closed == nil || db.closed == nil || !closed.Equal(*db.closed) {
			f.t.Fatalf("%s: closed_at ответа %v, база %v", key, task["closed_at"], db.closed)
		}
		if got := timeField(f.t, cardTask, "closed_at"); got == nil || !got.Equal(*closed) {
			f.t.Fatalf("%s: closed_at карточки %v, ответа %v", key, cardTask["closed_at"], task["closed_at"])
		}
	} else if _, present := task["closed_at"]; present || db.closed != nil {
		f.t.Fatalf("%s: нетерминальный статус, а closed_at ответа %v, база %v", key, task["closed_at"], db.closed)
	}

	updated := timeField(f.t, task, "updated_at")
	if updated == nil || !updated.Equal(db.updated) {
		f.t.Fatalf("%s: updated_at ответа %v, база %v", key, task["updated_at"], db.updated)
	}
	if got := timeField(f.t, cardTask, "updated_at"); got == nil || !got.Equal(*updated) {
		f.t.Fatalf("%s: updated_at карточки %v, ответа %v", key, cardTask["updated_at"], task["updated_at"])
	}
	if !updated.After(before) {
		f.t.Fatalf("%s: updated_at ответа %v не позже значения до перехода %v", key, updated, before)
	}
}

func (f *auditFixture) newCloseTask(title string, stages map[string]any) string {
	f.t.Helper()
	body := map[string]any{"title": title}
	if stages != nil {
		body["optional_stages"] = stages
	}
	code, created := f.json("POST", "/v1/projects/SDLC/tasks", f.root, body)
	if code != http.StatusCreated {
		f.t.Fatalf("задача %q: %d %v", title, code, created)
	}
	return created["key"].(string)
}

// move выполняет переход и отдаёт task из ответа; before — updated_at в базе
// до перехода.
func (f *auditFixture) move(key, transition string) (map[string]any, map[string]any, time.Time) {
	f.t.Helper()
	before := f.dbTimes(key).updated
	code, body := f.json("POST", "/v1/tasks/"+key+"/transitions/"+transition, f.root, map[string]any{})
	if code != http.StatusOK {
		f.t.Fatalf("%s %s: %d %v", key, transition, code, body)
	}
	return body["task"].(map[string]any), body, before
}

func TestTransitionResponseCarriesDBTimes(t *testing.T) {
	f := newAuditFixture(t)
	code, report := f.json("POST", "/v1/projects/SDLC/import", f.root, closeBundle())
	if code != http.StatusOK {
		t.Fatalf("импорт цикла: %d %v", code, report)
	}
	wf := report["workflows"].([]any)[0].(map[string]any)
	if code, body := f.json("POST", "/v1/workflows/"+wf["id"].(string)+"/activate", f.root, nil); code != http.StatusOK {
		t.Fatalf("активация: %d %v", code, body)
	}

	// AC-1, AC-4: прямой переход в терминальный статус.
	direct := f.newCloseTask("Прямое закрытие", nil)
	for _, tr := range []string{"start", "analysis_done"} {
		task, _, before := f.move(direct, tr)
		// AC-5: нетерминальный статус — closed_at нет, updated_at верный.
		f.wantTaskMatchesDB(direct, task, false, before)
	}
	task, body, before := f.move(direct, "docs_done")
	if task["status_key"] != "done" {
		t.Fatalf("прямое закрытие: %v", task)
	}
	f.wantTaskMatchesDB(direct, task, true, before)
	if _, present := body["skips"]; present && len(body["skips"].([]any)) != 0 {
		t.Fatalf("прямой переход с пропусками: %v", body["skips"])
	}

	// AC-2: цепочка пропуска необязательного этапа заканчивается терминальным.
	chain := f.newCloseTask("Цепочка", map[string]any{"docs": false})
	f.move(chain, "start")
	task, body, before = f.move(chain, "analysis_done")
	if skips, _ := body["skips"].([]any); task["status_key"] != "done" || len(skips) != 1 || skips[0] != "docs" {
		t.Fatalf("цепочка: статус %v, skips %v", task["status_key"], body["skips"])
	}
	f.wantTaskMatchesDB(chain, task, true, before)

	// AC-3: cancel.
	cancelled := f.newCloseTask("Отмена", nil)
	f.move(cancelled, "start")
	task, _, before = f.move(cancelled, "cancel")
	if task["status_key"] != "cancelled" {
		t.Fatalf("cancel: %v", task)
	}
	f.wantTaskMatchesDB(cancelled, task, true, before)

	// AC-6: переход, закрытый гейтом, — 409, задача и closed_at не меняются.
	blocked := f.newCloseTask("Закрыто гейтом", nil)
	f.move(blocked, "start")
	was := f.dbTimes(blocked)
	code, resp := f.json("POST", "/v1/tasks/"+blocked+"/transitions/gated_finish", f.root, map[string]any{})
	if code != http.StatusConflict {
		t.Fatalf("переход под гейтом: %d %v", code, resp)
	}
	now := f.dbTimes(blocked)
	if now.closed != nil || !now.updated.Equal(was.updated) {
		t.Fatalf("заблокированный переход изменил задачу: было %+v, стало %+v", was, now)
	}
}
