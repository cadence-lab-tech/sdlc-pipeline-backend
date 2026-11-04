package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Ранг бэклога (CLAB-399, решение 0075): POST /v1/tasks/:key/rank, событие
// task.ranked, порядок order=backlog, выборка backlog=true и поле in_work.
// Номера — критерии К-N анализа CLAB-399.

const rankPath = "/v1/tasks/%s/rank"

func (f *ovFixture) rank(key, token string, body any) (int, map[string]any) {
	f.t.Helper()
	return f.json("POST", fmt.Sprintf(rankPath, key), token, body)
}

func (f *ovFixture) rankOK(key string, body any) map[string]any {
	f.t.Helper()
	code, out := f.rank(key, f.root, body)
	if code != http.StatusOK {
		f.t.Fatalf("rank %s %v: %d %v", key, body, code, out)
	}
	return out
}

// backlog — ключи группы в порядке order=backlog: parent "" — верхний
// уровень проекта.
func (f *ovFixture) backlog(parent string) []string {
	f.t.Helper()
	q := "order=backlog&limit=500"
	if parent == "" {
		q += "&top_level=true"
	} else {
		q += "&parent=" + parent
	}
	items, _ := f.list(f.root, q)
	return taskKeysOf(items)
}

// only — ключи из keys в порядке all.
func only(all []string, keys ...string) []string {
	out := []string{}
	for _, k := range all {
		if slices.Contains(keys, k) {
			out = append(out, k)
		}
	}
	return out
}

type rankRow struct {
	rank    string
	updated time.Time
}

// ranks — ранг и updated_at всех задач проекта SDLC.
func (f *ovFixture) ranks() map[string]rankRow {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `
		SELECT t.key, t.rank, t.updated_at FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE p.key = 'SDLC'`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]rankRow{}
	for rows.Next() {
		var k string
		var r rankRow
		if err := rows.Scan(&k, &r.rank, &r.updated); err != nil {
			f.t.Fatal(err)
		}
		out[k] = r
	}
	return out
}

// rankEvents — нагрузки task.ranked задачи по порядку.
func (f *ovFixture) rankEvents(key string) []map[string]any {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `
		SELECT e.payload, e.stage_run_id IS NULL, coalesce(p.handle, '')
		FROM task_events e JOIN tasks t ON t.id = e.task_id
		LEFT JOIN principals p ON p.id = e.actor_id
		WHERE t.key = $1 AND e.type = $2 ORDER BY e.created_at`, key, domain.EventTaskRanked)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw []byte
		var noRun bool
		var actor string
		if err := rows.Scan(&raw, &noRun, &actor); err != nil {
			f.t.Fatal(err)
		}
		m := map[string]any{}
		_ = json.Unmarshal(raw, &m)
		if !noRun {
			f.t.Fatalf("task.ranked со stage_run_id: %s", raw)
		}
		m["_actor"] = actor
		out = append(out, m)
	}
	return out
}

func (f *ovFixture) eventCount() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM task_events`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func strOrNil(v any) string {
	if v == nil {
		return "<nil>"
	}
	return v.(string)
}

// checkRanked сверяет ответ перемещения с последним событием task.ranked
// задачи и ждёт соседей above и below ("<nil>" — края нет).
func (f *ovFixture) checkRanked(key string, out map[string]any, parent, above, below string) {
	f.t.Helper()
	if out["changed"] != true || strOrNil(out["parent"]) != parent ||
		strOrNil(out["above"]) != above || strOrNil(out["below"]) != below {
		f.t.Fatalf("%s: ответ %v, ждали parent=%s above=%s below=%s", key, out, parent, above, below)
	}
	task := out["task"].(map[string]any)
	if task["key"] != key || task["rank"] == "" || task["rank"] == nil {
		f.t.Fatalf("%s: задача в ответе %v", key, task)
	}
	evs := f.rankEvents(key)
	ev := evs[len(evs)-1]
	for _, k := range []string{"parent", "above", "below"} {
		if strOrNil(ev[k]) != strOrNil(out[k]) {
			f.t.Fatalf("%s: событие %v, ответ %v", key, ev, out)
		}
	}
}

// К-3, К-4, К-7, К-8, К-9: выше, ниже, в начало, в конец; меняется один
// ранг; событие и ответ; updated_at и last_moved_at прежние.
func TestRankMoves(t *testing.T) {
	f := newOvFixture(t)
	a, b, c, d := f.create("Ранг A", nil), f.create("Ранг B", nil), f.create("Ранг C", nil), f.create("Ранг D", nil)
	mine := []string{a, b, c, d}
	if got := only(f.backlog(""), mine...); !slices.Equal(got, mine) {
		t.Fatalf("новые задачи не в порядке заведения: %v", got)
	}
	moved := f.item(f.root, d)["last_moved_at"]

	// before: D между A и B.
	snap := f.ranks()
	out := f.rankOK(d, map[string]any{"before": b})
	if got := only(f.backlog(""), mine...); !slices.Equal(got, []string{a, d, b, c}) {
		t.Fatalf("before: %v", got)
	}
	f.checkRanked(d, out, "<nil>", a, b)
	after := f.ranks()
	for k, r := range snap {
		if k == d {
			if after[k].rank == r.rank || !after[k].updated.Equal(r.updated) {
				t.Fatalf("у перемещённой: ранг %q→%q, updated_at %v→%v", r.rank, after[k].rank, r.updated, after[k].updated)
			}
			continue
		}
		if after[k] != r {
			t.Fatalf("у %s изменилось %v → %v", k, r, after[k])
		}
	}
	if got := f.item(f.root, d)["last_moved_at"]; got != moved {
		t.Fatalf("last_moved_at %v → %v", moved, got)
	}
	if slices.Contains(domain.MovementEvents, domain.EventTaskRanked) {
		t.Fatal("task.ranked считается движением")
	}
	ev := f.rankEvents(d)
	if len(ev) != 1 || ev[0]["_actor"] != "root" {
		t.Fatalf("событие: %v", ev)
	}

	// after: A в конец через after C (у края).
	out = f.rankOK(a, map[string]any{"after": c})
	if got := only(f.backlog(""), mine...); !slices.Equal(got, []string{d, b, c, a}) {
		t.Fatalf("after: %v", got)
	}
	f.checkRanked(a, out, "<nil>", c, "<nil>")

	// top: C — первая на верхнем уровне (над SDLC-4 демо).
	out = f.rankOK(c, map[string]any{"position": "top"})
	if all := f.backlog(""); all[0] != c {
		t.Fatalf("top: %v", all)
	}
	f.checkRanked(c, out, "<nil>", "<nil>", f.backlog("")[1])

	// bottom: D — последняя.
	out = f.rankOK(d, map[string]any{"position": "bottom"})
	if all := f.backlog(""); all[len(all)-1] != d {
		t.Fatalf("bottom: %v", all)
	}
	f.checkRanked(d, out, "<nil>", a, "<nil>")

	// Сосед — терминальная задача: закрытая между двумя ключом
	// учитывается, соседом события — нет.
	x, y, z := f.create("Ранг X", nil), f.create("Ранг Y", nil), f.create("Ранг Z", nil)
	f.move(y, "cancel", f.root)
	out = f.rankOK(a, map[string]any{"before": z}) // A встаёт между Y (закрыта) и Z
	if got := only(f.backlog(""), x, y, z, a); !slices.Equal(got, []string{x, y, a, z}) {
		t.Fatalf("рядом с закрытой: %v", got)
	}
	f.checkRanked(a, out, "<nil>", x, z)
	out = f.rankOK(d, map[string]any{"after": x}) // D между X и Y (закрыта)
	if got := only(f.backlog(""), x, y, z, a, d); !slices.Equal(got, []string{x, d, y, a, z}) {
		t.Fatalf("после соседа перед закрытой: %v", got)
	}
	f.checkRanked(d, out, "<nil>", x, a)

	// Дети эпика: группа parent=<эпик>, parent в событии.
	epic := f.create("Ранг эпик", map[string]any{"type": "epic"})
	k1 := f.create("Ранг ребёнок 1", map[string]any{"parent_key": epic})
	k2 := f.create("Ранг ребёнок 2", map[string]any{"parent_key": epic})
	k3 := f.create("Ранг ребёнок 3", map[string]any{"parent_key": epic})
	if got := f.backlog(epic); !slices.Equal(got, []string{k1, k2, k3}) {
		t.Fatalf("дети по заведению: %v", got)
	}
	out = f.rankOK(k3, map[string]any{"position": "top"})
	if got := f.backlog(epic); !slices.Equal(got, []string{k3, k1, k2}) {
		t.Fatalf("дети после top: %v", got)
	}
	f.checkRanked(k3, out, epic, "<nil>", k1)
	// Лента задачи и лента проекта несут событие.
	feeds := []string{"/v1/tasks/" + k1 + "/events", "/v1/projects/SDLC/events"}
	cursors := map[string]string{}
	for _, p := range feeds {
		cursors[p] = f.ok("GET", p, f.root, nil)["cursor"].(string)
	}
	out = f.rankOK(k1, map[string]any{"after": k2})
	f.checkRanked(k1, out, epic, k2, "<nil>")
	for _, p := range feeds {
		items := f.ok("GET", p+"?wait=0&after="+url.QueryEscape(cursors[p]), f.root, nil)
		if !strings.Contains(fmt.Sprint(items), domain.EventTaskRanked) {
			t.Fatalf("лента %s: %v", p, items)
		}
	}
}

// К-6: то же место — 200, changed=false, событий нет.
func TestRankSamePlace(t *testing.T) {
	f := newOvFixture(t)
	a, b, c := f.create("Место A", nil), f.create("Место B", nil), f.create("Место C", nil)
	f.rankOK(a, map[string]any{"position": "top"})
	same := func(key string, body map[string]any, above, below string) {
		t.Helper()
		before, events := f.ranks(), f.eventCount()
		out := f.rankOK(key, body)
		if out["changed"] != false || strOrNil(out["above"]) != above || strOrNil(out["below"]) != below {
			t.Fatalf("%s %v: %v", key, body, out)
		}
		if f.eventCount() != events || f.ranks()[key] != before[key] {
			t.Fatalf("%s %v: что-то записано", key, body)
		}
	}
	all := f.backlog("")
	same(a, map[string]any{"position": "top"}, "<nil>", all[1])
	same(c, map[string]any{"position": "bottom"}, b, "<nil>")
	same(b, map[string]any{"before": c}, all[len(all)-3], c)
	same(c, map[string]any{"after": b}, b, "<nil>")
	// Повтор одного и того же вызова.
	f.rankOK(c, map[string]any{"before": b})
	same(c, map[string]any{"before": b}, all[len(all)-3], b)
	// Закрытая между двумя место не меняет.
	x, gone, y := f.create("Место X", nil), f.create("Место снятая", nil), f.create("Место Y", nil)
	f.move(gone, "cancel", f.root)
	same(x, map[string]any{"before": y}, b, y)
	same(y, map[string]any{"after": x}, x, "<nil>")
	// Единственная задача группы.
	epic := f.create("Место эпик", map[string]any{"type": "epic"})
	solo := f.create("Место один", map[string]any{"parent_key": epic})
	same(solo, map[string]any{"position": "top"}, "<nil>", "<nil>")
	same(solo, map[string]any{"position": "bottom"}, "<nil>", "<nil>")
}

func errMessage(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return msg
}

// К-5: отказы — код, текст по решению 0075, ранги и лента не меняются.
func TestRankErrors(t *testing.T) {
	f := newOvFixture(t)
	testdb.ForeignTask(t, postgres.New(f.pool))
	a, b := f.create("Отказ A", nil), f.create("Отказ B", nil)
	gone := f.create("Отказ снятая", nil)
	f.move(gone, "cancel", f.root)
	e1 := f.create("Отказ эпик 1", map[string]any{"type": "epic"})
	e2 := f.create("Отказ эпик 2", map[string]any{"type": "epic"})
	k1 := f.create("Отказ ребёнок 1", map[string]any{"parent_key": e1})
	k2 := f.create("Отказ ребёнок 2", map[string]any{"parent_key": e2})
	// Имя статуса — из версии цикла задачи, не из кода.
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE workflow_statuses ws SET name = 'Снята насовсем' FROM tasks t
		WHERE t.key = $1 AND ws.id = t.status_id`, gone); err != nil {
		t.Fatal(err)
	}

	const body400 = "укажите ровно одно из before, after, position"
	group := func(x, y string) string {
		return x + " и " + y + " в разных группах бэклога: ставить можно рядом с задачей того же эпика " +
			"или, на верхнем уровне, рядом с задачей верхнего уровня"
	}
	for _, c := range []struct {
		name string
		key  string
		body any
		code int
		text string
	}{
		{"пустое тело", a, nil, 400, body400},
		{"без места", a, map[string]any{}, 400, body400},
		{"два места", a, map[string]any{"before": b, "after": b}, 400, body400},
		{"место и край", a, map[string]any{"before": b, "position": "top"}, 400, body400},
		{"чужой край", a, map[string]any{"position": "middle"}, 400, body400},
		{"нет соседа", a, map[string]any{"before": "SDLC-999"}, 404, "задачи SDLC-999 в проекте SDLC нет"},
		{"сосед чужого проекта", a, map[string]any{"after": "OTHER-1"}, 404, "задачи OTHER-1 в проекте SDLC нет"},
		{"сосед не ключ", a, map[string]any{"after": "мусор"}, 404, "задачи мусор в проекте SDLC нет"},
		{"сама задача", a, map[string]any{"before": a}, 400, "задачу нельзя поставить рядом с ней самой"},
		{"ребёнок и верхний уровень", k1, map[string]any{"before": a}, 400, group(k1, a)},
		{"верхний уровень и ребёнок", a, map[string]any{"after": k1}, 400, group(a, k1)},
		{"дети разных эпиков", k1, map[string]any{"before": k2}, 400, group(k1, k2)},
		{"задача закрыта", gone, map[string]any{"position": "top"}, 409, gone + " закрыта (Снята насовсем): закрытую задачу не двигают"},
		{"сосед закрыт", a, map[string]any{"before": gone}, 409, gone + " закрыта (Снята насовсем): рядом с закрытой задачей не ставят"},
		{"задачи нет", "SDLC-998", map[string]any{"position": "top"}, 404, ""},
	} {
		before, events := f.ranks(), f.eventCount()
		var code int
		var out map[string]any
		if c.body == nil {
			rec := f.do("POST", fmt.Sprintf(rankPath, c.key), f.root, nil)
			code = rec.Code
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
		} else {
			code, out = f.rank(c.key, f.root, c.body)
		}
		if code != c.code || (c.text != "" && errMessage(out) != c.text) {
			t.Errorf("%s: %d %q, ждали %d %q", c.name, code, errMessage(out), c.code, c.text)
		}
		if f.eventCount() != events || fmt.Sprint(f.ranks()) != fmt.Sprint(before) {
			t.Errorf("%s: что-то записано", c.name)
		}
	}
}

// К-5: сосед закрылся, пока перемещение ждало блокировку группы.
func TestRankNeighborClosedWhileWaiting(t *testing.T) {
	f := newOvFixture(t)
	a, b := f.create("Ждёт A", nil), f.create("Ждёт B", nil)
	store := postgres.New(f.pool)
	task, err := store.GetTaskByKey(f.ctx, store.DB(), b)
	if err != nil {
		t.Fatal(err)
	}
	// Второе соединение держит блокировку группы и закрывает соседа.
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.LockRankGroup(f.ctx, tx, task.ProjectID, task.ParentID); err != nil {
		t.Fatal(err)
	}
	done := make(chan [2]any)
	go func() {
		code, out := f.rank(a, f.root, map[string]any{"before": b})
		done <- [2]any{code, out}
	}()
	select {
	case r := <-done:
		t.Fatalf("перемещение не ждало блокировку: %v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(f.ctx, `
		UPDATE tasks t SET status_id = ws.id FROM workflow_statuses ws
		WHERE t.key = $1 AND ws.workflow_id = t.workflow_id AND ws.key = 'cancelled'`, b); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r[0] != http.StatusConflict || !strings.Contains(errMessage(r[1].(map[string]any)), "рядом с закрытой задачей не ставят") {
		t.Fatalf("после закрытия соседа: %v", r)
	}
}

// К-10, К-11, К-12: личные токены, токен задачи, белый список и журнал.
func TestRankRights(t *testing.T) {
	f := newOvFixture(t)
	top1, top2 := f.create("Права верх 1", nil), f.create("Права верх 2", nil)

	// К-10: viewer — 403 до тела; agent, maintainer, admin — 200.
	viewer := f.tokenFor("user:vera", domain.MemberViewer)
	maint := f.tokenFor("user:mila", domain.MemberMaintainer)
	for _, body := range []any{map[string]any{"position": "top"}, map[string]any{}} {
		if code, out := f.rank(top1, viewer, body); code != http.StatusForbidden || !strings.Contains(errMessage(out), "agent") {
			t.Fatalf("viewer %v: %d %v", body, code, out)
		}
	}
	for name, tok := range map[string]string{"agent": f.bot, "maintainer": maint, "admin": f.root} {
		if code, out := f.rank(top2, tok, map[string]any{"position": "top"}); code != http.StatusOK {
			t.Fatalf("%s: %d %v", name, code, out)
		}
		if code, out := f.rank(top2, tok, map[string]any{"position": "bottom"}); code != http.StatusOK {
			t.Fatalf("%s: %d %v", name, code, out)
		}
	}

	// К-11: токен задачи своей задачи own (ребёнок эпика), дети own.
	epic := f.create("Права эпик", map[string]any{"type": "epic"})
	own := f.create("Права своя", map[string]any{"parent_key": epic})
	brother := f.create("Права брат", map[string]any{"parent_key": epic})
	c1 := f.create("Права ребёнок 1", map[string]any{"parent_key": own})
	c2 := f.create("Права ребёнок 2", map[string]any{"parent_key": own})
	c3 := f.create("Права ребёнок 3", map[string]any{"parent_key": own})
	f.move(own, "start", f.root)
	f.ok("POST", "/v1/tasks/"+own+"/claim", f.bot, map[string]any{})
	task := f.ok("POST", "/v1/tasks/"+own+"/task-token", f.bot, nil)["token"].(string)

	for _, body := range []map[string]any{{"before": c1}, {"after": c1}, {"position": "top"}, {"position": "bottom"}} {
		if code, out := f.rank(c3, task, body); code != http.StatusOK {
			t.Fatalf("токен задачи, ребёнок %v: %d %v", body, code, out)
		}
	}
	if ev := f.rankEvents(c3); ev[len(ev)-1]["_actor"] != "bot" {
		t.Fatalf("актор токена задачи: %v", ev)
	}
	for name, c := range map[string]struct {
		key  string
		body map[string]any
	}{
		"своя задача":          {own, map[string]any{"position": "top"}},
		"своя рядом с братом":  {own, map[string]any{"before": brother}},
		"брат своей":           {brother, map[string]any{"position": "top"}},
		"верхний уровень":      {top1, map[string]any{"before": top2}},
		"эпик":                 {epic, map[string]any{"position": "top"}},
		"ребёнок чужого эпика": {"SDLC-1", map[string]any{"position": "top"}},
	} {
		before := f.ranks()
		code, out := f.rank(c.key, task, c.body)
		msg := errMessage(out)
		if code != http.StatusForbidden || !strings.HasPrefix(msg, workflow.ExecutorRefusal) ||
			!strings.Contains(msg, "порядок бэклога проекта — решение человека") {
			t.Errorf("%s: %d %q", name, code, msg)
		}
		if fmt.Sprint(f.ranks()) != fmt.Sprint(before) {
			t.Errorf("%s: ранги изменились", name)
		}
	}
	// Правка приоритета токеном задачи — как раньше.
	if code, out := f.json("PATCH", "/v1/tasks/"+c2, task, map[string]any{"priority": "urgent"}); code != http.StatusOK {
		t.Fatalf("приоритет ребёнка токеном задачи: %d %v", code, out)
	}

	// К-12: строка в белом списке; журнал видит вызовы и отказы.
	if !slices.Contains(workflow.ExecutorActions(), workflow.RESTAction("POST", "/v1/tasks/:taskKey/rank")) {
		t.Fatal("ручки нет в белом списке токена задачи")
	}
	rows := f.rows("a.action = 'POST /v1/tasks/:taskKey/rank'")
	outcomes := map[string]int{}
	for _, r := range rows {
		kind := ""
		if r.TokenKind != nil {
			kind = *r.TokenKind
		}
		status := 0
		if r.Status != nil {
			status = *r.Status
		}
		outcomes[fmt.Sprintf("%s %d", kind, status)]++
	}
	if outcomes["task 200"] != 4 || outcomes["task 403"] != 6 || outcomes["api 403"] != 2 || outcomes["api 200"] != 6 {
		t.Fatalf("журнал перемещений: %v", outcomes)
	}
}

// К-13: параллельные перемещения одной группы — по очереди, без потерь.
func TestRankConcurrentMoves(t *testing.T) {
	f := newOvFixture(t)
	epic := f.create("Параллель эпик", map[string]any{"type": "epic"})
	var keys []string
	for i := range 8 {
		keys = append(keys, f.create(fmt.Sprintf("Параллель %d", i), map[string]any{"parent_key": epic}))
	}
	initial := f.backlog(epic)

	type call struct {
		key  string
		body map[string]any
	}
	var calls []call
	for i := range 24 {
		k := keys[i%len(keys)]
		if i%3 == 0 {
			k = keys[0] // одна и та же задача — часть вызовов
		}
		var body map[string]any
		switch i % 4 {
		case 0:
			body = map[string]any{"position": "top"}
		case 1:
			body = map[string]any{"position": "bottom"}
		case 2:
			body = map[string]any{"before": keys[(i+3)%len(keys)]}
		default:
			body = map[string]any{"after": keys[(i+5)%len(keys)]}
		}
		if body["before"] == k || body["after"] == k {
			body = map[string]any{"position": "top"}
		}
		calls = append(calls, call{k, body})
	}
	var wg sync.WaitGroup
	results := make([]map[string]any, len(calls))
	codes := make([]int, len(calls))
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], results[i] = f.rank(c.key, f.root, c.body)
		}()
	}
	wg.Wait()
	changed := 0
	for i, c := range calls {
		if codes[i] != http.StatusOK {
			t.Fatalf("%v: %d %v", c, codes[i], results[i])
		}
		r := results[i]
		// Каждый вызов применён относительно названного соседа.
		switch {
		case c.body["position"] == "top" && r["above"] != nil,
			c.body["position"] == "bottom" && r["below"] != nil,
			c.body["before"] != nil && r["below"] != c.body["before"],
			c.body["after"] != nil && r["above"] != c.body["after"]:
			t.Fatalf("%v: соседи %v", c, r)
		}
		if r["changed"] == true {
			changed++
		}
	}

	// Порядок полон и детерминирован.
	final := f.backlog(epic)
	if len(final) != len(keys) || !slices.Equal(f.backlog(epic), final) {
		t.Fatalf("итог %v", final)
	}
	sorted := slices.Clone(final)
	slices.Sort(sorted)
	want := slices.Clone(keys)
	slices.Sort(want)
	if !slices.Equal(sorted, want) {
		t.Fatalf("задачи потерялись или повторились: %v", final)
	}

	// Повтор по событиям в порядке коммитов даёт итоговый порядок: каждое
	// перемещение легло относительно соседей на момент своей очереди.
	rows, err := f.pool.Query(f.ctx, `
		SELECT t.key, e.payload FROM task_events e JOIN tasks t ON t.id = e.task_id
		WHERE e.type = $1 AND t.parent_id = (SELECT id FROM tasks WHERE key = $2)
		ORDER BY e.created_at`, domain.EventTaskRanked, epic)
	if err != nil {
		t.Fatal(err)
	}
	replay := slices.Clone(initial)
	events := 0
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			t.Fatal(err)
		}
		var p struct{ Above, Below *string }
		_ = json.Unmarshal(raw, &p)
		replay = slices.DeleteFunc(replay, func(k string) bool { return k == key })
		at := 0
		if p.Above != nil {
			at = slices.Index(replay, *p.Above) + 1
		}
		replay = slices.Insert(replay, at, key)
		if (p.Below == nil) != (at == len(replay)-1) || (p.Below != nil && replay[at+1] != *p.Below) {
			t.Fatalf("событие %s %s не сходится с повтором %v", key, raw, replay)
		}
		events++
	}
	rows.Close()
	if events != changed || !slices.Equal(replay, final) {
		t.Fatalf("повтор %v (%d событий), итог %v (%d изменений)", replay, events, final, changed)
	}
}

// К-4, К-14: ключ вышел длиннее порога — группа перераздаётся в прежнем
// порядке, без событий у соседей и без updated_at.
func TestRankSpreadOnLongKey(t *testing.T) {
	f := newOvFixture(t)
	epic := f.create("Порог эпик", map[string]any{"type": "epic"})
	a := f.create("Порог A", map[string]any{"parent_key": epic})
	b := f.create("Порог B", map[string]any{"parent_key": epic})
	c := f.create("Порог C", map[string]any{"parent_key": epic})
	d := f.create("Порог D", map[string]any{"parent_key": epic})
	long := strings.Repeat("V", domain.RankMaxLen-1)
	for k, r := range map[string]string{a: long + "1", b: long + "2", c: "W", d: "X"} {
		if _, err := f.pool.Exec(f.ctx, `UPDATE tasks SET rank = $2 WHERE key = $1`, k, r); err != nil {
			t.Fatal(err)
		}
	}
	before, events := f.ranks(), f.eventCount()
	out := f.rankOK(d, map[string]any{"after": a}) // между a и b — ключ длиннее порога
	if got := f.backlog(epic); !slices.Equal(got, []string{a, d, b, c}) {
		t.Fatalf("после перераздачи: %v", got)
	}
	f.checkRanked(d, out, epic, a, b)
	after := f.ranks()
	for _, k := range []string{a, b, c, d} {
		if len(after[k].rank) > domain.RankMaxLen || after[k].rank == before[k].rank ||
			!after[k].updated.Equal(before[k].updated) {
			t.Fatalf("%s: %v → %v", k, before[k], after[k])
		}
	}
	if f.eventCount() != events+1 {
		t.Fatalf("событий %d, ждали одно", f.eventCount()-events)
	}
	// Равные ранги соседей — тоже перераздача, а не отказ.
	if _, err := f.pool.Exec(f.ctx, `UPDATE tasks SET rank = 'k' WHERE key IN ($1, $2)`, a, d); err != nil {
		t.Fatal(err)
	}
	order := f.backlog(epic) // b, a, d, c: у a и d ранг один, порядок — номер
	if !slices.Equal(order, []string{b, a, d, c}) {
		t.Fatalf("равные ранги: %v", order)
	}
	f.rankOK(c, map[string]any{"after": a})
	if got := f.backlog(epic); !slices.Equal(got, []string{b, a, c, d}) {
		t.Fatalf("при равных рангах: %v", got)
	}
}

// К-15: новая задача — в конец своей группы; параллельные заведения — в
// порядке номеров.
func TestRankNewTaskAtEnd(t *testing.T) {
	f := newOvFixture(t)
	first := f.create("Конец первая", nil)
	f.rankOK(first, map[string]any{"position": "bottom"})
	next := f.create("Конец вторая", nil)
	if all := f.backlog(""); all[len(all)-1] != next {
		t.Fatalf("новая не последняя: %v", all)
	}
	epic := f.create("Конец эпик", map[string]any{"type": "epic"})
	k1 := f.create("Конец ребёнок 1", map[string]any{"parent_key": epic})
	k2 := f.create("Конец ребёнок 2", map[string]any{"parent_key": epic})
	f.rankOK(k2, map[string]any{"position": "top"})
	k3 := f.create("Конец ребёнок 3", map[string]any{"parent_key": epic})
	if got := f.backlog(epic); !slices.Equal(got, []string{k2, k1, k3}) {
		t.Fatalf("ребёнок не последний: %v", got)
	}
	// Задача другого типа (своего цикла) — в конец так же: заведение одно
	// для всех типов (CreateTask), выпуск версии и нарезка эпика идут им же.
	rel := f.create("Конец эпик второй", map[string]any{"type": "epic"})
	if all := f.backlog(""); all[len(all)-1] != rel {
		t.Fatalf("выпуск не последний: %v", all)
	}

	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.create(fmt.Sprintf("Конец параллель %d", i), nil)
		}()
	}
	wg.Wait()
	all := f.backlog("")
	tail := all[len(all)-5:]
	rs := f.ranks()
	for i := 1; i < len(tail); i++ {
		if rs[tail[i-1]].rank >= rs[tail[i]].rank {
			t.Fatalf("ранги параллельных не возрастают: %v", tail)
		}
	}
	seqs := make([]int, len(tail))
	for i, k := range tail {
		_, _ = fmt.Sscanf(k, "SDLC-%d", &seqs[i])
	}
	if !slices.IsSorted(seqs) {
		t.Fatalf("параллельные не в порядке номеров: %v", tail)
	}
}

// К-16, К-17, К-22: выборка backlog=true, поле in_work и rank.
func TestRankBacklogSelection(t *testing.T) {
	f := newOvFixture(t)
	fresh := f.create("Выборка неначатая", nil)
	working := f.create("Выборка в работе", nil)
	f.move(working, "start", f.root)
	gone := f.create("Выборка снятая", nil)
	f.move(gone, "cancel", f.root)
	// Эпик в работе с неначатым ребёнком и эпик в работе без таких.
	open := f.create("Выборка эпик с неначатым", map[string]any{"type": "epic"})
	openKid := f.create("Выборка неначатый ребёнок", map[string]any{"parent_key": open})
	openBusy := f.create("Выборка ребёнок в работе", map[string]any{"parent_key": open})
	f.move(openBusy, "start", f.root)
	busy := f.create("Выборка эпик без неначатых", map[string]any{"type": "epic"})
	busyKid := f.create("Выборка ребёнок эпика в работе", map[string]any{"parent_key": busy})
	f.move(busyKid, "start", f.root)
	busyDone := f.create("Выборка готовый ребёнок", map[string]any{"parent_key": busy})
	f.setStatus(busyDone, "done")

	base := "q=" + url.QueryEscape("Выборка") + "&order=backlog"
	q := base + "&limit=100"
	items, total := f.list(f.root, q+"&backlog=true&top_level=true")
	if got := taskKeysOf(items); !slices.Equal(got, []string{fresh, open}) || total != 2 {
		t.Fatalf("backlog=true наверху: %v (%d)", got, total)
	}
	if items, _ := f.list(f.root, q+"&backlog=true&parent="+open); !slices.Equal(taskKeysOf(items), []string{openKid}) {
		t.Fatalf("backlog=true детей: %v", taskKeysOf(items))
	}
	if items, _ := f.list(f.root, q+"&backlog=true&type=epic"); !slices.Equal(taskKeysOf(items), []string{open}) {
		t.Fatalf("backlog=true с type: %v", taskKeysOf(items))
	}
	if items, _ := f.list(f.root, q+"&backlog=true&terminal=true"); len(items) != 0 {
		t.Fatalf("backlog=true с terminal=true: %v", taskKeysOf(items))
	}
	// Листание: total совпадает со строками.
	var paged []string
	full, fullTotal := f.list(f.root, q+"&backlog=true")
	for off := 0; off < fullTotal; off++ {
		page, n := f.list(f.root, fmt.Sprintf("%s&backlog=true&limit=1&offset=%d", base, off))
		if n != fullTotal {
			t.Fatalf("total листания %d против %d", n, fullTotal)
		}
		paged = append(paged, taskKeysOf(page)...)
	}
	if !slices.Equal(paged, taskKeysOf(full)) || fullTotal != len(full) {
		t.Fatalf("листание %v против %v", paged, taskKeysOf(full))
	}
	// Режим внимания.
	if _, n := f.list(f.root, q+"&backlog=true&attention=true"); n > fullTotal {
		t.Fatalf("внимание шире выборки: %d", n)
	}
	// Без параметра и с false — прежняя выборка; не логическое — 400.
	_, all := f.list(f.root, q)
	_, off := f.list(f.root, q+"&backlog=false")
	if all != off || all <= fullTotal {
		t.Fatalf("без параметра %d, false %d, backlog %d", all, off, fullTotal)
	}
	if rec := f.do("GET", "/v1/projects/SDLC/tasks?backlog=maybe", f.root, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("backlog=maybe: %d", rec.Code)
	}

	// in_work у строки совпадает с попаданием в in_work=true, поле rank есть.
	inWork := map[string]bool{}
	worked, _ := f.list(f.root, q+"&in_work=true")
	for _, k := range taskKeysOf(worked) {
		inWork[k] = true
	}
	rows, _ := f.list(f.root, q)
	for _, it := range rows {
		k := it["key"].(string)
		v, ok := it["in_work"].(bool)
		if !ok || v != inWork[k] {
			t.Errorf("%s: in_work %v, в выборке in_work=true %v", k, it["in_work"], inWork[k])
		}
		if r, _ := it["rank"].(string); r == "" {
			t.Errorf("%s без rank", k)
		}
	}
	if !inWork[working] || !inWork[busy] || !inWork[open] || inWork[fresh] || inWork[gone] || inWork[openKid] {
		t.Fatalf("in_work: %v", inWork)
	}
	// Карточка задачи: rank есть, in_work — нет.
	card := f.ok("GET", "/v1/tasks/"+fresh, f.root, nil)["task"].(map[string]any)
	if r, _ := card["rank"].(string); r == "" {
		t.Fatalf("rank в карточке: %v", card)
	}
	if _, ok := card["in_work"]; ok {
		t.Fatal("in_work в карточке")
	}
}

// К-16: выборка по данным цикла — на втором цикле со своими ключами
// статусов.
func TestRankBacklogOtherCycle(t *testing.T) {
	f := newOvFixture(t)
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE workflow_statuses SET key = key || '_x'
		WHERE workflow_id = (SELECT workflow_id FROM tasks WHERE key = 'SDLC-1')`); err != nil {
		t.Fatal(err)
	}
	fresh := f.create("Цикл неначатая", nil)
	working := f.create("Цикл в работе", nil)
	f.move(working, "start", f.root)
	items, _ := f.list(f.root, "q="+url.QueryEscape("Цикл")+"&backlog=true")
	if got := taskKeysOf(items); !slices.Equal(got, []string{fresh}) {
		t.Fatalf("выборка на другом цикле: %v", got)
	}
}
