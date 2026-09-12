package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Правка постановки задачи (CLAB-58): нарезка эпика уточняется, а не
// заводится заново. Проверяется по настоящей схеме: рамка агента стоит на
// живой аренде, а прежние значения остаются в ленте событий.

func str(s string) *string { return &s }

// Правка одного поля не затирает остальные, а пустое описание его очищает.
func TestUpdateTaskChangesOnlyPassedFields(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{
			Title: "Было", Description: str("Старое описание"),
			Priority: "normal", Labels: []string{"bug"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Title: str("Стало"), Level: domain.MemberMaintainer,
	})
	if err != nil {
		t.Fatalf("правка заголовка: %v", err)
	}
	if out.Title != "Стало" || out.Description == nil || *out.Description != "Старое описание" {
		t.Fatalf("заголовок затронул описание: %q %v", out.Title, out.Description)
	}
	if out.Priority != "normal" || len(out.Labels) != 1 || out.Labels[0] != "bug" {
		t.Fatalf("заголовок затронул приоритет или метки: %q %v", out.Priority, out.Labels)
	}
	if !out.UpdatedAt.After(task.UpdatedAt) {
		t.Fatalf("updated_at не двинулся: %v → %v", task.UpdatedAt, out.UpdatedAt)
	}
	if out.Key != task.Key || out.Seq != task.Seq || out.StatusID != task.StatusID ||
		out.Type() != task.Type() || !out.CreatedAt.Equal(task.CreatedAt) {
		t.Fatalf("правка задела то, что не её: %+v", out)
	}

	out, err = engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Description: str("Новое описание"), Level: domain.MemberMaintainer,
	})
	if err != nil || out.Title != "Стало" || out.Description == nil || *out.Description != "Новое описание" {
		t.Fatalf("правка описания: %v %q %v", err, out.Title, out.Description)
	}

	// Пустая строка означает «описания нет»: в базе это NULL, а не пустая
	// строка — двух способов сказать одно и то же не держим.
	out, err = engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Description: str(""), Level: domain.MemberMaintainer,
	})
	if err != nil || out.Description != nil {
		t.Fatalf("очистка описания: %v %v", err, out.Description)
	}

	labels := []string{"phase8", "ux"}
	out, err = engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Priority: str("urgent"), Labels: &labels, Level: domain.MemberMaintainer,
	})
	if err != nil || out.Priority != "urgent" {
		t.Fatalf("правка приоритета: %v %q", err, out.Priority)
	}
	if len(out.Labels) != 2 || out.Labels[0] != "phase8" || out.Labels[1] != "ux" {
		t.Fatalf("метки заменяются целиком и в переданном порядке: %v", out.Labels)
	}

	empty := []string{}
	out, err = engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Labels: &empty, Level: domain.MemberMaintainer,
	})
	if err != nil || len(out.Labels) != 0 {
		t.Fatalf("очистка меток: %v %v", err, out.Labels)
	}

	events, err := store.ListEvents(ctx, store.DB(), task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	updates := 0
	for _, e := range events {
		if e.Type == domain.EventTaskUpdated {
			updates++
		}
	}
	if updates != 5 {
		t.Fatalf("правок в ленте %d, ожидали 5", updates)
	}
}

// Негодную постановку отбивает движок, а не CHECK в схеме: клиенту нужна
// понятная ошибка с пределом и перечнем, а не текст драйвера базы.
func TestUpdateTaskValidatesInput(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Постановка", Priority: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}

	cyrillic80 := strings.Repeat("я", 80)
	cases := []struct {
		name string
		in   UpdateTaskInput
		ok   bool
	}{
		{"пустой заголовок", UpdateTaskInput{Title: str("")}, false},
		{"заголовок из пробелов", UpdateTaskInput{Title: str("   ")}, false},
		{"81 символ", UpdateTaskInput{Title: str(cyrillic80 + "я")}, false},
		{"80 символов кириллицей", UpdateTaskInput{Title: str(cyrillic80)}, true},
		{"негодный приоритет", UpdateTaskInput{Priority: str("critical")}, false},
		{"пустое тело", UpdateTaskInput{}, false},
	}
	for _, tc := range cases {
		tc.in.Level = domain.MemberMaintainer
		out, err := engine.UpdateTask(ctx, task.Key, system.ID, tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if apperr.As(err).Code != apperr.CodeInvalidInput {
			t.Errorf("%s: ожидали invalid_input, получили %v", tc.name, err)
		}
		if out.Key != "" {
			t.Errorf("%s: задача вернулась изменённой: %+v", tc.name, out)
		}
	}
	// Предел с перечнем — в тексте ошибки: иначе клиенту нечего показать.
	_, err = engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Title: str(cyrillic80 + "яя"), Level: domain.MemberMaintainer,
	})
	if msg := apperr.As(err).Message; !strings.Contains(msg, "80") || !strings.Contains(msg, "82") {
		t.Errorf("в ошибке нет предела и фактической длины: %q", msg)
	}
	_, err = engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Priority: str("critical"), Level: domain.MemberMaintainer,
	})
	if msg := apperr.As(err).Message; !strings.Contains(msg, "urgent") {
		t.Errorf("в ошибке нет перечня приоритетов: %q", msg)
	}

	// Ничего из отбитого не записалось, приоритет прежний.
	fresh, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Title != cyrillic80 || fresh.Priority != "high" {
		t.Fatalf("задача изменена отбитым запросом: %q %q", fresh.Title, fresh.Priority)
	}

	// Задача с заголовком длиннее предела правится по описанию: предел
	// проверяется только у переданного заголовка, иначе старые задачи
	// оказались бы неправимыми.
	if _, err := store.DB().Exec(ctx,
		`UPDATE tasks SET title = $2 WHERE id = $1`, task.ID, strings.Repeat("д", 120)); err != nil {
		t.Fatal(err)
	}
	out, err := engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Description: str("Правка описания у длинного заголовка"), Level: domain.MemberMaintainer,
	})
	if err != nil || out.Description == nil {
		t.Fatalf("длинный заголовок из прошлого мешает правке описания: %v", err)
	}
}

// Рамка агента: своя задача по живой аренде и её дочерние. Чужую задачу и
// задачу с протухшей арендой агент не правит.
func TestAgentEditsOnlyHeldTaskAndItsChildren(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	epic := epicTask(t, ctx, engine, system, "Эпик под правкой")
	other := epicTask(t, ctx, engine, system, "Чужой эпик")
	bot, err := store.ResolveActor(ctx, store.DB(), "analytics-bot", domain.PrincipalAgent)
	if err != nil {
		t.Fatal(err)
	}

	// Пока эпик не забран, править в нём нечего.
	_, err = engine.UpdateTask(ctx, epic.Key, bot.ID, UpdateTaskInput{
		Title: str("Рано"), Level: domain.MemberAgent,
	})
	if apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("незахваченную задачу править нельзя: %v", err)
	}

	claim, err := engine.Claim(ctx, epic.Key, bot.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	child, err := engine.CreateTask(ctx, "SDLC", bot.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{ParentKey: epic.Key, Title: "Кусок нарезки"},
		Level:           domain.MemberAgent,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Держит эпик — правит и его, и дочернюю: это и есть уточнение нарезки.
	if _, err := engine.UpdateTask(ctx, epic.Key, bot.ID, UpdateTaskInput{
		Description: str("Уточнённый эпик"), Level: domain.MemberAgent,
	}); err != nil {
		t.Fatalf("правка своего эпика: %v", err)
	}
	out, err := engine.UpdateTask(ctx, child.Key, bot.ID, UpdateTaskInput{
		Description: str("Ручка уже есть в коде"), Priority: str("high"), Level: domain.MemberAgent,
	})
	if err != nil || out.Priority != "high" {
		t.Fatalf("правка дочерней задачи: %v %q", err, out.Priority)
	}

	// Чужой эпик — отказ, и в объяснении назван он.
	_, err = engine.UpdateTask(ctx, other.Key, bot.ID, UpdateTaskInput{
		Title: str("Не своё"), Level: domain.MemberAgent,
	})
	if apperr.As(err).Code != apperr.CodeForbidden || !strings.Contains(apperr.As(err).Message, other.Key) {
		t.Fatalf("чужую задачу править нельзя: %v", err)
	}

	// Уровень viewer не правит даже то, что кто-то держит.
	viewer, err := store.ResolveActor(ctx, store.DB(), "reader", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.UpdateTask(ctx, epic.Key, viewer.ID, UpdateTaskInput{
		Title: str("Мимо"), Level: domain.MemberViewer,
	}); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("viewer не правит постановку: %v", err)
	}

	// Отпустил задачу — права на правку нет: аренда снимается явно.
	if err := engine.Release(ctx, claim.StageRunID, bot.ID, "пауза"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.UpdateTask(ctx, epic.Key, bot.ID, UpdateTaskInput{
		Title: str("После release"), Level: domain.MemberAgent,
	}); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("после release править нельзя: %v", err)
	}
	if _, err := engine.UpdateTask(ctx, child.Key, bot.ID, UpdateTaskInput{
		Title: str("И ребёнка тоже"), Level: domain.MemberAgent,
	}); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("после release дочернюю править нельзя: %v", err)
	}
}

// Событие ленты несёт прежние значения: версий у постановки нет, и лента —
// единственное место, где остаётся, что было до правки. Актор в нём —
// ссылка на principals, а не строка (решение 0010).
func TestUpdateTaskEventKeepsPreviousValues(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{
			Title: "Первый заголовок", Description: str("Первое описание"),
			Priority: "low", Labels: []string{"bug"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	labels := []string{"phase8"}
	if _, err := engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Title: str("Второй заголовок"), Description: str(""),
		Priority: str("high"), Labels: &labels, Level: domain.MemberMaintainer,
	}); err != nil {
		t.Fatal(err)
	}

	events, err := store.ListEvents(ctx, store.DB(), task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found *domain.TaskEvent
	for i := range events {
		if events[i].Type == domain.EventTaskUpdated {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("события %s нет в ленте: %+v", domain.EventTaskUpdated, events)
	}
	if found.ActorID == nil || *found.ActorID != system.ID {
		t.Fatalf("актор события: %v", found.ActorID)
	}
	if found.ActorHandle == nil || *found.ActorHandle != system.Handle {
		t.Fatalf("хэндл подставляется из principals: %v", found.ActorHandle)
	}

	var payload struct {
		Fields  []string `json:"fields"`
		Changed struct {
			Title struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"title"`
			Description struct {
				From *string `json:"from"`
				To   *string `json:"to"`
			} `json:"description"`
			Priority struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"priority"`
			Labels struct {
				From []string `json:"from"`
				To   []string `json:"to"`
			} `json:"labels"`
		} `json:"changed"`
	}
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Fields) != 4 {
		t.Fatalf("перечень изменённых полей: %v", payload.Fields)
	}
	c := payload.Changed
	if c.Title.From != "Первый заголовок" || c.Title.To != "Второй заголовок" {
		t.Errorf("заголовок в событии: %+v", c.Title)
	}
	if c.Description.From == nil || *c.Description.From != "Первое описание" || c.Description.To != nil {
		t.Errorf("очистка описания в событии: %+v", c.Description)
	}
	if c.Priority.From != "low" || c.Priority.To != "high" {
		t.Errorf("приоритет в событии: %+v", c.Priority)
	}
	if len(c.Labels.From) != 1 || c.Labels.From[0] != "bug" ||
		len(c.Labels.To) != 1 || c.Labels.To[0] != "phase8" {
		t.Errorf("метки в событии: %+v", c.Labels)
	}
}

// Особого правила по статусам нет: закрытую задачу сопровождающий правит.
// Иначе движок знал бы про конкретные статусы, чего он делать не должен.
func TestUpdateTaskAllowedInTerminalStatus(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)

	task, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Снятая задача"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Execute(ctx, task.Key, "cancel", system.ID, "не нужна")
	if err != nil || res.Blocked {
		t.Fatalf("снятие задачи: %v %+v", err, res.Gates)
	}
	closed, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if closed.ClosedAt == nil {
		t.Fatalf("статус %q не терминальный, проверять нечего: %+v", closed.StatusKey, closed)
	}

	out, err := engine.UpdateTask(ctx, task.Key, system.ID, UpdateTaskInput{
		Title: str("Снятая задача (опечатка)"), Level: domain.MemberMaintainer,
	})
	if err != nil || out.Title != "Снятая задача (опечатка)" {
		t.Fatalf("правка закрытой задачи сопровождающим: %v %q", err, out.Title)
	}

	// А агенту закрытая задача недоступна и без особого правила: аренды у
	// неё нет.
	bot := uuid.New()
	if _, err := engine.UpdateTask(ctx, task.Key, bot, UpdateTaskInput{
		Title: str("Агентом"), Level: domain.MemberAgent,
	}); apperr.As(err).Code != apperr.CodeForbidden {
		t.Fatalf("агент правит закрытую задачу: %v", err)
	}
}
