package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Правило «ждёт апрува» на таблицах гейтов (CLAB-324, AC-1): то же, что
// awaitsApproval карточки интерфейса.
func TestAwaitsApprovalRule(t *testing.T) {
	gate := func(typ, status string, blocking bool) GateOutcome {
		return GateOutcome{GateKey: typ + status, Type: typ, Status: status, Blocking: blocking}
	}
	approval := func(status string) GateOutcome { return gate(domain.GateHumanApproval, status, true) }
	other := func(status string) GateOutcome { return gate("artifacts_required", status, true) }

	cases := []struct {
		name string
		ts   []TransitionPreview
		want bool
	}{
		{"всё прошло", []TransitionPreview{{Kind: "advance", Gates: []GateOutcome{other("passed"), approval("passed")}}}, false},
		{"ждёт апрува", []TransitionPreview{{Kind: "advance", Gates: []GateOutcome{other("passed"), gate("checklist", "waived", true), gate("drift_clear", "skipped", true), approval("pending")}}}, true},
		{"апрув ждёт, но другой гейт не прошёл", []TransitionPreview{{Kind: "advance", Gates: []GateOutcome{other("failed"), approval("pending")}}}, false},
		{"несколько переходов вперёд", []TransitionPreview{
			{Kind: "advance", Gates: []GateOutcome{other("pending"), approval("pending")}},
			{Kind: "advance", Gates: []GateOutcome{other("passed"), approval("pending")}},
		}, true},
		{"нет переходов", nil, false},
		{"апрув на возврате не считается", []TransitionPreview{{Kind: "reject", Gates: []GateOutcome{approval("pending")}}}, false},
		{"неблокирующий провал не мешает", []TransitionPreview{{Kind: "advance", Gates: []GateOutcome{gate("drift_clear", "failed", false), approval("pending")}}}, true},
		{"неблокирующий апрув не держит", []TransitionPreview{{Kind: "advance", Gates: []GateOutcome{gate(domain.GateHumanApproval, "pending", false)}}}, false},
	}
	for _, c := range cases {
		if got := TransitionsAwaitApproval(c.ts); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Дети в карточке родителя (AC-7а): категория статуса, «ждёт апрува» тем же
// правилом, что предпросмотр самого ребёнка, и входящие blocks по порядку.
// Дети на двух циклах, один отменён.
func TestChildCards(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	human, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}

	epic := epicTask(t, ctx, engine, system, "Родитель")
	child := func(title, typ string) domain.Task {
		t.Helper()
		c, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
			CreateTaskInput: postgres.CreateTaskInput{ParentKey: epic.Key, TaskType: typ, Title: title},
		})
		if err != nil {
			t.Fatalf("%s: %v", title, err)
		}
		return c
	}
	waiting := child("Ждёт апрува", "")
	working := child("В работе", "")
	backlog := child("Не начат", "")
	cancelled := child("Отменён", "")
	nested := child("Вложенный эпик", "epic")

	for _, c := range []domain.Task{waiting, working} {
		if res, err := engine.Execute(ctx, c.Key, "start", system.ID, ""); err != nil || res.Blocked {
			t.Fatalf("start %s: %v %+v", c.Key, err, res.Gates)
		}
	}
	if _, err := engine.Execute(ctx, cancelled.Key, "cancel", system.ID, "не нужна"); err != nil {
		t.Fatal(err)
	}
	if res, err := engine.Execute(ctx, nested.Key, "start", system.ID, ""); err != nil || res.Blocked {
		t.Fatalf("start %s: %v", nested.Key, err)
	}

	// Всё блокирующее на переходах вперёд, кроме апрува, человек снял: задача
	// ждёт только апрува.
	task, err := store.GetTaskByKey(ctx, store.DB(), waiting.Key)
	if err != nil {
		t.Fatal(err)
	}
	previews, err := engine.Preview(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range previews {
		if p.Kind != "advance" {
			continue
		}
		for _, g := range p.Gates {
			if g.Blocking && g.Type != domain.GateHumanApproval && !g.Passed() {
				if _, err := engine.DecideGate(ctx, waiting.Key, p.Key, g.GateKey, human.ID, nil, domain.GateResultWaived, nil); err != nil {
					t.Fatalf("снять %s/%s: %v", p.Key, g.GateKey, err)
				}
			}
		}
	}

	// Порядок: «Не начат» после «Ждёт апрува» и «В работе»; заведены в
	// обратном порядке — ответ по порядку заведения задач.
	for _, from := range []domain.Task{working, waiting} {
		if _, err := engine.LinkTasks(ctx, from.Key, backlog.Key, domain.TaskLinkBlocks, "", system.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	// Связь другого вида в blocked_by не попадает.
	if _, err := engine.LinkTasks(ctx, cancelled.Key, working.Key, domain.TaskLinkRelatesTo, "", system.ID, false); err != nil {
		t.Fatal(err)
	}

	cards, err := engine.ChildCards(ctx, store.DB(), epic.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		key, category string
		awaiting      bool
		blockedBy     []string
	}{
		{waiting.Key, "active", true, []string{}},
		{working.Key, "active", false, []string{}},
		{backlog.Key, "backlog", false, []string{waiting.Key, working.Key}},
		{cancelled.Key, "cancelled", false, []string{}},
		{nested.Key, "active", false, []string{}},
	}
	if len(cards) != len(want) {
		t.Fatalf("детей %d, want %d", len(cards), len(want))
	}
	for i, w := range want {
		c := cards[i]
		if c.Key != w.key || c.StatusCategory != w.category || c.AwaitingApproval != w.awaiting ||
			fmt.Sprint(c.BlockedBy) != fmt.Sprint(w.blockedBy) || c.BlockedBy == nil {
			t.Errorf("ребёнок %d: %s %s awaiting=%v blocked_by=%v, want %+v", i, c.Key, c.StatusCategory, c.AwaitingApproval, c.BlockedBy, w)
		}
	}
	if cards[4].WorkflowID == cards[0].WorkflowID {
		t.Fatal("дети должны быть на двух циклах")
	}

	// «Ждёт апрува» ребёнка — то же, что предпросмотр его самого.
	for _, c := range cards {
		task, err := store.GetTaskByKey(ctx, store.DB(), c.Key)
		if err != nil {
			t.Fatal(err)
		}
		previews, err := engine.Preview(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		inWork := c.StatusCategory == "active" || c.StatusCategory == "waiting"
		if got := inWork && TransitionsAwaitApproval(previews); got != c.AwaitingApproval {
			t.Errorf("%s: в карточке родителя %v, по предпросмотру %v", c.Key, c.AwaitingApproval, got)
		}
	}

	// Апрув поставлен — ребёнок больше не ждёт.
	for _, p := range previews {
		for _, g := range p.Gates {
			if p.Kind == "advance" && g.Type == domain.GateHumanApproval {
				if _, err := engine.DecideGate(ctx, waiting.Key, p.Key, g.GateKey, human.ID, nil, domain.GateResultPassed, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	cards, err = engine.ChildCards(ctx, store.DB(), epic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cards[0].AwaitingApproval {
		t.Error("после апрува ребёнок всё ещё ждёт апрува")
	}

	// Без детей — пустой список, не null.
	cards, err = engine.ChildCards(ctx, store.DB(), backlog.ID)
	if err != nil || cards == nil || len(cards) != 0 {
		t.Fatalf("без детей: %v %v", cards, err)
	}
}

// Карточка эпика на 30 детях (AC-7в): два запроса на всех детей и
// вычисление гейтов только у детей в работе на переходах с апрувом. Время —
// в журнал теста, его переносит разбор изменений.
func TestChildCardsThirtyChildren(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	epic := epicTask(t, ctx, engine, system, "Тридцать детей")
	for i := 0; i < 30; i++ {
		c, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
			CreateTaskInput: postgres.CreateTaskInput{ParentKey: epic.Key, Title: fmt.Sprintf("Ребёнок %d", i)},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Две трети детей в работе: у них гейты апрува вычисляются.
		if i%3 != 0 {
			if _, err := engine.Execute(ctx, c.Key, "start", system.ID, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Прогрев: первый вызов платит за планы запросов.
	if _, err := engine.ChildCards(ctx, store.DB(), epic.ID); err != nil {
		t.Fatal(err)
	}
	const rounds = 5
	started := time.Now()
	for i := 0; i < rounds; i++ {
		cards, err := engine.ChildCards(ctx, store.DB(), epic.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(cards) != 30 {
			t.Fatalf("детей %d", len(cards))
		}
	}
	per := time.Since(started) / rounds
	t.Logf("ChildCards на 30 детях (20 в работе): %v на ответ", per)
	if per > 2*time.Second {
		t.Fatalf("карточка эпика на 30 детях считается %v", per)
	}
}
