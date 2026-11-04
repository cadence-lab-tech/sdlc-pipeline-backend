package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Порядок бэклога в CLI (CLAB-400): тесты идут на фейковом сервере, критерии
// К-N — из анализа задачи.

// fakeBacklog — сервер, который отвечает по пути и запросу и считает вызовы.
type fakeBacklog struct {
	mu      sync.Mutex
	calls   []string
	bodies  []string
	version string
	handler func(w http.ResponseWriter, r *http.Request)
}

func newFakeBacklog(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*Client, *fakeBacklog) {
	t.Helper()
	f := &fakeBacklog{handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
		f.bodies = append(f.bodies, string(raw))
		version := f.version
		f.mu.Unlock()
		if version != "" {
			w.Header().Set(headerServerVersion, version)
		}
		f.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL + "/v1", origin: "https://ui.example", actor: "user:alice", http: srv.Client()}, f
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// ── clab rank ───────────────────────────────────────────────────────────────

func TestRankPlacesAndTexts(t *testing.T) {
	withJSON(t, false)
	cases := []struct {
		name  string
		args  []string
		reply string
		body  string
		want  string
	}{
		{"before", []string{"--before", "CLAB-15"}, `{"changed":true,"parent":null,"above":"CLAB-14","below":"CLAB-15"}`,
			`{"before":"CLAB-15"}`, "CLAB-12 поставлена над CLAB-15.\n"},
		{"after", []string{"--after", "CLAB-15"}, `{"changed":true,"parent":null,"above":"CLAB-15","below":"CLAB-16"}`,
			`{"after":"CLAB-15"}`, "CLAB-12 поставлена под CLAB-15.\n"},
		{"top", []string{"--top"}, `{"changed":true,"parent":null,"above":null,"below":"CLAB-3"}`,
			`{"position":"top"}`, "CLAB-12 — в начале бэклога.\n"},
		{"bottom", []string{"--bottom"}, `{"changed":true,"parent":null,"above":"CLAB-3","below":null}`,
			`{"position":"bottom"}`, "CLAB-12 — в конце бэклога.\n"},
		{"top в эпике", []string{"--top"}, `{"changed":true,"parent":"CLAB-85","above":null,"below":"CLAB-3"}`,
			`{"position":"top"}`, "CLAB-12 — в начале эпика CLAB-85.\n"},
		{"bottom в эпике", []string{"--bottom"}, `{"changed":true,"parent":"CLAB-85","above":"CLAB-3","below":null}`,
			`{"position":"bottom"}`, "CLAB-12 — в конце эпика CLAB-85.\n"},
		// К-3: то же место.
		{"то же место", []string{"--top"}, `{"changed":false,"parent":null,"above":null,"below":"CLAB-3"}`,
			`{"position":"top"}`, "CLAB-12 уже на этом месте.\n"},
	}
	for _, tc := range cases {
		c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, tc.reply) })
		out, _, err := capture(t, func() error {
			return cmdRank(context.Background(), c, append([]string{"CLAB-12"}, tc.args...))
		})
		if err != nil || out != tc.want {
			t.Errorf("%s: %q, %v; ждали %q", tc.name, out, err, tc.want)
		}
		if len(f.calls) != 1 || f.calls[0] != "POST /v1/tasks/CLAB-12/rank" || !equalJSONText(f.bodies[0], tc.body) {
			t.Errorf("%s: запрос %v тело %v, ждали %s", tc.name, f.calls, f.bodies, tc.body)
		}
	}
}

func equalJSONText(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return fmt.Sprint(x) == fmt.Sprint(y)
}

// К-2: ровно одно место, запроса нет.
func TestRankNeedsExactlyOnePlace(t *testing.T) {
	withJSON(t, false)
	for _, args := range [][]string{
		{"CLAB-12"},
		{"CLAB-12", "--before", "CLAB-15", "--top"},
		{"CLAB-12", "--top", "--bottom"},
		{"CLAB-12", "--before", "CLAB-15", "--after", "CLAB-16"},
	} {
		c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, `{}`) })
		_, _, err := capture(t, func() error { return cmdRank(context.Background(), c, args) })
		if err == nil || err.Error() != "укажите ровно одно место: --before <ключ>, --after <ключ>, --top или --bottom" {
			t.Errorf("%v: %v", args, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("%v: ушёл запрос %v", args, f.calls)
		}
	}
	// Без ключа задачи — отказ тоже без запроса.
	c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, `{}`) })
	if _, _, err := capture(t, func() error { return cmdRank(context.Background(), c, []string{"--top"}) }); err == nil || len(f.calls) != 0 {
		t.Errorf("без ключа: %v %v", err, f.calls)
	}
}

// К-4: текст отказа сервера — как есть.
func TestRankServerRefusalsVerbatim(t *testing.T) {
	withJSON(t, false)
	for _, tc := range []struct {
		status int
		code   string
		msg    string
	}{
		{400, "invalid_input", "CLAB-12 и CLAB-15 в разных группах бэклога: ставить можно рядом с задачей того же эпика или, на верхнем уровне, рядом с задачей верхнего уровня"},
		{400, "invalid_input", "задачу нельзя поставить рядом с ней самой"},
		{404, "not_found", "задачи CLAB-999 в проекте CLAB нет"},
		{409, "conflict", "CLAB-12 закрыта (Снята): закрытую задачу не двигают"},
		{409, "conflict", "CLAB-15 закрыта (Снята): рядом с закрытой задачей не ставят"},
		{403, "forbidden", "нужен уровень agent или выше"},
		{403, "forbidden", "токен задачи: порядок бэклога проекта — решение человека: токен задачи двигает только задачи своей нарезки"},
	} {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": tc.code, "message": tc.msg}})
		c, _ := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, tc.status, string(body)) })
		_, _, err := capture(t, func() error {
			return cmdRank(context.Background(), c, []string{"CLAB-12", "--before", "CLAB-15"})
		})
		var apiErr *APIError
		if err == nil || !asAPI(err, &apiErr) || apiErr.Message != tc.msg || apiErr.Code != tc.code || apiErr.Status != tc.status {
			t.Errorf("%d %s: %v", tc.status, tc.msg, err)
		}
	}
}

func asAPI(err error, out **APIError) bool {
	e, ok := err.(*APIError)
	if ok {
		*out = e
	}
	return ok
}

// К-8: --json — тело ответа сервера без изменений.
func TestRankAndBacklogJSON(t *testing.T) {
	withJSON(t, true)
	const ranked = `{"task":{"key":"CLAB-12","rank":"V"},"changed":true,"parent":null,"above":null,"below":"CLAB-3"}`
	c, _ := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, ranked) })
	out, _, err := capture(t, func() error {
		return cmdRank(context.Background(), c, []string{"CLAB-12", "--top"})
	})
	if err != nil || !equalJSONText(out, ranked) {
		t.Fatalf("rank --json: %q %v", out, err)
	}

	list := backlogReply(rows(row{key: "CLAB-1"}, row{key: "CLAB-2", blocked: []string{"CLAB-1"}}), 2)
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, list) })
	out, _, err = capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB"})
	})
	if err != nil || !equalJSONText(out, list) {
		t.Fatalf("backlog --json: %q %v", out, err)
	}
}

// К-9: сервер прежней версии.
func TestRankOldServer(t *testing.T) {
	withJSON(t, false)
	want := "сервер не знает порядка бэклога: обновите сервер до v0.25.0 или новее"
	// Plain-text 404 маршрута без кода ошибки.
	c, _ := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte("404 page not found"))
	})
	_, _, err := capture(t, func() error {
		return cmdRank(context.Background(), c, []string{"CLAB-12", "--top"})
	})
	if err == nil || err.Error() != want {
		t.Fatalf("404 без кода: %v", err)
	}
	// 404 «задачи нет» с кодом — это не старый сервер.
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 404, `{"error":{"code":"not_found","message":"задачи CLAB-999 в проекте CLAB нет"}}`)
	})
	_, _, err = capture(t, func() error {
		return cmdRank(context.Background(), c, []string{"CLAB-12", "--before", "CLAB-999"})
	})
	if err == nil || strings.Contains(err.Error(), "обновите сервер") || !strings.Contains(err.Error(), "CLAB-999") {
		t.Fatalf("404 с кодом: %v", err)
	}
}

// ── clab backlog ────────────────────────────────────────────────────────────

type row struct {
	key, title, priority, typ string
	inWork                    bool
	blocked                   []string
	children                  map[string]int
	noRank                    bool
}

func rows(rs ...row) []row { return rs }

func (r row) json() string {
	if r.title == "" {
		r.title = "Задача " + r.key
	}
	if r.priority == "" {
		r.priority = "normal"
	}
	if r.typ == "" {
		r.typ = "task"
	}
	m := map[string]any{
		"key": r.key, "title": r.title, "priority": r.priority, "status_key": "new",
		"properties": map[string]any{"type": r.typ}, "in_work": r.inWork, "blocked_by": r.blocked,
	}
	if !r.noRank {
		m["rank"] = "V"
	}
	if r.children != nil {
		m["child_counts"] = r.children
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

func backlogReply(rs []row, total int) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.json()
	}
	return `{"tasks":[` + strings.Join(parts, ",") + `],"total":` + strconv.Itoa(total) + `}`
}

// serveBacklog — фейк с постраничной выдачей: верхний уровень и дети эпиков.
func serveBacklog(top []row, kids map[string][]row, titles map[string]string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/tasks/") {
			key := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
			reply(w, 200, fmt.Sprintf(`{"task":{"key":%q,"title":%q}}`, key, titles[key]))
			return
		}
		q := r.URL.Query()
		list := top
		if p := q.Get("parent"); p != "" {
			list = kids[p]
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		if limit == 0 {
			limit = 200
		}
		end := min(offset+limit, len(list))
		page := []row{}
		if offset < len(list) {
			page = list[offset:end]
		}
		reply(w, 200, backlogReply(page, len(list)))
	}
}

// К-5: таблица, колонки, пометки, «показано», порядок сервера.
func TestBacklogTable(t *testing.T) {
	withJSON(t, false)
	list := rows(
		row{key: "CLAB-9", priority: "urgent", title: "Срочная"},
		row{key: "CLAB-85", typ: "epic", inWork: true, children: map[string]int{"backlog": 4, "active": 1}, title: "Эпик"},
		row{key: "CLAB-3", priority: "low", blocked: []string{"CLAB-1", "CLAB-2", "CLAB-4", "CLAB-5", "CLAB-6"}},
		row{key: "CLAB-7", typ: "epic", children: map[string]int{"backlog": 0}},
		row{key: "CLAB-8", inWork: true, blocked: []string{"CLAB-1"}},
	)
	c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, backlogReply(list, 76)) })
	out, _, err := capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB"})
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("строк %d:\n%s", len(lines), out)
	}
	header := strings.Fields(lines[0])
	if strings.Join(header, " ") != "№ КЛЮЧ ТИП ПРИОРИТЕТ ЗАДАЧ ПОМЕТКИ ЗАГОЛОВОК" {
		t.Errorf("шапка: %q", lines[0])
	}
	// Порядок сервера не меняется; слова «заблокирована» нет.
	var keys []string
	for _, l := range lines[1:6] {
		keys = append(keys, strings.Fields(l)[1])
	}
	if strings.Join(keys, " ") != "CLAB-9 CLAB-85 CLAB-3 CLAB-7 CLAB-8" || strings.Contains(out, "заблокирована") {
		t.Errorf("порядок: %v\n%s", keys, out)
	}
	for _, want := range []string{"epic  normal", "в работе", "ждёт CLAB-1, CLAB-2, CLAB-4 и ещё 2", "в работе · ждёт CLAB-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q:\n%s", want, out)
		}
	}
	// ЗАДАЧ: счёт backlog у эпиков, прочерк у задач без детей, 0 у эпика без неначатых.
	for key, want := range map[string]string{"CLAB-85": "4", "CLAB-7": "0", "CLAB-9": "—"} {
		for _, l := range lines[1:6] {
			if fs := strings.Fields(l); fs[1] == key && fs[4] != want {
				t.Errorf("%s ЗАДАЧ %q, ждали %q", key, fs[4], want)
			}
		}
	}
	if lines[6] != "Показано 5 из 76. Дальше: clab backlog --limit 76" {
		t.Errorf("подвал: %q", lines[6])
	}
	if len(f.calls) != 1 {
		t.Fatalf("запросы: %v", f.calls)
	}
	u, _ := url.Parse(strings.TrimPrefix(f.calls[0], "GET "))
	q := u.Query()
	if u.Path != "/v1/projects/CLAB/tasks" || q.Get("backlog") != "true" || q.Get("top_level") != "true" ||
		q.Get("order") != "backlog" || q.Get("limit") != "50" {
		t.Errorf("запрос: %s", f.calls[0])
	}
}

func TestBacklogLimitAndEmpty(t *testing.T) {
	withJSON(t, false)
	c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, backlogReply(nil, 0)) })
	for _, bad := range []string{"0", "-1", "501"} {
		_, _, err := capture(t, func() error {
			return cmdBacklog(context.Background(), c, []string{"--project", "CLAB", "--limit", bad})
		})
		if err == nil || !strings.Contains(err.Error(), "--limit") {
			t.Errorf("--limit %s: %v", bad, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("запросы при плохом --limit: %v", f.calls)
	}
	out, _, err := capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB", "--limit", "500"})
	})
	if err != nil || out != "Бэклог пуст.\n" {
		t.Errorf("пусто: %q %v", out, err)
	}
	// Больше 500 — без предложения увеличить предел.
	big := make([]row, 3)
	for i := range big {
		big[i] = row{key: fmt.Sprintf("CLAB-%d", i+1)}
	}
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, backlogReply(big, 900)) })
	out, _, _ = capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB", "--limit", "3"})
	})
	if !strings.Contains(out, "Показано 3 из 900. Дальше: clab backlog --limit 500") {
		t.Errorf("подвал: %q", out)
	}
}

// К-6: бэклог эпика.
func TestBacklogParent(t *testing.T) {
	withJSON(t, false)
	kids := map[string][]row{"CLAB-85": rows(row{key: "CLAB-86"}, row{key: "CLAB-87", blocked: []string{"CLAB-86"}}), "CLAB-90": nil}
	titles := map[string]string{"CLAB-85": "Пакет поставки", "CLAB-90": "Пустой"}
	c, f := newFakeBacklog(t, serveBacklog(nil, kids, titles))
	out, _, err := capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB", "--parent", "CLAB-85"})
	})
	if err != nil || !strings.HasPrefix(out, "Бэклог эпика CLAB-85 · Пакет поставки\n") ||
		!strings.Contains(out, "CLAB-87") || !strings.Contains(out, "ждёт CLAB-86") {
		t.Fatalf("%q %v", out, err)
	}
	found := false
	for _, call := range f.calls {
		if strings.Contains(call, "parent=CLAB-85") && strings.Contains(call, "backlog=true") &&
			strings.Contains(call, "order=backlog") && !strings.Contains(call, "top_level") {
			found = true
		}
	}
	if !found {
		t.Errorf("запрос детей: %v", f.calls)
	}
	out, _, err = capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB", "--parent", "CLAB-90"})
	})
	if err != nil || out != "Бэклог эпика CLAB-90 · Пустой\nВ эпике CLAB-90 нет задач в бэклоге.\n" {
		t.Errorf("пустой эпик: %q %v", out, err)
	}
	// Нет такого эпика: отказ сервера с его текстом, таблицы нет.
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 404, `{"error":{"code":"not_found","message":"задача CLAB-999 не найдена"}}`)
	})
	if _, _, err = capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB", "--parent", "CLAB-999"})
	}); err == nil || !strings.Contains(err.Error(), "CLAB-999") {
		t.Errorf("нет эпика: %v", err)
	}
}

// К-9: сервер прежней версии отдаёт обычный список — без поля rank.
func TestBacklogOldServer(t *testing.T) {
	withJSON(t, false)
	want := "сервер не знает порядка бэклога: обновите сервер до v0.25.0 или новее"
	old := backlogReply(rows(row{key: "CLAB-1", noRank: true}, row{key: "CLAB-2", noRank: true}), 2)
	c, _ := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, old) })
	_, _, err := capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB"})
	})
	if err == nil || err.Error() != want {
		t.Fatalf("список без rank: %v", err)
	}
	// То же для --next.
	if _, _, err = capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB", "--next"})
	}); err == nil || err.Error() != want {
		t.Fatalf("--next: %v", err)
	}

	// Пустой ответ: версия из заголовка решает; без версии и у новой — «пуст».
	for _, tc := range []struct {
		version string
		old     bool
	}{{"v0.24.0", true}, {"v0.25.0", false}, {"v0.26.1", false}, {"dev", false}, {"3f2a9c1", false}, {"", false}} {
		c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, backlogReply(nil, 0)) })
		f.version = tc.version
		out, _, err := capture(t, func() error {
			return cmdBacklog(context.Background(), c, []string{"--project", "CLAB"})
		})
		if tc.old && (err == nil || err.Error() != want) {
			t.Errorf("пустой ответ %s: %q %v", tc.version, out, err)
		}
		if !tc.old && (err != nil || out != "Бэклог пуст.\n") {
			t.Errorf("пустой ответ %q: %q %v", tc.version, out, err)
		}
	}
}

// ── clab backlog --next ─────────────────────────────────────────────────────

func nextOf(t *testing.T, top []row, kids map[string][]row, extra ...string) (string, *fakeBacklog) {
	t.Helper()
	titles := map[string]string{"CLAB-85": "Пакет поставки"}
	c, f := newFakeBacklog(t, serveBacklog(top, kids, titles))
	out, _, err := capture(t, func() error {
		return cmdBacklog(context.Background(), c, append([]string{"--project", "CLAB", "--next"}, extra...))
	})
	if err != nil {
		t.Fatal(err)
	}
	return out, f
}

// К-7: первая свободная — 110-я из 120, читаются все страницы, --limit не
// действует.
func TestBacklogNextReadsAllPages(t *testing.T) {
	withJSON(t, false)
	var top []row
	for i := 1; i <= 120; i++ {
		r := row{key: fmt.Sprintf("CLAB-%d", i), blocked: []string{"CLAB-999"}}
		if i == 110 {
			r.blocked, r.priority, r.title = nil, "urgent", "Первая свободная"
		}
		top = append(top, r)
	}
	out, _ := nextOf(t, top, nil, "--limit", "5")
	want := "Следующая: CLAB-110  urgent  Первая свободная\nпропущены заблокированные: CLAB-1, CLAB-2, CLAB-3 и ещё 106\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}

	// Больше 500 строк: вторая страница по offset.
	top = top[:0]
	for i := 1; i <= 700; i++ {
		r := row{key: fmt.Sprintf("CLAB-%d", i), blocked: []string{"CLAB-999"}}
		if i == 650 {
			r.blocked = nil
		}
		top = append(top, r)
	}
	out, f := nextOf(t, top, nil)
	if !strings.HasPrefix(out, "Следующая: CLAB-650  normal  Задача CLAB-650\n") {
		t.Fatalf("%q", out)
	}
	if len(f.calls) != 2 || !strings.Contains(f.calls[0], "limit=500") || !strings.Contains(f.calls[1], "offset=500") {
		t.Fatalf("страницы: %v", f.calls)
	}
}

func TestBacklogNextEpicInWork(t *testing.T) {
	withJSON(t, false)
	epic := row{key: "CLAB-85", typ: "epic", inWork: true, title: "Пакет поставки", children: map[string]int{"backlog": 3}}
	kids := map[string][]row{"CLAB-85": rows(
		row{key: "CLAB-86", blocked: []string{"CLAB-80"}},
		row{key: "CLAB-87", priority: "high", title: "Свободный ребёнок"},
		row{key: "CLAB-88"},
	)}
	// Эпик в работе раскрывается до первого свободного ребёнка.
	out, _ := nextOf(t, rows(epic, row{key: "CLAB-1"}), kids)
	want := "Следующая: CLAB-87  high  Свободный ребёнок\nв эпике CLAB-85 · Пакет поставки\nпропущены заблокированные: CLAB-86\n"
	if out != want {
		t.Fatalf("%q", out)
	}
	// Все дети заблокированы — эпик пропускается, идём дальше.
	kids = map[string][]row{"CLAB-85": rows(row{key: "CLAB-86", blocked: []string{"CLAB-80"}})}
	out, _ = nextOf(t, rows(epic, row{key: "CLAB-1", title: "Простая"}), kids)
	if out != "Следующая: CLAB-1  normal  Простая\nпропущены заблокированные: CLAB-86\n" {
		t.Fatalf("все дети заблокированы: %q", out)
	}
	// Сам эпик заблокирован — пропускается целиком, детей не читаем.
	blockedEpic := epic
	blockedEpic.blocked = []string{"CLAB-70"}
	out, f := nextOf(t, rows(blockedEpic, row{key: "CLAB-1"}), kids)
	if !strings.HasPrefix(out, "Следующая: CLAB-1 ") || !strings.Contains(out, "пропущены заблокированные: CLAB-85") {
		t.Fatalf("заблокированный эпик: %q", out)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "parent=CLAB-85") {
			t.Errorf("дети заблокированного эпика прочитаны: %v", f.calls)
		}
	}
	// Эпик не в работе — обычная строка, детей не читаем.
	plain := epic
	plain.inWork = false
	out, f = nextOf(t, rows(plain), kids)
	if !strings.HasPrefix(out, "Следующая: CLAB-85 ") || len(f.calls) != 1 {
		t.Fatalf("эпик не в работе: %q %v", out, f.calls)
	}
}

func TestBacklogNextBlockedAndEmpty(t *testing.T) {
	withJSON(t, false)
	out, _ := nextOf(t, rows(
		row{key: "CLAB-390", blocked: []string{"CLAB-399", "CLAB-400"}},
		row{key: "CLAB-391", blocked: []string{"CLAB-390"}},
	), nil)
	if out != "Свободной задачи нет: все в бэклоге ждут блокирующих. Первая по порядку — CLAB-390, ждёт CLAB-399, CLAB-400.\n" {
		t.Fatalf("все заблокированы: %q", out)
	}
	if out, _ = nextOf(t, nil, nil); out != "Бэклог пуст.\n" {
		t.Fatalf("пусто: %q", out)
	}
	// --parent --next: свободный ребёнок эпика.
	kids := map[string][]row{"CLAB-85": rows(row{key: "CLAB-86", blocked: []string{"CLAB-1"}}, row{key: "CLAB-87"})}
	c, _ := newFakeBacklog(t, serveBacklog(nil, kids, nil))
	out, _, err := capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB", "--parent", "CLAB-85", "--next"})
	})
	if err != nil || !strings.HasPrefix(out, "Следующая: CLAB-87 ") {
		t.Fatalf("--parent --next: %q %v", out, err)
	}
}

func TestBacklogNextJSON(t *testing.T) {
	withJSON(t, true)
	epic := row{key: "CLAB-85", typ: "epic", inWork: true, children: map[string]int{"backlog": 2}}
	kids := map[string][]row{"CLAB-85": rows(row{key: "CLAB-86", blocked: []string{"CLAB-80"}}, row{key: "CLAB-87"})}
	c, _ := newFakeBacklog(t, serveBacklog(rows(epic), kids, nil))
	out, _, err := capture(t, func() error {
		return cmdBacklog(context.Background(), c, []string{"--project", "CLAB", "--next"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Next, Epic   map[string]any
		Skipped      []string
		FirstBlocked map[string]any `json:"first_blocked"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got.Next["key"] != "CLAB-87" || got.Epic["key"] != "CLAB-85" || len(got.Skipped) != 1 || got.FirstBlocked["key"] != "CLAB-86" {
		t.Fatalf("%s", out)
	}
}
