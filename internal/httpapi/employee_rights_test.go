package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Право сотрудника на работу с задачей (CLAB-411, эпик CLAB-404): тест-
// таблица K-3 «класс актора × действие» по REST. Каждая ячейка — ожидаемый
// код реальной ручки и прогноз actor_rights карточки той же задачи тем же
// токеном: прогноз и исход совпадают, у отказа — тот же код ошибки (AC-17).
// Классы: сотрудник (user уровня agent), сопровождающий, бот (agent:bot
// фикстуры с api-токеном), токен задачи на своей и чужой задаче, viewer.
func TestEmployeeRightsTable(t *testing.T) {
	f := newAuditFixture(t)
	f.activateSkipCycle()
	employee := f.memberToken("user:anna", domain.MemberAgent, nil)
	lead := f.memberToken("user:lead", domain.MemberMaintainer, nil)
	viewer := f.memberToken("user:watcher", domain.MemberViewer, nil)
	const backend, docs = "cadence-lab-tech/sdlc-pipeline-backend", "cadence-lab-tech/sdlc-pipeline-docs"

	create := func(token string, body map[string]any) (int, map[string]any) {
		t.Helper()
		if _, ok := body["title"]; !ok {
			body["title"] = "Задача"
		}
		return f.json(http.MethodPost, "/v1/projects/SDLC/tasks", token, body)
	}
	newTask := func(body map[string]any) string {
		t.Helper()
		code, created := create(f.root, body)
		if code != http.StatusCreated {
			t.Fatalf("заведение: %d %v", code, created)
		}
		return created["key"].(string)
	}
	move := func(key, tr string) {
		t.Helper()
		if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/transitions/"+tr, f.root, map[string]any{}); code != http.StatusOK {
			t.Fatalf("%s %s: %d %v", key, tr, code, body)
		}
	}
	taskToken := func(key, holder string) string {
		t.Helper()
		if code, body := f.json(http.MethodPost, "/v1/tasks/"+key+"/claim", holder, map[string]any{}); code != http.StatusOK {
			t.Fatalf("захват %s: %d %v", key, code, body)
		}
		code, issued := f.json(http.MethodPost, "/v1/tasks/"+key+"/task-token", holder, nil)
		if code != http.StatusCreated {
			t.Fatalf("токен задачи %s: %d %v", key, code, issued)
		}
		return issued["token"].(string)
	}

	// P — своя задача токена задачи (держит сопровождающий), C — её ребёнок;
	// O — чужая задача без аренды; X — ещё одна чужая; D — закрытая; B —
	// задача, этап которой держит бот.
	single := map[string]any{"repositories": []string{backend}}
	multi := func() map[string]any {
		return map[string]any{"repositories": []string{backend}, "properties": map[string]any{"multi_repo": true}}
	}
	P := newTask(map[string]any{"title": "Своя", "repositories": []string{backend}})
	move(P, "start")
	tt := taskToken(P, lead)
	C := newTask(map[string]any{"title": "Ребёнок своей", "parent_key": P, "repositories": []string{backend}})
	O := newTask(multi())
	X := newTask(single)
	D := newTask(single)
	move(D, "start")
	move(D, "cancel")
	B := newTask(single)
	move(B, "start")
	taskToken(B, f.bot)

	type actor struct{ name, token string }
	emp, mnt, bot, tok, view := actor{"сотрудник", employee}, actor{"сопровождающий", lead},
		actor{"бот", f.bot}, actor{"токен задачи", tt}, actor{"viewer", viewer}

	edit := func(key string) func(string) (int, map[string]any) {
		return func(token string) (int, map[string]any) {
			return f.json(http.MethodPatch, "/v1/tasks/"+key, token, map[string]any{"description": "правка"})
		}
	}
	choice := func(key string, stages map[string]any) func(string) (int, map[string]any) {
		return func(token string) (int, map[string]any) {
			return f.json(http.MethodPatch, "/v1/tasks/"+key, token, map[string]any{"optional_stages": stages})
		}
	}
	child := func(parent string, repo string) func(string) (int, map[string]any) {
		return func(token string) (int, map[string]any) {
			return create(token, map[string]any{"title": "Ребёнок", "parent_key": parent, "repositories": []string{repo}})
		}
	}
	top := func(token string) (int, map[string]any) {
		return create(token, map[string]any{"title": "Бэклог", "repositories": []string{backend}})
	}
	assign := func(key string) func(string) (int, map[string]any) {
		return func(token string) (int, map[string]any) {
			return f.json(http.MethodPut, "/v1/tasks/"+key+"/assignee", token, map[string]any{"assignee": ""})
		}
	}
	link := func(from, rel, to string) func(string) (int, map[string]any) {
		return func(token string) (int, map[string]any) {
			return f.json(http.MethodPost, "/v1/tasks/"+from+"/links", token, map[string]any{"task": to, "rel": rel})
		}
	}
	scope := func(key, op string) func(string) (int, map[string]any) {
		return func(token string) (int, map[string]any) {
			return f.json(http.MethodPatch, "/v1/tasks/"+key+"/scope", token,
				map[string]any{op + "_repositories": []string{docs}, "note": "по таблице"})
		}
	}
	props := func(key string) func(string) (int, map[string]any) {
		return func(token string) (int, map[string]any) {
			return f.json(http.MethodPatch, "/v1/tasks/"+key+"/scope", token,
				map[string]any{"properties": map[string]any{"multi_repo": true}})
		}
	}

	type cell struct {
		who    actor
		action string
		// Задача, чью карточку спрашивают о прогнозе, и поле actor_rights;
		// пусто — у действия нет поля в карточке.
		card, field string
		do          func(string) (int, map[string]any)
		want        int
	}
	const (
		ok, created, forbidden, conflict = http.StatusOK, http.StatusCreated, http.StatusForbidden, http.StatusConflict
	)
	var cells []cell
	add := func(action, card, field string, do func(string) (int, map[string]any), want map[actor]int) {
		for _, who := range []actor{emp, mnt, bot, tok, view} {
			if code, ok := want[who]; ok {
				cells = append(cells, cell{who, action, card, field, do, code})
			}
		}
	}
	add("правка своей задачи токена", P, "edit", edit(P), map[actor]int{emp: ok, mnt: ok, bot: forbidden, tok: ok, view: forbidden})
	add("правка чужой задачи", O, "edit", edit(O), map[actor]int{emp: ok, mnt: ok, bot: forbidden, tok: forbidden, view: forbidden})
	add("правка закрытой", D, "edit", edit(D), map[actor]int{emp: ok, mnt: ok, bot: forbidden, tok: forbidden, view: forbidden})
	add("правка задачи бота", B, "edit", edit(B), map[actor]int{bot: ok})
	add("выбор этапов своей задачи токена", P, "stage_choice", choice(P, map[string]any{"design": true}),
		map[actor]int{emp: ok, mnt: ok, bot: forbidden, tok: forbidden, view: forbidden})
	add("выбор этапов ребёнка своей", C, "stage_choice", choice(C, map[string]any{"design": true}),
		map[actor]int{emp: ok, tok: ok, bot: forbidden})
	add("выбор этапов закрытой", D, "stage_choice", choice(D, map[string]any{"design": true}),
		map[actor]int{emp: conflict, mnt: conflict, bot: forbidden, tok: forbidden, view: forbidden})
	add("заведение без parent", "", "", top, map[actor]int{emp: created, mnt: created, bot: forbidden, tok: forbidden, view: forbidden})
	add("ребёнок своей в области", P, "create_child", child(P, "backend"),
		map[actor]int{emp: created, mnt: created, bot: forbidden, tok: created, view: forbidden})
	add("ребёнок своей вне области", "", "", child(P, docs),
		map[actor]int{emp: created, mnt: created, bot: forbidden, tok: conflict})
	add("ребёнок чужой", O, "create_child", child(O, "backend"), map[actor]int{emp: created, tok: forbidden, bot: forbidden})
	add("ребёнок задачи бота вне области", "", "", child(B, docs), map[actor]int{bot: conflict, emp: created})
	add("ребёнок задачи бота в области", B, "create_child", child(B, "backend"), map[actor]int{bot: created})
	add("назначение чужой", O, "assign", assign(O), map[actor]int{emp: ok, mnt: ok, bot: ok, tok: forbidden, view: forbidden})
	add("назначение своей", C, "assign", assign(C), map[actor]int{tok: ok, emp: ok})
	add("связь обычная", O, "link", link(O, domain.TaskLinkRelatesTo, X),
		map[actor]int{emp: ok, mnt: ok, bot: ok, tok: forbidden, view: forbidden})
	add("своя blocks чужую", P, "link", link(P, domain.TaskLinkBlocks, O), map[actor]int{tok: ok, emp: ok})
	add("чужая blocks свою", O, "link", link(O, domain.TaskLinkBlocks, P), map[actor]int{tok: forbidden, emp: ok})
	// Прогноз карточки — связь от задачи с любой задачей проекта; «чужая
	// relates_to своя» токену задачи открыта парой задач (RequireLink), и
	// прогноза у такой ячейки нет.
	add("чужая relates_to свою", "", "", link(X, domain.TaskLinkRelatesTo, P), map[actor]int{tok: ok})
	for _, who := range []actor{emp, mnt} {
		add("расширение области", O, "scope", scope(O, "add"), map[actor]int{who: ok})
		add("сужение области", O, "scope", scope(O, "remove"), map[actor]int{who: ok})
	}
	add("расширение области", O, "scope", scope(O, "add"), map[actor]int{bot: forbidden, tok: forbidden, view: forbidden})
	add("сужение области", O, "scope", scope(O, "remove"), map[actor]int{bot: forbidden, tok: forbidden, view: forbidden})
	add("свойства без репозиториев", "", "", props(O), map[actor]int{emp: ok, mnt: ok, bot: ok, tok: forbidden, view: forbidden})

	predictedCells := 0
	for _, c := range cells {
		if c.field != "" {
			predictedCells++
		}
	}
	t.Logf("ячеек %d, с прогнозом %d", len(cells), predictedCells)
	for _, c := range cells {
		name := fmt.Sprintf("%s: %s", c.who.name, c.action)
		var predicted bool
		var deny map[string]any
		if c.field != "" {
			code, card := f.json(http.MethodGet, "/v1/tasks/"+c.card, c.who.token, nil)
			if code != http.StatusOK {
				t.Fatalf("%s: карточка %d %v", name, code, card)
			}
			rights, _ := card["actor_rights"].(map[string]any)
			predicted, _ = rights[c.field].(bool)
			if all, _ := rights["actor_deny"].(map[string]any); all != nil {
				deny, _ = all[c.field].(map[string]any)
			}
		}
		code, body := c.do(c.who.token)
		if code != c.want {
			t.Errorf("%s: %d, ждали %d: %v", name, code, c.want, body)
			continue
		}
		if c.field == "" {
			continue
		}
		if predicted != (code < 300) {
			t.Errorf("%s: прогноз %s=%v, ответ %d", name, c.field, predicted, code)
		}
		if !predicted && (deny == nil || deny["code"] != errCode(body) || deny["message"] == "") {
			t.Errorf("%s: actor_deny %v, ответ %v", name, deny, body)
		}
		if predicted && deny != nil {
			t.Errorf("%s: разрешено, а actor_deny %v", name, deny)
		}
	}

	// K-4: закрытая задача — stage_choice=false даже сопровождающему, код и
	// текст — те же, что у PATCH; правка разрешена.
	_, card := f.json(http.MethodGet, "/v1/tasks/"+D, lead, nil)
	rights := card["actor_rights"].(map[string]any)
	_, refusal := choice(D, map[string]any{"design": true})(lead)
	deny := rights["actor_deny"].(map[string]any)["stage_choice"].(map[string]any)
	msg, _ := errOf(refusal)
	if rights["edit"] != true || rights["stage_choice"] != false || deny["code"] != "conflict" ||
		deny["message"] != msg || !strings.Contains(msg, "выбор этапов у неё не меняется") {
		t.Fatalf("закрытая задача: %v, ответ %v", rights, refusal)
	}
	// Бот без аренды: причина отказа правки — lease.
	_, card = f.json(http.MethodGet, "/v1/tasks/"+O, f.bot, nil)
	if d := card["actor_rights"].(map[string]any)["actor_deny"].(map[string]any)["edit"].(map[string]any); d["reason"] != workflow.DenyLease ||
		!strings.Contains(d["message"].(string), "держите не вы") {
		t.Fatalf("отказ боту: %v", d)
	}
	// Токен задачи на области — отказ исполнителю (ExecutorRefusal).
	if _, body := scope(O, "add")(tt); !executorRefused(body) {
		t.Fatalf("область токеном задачи: %v", body)
	}
}
