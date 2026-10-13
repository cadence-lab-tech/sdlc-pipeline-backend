package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// RecordGateResult перезаписывает результат гейта в рамках попытки этапа.
//
// Таблица держит текущее состояние, а не историю: человек, передумавший с
// апрувом, должен увидеть новый результат, а не второй ряд. История прогонов
// остаётся в task_events — она append-only и для разбора годится лучше.
func (s *Store) RecordGateResult(ctx context.Context, q Querier, stageRunID, gateID, transitionID uuid.UUID, status string, details any, decidedBy *uuid.UUID) error {
	body := json.RawMessage(`{}`)
	if details != nil {
		b, err := json.Marshal(details)
		if err != nil {
			return translate(err)
		}
		body = b
	}
	_, err := q.Exec(ctx, `
		INSERT INTO gate_results (stage_run_id, gate_id, transition_id, attempt,
		                          status, details, decided_by, decided_at)
		VALUES ($1, $2, $3, 1, $4, $5, $6, CASE WHEN $6::uuid IS NULL THEN NULL ELSE now() END)
		ON CONFLICT (stage_run_id, gate_id, attempt) DO UPDATE
		SET status     = EXCLUDED.status,
		    details    = EXCLUDED.details,
		    decided_by = EXCLUDED.decided_by,
		    decided_at = EXCLUDED.decided_at`,
		stageRunID, gateID, transitionID, status, body, decidedBy)
	return translate(err)
}

// GateDecision — записанное решение по гейту, если оно есть.
// Возвращает nil без ошибки, когда решения ещё нет: для гейтов вроде
// human_approval это штатное "ждём человека", а не сбой.
func (s *Store) GateDecision(ctx context.Context, q Querier, stageRunID, gateID uuid.UUID) (*domain.GateResult, error) {
	var r domain.GateResult
	err := q.QueryRow(ctx, `
		SELECT id, stage_run_id, gate_id, transition_id, attempt, status, details,
		       decided_by, decided_at, created_at
		FROM gate_results WHERE stage_run_id = $1 AND gate_id = $2 AND attempt = 1`,
		stageRunID, gateID,
	).Scan(&r.ID, &r.StageRunID, &r.GateID, &r.TransitionID, &r.Attempt, &r.Status,
		&r.Details, &r.DecidedBy, &r.DecidedAt, &r.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, translate(err)
	}
	return &r, nil
}

func (s *Store) ListGateResults(ctx context.Context, q Querier, stageRunID uuid.UUID) ([]domain.GateResult, error) {
	rows, err := q.Query(ctx, `
		SELECT id, stage_run_id, gate_id, transition_id, attempt, status, details,
		       decided_by, decided_at, created_at
		FROM gate_results WHERE stage_run_id = $1 ORDER BY created_at`, stageRunID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.GateResult{}
	for rows.Next() {
		var r domain.GateResult
		if err := rows.Scan(&r.ID, &r.StageRunID, &r.GateID, &r.TransitionID, &r.Attempt,
			&r.Status, &r.Details, &r.DecidedBy, &r.DecidedAt, &r.CreatedAt); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// CountApprovals считает независимые апрувы по гейту в рамках попытки этапа,
// поставленные после момента since (нулевое время — все).
//
// Источник — лента событий, а не gate_results: там строка одна на гейт, и
// min_approvals больше единицы по ней не посчитать. Различаются именно акторы,
// поэтому один человек, нажавший апрув дважды, второго апрува не даёт. Граница
// since — последняя сдача артефакта на этапе: апрув, поставленный до неё,
// утверждал не то, что лежит в задаче сейчас (решение 0042).
func (s *Store) CountApprovals(ctx context.Context, q Querier, stageRunID, gateID uuid.UUID, since time.Time) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(DISTINCT actor_id)
		FROM task_events
		WHERE stage_run_id = $1
		  AND type = $2
		  AND payload->>'gate_id' = $3::text
		  AND payload->>'status' = 'passed'
		  AND created_at > $4`,
		stageRunID, domain.EventGateDecided, gateID.String(), since).Scan(&n)
	if err != nil {
		return 0, translate(err)
	}
	return n, nil
}
