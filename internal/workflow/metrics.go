package workflow

import (
	"context"
	"time"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
)

// Metrics — метрики цикла проекта за период: где задачи застревают, какие
// гейты валятся, сколько дрейфа и что с ним сделали.
type Metrics struct {
	Since      time.Time                 `json:"since"`
	Until      time.Time                 `json:"until"`
	Throughput postgres.ThroughputMetric `json:"throughput"`
	Statuses   []postgres.StatusMetric   `json:"statuses"`
	Gates      []postgres.GateMetric     `json:"gates"`
	Drift      []postgres.DriftMetric    `json:"drift"`
	// Стоимость по статусам, ролям и моделям (решение 0026).
	Cost []postgres.CostMetric `json:"cost"`
}

// ProjectMetrics считает всё по накопленным таблицам. period — окно назад от
// текущего момента; ноль означает 30 дней.
func (e *Engine) ProjectMetrics(ctx context.Context, projectKey string, period time.Duration) (Metrics, error) {
	if period <= 0 {
		period = 30 * 24 * time.Hour
	}
	if period > 5*365*24*time.Hour {
		return Metrics{}, apperr.InvalidInput("период больше пяти лет")
	}
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return Metrics{}, err
	}

	now := time.Now()
	out := Metrics{Since: now.Add(-period), Until: now}
	if out.Throughput, err = e.store.Throughput(ctx, q, project.ID, out.Since); err != nil {
		return out, err
	}
	if out.Statuses, err = e.store.StatusMetrics(ctx, q, project.ID, out.Since); err != nil {
		return out, err
	}
	if out.Gates, err = e.store.GateMetrics(ctx, q, project.ID, out.Since); err != nil {
		return out, err
	}
	if out.Drift, err = e.store.DriftMetrics(ctx, q, project.ID, out.Since); err != nil {
		return out, err
	}
	if out.Cost, err = e.store.CostMetrics(ctx, q, project.ID, out.Since); err != nil {
		return out, err
	}
	return out, nil
}
