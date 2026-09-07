package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

// AppendEvent пишет в append-only ленту задачи. Лента — единственный источник
// правды для таймлайна в UI и для разбора "почему задача откатилась", поэтому
// событие пишется в той же транзакции, что и само изменение.
func (s *Store) AppendEvent(ctx context.Context, q Querier, taskID uuid.UUID, stageRunID *uuid.UUID, eventType string, actor uuid.UUID, payload any) error {
	body := json.RawMessage(`{}`)
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return translate(err)
		}
		body = b
	}
	// clock_timestamp(), а не now(): now() — это время начала транзакции, и
	// события, записанные в одной транзакции (сдача артефакта, закрытие
	// перепроверки, переход), получили бы одинаковую метку и легли бы в
	// ленту в случайном порядке.
	_, err := q.Exec(ctx, `
		INSERT INTO task_events (task_id, stage_run_id, type, actor_id, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, clock_timestamp())`,
		taskID, stageRunID, eventType, actor, body)
	return translate(err)
}

func (s *Store) ListEvents(ctx context.Context, q Querier, taskID uuid.UUID, limit int) ([]domain.TaskEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := q.Query(ctx, `
		SELECT e.id, e.task_id, e.stage_run_id, e.type, e.actor_id, p.handle,
		       e.payload, e.created_at
		FROM task_events e
		LEFT JOIN principals p ON p.id = e.actor_id
		WHERE e.task_id = $1
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.TaskEvent{}
	for rows.Next() {
		var e domain.TaskEvent
		if err := rows.Scan(&e.ID, &e.TaskID, &e.StageRunID, &e.Type, &e.ActorID,
			&e.ActorHandle, &e.Payload, &e.CreatedAt); err != nil {
			return nil, translate(err)
		}
		out = append(out, e)
	}
	return out, translate(rows.Err())
}
