package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Факты движка для строк списка задач (CLAB-363): блокирующие, последнее
// движение, текущие прогоны, решения по гейтам, события аренды и стоимость.
// Каждый факт — один запрос на всю пачку задач, а не на задачу: число
// запросов списка не растёт с размером страницы. Правила поверх фактов
// (причины внимания, текущий прогон, предел этапа) живут в движке.

// TaskFactsInput — какие задачи и что про них читать. All — все задачи
// пачки: последнее движение и стоимость есть у любой. Live — нетерминальные:
// у них прогоны и блокирующие. Parents — задачи с детьми: у них поля
// поддерева.
type TaskFactsInput struct {
	All     []uuid.UUID
	Live    []uuid.UUID
	Parents []uuid.UUID
}

// TaskFacts — факты одной задачи.
type TaskFacts struct {
	// Ключи источников связи blocks, чей статус не в категории done, по
	// возрастанию номера. Отменённая блокирующая остаётся: так считает и
	// гейт dependencies_closed по умолчанию.
	BlockedBy []string
	// Время последнего события движения (domain.MovementEvents), без событий —
	// время заведения задачи.
	LastMovedAt time.Time
	// Максимум LastMovedAt по задаче и всему поддереву; только у Parents.
	SubtreeLastMovedAt *time.Time
	// Последняя попытка этапа на текущем статусе — та же строка, что у
	// CurrentStageRun; может быть закрытой, если данные неполны.
	StageRun *domain.StageRun
	// Первая по входу открытая перепроверка этой попытки этапа.
	ReviewRun *domain.StageRun
	// Имена ролей прогонов: ключ — role_id.
	RoleNames map[uuid.UUID]string
	// Последнее из событий stage.claimed, stage.released, lease.expired
	// прогона, ожидающего исполнителя (pending), — для перепроверки, если
	// она открыта, иначе для этапа.
	LeaseEvent *LeaseEvent
	// Решения по гейтам попытки этапа: отклонения с решающим и снятия.
	GateDecisions []RunGateDecision
	// Стоимость собственных прогонов; nil — строк учёта нет.
	Cost *domain.TaskCost
	// Стоимость поддерева вместе с задачей; только у Parents.
	SubtreeCostUSD *float64
}

// LeaseEvent — событие аренды прогона: тип, время и держатель (у
// lease.expired — из payload события, у остальных — пусто).
type LeaseEvent struct {
	Type   string
	At     time.Time
	Holder *string
}

// RunGateDecision — записанное решение по гейту попытки этапа с ключами
// перехода и гейта.
type RunGateDecision struct {
	GateID          uuid.UUID
	GateKey         string
	GateType        string
	TransitionKey   string
	Status          string
	Details         json.RawMessage
	DecidedBy       *uuid.UUID
	DecidedByHandle *string
	DecidedAt       *time.Time
}

// TaskFacts читает факты пачки задач. Задача без фактов всё равно получает
// запись: LastMovedAt у неё — время заведения.
func (s *Store) TaskFacts(ctx context.Context, q Querier, in TaskFactsInput) (map[uuid.UUID]*TaskFacts, error) {
	out := make(map[uuid.UUID]*TaskFacts, len(in.All))
	for _, id := range in.All {
		out[id] = &TaskFacts{BlockedBy: []string{}, RoleNames: map[uuid.UUID]string{}}
	}
	if len(in.All) == 0 {
		return out, nil
	}
	parents := in.Parents
	if parents == nil {
		parents = []uuid.UUID{}
	}
	if err := s.factsMoved(ctx, q, in.All, parents, out); err != nil {
		return nil, err
	}
	if err := s.factsCost(ctx, q, in.All, parents, out); err != nil {
		return nil, err
	}
	if len(in.Live) == 0 {
		return out, nil
	}
	if err := s.factsBlocked(ctx, q, in.Live, out); err != nil {
		return nil, err
	}
	if err := s.factsRuns(ctx, q, in.Live, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) factsBlocked(ctx context.Context, q Querier, ids []uuid.UUID, out map[uuid.UUID]*TaskFacts) error {
	rows, err := q.Query(ctx, `
		SELECT l.to_task_id, src.key
		FROM task_links l
		JOIN tasks src ON src.id = l.from_task_id
		JOIN workflow_statuses ws ON ws.id = src.status_id
		WHERE l.to_task_id = ANY($1) AND l.rel = $2 AND ws.category <> 'done'
		ORDER BY l.to_task_id, src.seq`, ids, domain.TaskLinkBlocks)
	if err != nil {
		return translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return translate(err)
		}
		if f := out[id]; f != nil {
			f.BlockedBy = append(f.BlockedBy, key)
		}
	}
	return translate(rows.Err())
}

// factsMoved — последнее движение задачи и поддерева. Событие ищется
// боковым подзапросом по индексу (task_id, created_at DESC): с конца ленты
// задачи до первого события движения, heartbeat'ы пропускаются по пути.
// Рекурсия по детям идёт только от задач с детьми.
func (s *Store) factsMoved(ctx context.Context, q Querier, ids, parents []uuid.UUID, out map[uuid.UUID]*TaskFacts) error {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE sub(root, id) AS (
			SELECT id, id FROM tasks WHERE id = ANY($1)
			UNION ALL
			SELECT s.root, t.id FROM tasks t JOIN sub s ON t.parent_id = s.id
			WHERE s.root = ANY($2)
		),
		moved AS (
			SELECT s.root, s.id, coalesce(lm.at, t.created_at) AS at
			FROM sub s
			JOIN tasks t ON t.id = s.id
			LEFT JOIN LATERAL (
				SELECT e.created_at AS at FROM task_events e
				WHERE e.task_id = s.id AND e.type = ANY($3)
				ORDER BY e.created_at DESC LIMIT 1) lm ON true
		)
		SELECT root, max(at) FILTER (WHERE id = root), max(at)
		FROM moved GROUP BY root`, ids, parents, domain.MovementEvents)
	if err != nil {
		return translate(err)
	}
	defer rows.Close()
	isParent := make(map[uuid.UUID]bool, len(parents))
	for _, id := range parents {
		isParent[id] = true
	}
	for rows.Next() {
		var id uuid.UUID
		var own, subtree time.Time
		if err := rows.Scan(&id, &own, &subtree); err != nil {
			return translate(err)
		}
		f := out[id]
		if f == nil {
			continue
		}
		f.LastMovedAt = own
		if isParent[id] {
			f.SubtreeLastMovedAt = &subtree
		}
	}
	return translate(rows.Err())
}

// factsCost — стоимость по stage_runs(task_id) и stage_run_usage(run_id),
// деньги и подписка — те же предикаты, что у TaskUsage.
func (s *Store) factsCost(ctx context.Context, q Querier, ids, parents []uuid.UUID, out map[uuid.UUID]*TaskFacts) error {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE sub(root, id) AS (
			SELECT id, id FROM tasks WHERE id = ANY($1)
			UNION ALL
			SELECT s.root, t.id FROM tasks t JOIN sub s ON t.parent_id = s.id
			WHERE s.root = ANY($2)
		)
		SELECT s.root,
		       count(u.id) FILTER (WHERE s.id = s.root)::int,
		       coalesce(sum(u.cost_usd) FILTER (WHERE s.id = s.root), 0)::float8,
		       coalesce(sum(u.cost_usd) FILTER (WHERE s.id = s.root AND `+moneyPredicate+`), 0)::float8,
		       coalesce(sum(u.cost_usd) FILTER (WHERE s.id = s.root AND `+subscriptionPredicate+`), 0)::float8,
		       coalesce(sum(u.cost_usd), 0)::float8
		FROM sub s
		JOIN stage_runs sr ON sr.task_id = s.id
		JOIN stage_run_usage u ON u.run_id = sr.id
		GROUP BY s.root`, ids, parents)
	if err != nil {
		return translate(err)
	}
	defer rows.Close()
	isParent := make(map[uuid.UUID]bool, len(parents))
	for _, id := range parents {
		isParent[id] = true
	}
	zero := 0.0
	for _, id := range parents {
		if f := out[id]; f != nil {
			z := zero
			f.SubtreeCostUSD = &z
		}
	}
	for rows.Next() {
		var id uuid.UUID
		var c domain.TaskCost
		var subtree float64
		if err := rows.Scan(&id, &c.Reports, &c.CostUSD, &c.APICostUSD, &c.SubscriptionCostUSD, &subtree); err != nil {
			return translate(err)
		}
		f := out[id]
		if f == nil {
			continue
		}
		if c.Reports > 0 {
			cost := c
			f.Cost = &cost
		}
		if isParent[id] {
			v := subtree
			f.SubtreeCostUSD = &v
		}
	}
	return translate(rows.Err())
}

// factsRuns — прогоны, имена ролей, события аренды и решения по гейтам.
func (s *Store) factsRuns(ctx context.Context, q Querier, ids []uuid.UUID, out map[uuid.UUID]*TaskFacts) error {
	// Последняя попытка этапа на текущем статусе задачи — как CurrentStageRun.
	rows, err := q.Query(ctx, stageRunSelect+`
		JOIN tasks tk ON tk.id = sr.task_id AND tk.status_id = sr.status_id
		WHERE sr.task_id = ANY($1) AND sr.kind = 'stage'
		ORDER BY sr.task_id, sr.attempt DESC`, ids)
	if err != nil {
		return translate(err)
	}
	stageByRun := map[uuid.UUID]*TaskFacts{}
	stageIDs := []uuid.UUID{}
	roleIDs := []uuid.UUID{}
	for rows.Next() {
		r, err := scanStageRun(rows)
		if err != nil {
			rows.Close()
			return translate(err)
		}
		f := out[r.TaskID]
		if f == nil || f.StageRun != nil {
			continue
		}
		run := r
		f.StageRun = &run
		stageByRun[r.ID] = f
		stageIDs = append(stageIDs, r.ID)
		if r.RoleID != nil {
			roleIDs = append(roleIDs, *r.RoleID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return translate(err)
	}
	if len(stageIDs) == 0 {
		return nil
	}

	rows, err = q.Query(ctx, stageRunSelect+`
		WHERE sr.parent_run_id = ANY($1) AND sr.kind = 'review'
		  AND sr.state IN ('pending', 'claimed', 'in_progress', 'submitted')
		ORDER BY sr.entered_at, sr.id`, stageIDs)
	if err != nil {
		return translate(err)
	}
	for rows.Next() {
		r, err := scanStageRun(rows)
		if err != nil {
			rows.Close()
			return translate(err)
		}
		if r.ParentRunID == nil {
			continue
		}
		f := stageByRun[*r.ParentRunID]
		if f == nil || f.ReviewRun != nil {
			continue
		}
		run := r
		f.ReviewRun = &run
		if r.RoleID != nil {
			roleIDs = append(roleIDs, *r.RoleID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return translate(err)
	}

	if len(roleIDs) > 0 {
		names := map[uuid.UUID]string{}
		rows, err = q.Query(ctx, `SELECT id, name FROM roles WHERE id = ANY($1)`, roleIDs)
		if err != nil {
			return translate(err)
		}
		for rows.Next() {
			var id uuid.UUID
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return translate(err)
			}
			names[id] = name
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return translate(err)
		}
		for _, f := range stageByRun {
			for _, r := range []*domain.StageRun{f.StageRun, f.ReviewRun} {
				if r != nil && r.RoleID != nil {
					if name, ok := names[*r.RoleID]; ok {
						f.RoleNames[*r.RoleID] = name
					}
				}
			}
		}
	}

	if err := s.factsLeaseEvents(ctx, q, stageByRun); err != nil {
		return err
	}
	return s.factsGateDecisions(ctx, q, stageIDs, stageByRun)
}

// factsLeaseEvents — последнее событие аренды у прогонов, ожидающих
// исполнителя. Живой и забранный прогон своё состояние несёт в строке, а
// «аренда протухла» после прохода воркера осталась только в ленте. Боковой
// подзапрос идёт по индексу (task_id, created_at DESC) с конца ленты задачи.
func (s *Store) factsLeaseEvents(ctx context.Context, q Querier, stageByRun map[uuid.UUID]*TaskFacts) error {
	var tasks, runs []uuid.UUID
	byRun := map[uuid.UUID]*TaskFacts{}
	for _, f := range stageByRun {
		r := f.ReviewRun
		if r == nil {
			r = f.StageRun
		}
		if r.State != domain.StagePending || r.FinishedAt != nil {
			continue
		}
		tasks = append(tasks, r.TaskID)
		runs = append(runs, r.ID)
		byRun[r.ID] = f
	}
	if len(runs) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `
		SELECT r.run_id, x.type, x.created_at, x.holder
		FROM unnest($1::uuid[], $2::uuid[]) AS r(task_id, run_id)
		CROSS JOIN LATERAL (
			SELECT e.type, e.created_at, nullif(e.payload->>'holder', '') AS holder
			FROM task_events e
			WHERE e.task_id = r.task_id AND e.stage_run_id = r.run_id AND e.type = ANY($3)
			ORDER BY e.created_at DESC LIMIT 1) x`,
		tasks, runs, []string{domain.EventStageClaimed, domain.EventStageReleased, domain.EventLeaseExpired})
	if err != nil {
		return translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var runID uuid.UUID
		var ev LeaseEvent
		if err := rows.Scan(&runID, &ev.Type, &ev.At, &ev.Holder); err != nil {
			return translate(err)
		}
		if f := byRun[runID]; f != nil {
			e := ev
			f.LeaseEvent = &e
		}
	}
	return translate(rows.Err())
}

// factsGateDecisions — отклонения с решающим и снятия по гейтам попыток
// этапа. Вердикт вычислителя (failed без decided_by) пишется при неудачной
// попытке перехода и устаревает с данными — отклонением он не считается.
func (s *Store) factsGateDecisions(ctx context.Context, q Querier, stageIDs []uuid.UUID, stageByRun map[uuid.UUID]*TaskFacts) error {
	rows, err := q.Query(ctx, `
		SELECT gr.stage_run_id, gr.gate_id, g.key, g.type, tr.key, gr.status, gr.details,
		       gr.decided_by, pr.handle, gr.decided_at
		FROM gate_results gr
		JOIN gates g ON g.id = gr.gate_id
		JOIN workflow_transitions tr ON tr.id = gr.transition_id
		LEFT JOIN principals pr ON pr.id = gr.decided_by
		WHERE gr.stage_run_id = ANY($1) AND gr.attempt = 1
		  AND ((gr.status = 'failed' AND gr.decided_by IS NOT NULL) OR gr.status = 'waived')
		ORDER BY gr.stage_run_id, tr.position, tr.key, g.position, g.key`, stageIDs)
	if err != nil {
		return translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var runID uuid.UUID
		var d RunGateDecision
		if err := rows.Scan(&runID, &d.GateID, &d.GateKey, &d.GateType, &d.TransitionKey, &d.Status,
			&d.Details, &d.DecidedBy, &d.DecidedByHandle, &d.DecidedAt); err != nil {
			return translate(err)
		}
		if f := stageByRun[runID]; f != nil {
			f.GateDecisions = append(f.GateDecisions, d)
		}
	}
	return translate(rows.Err())
}
