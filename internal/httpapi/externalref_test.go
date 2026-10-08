package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Ссылка на основной трекер через REST (CLAB-95): транспорт только
// перекладывает поле, но отличить «не передано» от `{}` и донести параметр
// списка — его работа, и проверяется она здесь.
func TestExternalRefOverREST(t *testing.T) {
	store, _ := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{StrictGates: true})
	auth := workflow.NewAuth(store, log, workflow.AuthHeader)
	srv := NewServer(config.Config{Env: "test"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), auth, nil)

	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-SDLC-Actor", "user:lead")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	refOf := func(task map[string]any) string {
		raw, _ := json.Marshal(task["external_ref"])
		return string(raw)
	}
	const jira = `{"jira":{"key":"PROJ-12","url":"https://jira.example.com/browse/PROJ-12"}}`

	code, task := call(http.MethodPost, "/v1/projects/SDLC/tasks",
		`{"title":"По тикету","external_ref":`+jira+`}`)
	if code != http.StatusCreated || refOf(task) != jira {
		t.Fatalf("POST: %d %v", code, task)
	}
	key, _ := task["key"].(string)

	if code, got := call(http.MethodGet, "/v1/tasks/"+key, ""); code != http.StatusOK ||
		refOf(got["task"].(map[string]any)) != jira {
		t.Fatalf("GET: %d %v", code, got)
	}

	// PATCH только со ссылкой — правка, а не «ни одного поля».
	next := `{"jira":{"key":"PROJ-13","url":"https://jira.example.com/browse/PROJ-13"}}`
	if code, got := call(http.MethodPatch, "/v1/tasks/"+key, `{"external_ref":`+next+`}`); code != http.StatusOK || refOf(got) != next {
		t.Fatalf("PATCH замена: %d %v", code, got)
	}
	// Без поля — не трогает; null — тоже «не передано».
	if code, got := call(http.MethodPatch, "/v1/tasks/"+key, `{"title":"Новый заголовок"}`); code != http.StatusOK || refOf(got) != next {
		t.Fatalf("PATCH без поля: %d %v", code, got)
	}
	if code, got := call(http.MethodPatch, "/v1/tasks/"+key, `{"title":"Ещё","external_ref":null}`); code != http.StatusOK || refOf(got) != next {
		t.Fatalf("PATCH с null: %d %v", code, got)
	}

	// 400 с понятным текстом; задача после отказа прежняя.
	for _, bad := range []struct{ body, mention string }{
		{`{"external_ref":{"jira":{"key":"P-1","url":"jira.example.com/P-1"}}}`, "jira.example.com/P-1"},
		{`{"external_ref":{"jira":{"key":"P-1","url":"ftp://jira.example.com/P-1"}}}`, "ftp://"},
		{`{"external_ref":{"jira":{"key":"P-1","url":"https://"}}}`, "https://"},
		{`{"external_ref":{"jira":{"key":"","url":"https://jira.example.com/P-1"}}}`, "пустой key"},
		{`{"external_ref":{"Jira":{"key":"P-1","url":"https://jira.example.com/P-1"}}}`, "Jira"},
		{`{"external_ref":{"jira":"P-1"}}`, "jira"},
		{`{"external_ref":"jira:P-1"}`, "объектом"},
		{`{"external_ref":[]}`, "объектом"},
	} {
		code, got := call(http.MethodPatch, "/v1/tasks/"+key, bad.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: код %d %v", bad.body, code, got)
		}
		if body, _ := json.Marshal(got); !strings.Contains(string(body), bad.mention) {
			t.Fatalf("%s: в ответе нет %q: %s", bad.body, bad.mention, body)
		}
	}
	if code, got := call(http.MethodGet, "/v1/tasks/"+key, ""); code != http.StatusOK ||
		refOf(got["task"].(map[string]any)) != next {
		t.Fatalf("после 400 ссылка изменилась: %v", got["task"])
	}

	// Лента несёт правку ссылки с from/to.
	code, timeline := call(http.MethodGet, "/v1/tasks/"+key+"/timeline", "")
	if code != http.StatusOK {
		t.Fatalf("лента: %d", code)
	}
	body, _ := json.Marshal(timeline)
	if !strings.Contains(string(body), `"external_ref":{"from":`) || !strings.Contains(string(body), "PROJ-13") {
		t.Fatalf("в ленте нет changed.external_ref: %s", body)
	}

	// Фильтр списка: ключ в любой системе, регистр, сужение системой и
	// сочетание с остальными параметрами.
	list := func(query string) []string {
		t.Helper()
		code, got := call(http.MethodGet, "/v1/projects/SDLC/tasks?"+query, "")
		if code != http.StatusOK {
			t.Fatalf("%s: %d %v", query, code, got)
		}
		keys := []string{}
		items, _ := got["tasks"].([]any)
		for _, item := range items {
			keys = append(keys, item.(map[string]any)["key"].(string))
		}
		return keys
	}
	only := func(keys []string) bool { return len(keys) == 1 && keys[0] == key }
	for _, q := range []string{"external=PROJ-13", "external=proj-13", "external=jira:PROJ-13",
		"external=PROJ-13&type=task", "external=PROJ-13&status=" + task["status_key"].(string)} {
		if got := list(q); !only(got) {
			t.Fatalf("%s: %v", q, got)
		}
	}
	for _, q := range []string{"external=youtrack:PROJ-13", "external=PROJ-13&type=epic", "external=PROJ-13&parent=SDLC-999"} {
		if got := list(q); len(got) != 0 {
			t.Fatalf("%s нашёл лишнее: %v", q, got)
		}
	}

	// {} снимает ссылку, и фильтр её больше не находит.
	if code, got := call(http.MethodPatch, "/v1/tasks/"+key, `{"external_ref":{}}`); code != http.StatusOK || refOf(got) != `{}` {
		t.Fatalf("PATCH {}: %d %v", code, got)
	}
	if got := list("external=PROJ-13"); len(got) != 0 {
		t.Fatalf("снятая ссылка находится: %v", got)
	}
}
