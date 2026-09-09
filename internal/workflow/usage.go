// Стоимость работы агента (решение 0026). Сервер её не измеряет: он видит
// только вызовы MCP, а токены и деньги тратятся у агента. Единственный
// источник цифр — сам агент или раннер, который его запускал, поэтому здесь
// самоотчёт, а «неизвестно» честно отличается от нуля.
package workflow

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

type ReportUsageInput struct {
	// Прогон, о котором отчитываются. Пусто — тот, который актор держит или
	// держал последним на текущем статусе задачи.
	StageRunID       *uuid.UUID
	Model            string
	InputTokens      *int64
	OutputTokens     *int64
	CacheReadTokens  *int64
	CacheWriteTokens *int64
	CostUSD          *float64
	DurationMS       *int64
	Details          json.RawMessage
}

// ReportUsage записывает, во что обошёлся прогон этапа.
func (e *Engine) ReportUsage(ctx context.Context, taskKey string, actor uuid.UUID, in ReportUsageInput) (domain.StageRunUsage, error) {
	var out domain.StageRunUsage
	if in.Model == "" {
		return out, apperr.InvalidInput("нужна модель: без неё отчёт не с чем сравнивать")
	}

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.GetTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}

		var run domain.StageRun
		if in.StageRunID != nil {
			run, err = e.store.GetStageRun(ctx, tx, *in.StageRunID)
			if err != nil {
				return err
			}
			if run.TaskID != task.ID {
				return apperr.InvalidInput("прогон принадлежит другой задаче")
			}
		} else {
			run, err = e.HeldRunTx(ctx, tx, task, actor)
			if err != nil {
				return err
			}
		}

		// Отчитаться можно только о своей работе: чужой прогон не станет
		// дешевле или дороже от того, что мимо прошёл другой агент.
		if run.ClaimedBy == nil || *run.ClaimedBy != actor {
			return apperr.Forbidden(
				"о прогоне %s отчитывается тот, кто его забирал", run.ID)
		}

		out, err = e.store.RecordUsage(ctx, tx, postgres.UsageInput{
			RunID: run.ID, Model: in.Model,
			InputTokens: in.InputTokens, OutputTokens: in.OutputTokens,
			CacheReadTokens: in.CacheReadTokens, CacheWriteTokens: in.CacheWriteTokens,
			CostUSD: in.CostUSD, DurationMS: in.DurationMS,
			ReportedBy: actor, Details: in.Details,
		})
		if err != nil {
			return err
		}

		payload := map[string]any{"model": in.Model, "stage_run_id": run.ID.String()}
		if in.CostUSD != nil {
			payload["cost_usd"] = *in.CostUSD
		}
		return e.store.AppendEvent(ctx, tx, task.ID, &run.ID, domain.EventUsageReported, actor, payload)
	})

	return out, err
}

// TaskUsage — стоимость задачи и её поддерева: карточка эпика показывает,
// во что обошлась нарезка целиком.
func (e *Engine) TaskUsage(ctx context.Context, taskKey string) (domain.UsageSummary, error) {
	q := e.store.DB()
	task, err := e.store.GetTaskByKey(ctx, q, taskKey)
	if err != nil {
		return domain.UsageSummary{}, err
	}
	return e.store.TaskUsage(ctx, q, task.ID)
}

// HeldRunTx — прогон задачи, который держит или держал этот актор. Отдельно
// от HeldRun: отчёт о стоимости идёт внутри транзакции.
func (e *Engine) HeldRunTx(ctx context.Context, q postgres.Querier, task domain.Task, actor uuid.UUID) (domain.StageRun, error) {
	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return run, err
	}
	reviews, err := e.store.OpenReviewRuns(ctx, q, run.ID)
	if err != nil {
		return run, err
	}
	for _, r := range reviews {
		if r.ClaimedBy != nil && *r.ClaimedBy == actor {
			return r, nil
		}
	}
	if run.ClaimedBy != nil && *run.ClaimedBy == actor {
		return run, nil
	}

	// Прогон уже закрыт переходом: раннер отчитывается после того, как агент
	// довёл этап до конца, и текущий прогон принадлежит уже следующему статусу.
	last, err := e.store.LastRunClaimedBy(ctx, q, task.ID, actor)
	if err != nil {
		return run, err
	}
	return last, nil
}
