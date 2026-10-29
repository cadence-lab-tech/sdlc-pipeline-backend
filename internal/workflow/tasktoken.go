// Токен задачи — учётные данные исполнителя этапа (решение 0049).
//
// Аренда держится на принципале, а пул раннеров — один принципал на много
// задач (решение 0025): его собственный токен работает с любой задачей,
// которую держит пул. Саб-агенту этапа нужно меньше — его прогон, его
// задача; читать он может весь свой проект. Токен задачи выпускает держатель
// живой аренды; живёт он, пока жива аренда (проверка в TokenByHash), и гаснет
// на release, протухании, перехвате и закрытии этапа.
//
// Ограничения действуют только на токен задачи и только в режиме токенов:
// личные токены и сессии людей проверяются как раньше, а в режиме заголовка
// прав нет вовсе (0049 п. 2). Белый список тулов и запреты на правила —
// следующий шаг (CLAB-208), он расширяет эти же функции.
package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// TaskBinding — к чему привязан токен задачи.
type TaskBinding struct {
	StageRunID uuid.UUID
	TaskID     uuid.UUID
	TaskKey    string
}

// TaskToken — привязка предъявленного токена задачи; nil — токен не задачи
// или актор назван заголовком.
func (id Identity) TaskToken() *TaskBinding {
	t := id.Token
	if t == nil || t.TokenKind != domain.TokenTask || t.StageRunID == nil || t.TaskID == nil {
		return nil
	}
	b := &TaskBinding{StageRunID: *t.StageRunID, TaskID: *t.TaskID}
	if t.TaskKey != nil {
		b.TaskKey = *t.TaskKey
	}
	return b
}

// Bound — привязка, которую надо соблюдать: токен задачи в режиме токенов.
// nil — ограничений токена задачи нет.
func (a *Auth) Bound(id Identity) *TaskBinding {
	if !a.Enforced() {
		return nil
	}
	return id.TaskToken()
}

// refuseTaskToken — токеном задачи токены не выпускаются: иначе исполнитель
// этапа вышел бы из рамок своего токена, выпустив себе другой.
func (a *Auth) refuseTaskToken(id Identity) error {
	if a.Bound(id) != nil {
		return apperr.Forbidden("токеном задачи токены не выпускаются")
	}
	return nil
}

func notOwn(b *TaskBinding, what string) error {
	return apperr.Forbidden("токен задачи %s работает только со своей задачей и своим прогоном: %s",
		b.TaskKey, what).WithDetails(map[string]any{
		"task_key": b.TaskKey, "stage_run_id": b.StageRunID,
	})
}

// RequireOwnTask — запись токеном задачи только в своей задаче.
func (a *Auth) RequireOwnTask(id Identity, taskID uuid.UUID) error {
	b := a.Bound(id)
	if b == nil || b.TaskID == taskID {
		return nil
	}
	return notOwn(b, "задача чужая")
}

// RequireOwnTaskKey — то же по ключу задачи.
func (a *Auth) RequireOwnTaskKey(id Identity, taskKey string) error {
	b := a.Bound(id)
	if b == nil || b.TaskKey == taskKey {
		return nil
	}
	if taskKey == "" {
		// Пустой ключ приходит из create_task без parent: агенту надо
		// сказать, чего не хватает, а не «задача  чужая».
		return notOwn(b, "задача не указана — заводить можно только дочерние к своей (parent)")
	}
	return notOwn(b, fmt.Sprintf("задача %s чужая", taskKey))
}

// OwnRun приводит адресацию операции аренды к прогону токена задачи: ключ
// задачи обязан быть своим, явный прогон — своим, а без явного прогона
// берётся прогон токена, а не «первый, который держит принципал» (HeldRun):
// у пула принципал держит много прогонов. Не токен задачи — runID как есть.
func (a *Auth) OwnRun(id Identity, taskKey string, runID *uuid.UUID) (*uuid.UUID, error) {
	b := a.Bound(id)
	if b == nil {
		return runID, nil
	}
	if taskKey != "" && taskKey != b.TaskKey {
		return nil, notOwn(b, fmt.Sprintf("задача %s чужая", taskKey))
	}
	if runID != nil && *runID != b.StageRunID {
		return nil, notOwn(b, "прогон чужой")
	}
	own := b.StageRunID
	return &own, nil
}

// RequireReadable — чтение токеном задачи: любая задача своего проекта
// (решение 0065, пересмотр 0049 п. 5). Исполнителю нужен контекст вокруг —
// соседи по эпику и репозиторию, прошлые выпуски, похожие баги, — а запись
// это не открывает: её сужают RequireOwnTask, OwnRun, RequireWriteCircle и
// RequireLink в самих действиях. Задача другого проекта — отказ: граница
// проекта у токена задачи та же, что у Require.
func (a *Auth) RequireReadable(ctx context.Context, id Identity, taskID uuid.UUID) error {
	b := a.Bound(id)
	if b == nil || b.TaskID == taskID {
		return nil
	}
	task, err := a.store.GetTaskByID(ctx, a.store.DB(), taskID)
	if err != nil {
		return err
	}
	if id.Token.ProjectID != nil && *id.Token.ProjectID == task.ProjectID {
		return nil
	}
	return apperr.Forbidden("токен привязан к другому проекту").
		WithDetails(map[string]any{"task_key": b.TaskKey})
}

// ── выпуск ──────────────────────────────────────────────────────────────────

// IssuedTaskToken — ответ на выпуск токена задачи: сам токен виден один раз.
type IssuedTaskToken struct {
	Token          string             `json:"token"`
	Record         domain.AccessToken `json:"record"`
	TaskKey        string             `json:"task_key"`
	StageRunID     uuid.UUID          `json:"stage_run_id"`
	Role           string             `json:"role"`
	ProjectKey     string             `json:"project_key"`
	LeaseExpiresAt time.Time          `json:"lease_expires_at"`
}

// IssueTaskToken выпускает токен задачи держателю живой аренды её прогона.
//
// Прогон — тот, который держит принципал (HeldRun), а не тот, что передан:
// выпустить токен на чужую или протухшую аренду нельзя, и коды отказа те же,
// что у heartbeat — по ним раннер решает, захватывать ли заново.
func (a *Auth) IssueTaskToken(ctx context.Context, id Identity, taskKey string) (IssuedTaskToken, error) {
	var out IssuedTaskToken
	if err := a.refuseTaskToken(id); err != nil {
		return out, err
	}
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := a.store.GetTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		if err := a.Require(ctx, id, task.ProjectID, domain.MemberAgent); err != nil {
			return err
		}
		held, err := heldRunTx(ctx, a.store, tx, task, id.Principal.ID)
		if err != nil {
			return err
		}
		run, err := a.store.LockStageRun(ctx, tx, held.ID)
		if err != nil {
			return err
		}
		if !run.LeaseHeld(time.Now()) || run.ClaimedBy == nil || *run.ClaimedBy != id.Principal.ID {
			return leaseLost(run, id.Principal.ID)
		}

		token, hash, prefix, err := NewToken()
		if err != nil {
			return err
		}
		roles := []string{}
		if run.RoleKey != "" {
			roles = []string{run.RoleKey}
		}
		projectID := task.ProjectID
		record, err := a.store.CreateToken(ctx, tx, postgres.CreateTokenInput{
			PrincipalID: id.Principal.ID, ProjectID: &projectID,
			Name: fmt.Sprintf("task:%s/%s#%d", task.Key, run.StatusKey, run.Attempt),
			Kind: domain.TokenTask, StageRunID: &run.ID,
			TokenHash: hash, Prefix: prefix, RoleKeys: roles, CreatedBy: id.Principal.ID,
		})
		if err != nil {
			return err
		}
		out = IssuedTaskToken{
			Token: token, Record: record, TaskKey: task.Key, StageRunID: run.ID,
			Role: run.RoleKey, ProjectKey: task.ProjectKey, LeaseExpiresAt: *run.LeaseExpires,
		}
		return nil
	})
	return out, err
}

// heldRunTx — прогон задачи, который держит актор, внутри транзакции: тот же
// выбор, что у HeldRun.
func heldRunTx(ctx context.Context, store *postgres.Store, q postgres.Querier, task domain.Task, actor uuid.UUID) (domain.StageRun, error) {
	run, err := store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return domain.StageRun{}, err
	}
	reviews, err := store.OpenReviewRuns(ctx, q, run.ID)
	if err != nil {
		return domain.StageRun{}, err
	}
	for _, r := range reviews {
		if r.ClaimedBy != nil && *r.ClaimedBy == actor {
			return r, nil
		}
	}
	return run, nil
}
