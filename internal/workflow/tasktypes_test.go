package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Проект, заведённый после миграции 000011, живёт без определения свойства
// type. Активация цикла объявляет его типы задач сама: иначе цикл для эпиков
// активен, а эпик завести нельзя.
func TestActivationDeclaresTaskTypes(t *testing.T) {
	ctx := context.Background()
	store, engine, authoring, auth, _ := setupPhase4(t, AuthToken)
	admin := adminIdentity(t, ctx, auth)
	auth.OpenProjectCreation(true)
	owner, err := auth.OwnerTeamFor(ctx, admin, "")
	if err != nil {
		t.Fatal(err)
	}
	project, err := engine.CreateProject(ctx, admin.Principal.ID, postgres.CreateProjectInput{
		Key: "OCTO", Name: "Проект без цикла", OwnerTeamID: owner,
	})
	if err != nil {
		t.Fatal(err)
	}

	types, err := store.TaskTypes(ctx, store.DB(), project.ID)
	if err != nil || len(types) != 1 || types[0] != "task" {
		t.Fatalf("новый проект знает только обычные задачи: %v %v", types, err)
	}
	_, err = engine.CreateTask(ctx, "OCTO", admin.Principal.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{TaskType: "epic", Title: "Рано"},
	})
	if apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("эпик без цикла должен отбиваться: %v", err)
	}

	yes := true
	report, err := authoring.Import(ctx, "OCTO", admin.Principal.ID, Bundle{
		Version: BundleVersion,
		Roles: []BundleRole{{Key: "analytics", Name: "Аналитик", Instructions: "Режь эпик на задачи.",
			OutputContract: json.RawMessage(`[{"kind":"analysis.breakdown","required":true}]`)}},
		Workflows: []BundleWorkflow{{
			Name: "Epic", TaskTypes: []string{"epic"},
			Statuses: []BundleStatus{
				{Key: "planning", Name: "Planning", Category: "backlog", IsInitial: true},
				{Key: "analysis", Name: "Analysis", Category: "active", Role: "analytics"},
				{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
				{Key: "cancelled", Name: "Cancelled", Category: "cancelled", IsTerminal: true},
			},
			Transitions: []BundleTransition{
				{Key: "start", Name: "Отдать аналитику", From: "planning", To: "analysis", Kind: "advance"},
				{Key: "finish", Name: "Закрыть", From: "analysis", To: "done", Kind: "advance", Gates: []BundleGate{
					{Key: "artifacts", Name: "Разбивка сдана", Type: "artifacts_required", Blocking: &yes,
						Config: json.RawMessage(`{"kinds":["analysis.breakdown"]}`)}}},
				{Key: "cancel", Name: "Снять", To: "cancelled", Kind: "cancel"},
			},
		}},
	}, false)
	if err != nil {
		t.Fatalf("импорт связки: %v", err)
	}
	if len(report.Workflows) != 1 || report.Workflows[0].Outcome != "draft_created" {
		t.Fatalf("цикл должен лечь черновиком: %+v", report.Workflows)
	}
	draft, err := uuid.Parse(report.Workflows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authoring.Activate(ctx, draft, admin.Principal.ID); err != nil {
		t.Fatalf("активация: %v", err)
	}

	types, err = store.TaskTypes(ctx, store.DB(), project.ID)
	if err != nil || len(types) != 2 || types[0] != "task" || types[1] != "epic" {
		t.Fatalf("после активации проект знает task и epic: %v %v", types, err)
	}
	epic, err := engine.CreateTask(ctx, "OCTO", admin.Principal.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{TaskType: "epic", Title: "Первый эпик"},
	})
	if err != nil || epic.Type() != "epic" {
		t.Fatalf("эпик после активации цикла: %v %+v", err, epic)
	}

	// Повторное объявление ничего не дублирует и порядок не меняет.
	if err := store.DeclareTaskTypes(ctx, store.DB(), project.ID, []string{"epic", "story"}); err != nil {
		t.Fatal(err)
	}
	types, err = store.TaskTypes(ctx, store.DB(), project.ID)
	if err != nil || len(types) != 3 || types[0] != "task" || types[1] != "epic" || types[2] != "story" {
		t.Fatalf("объединение типов: %v %v", types, err)
	}
}
