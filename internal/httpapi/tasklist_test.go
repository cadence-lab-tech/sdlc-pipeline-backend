package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Список задач для плагина Claude (CLAB-327): листание offset, поиск q по
// всему проекту, total выборки, поля статуса из версии цикла, эпик с
// счётом детей по категории и совместимость ответа со старым клиентом.
func TestListTasksPaging(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()
	create := func(title, parent string) string {
		t.Helper()
		body := map[string]any{"title": title}
		if parent != "" {
			body["parent_key"] = parent
		}
		code, task := f.json(http.MethodPost, "/v1/projects/SDLC/tasks", f.root, body)
		if code != http.StatusCreated {
			t.Fatalf("задача %q: %d %v", title, code, task)
		}
		return task["key"].(string)
	}
	setStatus := func(key, status string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, `
			UPDATE tasks t SET status_id = ws.id
			FROM workflow_statuses ws
			WHERE t.key = $1 AND ws.workflow_id = t.workflow_id AND ws.key = $2`, key, status); err != nil {
			t.Fatal(err)
		}
	}
	list := func(query string) (keys []string, total float64, items []map[string]any) {
		t.Helper()
		rec := f.do(http.MethodGet, "/v1/projects/SDLC/tasks?"+query, f.root, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("список %q: %d %s", query, rec.Code, rec.Body)
		}
		var out struct {
			Tasks []map[string]any `json:"tasks"`
			Total *float64         `json:"total"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Total == nil {
			t.Fatalf("в ответе нет total: %s", rec.Body)
		}
		for _, it := range out.Tasks {
			keys = append(keys, it["key"].(string))
		}
		return keys, *out.Total, out.Tasks
	}

	// Одна эпик-подобная задача с тремя детьми и двенадцать обычных.
	epic := create("Импорт данных", "")
	kids := []string{create("Выгрузка CSV", epic), create("Кодировки", epic), create("История", epic)}
	var plain []string
	for i := 0; i < 12; i++ {
		plain = append(plain, create("Задача номер "+string(rune('A'+i)), ""))
	}
	setStatus(kids[0], "done")
	setStatus(kids[1], "cancelled")
	setStatus(plain[0], "analysis")
	setStatus(plain[1], "done")
	setStatus(plain[2], "cancelled")

	// Совместимость: без новых параметров — всё, что было, в порядке цикла, и
	// прежние поля на месте; новые идут после них.
	rec := f.do(http.MethodGet, "/v1/projects/SDLC/tasks", f.root, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var old struct {
		Tasks []struct {
			Key       string `json:"key"`
			Title     string `json:"title"`
			StatusKey string `json:"status_key"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &old); err != nil {
		t.Fatal(err)
	}
	if len(old.Tasks) < 16 {
		t.Fatalf("без параметров вернулось %d задач", len(old.Tasks))
	}
	if first := strings.Index(rec.Body.String(), `"optional_stages"`); first < 0 || first > strings.Index(rec.Body.String(), `"status_category"`) {
		t.Fatalf("новые поля не после полей задачи: %s", rec.Body)
	}

	// Страницы по десять: ключи не повторяются, total — вся выборка.
	all, total, _ := list("limit=500")
	page1, t1, _ := list("limit=10&offset=0")
	page2, t2, _ := list("limit=10&offset=10")
	if len(page1) != 10 || t1 != total || t2 != total {
		t.Fatalf("страница 1: %d, total %v/%v/%v", len(page1), t1, t2, total)
	}
	if !slices.Equal(append(slices.Clone(page1), page2...), all[:len(page1)+len(page2)]) {
		t.Fatalf("страницы не склеиваются в выборку: %v %v %v", page1, page2, all)
	}
	if past, tt, _ := list("limit=10&offset=1000"); len(past) != 0 || tt != total {
		t.Fatalf("за концом: %v %v", past, tt)
	}

	// Поиск q: часть ключа и слово заголовка без учёта регистра, по всему
	// проекту, а не по странице; «%» и «_» — обычные буквы.
	if got, n, _ := list("q=" + url.QueryEscape(strings.ToLower(kids[1]))); !slices.Equal(got, []string{kids[1]}) || n != 1 {
		t.Fatalf("поиск по ключу: %v %v", got, n)
	}
	if got, n, _ := list("q=" + url.QueryEscape("кодировки")); !slices.Equal(got, []string{kids[1]}) || n != 1 {
		t.Fatalf("поиск по слову: %v %v", got, n)
	}
	if got, n, _ := list("q=" + url.QueryEscape("номер")); len(got) != 12 || n != 12 {
		t.Fatalf("поиск «номер»: %d %v", len(got), n)
	}
	if got, n, _ := list("q=" + url.QueryEscape("номер") + "&limit=5&offset=10"); len(got) != 2 || n != 12 {
		t.Fatalf("страница поиска: %d %v", len(got), n)
	}
	for _, wild := range []string{"%", "_", "Задача%"} {
		if got, n, _ := list("q=" + url.QueryEscape(wild)); len(got) != 0 || n != 0 {
			t.Fatalf("q=%q сработал как шаблон: %v", wild, got)
		}
	}

	// Сегменты по полям статуса из версии цикла.
	has := func(keys []string, want ...string) bool {
		for _, k := range want {
			if !slices.Contains(keys, k) {
				return false
			}
		}
		return true
	}
	work, wn, _ := list("top_level=true&terminal=false&has_role=true&limit=500")
	if !has(work, plain[0]) || wn != float64(len(work)) || has(work, plain[1], plain[2], epic, plain[3]) {
		t.Fatalf("в работе: %v", work)
	}
	backlog, _, _ := list("top_level=true&terminal=false&has_role=false&limit=500")
	if !has(backlog, epic, plain[3]) || has(backlog, plain[0], plain[1], plain[2]) || has(backlog, kids...) {
		t.Fatalf("бэклог: %v", backlog)
	}
	done, dn, _ := list("top_level=true&terminal=true&category=done&limit=500")
	if !slices.Equal(done, []string{plain[1]}) || dn != 1 {
		t.Fatalf("готово: %v", done)
	}
	// Дети в сегменты верхнего уровня не попадают, но находятся поиском.
	if got, _, _ := list("q=" + url.QueryEscape(kids[0])); !slices.Equal(got, []string{kids[0]}) {
		t.Fatalf("ребёнок по ключу: %v", got)
	}
	if cat, _, _ := list("category=cancelled&limit=500"); !has(cat, kids[1], plain[2]) || has(cat, plain[1]) {
		t.Fatalf("категория cancelled: %v", cat)
	}

	// Исполнитель: по хэндлу и «без исполнителя».
	var handle string
	if err := f.pool.QueryRow(ctx, `
		UPDATE tasks SET assignee_id = p.id FROM (SELECT id, handle FROM principals WHERE kind = 'agent' LIMIT 1) p
		WHERE tasks.key = $1 RETURNING p.handle`, plain[3]).Scan(&handle); err != nil {
		t.Fatal(err)
	}
	if got, n, _ := list("assignee=" + url.QueryEscape(handle)); !slices.Equal(got, []string{plain[3]}) || n != 1 {
		t.Fatalf("исполнитель %q: %v %v", handle, got, n)
	}
	if got, _, _ := list("unassigned=true&limit=500"); slices.Contains(got, plain[3]) || len(got) < 10 {
		t.Fatalf("без исполнителя: %v", got)
	}

	// Порядок «обновлены недавно».
	setStatus(plain[5], "new")
	if _, err := f.pool.Exec(ctx, `UPDATE tasks SET updated_at = now() + interval '1 hour' WHERE key = $1`, plain[5]); err != nil {
		t.Fatal(err)
	}
	if recent, _, _ := list("order=updated&limit=3"); recent[0] != plain[5] {
		t.Fatalf("недавно обновлённые: %v", recent)
	}

	// Эпик: дети по категории статуса; задача без детей поля не несёт.
	_, _, items := list("q=" + url.QueryEscape("Импорт"))
	if len(items) != 1 {
		t.Fatalf("эпик: %v", items)
	}
	counts, _ := json.Marshal(items[0]["child_counts"])
	if string(counts) != `{"backlog":1,"cancelled":1,"done":1}` || items[0]["status_category"] != "backlog" {
		t.Fatalf("child_counts %s, категория %v", counts, items[0]["status_category"])
	}
	if _, _, one := list("q=" + url.QueryEscape("Выгрузка")); one[0]["status_category"] != "done" || one[0]["child_counts"] != nil {
		t.Fatalf("задача без детей: %v", one[0])
	}

	// Дети нескольких эпиков одним запросом.
	other := create("Второй эпик", "")
	create("Ребёнок второго", other)
	if got, n, _ := list("parent=" + url.QueryEscape(epic+","+other) + "&limit=100"); len(got) != 4 || n != 4 {
		t.Fatalf("дети двух эпиков: %v %v", got, n)
	}

	// Неверные значения — 400, а не молчаливое «все».
	for _, bad := range []string{"terminal=maybe", "unassigned=2x", "has_role=2x", "top_level=yes", "order=random"} {
		if rec := f.do(http.MethodGet, "/v1/projects/SDLC/tasks?"+bad, f.root, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
}
