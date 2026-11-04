package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
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
	return s.ListEventsByStatus(ctx, q, taskID, "", limit)
}

// ListEventsByStatus — лента задачи, суженная до прогонов одного статуса
// (CLAB-283). Пустой statusKey — вся лента, как у ListEvents. Попадают
// события прогонов этапа этого статуса и вложенных перепроверок (у них тот
// же статус, что у родителя; parent_run_id проверяется отдельно, чтобы лента
// не зависела от этого соглашения). Событий без прогона (task.created и
// подобных) в суженной ленте нет. Предел применяется после фильтра:
// heartbeat'ы одного статуса не вытесняют события другого. Незнакомый ключ —
// пустая лента.
func (s *Store) ListEventsByStatus(ctx context.Context, q Querier, taskID uuid.UUID, statusKey string, limit int) ([]domain.TaskEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := q.Query(ctx, `
		SELECT e.id, e.task_id, e.stage_run_id, e.type, e.actor_id, p.handle,
		       e.payload, e.created_at
		FROM task_events e
		LEFT JOIN principals p ON p.id = e.actor_id
		WHERE e.task_id = $1
		  AND ($3 = '' OR e.stage_run_id IN (
		        SELECT sr.id
		        FROM stage_runs sr
		        JOIN workflow_statuses ws ON ws.id = sr.status_id
		        LEFT JOIN stage_runs par  ON par.id = sr.parent_run_id
		        LEFT JOIN workflow_statuses pws ON pws.id = par.status_id
		        WHERE sr.task_id = $1 AND (ws.key = $3 OR pws.key = $3)))
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT $2`, taskID, limit, statusKey)
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

// RunExitsByTask — выход каждого завершённого прогона задачи одним запросом
// (CLAB-283): переход, целевой статус и комментарий берутся из события
// task.advanced / task.rejected / task.cancelled, которое движок пишет на
// прогон, закончившийся переходом, или task.moved — переводом в статус
// (CLAB-408: kind moved, перехода нет, комментарий — причина). Если таких событий у прогона несколько,
// берётся последнее. Прогона без выхода в ответе нет.
func (s *Store) RunExitsByTask(ctx context.Context, q Querier, taskID uuid.UUID) (map[uuid.UUID]domain.RunExit, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (e.stage_run_id)
		       e.stage_run_id, e.type, e.payload, p.handle, e.created_at
		FROM task_events e
		LEFT JOIN principals p ON p.id = e.actor_id
		WHERE e.task_id = $1 AND e.stage_run_id IS NOT NULL AND e.type IN ($2, $3, $4, $5)
		ORDER BY e.stage_run_id, e.created_at DESC, e.id DESC`,
		taskID, domain.EventTaskAdvanced, domain.EventTaskRejected, domain.EventTaskCancelled,
		domain.EventTaskMoved)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := map[uuid.UUID]domain.RunExit{}
	for rows.Next() {
		var (
			runID   uuid.UUID
			typ     string
			payload json.RawMessage
			x       domain.RunExit
		)
		if err := rows.Scan(&runID, &typ, &payload, &x.ByHandle, &x.At); err != nil {
			return nil, translate(err)
		}
		x.Kind = strings.TrimPrefix(typ, "task.")
		var body struct {
			Transition string `json:"transition"`
			To         string `json:"to"`
			Comment    string `json:"comment"`
			// У перевода (task.moved) перехода нет, а причина — reason.
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(payload, &body)
		x.Transition, x.ToStatus = body.Transition, body.To
		if body.Comment == "" {
			body.Comment = body.Reason
		}
		if body.Comment != "" {
			c := body.Comment
			x.Comment = &c
		}
		out[runID] = x
	}
	return out, translate(rows.Err())
}

// AppendTeamEvent пишет в ленту аудита арендатора. Отдельная лента нужна там,
// где событию негде остаться на задаче: удаление проекта уносит его task_events
// с собой, а команда-владелец остаётся (CLAB-10).
func (s *Store) AppendTeamEvent(ctx context.Context, q Querier, teamID uuid.UUID, eventType string, actor uuid.UUID, payload any) error {
	body := json.RawMessage(`{}`)
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return translate(err)
		}
		body = b
	}
	_, err := q.Exec(ctx, `
		INSERT INTO team_events (team_id, type, actor_id, payload, created_at)
		VALUES ($1, $2, nullif($3, '00000000-0000-0000-0000-000000000000'::uuid), $4, clock_timestamp())`,
		teamID, eventType, actor, body)
	return translate(err)
}

// ListTeamEvents — лента аудита команды, свежие сверху.
func (s *Store) ListTeamEvents(ctx context.Context, q Querier, teamID uuid.UUID, limit int) ([]domain.TeamEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := q.Query(ctx, `
		SELECT e.id, e.team_id, e.type, e.actor_id, p.handle, e.payload, e.created_at
		FROM team_events e
		LEFT JOIN principals p ON p.id = e.actor_id
		WHERE e.team_id = $1
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT $2`, teamID, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.TeamEvent{}
	for rows.Next() {
		var e domain.TeamEvent
		if err := rows.Scan(&e.ID, &e.TeamID, &e.Type, &e.ActorID, &e.ActorHandle,
			&e.Payload, &e.CreatedAt); err != nil {
			return nil, translate(err)
		}
		out = append(out, e)
	}
	return out, translate(rows.Err())
}

// runSelfBasisSQL — запрос RunSelfBasis, вынесенный в константу, чтобы тест
// плана гонял ровно его текст (export_test.go). Типы событий — литералы, а
// не параметры, по той же причине, что в countApprovalsSQL: аренда берёт
// частичный индекс task_events_stage_claimed (000032), который
// планировщик в общем плане с `type = $n` не доказывает. Сдача артефакта
// своего индекса не имеет: она читается по ленте задачи (task_id,
// created_at), сжатой снизу входом в этап — события прогона раньше него не
// бывают.
const runSelfBasisSQL = `
		SELECT
		  EXISTS (
		    SELECT 1 FROM task_events
		    WHERE stage_run_id = $2 AND actor_id = $3
		      AND type = '` + domain.EventStageClaimed + `'),
		  EXISTS (
		    SELECT 1 FROM task_events
		    WHERE task_id = $1 AND created_at >= $4
		      AND stage_run_id = $2 AND actor_id = $3
		      AND type = '` + domain.EventArtifactSubmitted + `')`

// RunSelfBasis — на каком основании актор «свой» в прогоне (CLAB-380): брал
// ли он аренду этого прогона и сдавал ли в нём артефакты. Источник — события
// ленты, а не claimed_by и artifacts: release, протухание аренды и новая
// версия артефакта прежних следов не стирают в ленте, а в этих таблицах
// стирают или прячут. entered — вход в этап (stage_runs.entered_at), нижняя
// граница чтения ленты.
func (s *Store) RunSelfBasis(ctx context.Context, q Querier, taskID, runID, actor uuid.UUID, entered time.Time) (lease, artifact bool, err error) {
	err = q.QueryRow(ctx, runSelfBasisSQL, taskID, runID, actor, entered).Scan(&lease, &artifact)
	if err != nil {
		return false, false, translate(err)
	}
	return lease, artifact, nil
}
