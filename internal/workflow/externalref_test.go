package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Ссылка на основной трекер (CLAB-95): запись, замена, снятие, лента и поиск
// по ключу тикета. Правила проверки — в движке, одни на REST и MCP.

const jiraRef = `{"jira":{"key":"PROJ-12","url":"https://jira.example.com/browse/PROJ-12"}}`

func rawRef(s string) *json.RawMessage {
	r := json.RawMessage(s)
	return &r
}

func refEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("external_ref не JSON: %s", got)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// Нормализация не ходит в базу: проверяется таблицей, без стека.
func TestNormalizeExternalRef(t *testing.T) {
	ok := []struct{ in, want string }{
		{``, `{}`},
		{`null`, `{}`},
		{`{}`, `{}`},
		{jiraRef, jiraRef},
		{`{"jira":{"key":"  PROJ-12 ","url":" https://jira.example.com/browse/PROJ-12 ","extra":1}}`, jiraRef},
		{`{"youtrack":{"key":"YT-1","url":"http://yt.example.com/issue/YT-1"},"linear_2":{"key":"L-3","url":"HTTPS://linear.example.com/L-3"}}`,
			`{"linear_2":{"key":"L-3","url":"HTTPS://linear.example.com/L-3"},"youtrack":{"key":"YT-1","url":"http://yt.example.com/issue/YT-1"}}`},
	}
	for _, tc := range ok {
		got, err := normalizeExternalRef(json.RawMessage(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if !refEqual(t, got, tc.want) {
			t.Fatalf("%s: %s, ждали %s", tc.in, got, tc.want)
		}
	}

	bad := []struct{ name, in, mention string }{
		{"строка", `"PROJ-12"`, "объектом"},
		{"массив", `[1]`, "объектом"},
		{"значение не объект", `{"jira":"PROJ-12"}`, "jira"},
		{"система с заглавной", `{"Jira":{"key":"P-1","url":"https://x.example.com"}}`, "Jira"},
		{"система с цифры", `{"1c":{"key":"P-1","url":"https://x.example.com"}}`, "1c"},
		{"система длиннее 32", `{"` + strings.Repeat("a", 33) + `":{"key":"P-1","url":"https://x.example.com"}}`, "до 32"},
		{"пустой ключ", `{"jira":{"key":"  ","url":"https://x.example.com"}}`, "пустой key"},
		{"нет ключа", `{"jira":{"url":"https://x.example.com"}}`, "пустой key"},
		{"ключ длиннее 128", `{"jira":{"key":"` + strings.Repeat("k", 129) + `","url":"https://x.example.com"}}`, "128"},
		{"нет URL", `{"jira":{"key":"P-1"}}`, "http"},
		{"URL без схемы", `{"jira":{"key":"P-1","url":"jira.example.com/browse/P-1"}}`, "jira.example.com/browse/P-1"},
		{"ftp", `{"jira":{"key":"P-1","url":"ftp://jira.example.com/P-1"}}`, "ftp://"},
		{"javascript", `{"jira":{"key":"P-1","url":"javascript:alert(1)"}}`, "javascript:"},
		{"без хоста", `{"jira":{"key":"P-1","url":"https:///browse/P-1"}}`, "https:///browse"},
	}
	for _, tc := range bad {
		_, err := normalizeExternalRef(json.RawMessage(tc.in))
		e := apperr.As(err)
		if err == nil || e.Code != apperr.CodeInvalidInput {
			t.Fatalf("%s: ждали 400, получили %v", tc.name, err)
		}
		if !strings.Contains(e.Message, tc.mention) {
			t.Fatalf("%s: в ошибке нет %q: %s", tc.name, tc.mention, e.Message)
		}
	}
}

// Заведение со ссылкой, замена, снятие и правка без поля, которая её не
// трогает. Прежнее значение остаётся в ленте.
func TestExternalRefCreateReplaceClear(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{
			Title: "По тикету", ExternalRef: json.RawMessage(jiraRef),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !refEqual(t, task.ExternalRef, jiraRef) {
		t.Fatalf("ссылка после заведения: %s", task.ExternalRef)
	}
	fresh, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil || !refEqual(t, fresh.ExternalRef, jiraRef) {
		t.Fatalf("ссылка в базе: %v %s", err, fresh.ExternalRef)
	}

	// Задача без ссылки получает пустой объект, а не NULL.
	plain, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Без тикета"},
	})
	if err != nil || !refEqual(t, plain.ExternalRef, `{}`) {
		t.Fatalf("задача без ссылки: %v %s", err, plain.ExternalRef)
	}

	// Правка без поля ссылку не трогает.
	out, err := engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Title: str("Другой заголовок"), Level: domain.MemberMaintainer,
	})
	if err != nil || !refEqual(t, out.ExternalRef, jiraRef) {
		t.Fatalf("правка заголовка задела ссылку: %v %s", err, out.ExternalRef)
	}

	// Только ссылка — тоже правка, а не «не указано ни одно поле».
	next := `{"jira":{"key":"PROJ-13","url":"https://jira.example.com/browse/PROJ-13"},"youtrack":{"key":"YT-7","url":"https://yt.example.com/issue/YT-7"}}`
	out, err = engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		ExternalRef: rawRef(next), Level: domain.MemberMaintainer,
	})
	if err != nil || !refEqual(t, out.ExternalRef, next) {
		t.Fatalf("замена ссылки: %v %s", err, out.ExternalRef)
	}
	if out.Title != "Другой заголовок" {
		t.Fatalf("замена ссылки задела заголовок: %q", out.Title)
	}

	// Замена целиком: прежней youtrack в новом значении нет — её нет и в задаче.
	out, err = engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		ExternalRef: rawRef(jiraRef), Level: domain.MemberMaintainer,
	})
	if err != nil || !refEqual(t, out.ExternalRef, jiraRef) {
		t.Fatalf("замена целиком: %v %s", err, out.ExternalRef)
	}

	out, err = engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		ExternalRef: rawRef(`{}`), Level: domain.MemberMaintainer,
	})
	if err != nil || !refEqual(t, out.ExternalRef, `{}`) {
		t.Fatalf("снятие ссылки: %v %s", err, out.ExternalRef)
	}

	events, err := store.ListEvents(ctx, store.DB(), task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Снятие ссылки — отдельная запись в ленте: fields только external_ref,
	// from — прежняя ссылка, to — пустой объект. Порядок ленты не важен.
	var created *domain.TaskEvent
	cleared := false
	for i := range events {
		switch events[i].Type {
		case domain.EventTaskCreated:
			created = &events[i]
		case domain.EventTaskUpdated:
			var payload struct {
				Fields  []string `json:"fields"`
				Changed struct {
					ExternalRef struct {
						From json.RawMessage `json:"from"`
						To   json.RawMessage `json:"to"`
					} `json:"external_ref"`
				} `json:"changed"`
			}
			if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Fields) == 1 && payload.Fields[0] == "external_ref" &&
				refEqual(t, payload.Changed.ExternalRef.From, jiraRef) &&
				refEqual(t, payload.Changed.ExternalRef.To, `{}`) {
				cleared = true
			}
		}
	}
	if created == nil || !strings.Contains(string(created.Payload), "PROJ-12") {
		t.Fatalf("ссылки нет в task.created: %v", created)
	}
	if !cleared {
		t.Fatal("в ленте нет task.updated со снятием ссылки: from/to external_ref")
	}
}

// Отбитая ссылка не меняет задачу: ни при заведении, ни при правке.
func TestExternalRefRejectedKeepsState(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	_, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{
			Title: "Плохая ссылка", ExternalRef: json.RawMessage(`{"jira":{"key":"P-1","url":"javascript:alert(1)"}}`),
		},
	})
	if apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("заведение с javascript: — %v", err)
	}
	list, err := store.ListTasks(ctx, store.DB(), projectID(t, store), postgres.ListTasksFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range list {
		if task.Title == "Плохая ссылка" {
			t.Fatal("задача с отбитой ссылкой заведена")
		}
	}

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Хорошая", ExternalRef: json.RawMessage(jiraRef)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"PROJ-12"`, `[]`, `{"jira":{"key":"","url":"https://x.example.com"}}`,
		`{"jira":{"key":"P-1","url":"ftp://x.example.com"}}`, `{"JIRA":{"key":"P-1","url":"https://x.example.com"}}`} {
		_, err := engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
			Title: str("Не должно примениться"), ExternalRef: rawRef(bad), Level: domain.MemberMaintainer,
		})
		if apperr.As(err).Code != apperr.CodeInvalidInput {
			t.Fatalf("%s: ждали 400, получили %v", bad, err)
		}
	}
	fresh, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Title != "Хорошая" || !refEqual(t, fresh.ExternalRef, jiraRef) {
		t.Fatalf("отбитая правка изменила задачу: %q %s", fresh.Title, fresh.ExternalRef)
	}
}

// Агент без аренды ссылку не правит — та же рамка, что для заголовка.
func TestExternalRefAgentNeedsLease(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	epic := epicTask(t, ctx, engine, system, "Эпик со ссылкой")
	bot, err := store.ResolveActor(ctx, store.DB(), "orchestrator-bot", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.UpdateTask(ctx, epic.Key, bot.ID, UpdateTaskInput{
		ExternalRef: rawRef(jiraRef), Level: domain.MemberAgent,
	})
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("агент без аренды поправил ссылку: %v", err)
	}

	if _, err := engine.Claim(ctx, epic.Key, bot.ID, "test"); err != nil {
		t.Fatal(err)
	}
	child, err := engine.CreateTask(ctx, "SDLC", bot.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{
			ParentKey: epic.Key, Title: "Кусок по подзадаче", ExternalRef: json.RawMessage(jiraRef),
		},
		Level: domain.MemberAgent,
	})
	if err != nil || !refEqual(t, child.ExternalRef, jiraRef) {
		t.Fatalf("оркестратор не завёл кусок со ссылкой: %v %s", err, child.ExternalRef)
	}
	out, err := engine.UpdateTask(ctx, child.Key, bot.ID, UpdateTaskInput{
		ExternalRef: rawRef(`{}`), Level: domain.MemberAgent,
	})
	if err != nil || !refEqual(t, out.ExternalRef, `{}`) {
		t.Fatalf("держатель эпика снимает ссылку у куска: %v %s", err, out.ExternalRef)
	}
}

// Поиск по ключу тикета: в любой системе, без учёта регистра, с сужением
// системой и вместе с остальными фильтрами. Запись legacy_epic_key запросу
// не мешает.
func TestListTasksByExternalRef(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	pid := projectID(t, store)

	linked, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Со ссылкой", ExternalRef: json.RawMessage(jiraRef)},
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Другая ссылка",
			ExternalRef: json.RawMessage(`{"youtrack":{"key":"PROJ-120","url":"https://yt.example.com/PROJ-120"}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Старый эпик"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(ctx,
		`UPDATE tasks SET external_ref = '{"legacy_epic_key": "PROJ-12"}' WHERE id = $1`, legacy.ID); err != nil {
		t.Fatal(err)
	}

	keys := func(f postgres.ListTasksFilter) []string {
		t.Helper()
		list, err := store.ListTasks(ctx, store.DB(), pid, f)
		if err != nil {
			t.Fatalf("%+v: %v", f, err)
		}
		out := []string{}
		for _, task := range list {
			out = append(out, task.Key)
		}
		return out
	}
	only := func(got []string, want string) bool { return len(got) == 1 && got[0] == want }

	if got := keys(postgres.ListTasksFilter{External: "PROJ-12"}); !only(got, linked.Key) {
		t.Fatalf("PROJ-12: %v", got)
	}
	if got := keys(postgres.ListTasksFilter{External: "proj-12"}); !only(got, linked.Key) {
		t.Fatalf("регистр ключа: %v", got)
	}
	if got := keys(postgres.ListTasksFilter{External: "jira:PROJ-12"}); !only(got, linked.Key) {
		t.Fatalf("jira:PROJ-12: %v", got)
	}
	if got := keys(postgres.ListTasksFilter{External: "youtrack:PROJ-12"}); len(got) != 0 {
		t.Fatalf("youtrack:PROJ-12 нашёл чужое: %v", got)
	}
	if got := keys(postgres.ListTasksFilter{External: "PROJ-120"}); !only(got, other.Key) {
		t.Fatalf("PROJ-120: %v", got)
	}
	if got := keys(postgres.ListTasksFilter{External: "PROJ-12", StatusKey: linked.StatusKey, Type: "task"}); !only(got, linked.Key) {
		t.Fatalf("вместе со status и type: %v", got)
	}
	if got := keys(postgres.ListTasksFilter{External: "PROJ-12", Type: "epic"}); len(got) != 0 {
		t.Fatalf("type=epic сузил не тем: %v", got)
	}
	if got := keys(postgres.ListTasksFilter{External: "PROJ-12", ParentKey: "SDLC-999"}); len(got) != 0 {
		t.Fatalf("parent сузил не тем: %v", got)
	}
	// Без фильтра видны все, включая задачу со старой записью.
	if got := keys(postgres.ListTasksFilter{}); len(got) < 3 {
		t.Fatalf("без фильтра: %v", got)
	}
}
