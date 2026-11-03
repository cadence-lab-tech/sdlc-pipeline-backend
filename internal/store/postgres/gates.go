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

// countApprovalsSQL — запрос CountApprovals, вынесенный в константу, чтобы тест
// плана гонял ровно его текст (export_test.go).
//
// Тип события — литерал, а не параметр: частичный индекс
// task_events_gate_decided (000042) создан с условием type = 'gate.decided', и
// планировщик берёт его, только если условие запроса доказывает предикат.
// Подготовленное выражение pgx после пяти выполнений может перейти на общий
// план, а в нём `type = $2` предикат не доказывает — индекс бы молча
// перестал браться. Тот же приём у runClaimedSQL (usage.go, индекс 000032).
const countApprovalsSQL = `
		SELECT count(DISTINCT actor_id)
		FROM task_events
		WHERE stage_run_id = $1
		  AND type = '` + domain.EventGateDecided + `'
		  AND payload->>'gate_id' = $2::text
		  AND payload->>'status' = 'passed'
		  AND created_at > $3`

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
	err := q.QueryRow(ctx, countApprovalsSQL, stageRunID, gateID.String(), since).Scan(&n)
	if err != nil {
		return 0, translate(err)
	}
	return n, nil
}

// gateVerdictsSQL — запрос GateVerdictsByTask, вынесенный в константу, чтобы
// тест плана гонял ровно его текст (export_test.go).
//
// Пометка самоапрува (CLAB-393) читается из записанного решения: последнего
// gate.decided того же гейта в том же прогоне от того же актора, что стоит в
// gate_results.decided_by. Не пересчёт RunSelfBasis: после решения лента
// дополняется событиями того же актора (сдача артефакта), и пересчёт дал бы
// не то, что записано в gate.decided и показано в ленте. Условия на тип гейта
// и непустой decided_by лежат внутри подзапроса, чтобы для остальных строк
// он не читал ленту вовсе. Тип события — литерал по той же причине, что в
// countApprovalsSQL: частичный индекс task_events_gate_decided (000042).
const gateVerdictsSQL = `
		SELECT gr.stage_run_id, g.key, g.name, g.type, t.key, gr.status, gr.details,
		       p.handle, gr.decided_at, d.self_approval, d.self_basis
		FROM gate_results gr
		JOIN stage_runs sr           ON sr.id = gr.stage_run_id
		JOIN gates g                 ON g.id = gr.gate_id
		JOIN workflow_transitions t  ON t.id = gr.transition_id
		LEFT JOIN principals p       ON p.id = gr.decided_by
		LEFT JOIN LATERAL (
		    SELECT (e.payload->>'self_approval')::boolean AS self_approval,
		           e.payload->'self_basis'                AS self_basis
		    FROM task_events e
		    WHERE e.type = '` + domain.EventGateDecided + `'
		      AND g.type IN ('` + domain.GateHumanApproval + `', '` + domain.GateChecklist + `')
		      AND gr.decided_by IS NOT NULL
		      AND e.stage_run_id = gr.stage_run_id
		      AND e.actor_id = gr.decided_by
		      AND e.payload->>'gate_id' = gr.gate_id::text
		    ORDER BY e.created_at DESC, e.id DESC
		    LIMIT 1
		) d ON true
		WHERE sr.task_id = $1
		ORDER BY gr.stage_run_id, gr.created_at, g.position, g.key`

// GateVerdictsByTask — вердикты гейтов всех прогонов задачи одним запросом
// (CLAB-283): этапы и вложенные перепроверки, по образцу RunUsageByTask.
// Прогона без записей в ответе нет — пустой список дописывает тот, кто
// склеивает (CardStageRuns). Порядок внутри прогона не зависит от плана
// запроса: по времени записи, затем по позиции и ключу гейта. Пометка
// самоапрува (CLAB-393) приходит в том же запросе — см. gateVerdictsSQL.
func (s *Store) GateVerdictsByTask(ctx context.Context, q Querier, taskID uuid.UUID) (map[uuid.UUID][]domain.RunGateVerdict, error) {
	rows, err := q.Query(ctx, gateVerdictsSQL, taskID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := map[uuid.UUID][]domain.RunGateVerdict{}
	for rows.Next() {
		var (
			runID uuid.UUID
			v     domain.RunGateVerdict
			basis json.RawMessage
		)
		if err := rows.Scan(&runID, &v.GateKey, &v.GateName, &v.GateType, &v.TransitionKey,
			&v.Status, &v.Details, &v.DecidedByHandle, &v.DecidedAt, &v.SelfApproval, &basis); err != nil {
			return nil, translate(err)
		}
		if v.SelfApproval != nil {
			// Ключ self_basis всегда пишется вместе с self_approval; на
			// случай записи без него — пустой список, а не потеря признака.
			list := []string{}
			if len(basis) > 0 {
				if err := json.Unmarshal(basis, &list); err != nil {
					return nil, translate(err)
				}
			}
			v.SelfBasis = &list
		}
		out[runID] = append(out[runID], v)
	}
	return out, translate(rows.Err())
}
