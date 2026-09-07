package workflow

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
)

// DetectDrift прогоняет детектор по свежему прогону индексации.
//
// Сразу после индексации, а не по расписанию: задача, которая проехала мимо
// расхождения, узнает о нём на следующем этапе, когда чинить дороже. Оба
// запроса идемпотентны (частичный уникальный индекс по открытым сигналам),
// поэтому повтор прохода не плодит дубликатов.
func (e *Engine) DetectDrift(ctx context.Context, run postgres.IndexRun, actor uuid.UUID) ([]postgres.NewDriftSignal, error) {
	var signals []postgres.NewDriftSignal

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		baseline, err := e.store.DetectBaselineDrift(ctx, tx, run.ID)
		if err != nil {
			return err
		}
		overlap, err := e.store.DetectTaskOverlap(ctx, tx, run.ProjectID)
		if err != nil {
			return err
		}
		signals = append(baseline, overlap...)

		for _, s := range signals {
			if err := e.store.AppendEvent(ctx, tx, s.TaskID, nil, domain.EventDriftDetected, actor,
				map[string]any{
					"signal_id":  s.ID.String(),
					"kind":       s.Kind,
					"severity":   s.Severity,
					"repository": s.Repository,
					"commit":     run.CommitSHA,
					"overlap":    s.Overlap,
				}); err != nil {
				return err
			}
		}
		return nil
	})

	return signals, err
}

// DetectOverlaps ищет пересечения задач вне привязки к прогону индексации.
//
// Пересечение возникает не от движения main, а от того, что вторая задача
// объявила ту же зону изменений: индексация тут ни при чём, и ждать её было
// бы неверно.
func (e *Engine) DetectOverlaps(ctx context.Context, actor uuid.UUID) ([]postgres.NewDriftSignal, error) {
	projects, err := e.store.ProjectsWithOpenTasks(ctx, e.store.DB())
	if err != nil {
		return nil, err
	}

	var all []postgres.NewDriftSignal
	for _, projectID := range projects {
		err := e.store.InTx(ctx, func(tx pgx.Tx) error {
			signals, err := e.store.DetectTaskOverlap(ctx, tx, projectID)
			if err != nil {
				return err
			}
			for _, s := range signals {
				if err := e.store.AppendEvent(ctx, tx, s.TaskID, nil, domain.EventDriftDetected, actor,
					map[string]any{
						"signal_id": s.ID.String(), "kind": s.Kind,
						"severity": s.Severity, "overlap": s.Overlap,
					}); err != nil {
					return err
				}
			}
			all = append(all, signals...)
			return nil
		})
		if err != nil {
			return all, err
		}
	}
	return all, nil
}
