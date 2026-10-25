package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Метрики цикла: где задачи застревают, какие гейты чаще всего валятся,
// сколько задач разворачивает дрейф. Всё считается по уже накопленным
// таблицам — stage_runs, gate_results, task_drift_signals, task_events —
// отдельного учёта нет.

type StatusMetric struct {
	StatusKey  string `json:"status"`
	StatusName string `json:"status_name"`
	OpenTasks  int    `json:"open_tasks"`
	// Завершённые попытки этапа за период и их длительность.
	FinishedRuns  int     `json:"finished_runs"`
	AvgHours      float64 `json:"avg_hours"`
	MedianHours   float64 `json:"median_hours"`
	MaxHours      float64 `json:"max_hours"`
	Rejections    int     `json:"rejections"`
	RepeatedRuns  int     `json:"repeated_runs"`
	ExpiredLeases int     `json:"expired_leases"`
}

type GateMetric struct {
	Transition string `json:"transition"`
	GateKey    string `json:"gate"`
	GateName   string `json:"gate_name"`
	Type       string `json:"type"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
	Waived     int    `json:"waived"`
	Pending    int    `json:"pending"`
}

type DriftMetric struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Open     int    `json:"open"`
	Resolved int    `json:"resolved"`
	Dismiss  int    `json:"dismissed"`
}

type ThroughputMetric struct {
	Created      int     `json:"created"`
	Closed       int     `json:"closed"`
	Cancelled    int     `json:"cancelled"`
	AvgLeadHours float64 `json:"avg_lead_hours"`
	Reviews      int     `json:"review_runs"`
	ReviewFails  int     `json:"review_fails"`
}

// CostMetric — стоимость в одном разрезе: по статусу, по роли, по модели.
// Считается по stage_run_usage: прогоны, о которых никто не отчитался, видны
// отдельным счётчиком, а не растворяются в нулях (решение 0026).
type CostMetric struct {
	// status | role | model
	Dimension string  `json:"dimension"`
	Key       string  `json:"key"`
	Name      string  `json:"name,omitempty"`
	Runs      int     `json:"runs"`
	Reported  int     `json:"reported_runs"`
	Silent    int     `json:"silent_runs"`
	CostUSD   float64 `json:"cost_usd"`
	AvgUSD    float64 `json:"avg_cost_usd"`
	Tokens    int64   `json:"tokens"`
	// Раздельно по признаку оплаты (решение 0062): CostUSD — всё сообщённое,
	// CostUSD = APICostUSD + SubscriptionCostUSD. Деньги — api и без
	// признака, UnspecifiedCostUSD — часть денег без признака.
	APICostUSD          float64 `json:"api_cost_usd"`
	UnspecifiedCostUSD  float64 `json:"unspecified_cost_usd"`
	SubscriptionCostUSD float64 `json:"subscription_cost_usd"`
}

// CostMetrics — стоимость за период в трёх разрезах сразу: по статусу, по
// роли и по модели. Один запрос, потому что источник у всех трёх один.
func (s *Store) CostMetrics(ctx context.Context, q Querier, projectID uuid.UUID, since time.Time) ([]CostMetric, error) {
	rows, err := q.Query(ctx, `
		WITH runs AS (
		    SELECT sr.id, ws.key AS status_key, ws.name AS status_name,
		           coalesce(ro.key, '') AS role_key, coalesce(ro.name, '') AS role_name
		    FROM stage_runs sr
		    JOIN tasks t ON t.id = sr.task_id
		    JOIN workflow_statuses ws ON ws.id = sr.status_id
		    LEFT JOIN roles ro ON ro.id = sr.role_id
		    WHERE t.project_id = $1 AND sr.entered_at >= $2
		), per_run AS (
		    SELECT r.*, sum(u.cost_usd) AS cost,
		           sum(coalesce(u.input_tokens, 0) + coalesce(u.output_tokens, 0)) AS tokens,
		           sum(u.cost_usd) FILTER (WHERE `+moneyPredicate+`) AS api_cost,
		           sum(u.cost_usd) FILTER (WHERE u.billing IS NULL) AS unspecified_cost,
		           sum(u.cost_usd) FILTER (WHERE `+subscriptionPredicate+`) AS subscription_cost
		    FROM runs r LEFT JOIN stage_run_usage u ON u.run_id = r.id
		    GROUP BY r.id, r.status_key, r.status_name, r.role_key, r.role_name
		)
		SELECT 'status', status_key, max(status_name), count(*),
		       count(*) FILTER (WHERE cost IS NOT NULL),
		       count(*) FILTER (WHERE cost IS NULL),
		       coalesce(sum(cost), 0)::float8, coalesce(sum(tokens), 0)::bigint,
		       coalesce(sum(api_cost), 0)::float8, coalesce(sum(unspecified_cost), 0)::float8,
		       coalesce(sum(subscription_cost), 0)::float8
		FROM per_run GROUP BY status_key
		UNION ALL
		SELECT 'role', role_key, max(role_name), count(*),
		       count(*) FILTER (WHERE cost IS NOT NULL),
		       count(*) FILTER (WHERE cost IS NULL),
		       coalesce(sum(cost), 0)::float8, coalesce(sum(tokens), 0)::bigint,
		       coalesce(sum(api_cost), 0)::float8, coalesce(sum(unspecified_cost), 0)::float8,
		       coalesce(sum(subscription_cost), 0)::float8
		FROM per_run WHERE role_key <> '' GROUP BY role_key
		UNION ALL
		SELECT 'model', u.model, '', count(DISTINCT u.run_id),
		       count(DISTINCT u.run_id), 0,
		       coalesce(sum(u.cost_usd), 0)::float8,
		       coalesce(sum(coalesce(u.input_tokens, 0) + coalesce(u.output_tokens, 0)), 0)::bigint,
		       coalesce(sum(u.cost_usd) FILTER (WHERE `+moneyPredicate+`), 0)::float8,
		       coalesce(sum(u.cost_usd) FILTER (WHERE u.billing IS NULL), 0)::float8,
		       coalesce(sum(u.cost_usd) FILTER (WHERE `+subscriptionPredicate+`), 0)::float8
		FROM stage_run_usage u
		JOIN runs r ON r.id = u.run_id
		GROUP BY u.model
		ORDER BY 1, 7 DESC, 2`, projectID, since)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []CostMetric{}
	for rows.Next() {
		var m CostMetric
		if err := rows.Scan(&m.Dimension, &m.Key, &m.Name, &m.Runs, &m.Reported,
			&m.Silent, &m.CostUSD, &m.Tokens,
			&m.APICostUSD, &m.UnspecifiedCostUSD, &m.SubscriptionCostUSD); err != nil {
			return nil, translate(err)
		}
		if m.Reported > 0 {
			m.AvgUSD = m.CostUSD / float64(m.Reported)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

// StatusMetrics — по статусам активного и прежних циклов, сгруппировано по
// ключу статуса: задача, начатая на прошлой версии, доходит по ней, а метрика
// про «аналитику» одна.
func (s *Store) StatusMetrics(ctx context.Context, q Querier, projectID uuid.UUID, since time.Time) ([]StatusMetric, error) {
	rows, err := q.Query(ctx, `
		WITH runs AS (
		    SELECT ws.key, ws.name,
		           extract(epoch FROM (sr.finished_at - sr.entered_at)) / 3600 AS hours,
		           sr.outcome, sr.attempt
		    FROM stage_runs sr
		    JOIN tasks t ON t.id = sr.task_id
		    JOIN workflow_statuses ws ON ws.id = sr.status_id
		    WHERE t.project_id = $1 AND sr.kind = 'stage'
		      AND sr.finished_at IS NOT NULL AND sr.finished_at >= $2
		), open_tasks AS (
		    SELECT ws.key, count(*) AS n
		    FROM tasks t JOIN workflow_statuses ws ON ws.id = t.status_id
		    WHERE t.project_id = $1 AND t.closed_at IS NULL
		    GROUP BY ws.key
		), expired AS (
		    SELECT ws.key, count(*) AS n
		    FROM task_events e
		    JOIN stage_runs sr ON sr.id = e.stage_run_id
		    JOIN workflow_statuses ws ON ws.id = sr.status_id
		    JOIN tasks t ON t.id = e.task_id
		    WHERE t.project_id = $1 AND e.type = 'lease.expired' AND e.created_at >= $2
		    GROUP BY ws.key
		), keys AS (
		    SELECT DISTINCT ws.key, ws.name, ws.position
		    FROM workflows w JOIN workflow_statuses ws ON ws.workflow_id = w.id
		    WHERE w.project_id = $1 AND w.state = 'active'
		    UNION
		    SELECT key, name, 1000 FROM runs
		    UNION
		    SELECT key, key, 1000 FROM open_tasks
		)
		SELECT k.key, k.name,
		       coalesce((SELECT n FROM open_tasks o WHERE o.key = k.key), 0),
		       (SELECT count(*) FROM runs r WHERE r.key = k.key),
		       coalesce((SELECT avg(hours) FROM runs r WHERE r.key = k.key), 0),
		       coalesce((SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY hours) FROM runs r WHERE r.key = k.key), 0),
		       coalesce((SELECT max(hours) FROM runs r WHERE r.key = k.key), 0),
		       (SELECT count(*) FROM runs r WHERE r.key = k.key AND r.outcome = 'rejected'),
		       (SELECT count(*) FROM runs r WHERE r.key = k.key AND r.attempt > 1),
		       coalesce((SELECT n FROM expired e WHERE e.key = k.key), 0)
		FROM (SELECT key, min(name) AS name, min(position) AS position FROM keys GROUP BY key) k
		ORDER BY k.position, k.key`, projectID, since)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []StatusMetric{}
	for rows.Next() {
		var m StatusMetric
		if err := rows.Scan(&m.StatusKey, &m.StatusName, &m.OpenTasks, &m.FinishedRuns,
			&m.AvgHours, &m.MedianHours, &m.MaxHours, &m.Rejections, &m.RepeatedRuns,
			&m.ExpiredLeases); err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

// GateMetrics — вердикты по гейтам за период. Считаются последние
// результаты каждого прогона по каждому гейту, а не каждая перепроверка:
// иначе гейт, который движок пересчитывает при каждом Preview, задавил бы
// остальные.
func (s *Store) GateMetrics(ctx context.Context, q Querier, projectID uuid.UUID, since time.Time) ([]GateMetric, error) {
	rows, err := q.Query(ctx, `
		SELECT wt.key, g.key, g.name, g.type,
		       count(*) FILTER (WHERE gr.status = 'passed'),
		       count(*) FILTER (WHERE gr.status = 'failed'),
		       count(*) FILTER (WHERE gr.status = 'waived'),
		       count(*) FILTER (WHERE gr.status = 'pending')
		FROM gate_results gr
		JOIN gates g ON g.id = gr.gate_id
		JOIN workflow_transitions wt ON wt.id = g.transition_id
		JOIN stage_runs sr ON sr.id = gr.stage_run_id
		JOIN tasks t ON t.id = sr.task_id
		WHERE t.project_id = $1 AND coalesce(gr.decided_at, gr.created_at) >= $2
		GROUP BY wt.key, g.key, g.name, g.type, wt.position, g.position
		ORDER BY wt.position, wt.key, g.position, g.key`, projectID, since)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []GateMetric{}
	for rows.Next() {
		var m GateMetric
		if err := rows.Scan(&m.Transition, &m.GateKey, &m.GateName, &m.Type,
			&m.Passed, &m.Failed, &m.Waived, &m.Pending); err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

func (s *Store) DriftMetrics(ctx context.Context, q Querier, projectID uuid.UUID, since time.Time) ([]DriftMetric, error) {
	rows, err := q.Query(ctx, `
		SELECT d.kind, d.severity,
		       count(*) FILTER (WHERE d.state IN ('open', 'acknowledged')),
		       count(*) FILTER (WHERE d.state = 'resolved'),
		       count(*) FILTER (WHERE d.state = 'dismissed')
		FROM task_drift_signals d
		JOIN tasks t ON t.id = d.task_id
		WHERE t.project_id = $1 AND d.detected_at >= $2
		GROUP BY d.kind, d.severity
		ORDER BY d.kind, d.severity`, projectID, since)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []DriftMetric{}
	for rows.Next() {
		var m DriftMetric
		if err := rows.Scan(&m.Kind, &m.Severity, &m.Open, &m.Resolved, &m.Dismiss); err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

func (s *Store) Throughput(ctx context.Context, q Querier, projectID uuid.UUID, since time.Time) (ThroughputMetric, error) {
	var m ThroughputMetric
	err := q.QueryRow(ctx, `
		SELECT
		    (SELECT count(*) FROM tasks WHERE project_id = $1 AND created_at >= $2),
		    (SELECT count(*) FROM tasks t JOIN workflow_statuses ws ON ws.id = t.status_id
		      WHERE t.project_id = $1 AND t.closed_at >= $2 AND ws.category = 'done'),
		    (SELECT count(*) FROM tasks t JOIN workflow_statuses ws ON ws.id = t.status_id
		      WHERE t.project_id = $1 AND t.closed_at >= $2 AND ws.category = 'cancelled'),
		    coalesce((SELECT avg(extract(epoch FROM (closed_at - created_at)) / 3600)
		      FROM tasks WHERE project_id = $1 AND closed_at >= $2), 0),
		    (SELECT count(*) FROM stage_runs sr JOIN tasks t ON t.id = sr.task_id
		      WHERE t.project_id = $1 AND sr.kind = 'review' AND sr.entered_at >= $2),
		    (SELECT count(*) FROM stage_runs sr JOIN tasks t ON t.id = sr.task_id
		      WHERE t.project_id = $1 AND sr.kind = 'review' AND sr.entered_at >= $2
		        AND sr.outcome = 'rejected')`,
		projectID, since,
	).Scan(&m.Created, &m.Closed, &m.Cancelled, &m.AvgLeadHours, &m.Reviews, &m.ReviewFails)
	return m, translate(err)
}
