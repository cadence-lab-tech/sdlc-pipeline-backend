package workflow

import (
	"context"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Активация новой версии цикла: задача, которая ещё не начиналась (стоит в
// бэклоге, никто её не забирал), переезжает на новую версию; задача, уже
// взятая в работу, доходит по прежней (решения 0016 и 0036).
func TestActivationMovesWaitingTasksToNewVersion(t *testing.T) {
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)

	waiting, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Ждёт в бэклоге"},
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Уже в работе"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(ctx, started.Key, "start", system.ID, ""); err != nil {
		t.Fatalf("запуск задачи в цикл: %v", err)
	}
	oldWorkflow := waiting.WorkflowID

	draft, err := authoring.CreateDraft(ctx, "SDLC", system.ID, DraftInput{Name: "Default SDLC"})
	if err != nil {
		t.Fatalf("черновик: %v", err)
	}
	res, err := authoring.Activate(ctx, draft.ID, system.ID)
	if err != nil {
		t.Fatalf("активация: %v", err)
	}

	// В сиде есть свои задачи в бэклоге — они переезжают вместе с нашей.
	movedKeys := map[string]postgres.MovedTask{}
	for _, m := range res.Moved {
		movedKeys[m.Key] = m
	}
	if m, ok := movedKeys[waiting.Key]; !ok || m.FromStatus != "new" || m.ToStatus != "new" {
		t.Fatalf("задача из бэклога должна переехать: %+v", res.Moved)
	}
	if _, ok := movedKeys[started.Key]; ok {
		t.Fatalf("начатая задача не переезжает: %+v", res.Moved)
	}
	stayed := map[string]bool{}
	for _, c := range res.OnPreviousVersion {
		stayed[c.Key] = true
	}
	if !stayed[started.Key] || stayed[waiting.Key] {
		t.Fatalf("на прежней версии остаётся начатая задача, а не ожидающая: %+v", res.OnPreviousVersion)
	}

	moved, err := store.GetTaskByKey(ctx, store.DB(), waiting.Key)
	if err != nil {
		t.Fatal(err)
	}
	if moved.WorkflowID != draft.ID || moved.StatusKey != "new" {
		t.Fatalf("задача из бэклога должна стоять на новой версии: цикл %s, статус %s",
			moved.WorkflowID, moved.StatusKey)
	}
	run, err := store.CurrentStageRun(ctx, store.DB(), moved.ID, moved.StatusID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StagePending || run.ClaimedBy != nil {
		t.Fatalf("на новой версии открывается свежая попытка: %+v", run)
	}
	runs, err := store.ListStageRuns(ctx, store.DB(), moved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].State != domain.StageAbandoned {
		t.Fatalf("старая попытка закрывается как abandoned: %+v", runs)
	}
	events, err := store.ListEvents(ctx, store.DB(), moved.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == domain.EventTaskWorkflowMoved {
			found = true
		}
	}
	if !found {
		t.Fatal("в ленте задачи нет события переезда")
	}

	// Переехавшая задача идёт по новой версии: переход ищется в её цикле.
	if _, err := engine.Execute(ctx, moved.Key, "start", system.ID, ""); err != nil {
		t.Fatalf("переход по новой версии: %v", err)
	}

	kept, err := store.GetTaskByKey(ctx, store.DB(), started.Key)
	if err != nil {
		t.Fatal(err)
	}
	if kept.WorkflowID != oldWorkflow || kept.StatusKey != "analysis" {
		t.Fatalf("начатая задача остаётся на прежней версии: цикл %s, статус %s",
			kept.WorkflowID, kept.StatusKey)
	}

	// Следующая активация: запущенная задача уже не в бэклоге и не переезжает.
	again, err := authoring.CreateDraft(ctx, "SDLC", system.ID, DraftInput{Name: "Default SDLC"})
	if err != nil {
		t.Fatal(err)
	}
	res, err = authoring.Activate(ctx, again.ID, system.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Moved {
		if m.Key == moved.Key || m.Key == kept.Key {
			t.Fatalf("начатые задачи не переезжают: %+v", res.Moved)
		}
	}
}
