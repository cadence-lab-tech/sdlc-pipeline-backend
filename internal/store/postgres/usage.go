package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

// Стоимость работы агента (решение 0026). Считается на прогоне этапа: прогон
// знает роль, версию роли, попытку и исполнителя, и только на этом уровне
// сравнение «ревьюер маленькой моделью против большой» вообще возможно.

type UsageInput struct {
	RunID uuid.UUID
	Model string
	// Числа nullable: «не сообщили» отличается от нуля.
	InputTokens      *int64
	OutputTokens     *int64
	CacheReadTokens  *int64
	CacheWriteTokens *int64
	CostUSD          *float64
	DurationMS       *int64
	ReportedBy       uuid.UUID
	Details          json.RawMessage
}

const usageCols = `id, run_id, model, input_tokens, output_tokens, cache_read_tokens,
	cache_write_tokens, cost_usd, duration_ms, reported_by, reported_at, details`

func scanUsage(row interface{ Scan(...any) error }) (domain.StageRunUsage, error) {
	var u domain.StageRunUsage
	err := row.Scan(&u.ID, &u.RunID, &u.Model, &u.InputTokens, &u.OutputTokens,
		&u.CacheReadTokens, &u.CacheWriteTokens, &u.CostUSD, &u.DurationMS,
		&u.ReportedBy, &u.ReportedAt, &u.Details)
	return u, err
}

// RecordUsage добавляет отчёт о стоимости. Строк на прогон может быть
// несколько: агент отчитывается по ходу работы и может сменить модель.
func (s *Store) RecordUsage(ctx context.Context, q Querier, in UsageInput) (domain.StageRunUsage, error) {
	details := in.Details
	if len(details) == 0 {
		details = json.RawMessage(`{}`)
	}
	u, err := scanUsage(q.QueryRow(ctx, `
		INSERT INTO stage_run_usage (run_id, model, input_tokens, output_tokens,
		                             cache_read_tokens, cache_write_tokens, cost_usd,
		                             duration_ms, reported_by, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+usageCols,
		in.RunID, in.Model, in.InputTokens, in.OutputTokens, in.CacheReadTokens,
		in.CacheWriteTokens, in.CostUSD, in.DurationMS, in.ReportedBy, details))
	if err != nil {
		return u, translate(err)
	}
	return u, nil
}

func (s *Store) ListRunUsage(ctx context.Context, q Querier, runID uuid.UUID) ([]domain.StageRunUsage, error) {
	rows, err := q.Query(ctx, `SELECT `+usageCols+`
		FROM stage_run_usage WHERE run_id = $1 ORDER BY reported_at`, runID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.StageRunUsage{}
	for rows.Next() {
		u, err := scanUsage(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, u)
	}
	return out, translate(rows.Err())
}

// TaskUsage — стоимость задачи и, если она эпик, всего её поддерева.
//
// Отдельно считается, о скольких прогонах никто не отчитался: «неизвестно»
// не должно выглядеть как ноль (решение 0026).
func (s *Store) TaskUsage(ctx context.Context, q Querier, taskID uuid.UUID) (domain.UsageSummary, error) {
	var out domain.UsageSummary
	err := q.QueryRow(ctx, `
		WITH RECURSIVE subtree AS (
			SELECT id FROM tasks WHERE id = $1
			UNION ALL
			SELECT t.id FROM tasks t JOIN subtree s ON t.parent_id = s.id
		),
		runs AS (
			SELECT sr.id, sr.task_id FROM stage_runs sr JOIN subtree s ON s.id = sr.task_id
		)
		SELECT
			coalesce(sum(u.cost_usd) FILTER (WHERE r.task_id = $1), 0)::float8,
			coalesce(sum(u.cost_usd), 0)::float8,
			coalesce(sum(u.input_tokens + u.output_tokens), 0)::bigint,
			count(DISTINCT u.run_id),
			(SELECT count(*) FROM runs) - count(DISTINCT u.run_id)
		FROM runs r
		LEFT JOIN stage_run_usage u ON u.run_id = r.id`, taskID,
	).Scan(&out.CostUSD, &out.SubtreeCostUSD, &out.Tokens, &out.ReportedRuns, &out.SilentRuns)
	if err != nil {
		return out, translate(err)
	}
	return out, nil
}
