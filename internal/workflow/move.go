// Перевод задачи в статус (CLAB-408, эпик CLAB-404, R-8): операция движка
// вне переходов цикла. Сотрудник или сопровождающий ставит задачу в любой
// статус её версии цикла — текущий (рестарт этапа), начальный, терминальный —
// с причиной. Гейты не вычисляются, gate_results не пишутся, перепроверки не
// заводятся: в этом смысл операции, а след — task.moved в ленте.
//
// Это не переход: transitions, planLanding и finishSemantics здесь не
// участвуют, поэтому у прогона, открытого переводом, нет entered_via и нет
// предела времени этапа (stagelimit.go), а предел попыток считает его вход
// (nextAttemptSQL видит все прогоны статуса).
package workflow

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// MoveResult — исход перевода: задача в статусе назначения, новый прогон,
// закрытый прогон (nil — у задачи не было открытой попытки на текущем
// статусе) и число закрытых перепроверок. Ключи ответа — контракт CLAB-407.
type MoveResult struct {
	Task          domain.Task      `json:"task"`
	StageRun      domain.StageRun  `json:"stage_run"`
	PreviousRun   *domain.StageRun `json:"previous_run"`
	ClosedReviews int              `json:"closed_reviews"`
}

// MoveTask переводит задачу taskKey в статус to её версии цикла.
//
// Одна транзакция под блокировкой строки задачи, как переход: закрытие
// прежнего прогона и его перепроверок (abandoned/moved, аренда снята, токены
// задачи погашены в FinishStageRun), смена статуса с closed_at по
// is_terminal, новый прогон попыткой N+1 без rejected_from и entered_via,
// события task.moved и stage.entered. Порядок отказов: задачи нет (404) →
// право актора (TaskRight, ActMove: 403) → пустая причина (400) → статус не
// из версии цикла задачи (400 со списком) → выключенный необязательный этап
// (409 stage_disabled). Ни один отказ ничего не меняет.
func (e *Engine) MoveTask(ctx context.Context, taskKey, to, reason string, who Actor) (MoveResult, error) {
	var res MoveResult
	reason = strings.TrimSpace(reason)
	to = strings.TrimSpace(to)
	actor := who.Principal.ID

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		if err := e.mayTask(ctx, tx, who, ActMove, task); err != nil {
			return err
		}
		if reason == "" {
			return apperr.InvalidInput("причина перевода обязательна: reason не пустой")
		}

		g, err := e.cycleGraph(ctx, tx, task.WorkflowID)
		if err != nil {
			return err
		}
		target, err := moveTarget(g, task, to)
		if err != nil {
			return err
		}
		// Перевод ставит задачу ровно туда, куда попросили: цепочки обхода
		// (planLanding) у него нет, и выключенный этап — отказ, а не пропуск.
		if !stageEnabled(target, storedChoice(task.OptionalStages)) {
			return moveStageDisabled(task, target)
		}

		prev, closed, err := e.closeForMove(ctx, tx, task)
		if err != nil {
			return err
		}
		updatedAt, closedAt, err := e.store.SetTaskStatus(ctx, tx, task.ID, target.ID, target.IsTerminal)
		if err != nil {
			return err
		}
		newRun, err := e.store.EnterStatus(ctx, tx, task.ID, target.ID, nil, nil)
		if err != nil {
			return err
		}

		// via всегда level (D-1 CLAB-408): перевод роль этапа не проверяет;
		// stage_role — основная роль статуса, из которого ушли.
		// Событие — на закрытый прогон, как task.advanced: выход прогона в
		// карточке (RunExitsByTask) читается по нему.
		var prevID *uuid.UUID
		if prev != nil {
			prevID = &prev.ID
		}
		if err := e.store.AppendEvent(ctx, tx, task.ID, prevID, domain.EventTaskMoved, actor, map[string]any{
			"from":       task.StatusKey,
			"to":         target.Key,
			"reason":     reason,
			"level":      string(who.Level),
			"via":        ViaLevel,
			"token_kind": tokenKindOf(who),
			"stage_role": g.stageRole(task.StatusID),
			"attempt":    newRun.Attempt,
		}); err != nil {
			return err
		}
		if err := e.store.AppendEvent(ctx, tx, task.ID, &newRun.ID, domain.EventStageEntered, actor,
			map[string]any{"status": target.Key, "attempt": newRun.Attempt}); err != nil {
			return err
		}

		task.StatusID, task.StatusKey, task.StatusName = target.ID, target.Key, target.Name
		task.UpdatedAt, task.ClosedAt = updatedAt, closedAt
		res = MoveResult{Task: task, StageRun: newRun, PreviousRun: prev, ClosedReviews: closed}
		return nil
	})
	return res, err
}

// moveTarget — статус to версии цикла задачи; нет такого — 400 со списком
// ключей статусов версии (D-2), а не 404: ошибся вызывающий в аргументе.
func moveTarget(g cycleGraph, task domain.Task, to string) (domain.Status, error) {
	keys := make([]string, 0, len(g.statuses))
	for _, st := range g.statuses {
		if st.Key == to {
			return st, nil
		}
		keys = append(keys, st.Key)
	}
	if to == "" {
		return domain.Status{}, apperr.InvalidInput("статус перевода не указан: to — ключ статуса цикла задачи %s (%s)",
			task.Key, strings.Join(keys, ", ")).WithDetails(map[string]any{"statuses": keys})
	}
	return domain.Status{}, apperr.InvalidInput("статуса %q нет в цикле задачи %s: %s",
		to, task.Key, strings.Join(keys, ", ")).WithDetails(map[string]any{"statuses": keys})
}

// moveStageDisabled — отказ перевода в выключенный у задачи этап (AC-10): код
// stage_disabled и подсказка, как включить этап.
func moveStageDisabled(task domain.Task, st domain.Status) error {
	return apperr.New(apperr.CodeStageDisabled,
		"этап %s выключен у задачи %s: перевести её туда — сначала включить этап: clab edit %s --stage %s=on",
		st.Key, task.Key, task.Key, st.Key).WithDetails(map[string]any{
		"status": st.Key, "unavailable": UnavailableStageDisabled,
	})
}

// closeForMove закрывает открытую попытку текущего статуса и её открытые
// перепроверки исходом moved. Закрытую попытку (неполные данные) не трогает:
// её исход уже записан. Попытки нет вовсе — nil без ошибки.
func (e *Engine) closeForMove(ctx context.Context, tx pgx.Tx, task domain.Task) (*domain.StageRun, int, error) {
	run, err := e.store.CurrentStageRun(ctx, tx, task.ID, task.StatusID)
	if err != nil {
		if apperr.As(err).Code == apperr.CodeNotFound {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	reviews, err := e.store.OpenReviewRuns(ctx, tx, run.ID)
	if err != nil {
		return nil, 0, err
	}
	for _, r := range reviews {
		if err := e.store.FinishStageRun(ctx, tx, r.ID, domain.StageAbandoned, domain.RunOutcomeMoved); err != nil {
			return nil, 0, err
		}
	}
	if run.FinishedAt == nil {
		if err := e.store.FinishStageRun(ctx, tx, run.ID, domain.StageAbandoned, domain.RunOutcomeMoved); err != nil {
			return nil, 0, err
		}
	}
	closed, err := e.store.GetStageRun(ctx, tx, run.ID)
	if err != nil {
		return nil, 0, err
	}
	return &closed, len(reviews), nil
}
