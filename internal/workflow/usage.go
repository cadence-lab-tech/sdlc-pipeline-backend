// Стоимость работы агента (решение 0026). Сервер её не измеряет: он видит
// только вызовы MCP, а токены и деньги тратятся у агента. Единственный
// источник цифр — сам агент или раннер, который его запускал, поэтому здесь
// самоотчёт, а «неизвестно» честно отличается от нуля. Токены остаются
// самоотчётом; а сумму, когда в прайсе версии настройки агентов, снятой на
// прогон, есть модель строки, считает сервер (решение 0077 п. 4.11,
// usagepricing.go), и названная агентом сумма ложится рядом как оценка.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	// Признак оплаты (решение 0062): api, subscription или пусто — не
	// указано, строка считается деньгами. Проверка одна, здесь: все
	// транспорты передают значение как пришло.
	Billing string
}

// parseBilling — признак оплаты строки учёта: пусто — не указано (nil),
// иначе одно из domain.Billings. Регистр не приводится: «API» — не api.
func parseBilling(raw string) (*string, error) {
	if raw == "" {
		return nil, nil
	}
	if !domain.ValidBilling(raw) {
		return nil, apperr.InvalidInput(
			"billing: %q не поддерживается, допустимы %s; пусто — не указано, считается как api",
			raw, strings.Join(domain.Billings, ", "))
	}
	return &raw, nil
}

// ReportUsage записывает, во что обошёлся прогон этапа.
func (e *Engine) ReportUsage(ctx context.Context, taskKey string, actor uuid.UUID, in ReportUsageInput) (domain.StageRunUsage, error) {
	var out domain.StageRunUsage
	if in.Model == "" {
		return out, apperr.InvalidInput("нужна модель: без неё отчёт не с чем сравнивать")
	}
	billing, err := parseBilling(in.Billing)
	if err != nil {
		return out, err
	}

	err = e.store.InTx(ctx, func(tx pgx.Tx) error {
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
		claimed, err := e.claimedRun(ctx, tx, run, actor)
		if err != nil {
			return err
		}
		if !claimed {
			return apperr.Forbidden(
				"о прогоне %s отчитывается тот, кто его забирал", run.ID)
		}

		rec := postgres.UsageInput{
			RunID: run.ID, Model: in.Model,
			InputTokens: in.InputTokens, OutputTokens: in.OutputTokens,
			CacheReadTokens: in.CacheReadTokens, CacheWriteTokens: in.CacheWriteTokens,
			CostUSD: in.CostUSD, DurationMS: in.DurationMS,
			Billing: billing, ReportedBy: actor, Details: in.Details,
		}
		warnings, err := e.priceUsage(ctx, tx, run, in, &rec)
		if err != nil {
			return err
		}
		out, err = e.store.RecordUsage(ctx, tx, rec)
		if err != nil {
			return err
		}
		if warnings != nil {
			out.Warnings = warnings
		}

		// В ленту — итоговая сумма строки, та же, что в карточке; названная
		// агентом и версия прайса — рядом (CLAB-426).
		payload := map[string]any{"model": in.Model, "stage_run_id": run.ID.String()}
		if out.CostUSD != nil {
			payload["cost_usd"] = *out.CostUSD
		}
		if out.CostSource != nil {
			payload["cost_source"] = *out.CostSource
		}
		if out.PricingVersion != nil {
			payload["pricing_version"] = *out.PricingVersion
		}
		if out.ReportedCostUSD != nil {
			payload["reported_cost_usd"] = *out.ReportedCostUSD
		}
		if billing != nil {
			payload["billing"] = *billing
		}
		return e.store.AppendEvent(ctx, tx, task.ID, &run.ID, domain.EventUsageReported, actor, payload)
	})

	return out, err
}

// priceUsage решает, откуда сумма строки: по прайсу версии-снимка прогона
// или как сообщена. Цена берётся из версии, снятой на прогон при захвате
// (stage_runs.agent_settings_version_id), а не из текущей активной:
// активация новой версии идущий прогон не трогает, а прогон без снимка
// (взятый до настройки) считается как раньше. Заполняет CostMicroUSD и
// PricingVersionID у rec; предупреждения — в ответ.
func (e *Engine) priceUsage(ctx context.Context, q postgres.Querier, run domain.StageRun, in ReportUsageInput, rec *postgres.UsageInput) ([]string, error) {
	// Одно правило диапазона на оба источника суммы.
	if in.CostUSD != nil && !reportedCostInRange(*in.CostUSD) {
		return nil, errCostOutOfRange(fmt.Sprintf("названа агентом: %v USD", *in.CostUSD))
	}
	if run.AgentSettingsVersionID == nil {
		return nil, nil
	}
	version, err := e.store.AgentSettingsByID(ctx, q, *run.AgentSettingsVersionID)
	if err != nil {
		return nil, err
	}
	price, ok := modelPriceIn(version.Document, in.Model)
	if !ok {
		return nil, nil
	}
	tokens := usageTokens{in.InputTokens, in.OutputTokens, in.CacheReadTokens, in.CacheWriteTokens}
	if warnings := pricingWarnings(version.Version, in.Model, tokens); warnings != nil {
		return warnings, nil
	}
	micro, err := costMicroUSD(price, tokens)
	if err != nil {
		return nil, err
	}
	rec.CostMicroUSD = &micro
	rec.PricingVersionID = &version.ID
	return nil, nil
}

// claimedRun — забирал ли актор этот прогон. Текущий держатель — да; иначе
// ответ по ленте: аренду к моменту отчёта могли снять — release самого
// агента, уход прогона в gating под перепроверку, протухание, — и
// claimed_by тогда пуст, хотя работу оплатил именно этот актор. Раннер
// отчитывается после выхода агента и других путей не имеет (CLAB-214).
func (e *Engine) claimedRun(ctx context.Context, q postgres.Querier, run domain.StageRun, actor uuid.UUID) (bool, error) {
	if run.ClaimedBy != nil && *run.ClaimedBy == actor {
		return true, nil
	}
	return e.store.EverClaimedRun(ctx, q, run.TaskID, run.ID, actor)
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
