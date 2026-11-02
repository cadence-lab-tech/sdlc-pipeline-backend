package workflow

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Строка списка задач для «Обзора» (CLAB-363, решение 0070): текущий
// прогон, блокирующие, последнее движение, стоимость и причины внимания.
// Факты читаются пакетом на страницу (postgres.TaskFacts), графы версий
// цикла — один раз на версию, гейты — один раз на задачу в работе и только
// у переходов вперёд с человеческим гейтом: из одного вердикта берутся и
// awaiting_approval, и awaits_human.

// AttentionScanLimit — потолок незавершённых задач в выборке режима
// фильтров по причинам: причины считаются по всей выборке, и ручка не должна
// становиться ценой проекта целиком. Переменная, а не константа, — чтобы
// тест проверил потолок на малой выборке.
var AttentionScanLimit = 1000

// TaskListPage — страница списка задач с добавочными полями строк.
type TaskListPage struct {
	Items     []domain.TaskListItem
	Total     int
	Overviews map[uuid.UUID]TaskOverview
}

// ListTaskOverviews — страница списка задач. Без фильтров по причинам —
// прежняя страница (ListTaskItems), добавочные поля считаются только для
// неё. С фильтрами — причины по всей выборке, затем листание: total и первые
// строки зоны «Обзора» точные, а не дочищены на клиенте.
func (e *Engine) ListTaskOverviews(ctx context.Context, q postgres.Querier, projectID uuid.UUID, f postgres.ListTasksFilter, attention AttentionFilter, viewer uuid.UUID, now time.Time) (TaskListPage, error) {
	if attention.Active() {
		return e.listByAttention(ctx, q, projectID, f, attention, viewer, now)
	}
	items, total, err := e.store.ListTaskItems(ctx, q, projectID, f)
	if err != nil {
		return TaskListPage{}, err
	}
	overviews, err := e.TaskOverviews(ctx, q, items, viewer, now)
	if err != nil {
		return TaskListPage{}, err
	}
	return TaskListPage{Items: items, Total: total, Overviews: overviews}, nil
}

// listByAttention — режим фильтров по причинам: выборка по прочим фильтрам
// целиком (ключи, без полей задачи), причины незавершённых задач, фильтр,
// листание в порядке списка. Терминальные задачи причин не имеют: подходят,
// если фильтр пропускает задачу без причин, и в потолок не идут.
func (e *Engine) listByAttention(ctx context.Context, q postgres.Querier, projectID uuid.UUID, f postgres.ListTasksFilter, attention AttentionFilter, viewer uuid.UUID, now time.Time) (TaskListPage, error) {
	refs, err := e.store.ListTaskRefs(ctx, q, projectID, f)
	if err != nil {
		return TaskListPage{}, err
	}
	var live []uuid.UUID
	for _, r := range refs {
		if !r.Terminal {
			live = append(live, r.ID)
		}
	}
	if len(live) > AttentionScanLimit {
		return TaskListPage{}, apperr.InvalidInput(
			"фильтр по причинам считает не больше %d незавершённых задач, в выборке %d; "+
				"сузьте её параметрами in_work, parent, type или q", AttentionScanLimit, len(live))
	}
	liveItems, err := e.store.TaskItemsByIDs(ctx, q, live)
	if err != nil {
		return TaskListPage{}, err
	}
	overviews, err := e.TaskOverviews(ctx, q, liveItems, viewer, now)
	if err != nil {
		return TaskListPage{}, err
	}
	byID := make(map[uuid.UUID]domain.TaskListItem, len(liveItems))
	for _, it := range liveItems {
		byID[it.ID] = it
	}
	terminalMatch := attention.Match(nil)
	var selected []postgres.TaskRef
	for _, r := range refs {
		if r.Terminal {
			if terminalMatch {
				selected = append(selected, r)
			}
			continue
		}
		if ov, ok := overviews[r.ID]; ok && attention.Match(Codes(ov.Attention)) {
			selected = append(selected, r)
		}
	}
	total := len(selected)
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	from := min(max(f.Offset, 0), total)
	page := selected[from:min(from+limit, total)]

	var terminalIDs []uuid.UUID
	for _, r := range page {
		if r.Terminal {
			terminalIDs = append(terminalIDs, r.ID)
		}
	}
	if len(terminalIDs) > 0 {
		done, err := e.store.TaskItemsByIDs(ctx, q, terminalIDs)
		if err != nil {
			return TaskListPage{}, err
		}
		more, err := e.TaskOverviews(ctx, q, done, viewer, now)
		if err != nil {
			return TaskListPage{}, err
		}
		for _, it := range done {
			byID[it.ID] = it
			overviews[it.ID] = more[it.ID]
		}
	}
	items := make([]domain.TaskListItem, 0, len(page))
	for _, r := range page {
		if it, ok := byID[r.ID]; ok {
			items = append(items, it)
		}
	}
	return TaskListPage{Items: items, Total: total, Overviews: overviews}, nil
}

// TaskOverview — добавочные поля строки списка.
type TaskOverview struct {
	AwaitingApproval   bool
	BlockedBy          []string
	LastMovedAt        time.Time
	SubtreeLastMovedAt *time.Time
	CurrentRun         *CurrentRun
	Attention          []Attention
	Cost               *domain.TaskCost
	SubtreeCostUSD     *float64
}

// CurrentRun — прогон, в котором сейчас можно работать: открытая
// перепроверка, если она есть, иначе текущая попытка этапа (исполнитель
// этапа в gating работать не может). Attempt у перепроверки — попытка
// родительского этапа, своя — ReviewAttempt.
type CurrentRun struct {
	ID              uuid.UUID   `json:"id"`
	Kind            string      `json:"kind"`
	State           string      `json:"state"`
	Attempt         int         `json:"attempt"`
	ReviewAttempt   *int        `json:"review_attempt,omitempty"`
	StatusKey       string      `json:"status_key"`
	RoleKey         *string     `json:"role_key"`
	RoleName        *string     `json:"role_name"`
	ClaimedByHandle *string     `json:"claimed_by_handle"`
	ClaimedAt       *time.Time  `json:"claimed_at"`
	HeartbeatAt     *time.Time  `json:"heartbeat_at"`
	LeaseExpiresAt  *time.Time  `json:"lease_expires_at"`
	LeaseAlive      bool        `json:"lease_alive"`
	EnteredAt       time.Time   `json:"entered_at"`
	Claimable       bool        `json:"claimable"`
	StageLimit      *StageLimit `json:"stage_limit"`
}

// TaskOverviews — добавочные поля для строк списка. viewer — кто смотрит:
// от него зависит только current_run.claimable (задача, назначенная
// другому, для него не claimable — как в /queue); причины от смотрящего не
// зависят.
func (e *Engine) TaskOverviews(ctx context.Context, q postgres.Querier, items []domain.TaskListItem, viewer uuid.UUID, now time.Time) (map[uuid.UUID]TaskOverview, error) {
	out := make(map[uuid.UUID]TaskOverview, len(items))
	if len(items) == 0 {
		return out, nil
	}
	probe := e.newApprovalProbe()
	in := postgres.TaskFactsInput{}
	terminal := make(map[uuid.UUID]bool, len(items))
	for _, it := range items {
		g, err := probe.graph(ctx, q, it.WorkflowID)
		if err != nil {
			return nil, err
		}
		in.All = append(in.All, it.ID)
		if st, ok := g.byID[it.StatusID]; ok && st.IsTerminal {
			terminal[it.ID] = true
		} else {
			in.Live = append(in.Live, it.ID)
		}
		if len(it.ChildCounts) > 0 {
			in.Parents = append(in.Parents, it.ID)
		}
	}
	facts, err := e.store.TaskFacts(ctx, q, in)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		f := facts[it.ID]
		ov := TaskOverview{
			BlockedBy: []string{}, Attention: []Attention{},
			LastMovedAt: f.LastMovedAt, SubtreeLastMovedAt: f.SubtreeLastMovedAt,
			Cost: f.Cost, SubtreeCostUSD: f.SubtreeCostUSD,
		}
		if terminal[it.ID] {
			out[it.ID] = ov
			continue
		}
		ov.BlockedBy = f.BlockedBy
		g, err := probe.graph(ctx, q, it.WorkflowID)
		if err != nil {
			return nil, err
		}
		af := AttentionFacts{Now: now, BlockedBy: f.BlockedBy, LeaseEvent: f.LeaseEvent}

		// Человеческие гейты: та же выборка задач, что у /approvals (не
		// терминальная, не в done/cancelled, есть переход вперёд с
		// человеческим гейтом), и тот же прогон — последняя попытка этапа.
		if f.StageRun != nil && it.StatusCategory != "done" && it.StatusCategory != "cancelled" &&
			hasCandidates(g, it.StatusID, hasBlockingHuman) {
			verdicts, err := e.humanVerdicts(ctx, q, it.Task, g, *f.StageRun, hasBlockingHuman, nil)
			if err != nil {
				return nil, err
			}
			if it.StatusCategory == "active" || it.StatusCategory == "waiting" {
				ov.AwaitingApproval = verdictsAwaitApproval(verdicts)
			}
			for _, w := range humanWaits(verdicts, f.StageRun.LeaseHeld(now)) {
				af.HumanGates = append(af.HumanGates, AttentionGate{Transition: w.Transition.Key, Gate: w.Gate.GateKey})
			}
		}

		if f.StageRun != nil && f.StageRun.FinishedAt == nil {
			stage := *f.StageRun
			af.Stage = &stage
			af.Current = &stage
			if f.ReviewRun != nil {
				review := *f.ReviewRun
				af.Current = &review
			}
			if af.Limit, err = e.limitFromFacts(ctx, q, g, stage, f.GateDecisions, now); err != nil {
				return nil, err
			}
			for _, d := range f.GateDecisions {
				if d.Status != domain.GateResultFailed || d.DecidedBy == nil || d.DecidedAt == nil {
					continue
				}
				af.Rejected = append(af.Rejected, RejectedGate{
					Transition: d.TransitionKey, Gate: d.GateKey,
					Reason: decisionReason(d.Details), DecidedAt: *d.DecidedAt,
				})
			}
			ov.CurrentRun = currentRunView(it.Task, stage, f.ReviewRun, f.RoleNames, af.Limit, viewer, now)
		}
		ov.Attention = EvalAttention(af)
		out[it.ID] = ov
	}
	return out, nil
}

// limitFromFacts — предел попытки этапа: переход входа из графа версии
// цикла, снятие — из пакетных решений по гейтам. Переход не нашёлся в графе
// (задачу перевели на другую версию цикла) — тот же расчёт по базе.
func (e *Engine) limitFromFacts(ctx context.Context, q postgres.Querier, g cycleGraph, run domain.StageRun, decisions []postgres.RunGateDecision, now time.Time) (*StageLimit, error) {
	if !limitApplies(run) {
		return nil, nil
	}
	var via *domain.Transition
	for i := range g.transitions {
		if g.transitions[i].ID == *run.EnteredViaTransitionID {
			via = &g.transitions[i]
			break
		}
	}
	if via == nil {
		return e.limitFor(ctx, q, run, now)
	}
	limit, gate := limitOnClock(run, *via, now)
	if limit == nil {
		return nil, nil
	}
	for _, d := range decisions {
		if d.GateID == gate.ID && d.Status == domain.GateResultWaived {
			limit.Waived, limit.WaivedAt, limit.WaivedBy = true, d.DecidedAt, d.DecidedByHandle
		}
	}
	return limit, nil
}

// currentRunView — current_run строки: перепроверка, если открыта, иначе
// попытка этапа. claimable — по правилу /queue: есть роль, нет живой аренды,
// задача не назначена другому; плюс прогон в открытом состоянии.
func currentRunView(task domain.Task, stage domain.StageRun, review *domain.StageRun, roleNames map[uuid.UUID]string, limit *StageLimit, viewer uuid.UUID, now time.Time) *CurrentRun {
	r := stage
	if review != nil {
		r = *review
	}
	view := &CurrentRun{
		ID: r.ID, Kind: r.Kind, State: r.State, Attempt: stage.Attempt, StatusKey: stage.StatusKey,
		ClaimedByHandle: r.ClaimedHandle, ClaimedAt: r.ClaimedAt, HeartbeatAt: r.HeartbeatAt,
		LeaseExpiresAt: r.LeaseExpires, LeaseAlive: r.LeaseHeld(now), EnteredAt: r.EnteredAt,
		StageLimit: limit,
	}
	if review != nil {
		attempt := review.Attempt
		view.ReviewAttempt = &attempt
	}
	if r.RoleID != nil {
		key := r.RoleKey
		view.RoleKey = &key
		if name, ok := roleNames[*r.RoleID]; ok {
			view.RoleName = &name
		}
	}
	mine := task.AssigneeID == nil || (viewer != uuid.Nil && *task.AssigneeID == viewer)
	view.Claimable = r.RoleID != nil && r.Open() && !view.LeaseAlive && mine
	return view
}

// decisionReason — details.reason решения по гейту, если это непустая строка.
func decisionReason(details json.RawMessage) *string {
	var d struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(details, &d) != nil || d.Reason == "" {
		return nil
	}
	return &d.Reason
}
