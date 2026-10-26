package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
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
	// api, subscription или nil — не указано (решение 0062). Значение
	// проверяет движок, CHECK в схеме — страховка.
	Billing *string
}

// moneyPredicate — строка учёта считается деньгами: api или без признака
// (решение 0062). Одна константа на бюджет, карточку и метрики, чтобы
// определение денег не разошлось между запросами. IS DISTINCT FROM, а не <>:
// сравнение NULL с 'subscription' — не истина, и строка без признака выпала бы
// из денег.
const moneyPredicate = `u.billing IS DISTINCT FROM 'subscription'`

// subscriptionPredicate — условная стоимость на подписке.
const subscriptionPredicate = `u.billing = 'subscription'`

// Состояние учёта прогона (CLAB-219): одно SQL-определение на карточку
// (RunUsageStates), стоимость задачи (TaskUsage), метрики (CostMetrics) и
// долг по политике учёта (UsageDebts); его же берут CLAB-220 и CLAB-223.
// Признак «прогон забирали» живёт только в ленте: release и протухание
// аренды обнуляют stage_runs.claimed_by, а событие stage.claimed остаётся.
// Аргумент — выражение с id прогона во внешнем запросе (например, sr.id).

// runReportedSQL — у прогона есть хотя бы одна строка учёта: строка без
// стоимости и строка подписки — тоже учёт (решения 0026 и 0062).
func runReportedSQL(run string) string {
	return `EXISTS (SELECT 1 FROM stage_run_usage su WHERE su.run_id = ` + run + `)`
}

// runClaimedSQL — прогон забирали: событие stage.claimed с его id от любого
// принципала. Индекс task_events_stage_claimed (000032) делает это
// поиском по индексу, а не проходом ленты.
func runClaimedSQL(run string) string {
	return `EXISTS (SELECT 1 FROM task_events ce
		WHERE ce.stage_run_id = ` + run + ` AND ce.type = '` + domain.EventStageClaimed + `')`
}

// usageStateSQL — reported, missing или not_applicable (domain.UsageState*).
func usageStateSQL(run string) string {
	return `CASE WHEN ` + runReportedSQL(run) + ` THEN '` + domain.UsageStateReported + `'
		WHEN ` + runClaimedSQL(run) + ` THEN '` + domain.UsageStateMissing + `'
		ELSE '` + domain.UsageStateNotApplicable + `' END`
}

const usageCols = `id, run_id, model, input_tokens, output_tokens, cache_read_tokens,
	cache_write_tokens, cost_usd, duration_ms, billing, reported_by, reported_at, details`

func scanUsage(row interface{ Scan(...any) error }) (domain.StageRunUsage, error) {
	var u domain.StageRunUsage
	err := row.Scan(&u.ID, &u.RunID, &u.Model, &u.InputTokens, &u.OutputTokens,
		&u.CacheReadTokens, &u.CacheWriteTokens, &u.CostUSD, &u.DurationMS,
		&u.Billing, &u.ReportedBy, &u.ReportedAt, &u.Details)
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
		                             duration_ms, reported_by, details, billing)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+usageCols,
		in.RunID, in.Model, in.InputTokens, in.OutputTokens, in.CacheReadTokens,
		in.CacheWriteTokens, in.CostUSD, in.DurationMS, in.ReportedBy, details, in.Billing))
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
// не должно выглядеть как ноль (решение 0026). «Без учёта» — состояние
// missing (CLAB-219): прогоны без роли и ни разу не взятые сюда не входят,
// агент там не работал. Деньги, их часть без признака и условная стоимость —
// раздельно (решение 0062); cost_usd — всё сообщённое.
func (s *Store) TaskUsage(ctx context.Context, q Querier, taskID uuid.UUID) (domain.UsageSummary, error) {
	var out domain.UsageSummary
	err := q.QueryRow(ctx, `
		WITH RECURSIVE subtree AS (
			SELECT id FROM tasks WHERE id = $1
			UNION ALL
			SELECT t.id FROM tasks t JOIN subtree s ON t.parent_id = s.id
		),
		runs AS (
			SELECT sr.id, sr.task_id, `+usageStateSQL("sr.id")+` AS usage_state
			FROM stage_runs sr JOIN subtree s ON s.id = sr.task_id
		)
		SELECT
			coalesce(sum(u.cost_usd) FILTER (WHERE r.task_id = $1), 0)::float8,
			coalesce(sum(u.cost_usd), 0)::float8,
			coalesce(sum(u.input_tokens + u.output_tokens), 0)::bigint,
			(SELECT count(*) FROM runs WHERE usage_state = '`+domain.UsageStateReported+`'),
			(SELECT count(*) FROM runs WHERE usage_state = '`+domain.UsageStateMissing+`'),
			coalesce(sum(u.cost_usd) FILTER (WHERE r.task_id = $1 AND `+moneyPredicate+`), 0)::float8,
			coalesce(sum(u.cost_usd) FILTER (WHERE r.task_id = $1 AND u.billing IS NULL), 0)::float8,
			coalesce(sum(u.cost_usd) FILTER (WHERE r.task_id = $1 AND `+subscriptionPredicate+`), 0)::float8,
			coalesce(sum(u.cost_usd) FILTER (WHERE `+moneyPredicate+`), 0)::float8,
			coalesce(sum(u.cost_usd) FILTER (WHERE u.billing IS NULL), 0)::float8,
			coalesce(sum(u.cost_usd) FILTER (WHERE `+subscriptionPredicate+`), 0)::float8
		FROM runs r
		LEFT JOIN stage_run_usage u ON u.run_id = r.id`, taskID,
	).Scan(&out.CostUSD, &out.SubtreeCostUSD, &out.Tokens, &out.ReportedRuns, &out.SilentRuns,
		&out.APICostUSD, &out.UnspecifiedCostUSD, &out.SubscriptionCostUSD,
		&out.SubtreeAPICostUSD, &out.SubtreeUnspecifiedCostUSD, &out.SubtreeSubscriptionCostUSD)
	if err != nil {
		return out, translate(err)
	}
	return out, nil
}

// RunUsageByTask — учёт каждого прогона задачи одним запросом: этапы и
// вложенные перепроверки, только свои прогоны, без поддерева (CLAB-264).
//
// sum пропускает NULL и сам даёт NULL, если непустых значений нет: число,
// которого не сообщили ни в одной строке, остаётся «неизвестно», а строка без
// стоимости не обнуляет стоимость прогона (решение 0026). Модели — без
// повторов, по времени первого отчёта, при равенстве — по имени, чтобы
// порядок не зависел от плана запроса. Прогона без строк в ответе нет: «нет
// учёта» дописывает тот, кто склеивает (CardStageRuns). Деньги и условная
// стоимость по тому же правилу: null — строк такого вида со стоимостью нет.
func (s *Store) RunUsageByTask(ctx context.Context, q Querier, taskID uuid.UUID) (map[uuid.UUID]domain.RunUsage, error) {
	rows, err := q.Query(ctx, `
		WITH own AS (
			SELECT u.*,
			       CASE WHEN `+moneyPredicate+` THEN u.cost_usd END AS api_cost,
			       CASE WHEN u.billing IS NULL THEN u.cost_usd END AS unspecified_cost,
			       CASE WHEN `+subscriptionPredicate+` THEN u.cost_usd END AS subscription_cost
			FROM stage_run_usage u
			JOIN stage_runs sr ON sr.id = u.run_id
			WHERE sr.task_id = $1
		),
		models AS (
			SELECT run_id, array_agg(model ORDER BY first_at, model) AS models
			FROM (SELECT run_id, model, min(reported_at) AS first_at
			      FROM own GROUP BY run_id, model) m
			GROUP BY run_id
		)
		SELECT o.run_id, count(*)::int, m.models,
		       sum(o.input_tokens)::bigint, sum(o.output_tokens)::bigint,
		       sum(o.cache_read_tokens)::bigint, sum(o.cache_write_tokens)::bigint,
		       sum(o.cost_usd)::float8, sum(o.duration_ms)::bigint,
		       sum(o.api_cost)::float8, sum(o.unspecified_cost)::float8,
		       sum(o.subscription_cost)::float8
		FROM own o JOIN models m ON m.run_id = o.run_id
		GROUP BY o.run_id, m.models`, taskID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := map[uuid.UUID]domain.RunUsage{}
	for rows.Next() {
		var (
			runID uuid.UUID
			u     domain.RunUsage
		)
		if err := rows.Scan(&runID, &u.Reports, &u.Models, &u.InputTokens, &u.OutputTokens,
			&u.CacheReadTokens, &u.CacheWriteTokens, &u.CostUSD, &u.DurationMS,
			&u.APICostUSD, &u.UnspecifiedCostUSD, &u.SubscriptionCostUSD); err != nil {
			return nil, translate(err)
		}
		u.Reported = u.Reports > 0
		u.State = domain.UsageStateReported
		out[runID] = u
	}
	return out, translate(rows.Err())
}

// RunUsageStates — состояние учёта каждого прогона задачи одним запросом
// (CLAB-219): этапы и перепроверки, только свои прогоны, без поддерева.
func (s *Store) RunUsageStates(ctx context.Context, q Querier, taskID uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := q.Query(ctx, `
		SELECT sr.id, `+usageStateSQL("sr.id")+`
		FROM stage_runs sr WHERE sr.task_id = $1`, taskID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := map[uuid.UUID]string{}
	for rows.Next() {
		var (
			id    uuid.UUID
			state string
		)
		if err := rows.Scan(&id, &state); err != nil {
			return nil, translate(err)
		}
		out[id] = state
	}
	return out, translate(rows.Err())
}

// UsageDebt — прогон, за который принципал должен отчёт по политике учёта
// проекта.
type UsageDebt struct {
	TaskKey    string    `json:"task"`
	StageRunID uuid.UUID `json:"stage_run_id"`
	FinishedAt time.Time `json:"finished_at"`
}

// UsageDebts — долг принципала по учёту в проекте (CLAB-219): прогоны,
// которые он забирал, в состоянии missing, закрытые не раньше enabledAt и
// не позже чем graceMinutes назад по часам базы. Прогон, вернувшийся в
// pending, не закрыт и долгом не считается. Строка учёта любого, кто прогон
// забирал, гасит долг всех: учёт стоит на прогоне. Отдаёт первые limit
// записей по времени закрытия и полное число.
func (s *Store) UsageDebts(ctx context.Context, q Querier, projectID, actor uuid.UUID, enabledAt time.Time, graceMinutes, limit int) ([]UsageDebt, int, error) {
	rows, err := q.Query(ctx, `
		SELECT t.key, sr.id, sr.finished_at, count(*) OVER ()
		FROM stage_runs sr
		JOIN tasks t ON t.id = sr.task_id
		WHERE t.project_id = $1
		  AND sr.finished_at IS NOT NULL
		  AND sr.finished_at >= $3
		  AND sr.finished_at <= now() - make_interval(mins => $4)
		  AND EXISTS (SELECT 1 FROM task_events ae
		              WHERE ae.stage_run_id = sr.id AND ae.type = '`+domain.EventStageClaimed+`'
		                AND ae.actor_id = $2)
		  AND `+usageStateSQL("sr.id")+` = '`+domain.UsageStateMissing+`'
		ORDER BY sr.finished_at, sr.id
		LIMIT $5`, projectID, actor, enabledAt, graceMinutes, limit)
	if err != nil {
		return nil, 0, translate(err)
	}
	defer rows.Close()

	out := []UsageDebt{}
	total := 0
	for rows.Next() {
		var d UsageDebt
		if err := rows.Scan(&d.TaskKey, &d.StageRunID, &d.FinishedAt, &total); err != nil {
			return nil, 0, translate(err)
		}
		out = append(out, d)
	}
	return out, total, translate(rows.Err())
}

// CardStageRuns — прогоны задачи для карточки вместе с их учётом, номером
// версии роли, вердиктами гейтов и выходом (CLAB-283). Одно место для REST
// (getTask) и MCP (sdlc_get_task), чтобы транспорты отдавали одно и то же
// (решение 0011). Каждое из дополнений читается одним запросом на задачу, а
// не на прогон. ListStageRuns остаётся без дополнений: его зовёт ещё сборка
// контекста захвата, где они не нужны.
func (s *Store) CardStageRuns(ctx context.Context, q Querier, taskID uuid.UUID) ([]domain.CardStageRun, error) {
	runs, err := s.ListStageRuns(ctx, q, taskID)
	if err != nil {
		return nil, err
	}
	usage, err := s.RunUsageByTask(ctx, q, taskID)
	if err != nil {
		return nil, err
	}
	states, err := s.RunUsageStates(ctx, q, taskID)
	if err != nil {
		return nil, err
	}
	versions, err := s.RoleVersionsByTask(ctx, q, taskID)
	if err != nil {
		return nil, err
	}
	verdicts, err := s.GateVerdictsByTask(ctx, q, taskID)
	if err != nil {
		return nil, err
	}
	exits, err := s.RunExitsByTask(ctx, q, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.CardStageRun, 0, len(runs))
	for _, r := range runs {
		u, ok := usage[r.ID]
		if !ok {
			u = domain.NoRunUsage()
		}
		if st, ok := states[r.ID]; ok {
			u.State = st
		}
		card := domain.CardStageRun{StageRun: r, Usage: u, GateResults: []domain.RunGateVerdict{}}
		if v, ok := versions[r.ID]; ok {
			card.RoleVersion = &v
		}
		if g, ok := verdicts[r.ID]; ok {
			card.GateResults = g
		}
		if x, ok := exits[r.ID]; ok {
			card.Exit = &x
		}
		out = append(out, card)
	}
	return out, nil
}
