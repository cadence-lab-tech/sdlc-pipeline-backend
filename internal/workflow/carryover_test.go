package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// Задача из бэклога могла остаться не на только что снятой версии, а на
// более ранней: прежнюю активацию сделали без переноса бэклога. Следующая
// активация подбирает и такие задачи.
func TestActivationMovesTasksLeftOnOlderVersions(t *testing.T) {
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)

	left, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Осталась на первой версии"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := left.WorkflowID

	// Вторая версия активируется «по-старому»: без переноса бэклога.
	second, err := authoring.CreateDraft(ctx, "SDLC", system.ID, DraftInput{Name: "Default SDLC"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetireActiveWorkflows(ctx, store.DB(), second.ProjectID, second.TaskTypes); err != nil {
		t.Fatal(err)
	}
	if err := store.SetWorkflowState(ctx, store.DB(), second.ID, domain.WorkflowActive); err != nil {
		t.Fatal(err)
	}
	onSecond, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: "Заведена на второй версии"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if onSecond.WorkflowID != second.ID {
		t.Fatalf("новая задача должна лечь на вторую версию: %s", onSecond.WorkflowID)
	}

	third, err := authoring.CreateDraft(ctx, "SDLC", system.ID, DraftInput{Name: "Default SDLC"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := authoring.Activate(ctx, third.ID, system.ID)
	if err != nil {
		t.Fatalf("активация: %v", err)
	}
	if len(res.Retired) != 1 || res.Retired[0] != second.ID {
		t.Fatalf("снимается только вторая версия: %+v", res.Retired)
	}
	moved := map[string]bool{}
	for _, m := range res.Moved {
		moved[m.Key] = true
	}
	if !moved[left.Key] || !moved[onSecond.Key] {
		t.Fatalf("переезжают задачи и с первой, и со второй версии: %+v", res.Moved)
	}
	for _, key := range []string{left.Key, onSecond.Key} {
		task, err := store.GetTaskByKey(ctx, store.DB(), key)
		if err != nil {
			t.Fatal(err)
		}
		if task.WorkflowID != third.ID || task.StatusKey != "new" {
			t.Fatalf("%s должна стоять на третьей версии: цикл %s, статус %s", key, task.WorkflowID, task.StatusKey)
		}
	}
	if first == third.ID {
		t.Fatal("третья версия — новый цикл")
	}
}

// Отчёт активации перечисляет не весь бэклог, а первые ключи и счётчик:
// в проекте может быть тысячи задач.
func TestActivationReportIsCapped(t *testing.T) {
	ctx := context.Background()
	_, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	for i := 0; i < activationReportLimit+10; i++ {
		if _, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
			CreateTaskInput: postgres.CreateTaskInput{Title: fmt.Sprintf("бэклог %d", i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	draft, err := authoring.CreateDraft(ctx, "SDLC", system.ID, DraftInput{Name: "Default SDLC"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := authoring.Activate(ctx, draft.ID, system.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != activationReportLimit || res.MovedTotal < activationReportLimit+10 {
		t.Fatalf("список обрезается до %d, счётчик полный: %d в списке, всего %d",
			activationReportLimit, len(res.Moved), res.MovedTotal)
	}
	if res.OnPreviousVersionTotal != len(res.OnPreviousVersion) {
		t.Fatalf("счётчик прежней версии: %d при списке %d", res.OnPreviousVersionTotal, len(res.OnPreviousVersion))
	}
}

// Задача, которую заводят в момент активации, не должна остаться на снятой
// версии: активация ждёт коммита создания и переносит её вместе с бэклогом.
func TestActivationWaitsForTaskBeingCreated(t *testing.T) {
	ctx := context.Background()
	store, _, authoring, _, system := setupPhase4(t, AuthHeader)
	project, err := store.GetProjectByKey(ctx, store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	draft, err := authoring.CreateDraft(ctx, "SDLC", system.ID, DraftInput{Name: "Default SDLC"})
	if err != nil {
		t.Fatal(err)
	}

	created := make(chan domain.Task, 1)
	started := make(chan struct{})
	go func() {
		_ = store.InTx(ctx, func(tx pgx.Tx) error {
			task, err := store.CreateTask(ctx, tx, postgres.CreateTaskInput{
				ProjectID: project.ID, Title: "заводится во время активации", CreatedBy: system.ID,
			})
			if err != nil {
				return err
			}
			close(started)
			time.Sleep(400 * time.Millisecond) // создание ещё не закоммичено
			created <- task
			return nil
		})
	}()
	<-started

	res, err := authoring.Activate(ctx, draft.ID, system.ID)
	if err != nil {
		t.Fatalf("активация: %v", err)
	}
	task := <-created
	moved := false
	for _, m := range res.Moved {
		if m.Key == task.Key {
			moved = true
		}
	}
	if !moved {
		t.Fatalf("задача, заведённая во время активации, переезжает вместе с бэклогом: %+v", res.Moved)
	}
	got, err := store.GetTaskByKey(ctx, store.DB(), task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkflowID != draft.ID {
		t.Fatalf("задача должна стоять на новой версии %s, а стоит на %s", draft.ID, got.WorkflowID)
	}
}
