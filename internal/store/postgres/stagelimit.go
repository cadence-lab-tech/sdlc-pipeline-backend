package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TimeLimitedRun — открытый этап, вошедший в статус переходом с гейтом
// stage_time_limit, ещё не отмеченный просроченным и не снятый человеком
// (CLAB-221). Длительность лежит строкой в config гейта: срок считает
// движок, тем же разбором, что валидатор и отказ захвата.
type TimeLimitedRun struct {
	RunID         uuid.UUID
	TaskID        uuid.UUID
	EnteredAt     time.Time
	StatusKey     string
	TransitionKey string
	GateID        uuid.UUID
	GateKey       string
	GateConfig    json.RawMessage
}

// TimeLimitedRuns — кандидаты на отметку просрочки для прохода воркера.
//
// Предела по сроку в SQL нет: длительность — строка формата Go в jsonb, и
// интервал Postgres разобрал бы её по-своему (единицы, «1d»). Выборка и без
// того мала — это открытые этапы с переходом входа и гейтом предела, а не
// история прогонов; частичный индекс stage_runs_time_limited держит её
// дешёвой. Снятый человеком предел (waived на этом прогоне) в выборку не
// попадает: события о нём не будет.
func (s *Store) TimeLimitedRuns(ctx context.Context, q Querier) ([]TimeLimitedRun, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (sr.id)
		       sr.id, sr.task_id, sr.entered_at, ws.key, tr.key, g.id, g.key, g.config
		FROM stage_runs sr
		JOIN workflow_statuses ws    ON ws.id = sr.status_id
		JOIN workflow_transitions tr ON tr.id = sr.entered_via_transition_id
		JOIN gates g ON g.transition_id = tr.id AND g.type = 'stage_time_limit'
		WHERE sr.kind = 'stage' AND sr.finished_at IS NULL
		  AND sr.entered_via_transition_id IS NOT NULL AND sr.overdue_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM gate_results gr
		      WHERE gr.stage_run_id = sr.id AND gr.gate_id = g.id AND gr.status = 'waived')
		ORDER BY sr.id, g.position, g.key`)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []TimeLimitedRun{}
	for rows.Next() {
		var r TimeLimitedRun
		if err := rows.Scan(&r.RunID, &r.TaskID, &r.EnteredAt, &r.StatusKey, &r.TransitionKey,
			&r.GateID, &r.GateKey, &r.GateConfig); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// MarkStageOverdue ставит прогону отметку просрочки. false — отметка уже
// стоит или этап закрылся: условие в WHERE и есть однократность события,
// даже если проходов два одновременно.
func (s *Store) MarkStageOverdue(ctx context.Context, tx pgx.Tx, runID uuid.UUID, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE stage_runs SET overdue_at = $2
		WHERE id = $1 AND overdue_at IS NULL AND finished_at IS NULL`, runID, at)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() == 1, nil
}
