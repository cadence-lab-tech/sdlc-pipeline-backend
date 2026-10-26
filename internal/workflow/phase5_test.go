package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Оркестрация (решения 0024-0026) по настоящей схеме: эпик идёт своим циклом,
// гейт зависимостей читает дерево задач, агент ограничен своей задачей,
// назначение адресное, стоимость складывается по прогонам.

// epicTask заводит эпик и доводит его до статуса, на котором работает
// оркестратор: цикл эпиков в сиде начинается с планирования.
func epicTask(t *testing.T, ctx context.Context, engine *Engine, actor domain.Principal, title string) domain.Task {
	t.Helper()
	epic, err := engine.CreateTask(ctx, "SDLC", actor.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{TaskType: "epic", Title: title},
	})
	if err != nil {
		t.Fatalf("эпик: %v", err)
	}
	if epic.Type() != "epic" {
		t.Fatalf("тип задачи %q вместо epic", epic.Type())
	}
	res, err := engine.Execute(ctx, epic.Key, "start", actor.ID, "")
	if err != nil {
		t.Fatalf("отдать оркестратору: %v", err)
	}
	if res.Blocked {
		t.Fatalf("переход заблокирован: %+v", res.Gates)
	}
	return res.Task
}

func TestEpicRunsItsOwnWorkflow(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	epic := epicTask(t, ctx, engine, system, "Оркестрация")
	if epic.StatusKey != "decomposition" {
		t.Fatalf("эпик встал на %q, а не на decomposition", epic.StatusKey)
	}

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Обычная задача"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Разные типы — разные циклы: это и есть смысл task_types.
	if task.WorkflowID == epic.WorkflowID {
		t.Fatal("задача и эпик оказались на одном цикле")
	}

	// Тип, которого проект не объявлял, не заводится: набор типов — данные.
	_, err = engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{TaskType: "spike", Title: "Неизвестный тип"},
	})
	if apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("неизвестный тип должен отбиваться: %v", err)
	}

	// Ребёнок находится по родителю, а не по метке в свойствах.
	child, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{ParentKey: epic.Key, Title: "Кусок эпика"},
	})
	if err != nil {
		t.Fatal(err)
	}
	children, err := store.ChildTasks(ctx, store.DB(), epic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Key != child.Key {
		t.Fatalf("дети эпика: %+v", children)
	}
	if child.ParentKey == nil || *child.ParentKey != epic.Key {
		t.Fatalf("у ребёнка не проставлен родитель: %+v", child.ParentKey)
	}
}

func TestDependenciesClosedHoldsEpicUntilChildrenClose(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	epic := epicTask(t, ctx, engine, system, "Эпик с ребёнком")
	child, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{ParentKey: epic.Key, Title: "Единственный кусок"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Разбивка сдана и утверждена — эпик уходит ждать детей.
	if _, err := engine.SubmitArtifact(ctx, epic.Key, system.ID, SubmitArtifactInput{
		Kind: "planning.breakdown", Title: "Разбивка", Format: "markdown",
		Content: []byte("# Один кусок"),
	}); err != nil {
		t.Fatal(err)
	}
	// Апрув ставит человек, а не система: разбивку смотрит тот, кто отвечает
	// за границы задач.
	human, err := store.ResolveActor(ctx, store.DB(), "lead", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.DecideGate(ctx, epic.Key, "breakdown_done", "human_approval",
		human.ID, nil, domain.GateResultPassed, nil); err != nil {
		t.Fatal(err)
	}
	res, err := engine.Execute(ctx, epic.Key, "breakdown_done", system.ID, "")
	if err != nil || res.Blocked {
		t.Fatalf("разбивка не пропущена: %v %+v", err, res.Gates)
	}

	// Ребёнок открыт — эпик закрывать нельзя, и в объяснении назван он.
	outcome := gateOutcome(t, ctx, engine, res.Task, "children_done", "children")
	if outcome.Status != domain.GateResultFailed || !strings.Contains(outcome.Reason, child.Key) {
		t.Fatalf("гейт обязан держать эпик из-за %s: %+v", child.Key, outcome)
	}

	// Ребёнок снят — эпик закрывается: allow_cancelled в сиде включён.
	if _, err := engine.Execute(ctx, child.Key, "cancel", system.ID, "не нужен"); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTaskByKey(ctx, store.DB(), epic.Key)
	if err != nil {
		t.Fatal(err)
	}
	outcome = gateOutcome(t, ctx, engine, task, "children_done", "children")
	if outcome.Status != domain.GateResultPassed {
		t.Fatalf("после закрытия детей гейт обязан проходить: %+v", outcome)
	}
}

func gateOutcome(t *testing.T, ctx context.Context, engine *Engine, task domain.Task, transition, gate string) GateOutcome {
	t.Helper()
	previews, err := engine.Preview(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range previews {
		if p.Key != transition {
			continue
		}
		for _, g := range p.Gates {
			if g.GateKey == gate {
				return g
			}
		}
	}
	t.Fatalf("гейт %q на переходе %q не найден: %+v", gate, transition, previews)
	return GateOutcome{}
}

func TestAgentCreatesOnlyChildrenOfHeldTask(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	epic := epicTask(t, ctx, engine, system, "Эпик под агентом")
	bot, err := store.ResolveActor(ctx, store.DB(), "orchestrator-bot", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}

	// Пока эпик не забран, агенту заводить в нём нечего.
	_, err = engine.CreateTask(ctx, "SDLC", bot.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{ParentKey: epic.Key, Title: "Рано"},
		Level:           domain.MemberAgent,
	})
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("незахваченный эпик нарезать нельзя: %v", err)
	}

	if _, err := engine.Claim(ctx, epic.Key, bot.ID, "test"); err != nil {
		t.Fatal(err)
	}

	// Задача без родителя — это уже не нарезка своего эпика.
	_, err = engine.CreateTask(ctx, "SDLC", bot.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Сама по себе"},
		Level:           domain.MemberAgent,
	})
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("задача без родителя агенту недоступна: %v", err)
	}

	// Область эпика пуста, значит и расширять нечего: репозиторий вне области.
	_, err = engine.CreateTask(ctx, "SDLC", bot.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{
			ParentKey: epic.Key, Title: "Вне области", Repositories: []string{"backend"},
		},
		Level: domain.MemberAgent,
	})
	if apperr.As(err).Code != apperr.CodeScopeExpansionForbidden {
		t.Fatalf("расширение области агентом должно отбиваться: %v", err)
	}

	child, err := engine.CreateTask(ctx, "SDLC", bot.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{ParentKey: epic.Key, Title: "Свой кусок"},
		AssigneeHandle:  "orchestrator-bot",
		Level:           domain.MemberAgent,
	})
	if err != nil {
		t.Fatalf("нарезка своего эпика: %v", err)
	}
	if child.AssigneeID == nil || *child.AssigneeID != bot.ID {
		t.Fatalf("назначение не проставлено: %+v", child.AssigneeID)
	}
}

func TestAssignedTaskIsClaimedOnlyByAssignee(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	mine, err := store.ResolveActor(ctx, store.DB(), "runner-a", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.ResolveActor(ctx, store.DB(), "runner-b", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Назначенная"},
		AssigneeHandle:  "runner-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Задача стоит на начальном статусе без роли: доводим её до аналитики,
	// где исполнитель есть.
	if _, err := engine.Execute(ctx, task.Key, "start", system.ID, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.Claim(ctx, task.Key, other.ID, "чужой"); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("чужую назначенную задачу забирать нельзя: %v", err)
	}
	if _, err := engine.Claim(ctx, task.Key, mine.ID, "свой"); err != nil {
		t.Fatalf("назначенный обязан забрать свою задачу: %v", err)
	}

	// Снятое назначение возвращает задачу в общий доступ.
	if _, err := engine.Assign(ctx, task.Key, "", system.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.AssigneeID != nil {
		t.Fatalf("назначение не снято: %+v", fresh.AssigneeID)
	}
}

func TestUsageSumsUpOverSubtree(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	epic := epicTask(t, ctx, engine, system, "Эпик со счётом")
	bot, err := store.ResolveActor(ctx, store.DB(), "usage-bot", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Claim(ctx, epic.Key, bot.ID, "test"); err != nil {
		t.Fatal(err)
	}

	cost := 0.75
	if _, err := engine.ReportUsage(ctx, epic.Key, bot.ID, ReportUsageInput{
		Model: "claude-opus-5", CostUSD: &cost,
	}); err != nil {
		t.Fatalf("отчёт о стоимости: %v", err)
	}
	// Отчитаться о чужом прогоне нельзя: стоимость привязана к тому, кто работал.
	if _, err := engine.ReportUsage(ctx, epic.Key, system.ID, ReportUsageInput{
		Model: "claude-haiku-4-5", CostUSD: &cost,
	}); err == nil {
		t.Fatal("чужой прогон должен отбиваться")
	}
	// Модель обязательна: отчёт без неё не с чем сравнивать.
	if _, err := engine.ReportUsage(ctx, epic.Key, bot.ID, ReportUsageInput{CostUSD: &cost}); err == nil {
		t.Fatal("отчёт без модели должен отбиваться")
	}

	usage, err := engine.TaskUsage(ctx, epic.Key)
	if err != nil {
		t.Fatal(err)
	}
	if usage.CostUSD != cost || usage.ReportedRuns != 1 {
		t.Fatalf("стоимость эпика: %+v", usage)
	}
	// «Без учёта» — только забранные прогоны без строки (CLAB-219): прогон
	// планирования никто не забирал, а забранный отчитался.
	if usage.SilentRuns != 0 {
		t.Fatalf("незабранный прогон посчитан без учёта: %+v", usage)
	}

	// Стоимость ребёнка попадает в поддерево, но не в собственную стоимость эпика.
	child, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{ParentKey: epic.Key, Title: "Кусок со счётом"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(ctx, child.Key, "start", system.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Claim(ctx, child.Key, bot.ID, "test"); err != nil {
		t.Fatal(err)
	}
	childCost := 0.25
	if _, err := engine.ReportUsage(ctx, child.Key, bot.ID, ReportUsageInput{
		Model: "claude-haiku-4-5", CostUSD: &childCost,
	}); err != nil {
		t.Fatal(err)
	}

	usage, err = engine.TaskUsage(ctx, epic.Key)
	if err != nil {
		t.Fatal(err)
	}
	if usage.CostUSD != cost || usage.SubtreeCostUSD != cost+childCost {
		t.Fatalf("стоимость поддерева: %+v", usage)
	}
}
