package workflow

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// «Ждут меня» (CLAB-326): гейты проекта, которые сейчас ждут решения
// человека, по всем циклам и типам задач проекта. Правило — от типа гейта
// (словарь движка) и вердикта, а не от статуса или роли: какие этапы завёл
// проект, выборка не знает.

// humanGateTypes — гейты, которые закрывает решение человека: апрув и ручной
// чек-лист. Остальные типы вычисляет движок; снять их (waived) человек может,
// но ждать его они не ждут.
var humanGateTypes = []string{domain.GateHumanApproval, domain.GateChecklist}

func isHumanGate(typ string) bool {
	for _, t := range humanGateTypes {
		if t == typ {
			return true
		}
	}
	return false
}

// AwaitsHuman — человеческие гейты одного перехода вперёд, которые ждут
// решения: блокирующий апрув или чек-лист в pending, а все остальные
// блокирующие гейты прошли (passed, waived, skipped) либо тоже человеческие.
// failed сюда не входит: отклонённое ждёт исполнителя, а не человека. Шире
// AwaitsApproval: тот — только апрув при всех остальных пройденных, этот
// включает чек-листы и переходы, где рядом с апрувом стоит неотмеченный
// чек-лист.
func AwaitsHuman(gates []GateOutcome) []GateOutcome {
	var waiting []GateOutcome
	for _, g := range gates {
		if !g.Blocking {
			continue
		}
		if isHumanGate(g.Type) {
			if g.Status == domain.GateResultPending {
				waiting = append(waiting, g)
			}
			continue
		}
		if !g.Passed() {
			return nil
		}
	}
	return waiting
}

// Предел выборки по умолчанию и сверху (AC-15).
const (
	ApprovalsDefaultLimit = 100
	ApprovalsMaxLimit     = 200
)

// Approvals — ответ выборки. CanDecide — вправе ли вызывающий решать гейты
// человека по уровню (maintainer и выше); без права список пуст.
type Approvals struct {
	Approvals []Approval `json:"approvals"`
	CanDecide bool       `json:"can_decide"`
	// Элементов больше предела: показаны первые limit по номеру задачи.
	Truncated bool `json:"truncated"`
}

// Approval — один гейт, ждущий решения: задача, переход, гейт и прогон.
type Approval struct {
	Task       ApprovalTask       `json:"task"`
	Transition ApprovalTransition `json:"transition"`
	Gate       GateOutcome        `json:"gate"`
	StageRun   ApprovalRun        `json:"stage_run"`
}

type ApprovalTask struct {
	Key        string    `json:"key"`
	Title      string    `json:"title"`
	Type       string    `json:"type"`
	ParentKey  *string   `json:"parent_key,omitempty"`
	Priority   string    `json:"priority"`
	WorkflowID uuid.UUID `json:"workflow_id"`
	StatusKey  string    `json:"status_key"`
	StatusName string    `json:"status_name"`
}

// ApprovalTransition — переход, на котором стоит гейт. LandsIn — где встанет
// задача с её выбором этапов (решение 0066), с подписью из версии цикла.
type ApprovalTransition struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	ToStatus    string `json:"to_status"`
	LandsIn     string `json:"lands_in"`
	LandsInName string `json:"lands_in_name"`
}

// ApprovalRun — текущий прогон этапа. LatestArtifactID — последний артефакт
// этапа: его клиент передаёт условием сверки (seen_artifact_id), если решает
// прямо из списка; null — на этапе ничего не сдано.
type ApprovalRun struct {
	ID               uuid.UUID `json:"id"`
	Attempt          int       `json:"attempt"`
	LeaseHeld        bool      `json:"lease_held"`
	LatestArtifactID *string   `json:"latest_artifact_id"`
}

// Approvals — гейты проекта, ждущие решения человека.
//
// Задачи отбирает один запрос по графу цикла (ApprovalCandidates): дальше
// идут только те, у чьего статуса есть переход вперёд с блокирующим апрувом
// или чек-листом. Граф версии читается один раз на версию, гейты — только у
// таких переходов. Чек-лист при живой аренде прогона в выборку не входит:
// его, по циклу, отмечает исполнитель этапа, и человеку он достаётся, только
// когда исполнителя нет.
func (e *Engine) Approvals(ctx context.Context, q postgres.Querier, projectID uuid.UUID, canDecide bool, limit int) (Approvals, error) {
	out := Approvals{Approvals: []Approval{}, CanDecide: canDecide}
	if !canDecide {
		return out, nil
	}
	if limit <= 0 {
		limit = ApprovalsDefaultLimit
	}
	limit = min(limit, ApprovalsMaxLimit)

	tasks, err := e.store.ApprovalCandidates(ctx, q, projectID, humanGateTypes)
	if err != nil {
		return out, err
	}
	probe := e.newApprovalProbe()
	now := time.Now()
	for _, task := range tasks {
		g, err := probe.graph(ctx, q, task.WorkflowID)
		if err != nil {
			return out, err
		}
		items, err := e.taskApprovals(ctx, q, task, g, now)
		if err != nil {
			return out, err
		}
		for _, it := range items {
			if len(out.Approvals) == limit {
				out.Truncated = true
				return out, nil
			}
			out.Approvals = append(out.Approvals, it)
		}
	}
	return out, nil
}

// taskApprovals — элементы выборки одной задачи, по переходам в порядке цикла.
func (e *Engine) taskApprovals(ctx context.Context, q postgres.Querier, task domain.Task, g cycleGraph, now time.Time) ([]Approval, error) {
	var candidates []domain.Transition
	for _, t := range g.available(task.StatusID) {
		if t.Kind == "advance" && hasBlockingHuman(t) {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		// Без прогона на статусе нечего решать: задача пропускается, а
		// выборка не падает из-за одной такой строки (как awaitsApproval в
		// children.go, CLAB-327).
		if apperr.As(err).Code == apperr.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	stageArtifacts, taskArtifacts, err := e.artifacts(ctx, q, task, run)
	if err != nil {
		return nil, err
	}
	runView := ApprovalRun{ID: run.ID, Attempt: run.Attempt, LeaseHeld: run.LeaseHeld(now)}
	var latest time.Time
	for _, a := range stageArtifacts {
		if runView.LatestArtifactID == nil || a.CreatedAt.After(latest) {
			id := a.ID.String()
			runView.LatestArtifactID, latest = &id, a.CreatedAt
		}
	}
	taskView := ApprovalTask{
		Key: task.Key, Title: task.Title, Type: task.Type(), ParentKey: task.ParentKey,
		Priority: task.Priority, WorkflowID: task.WorkflowID,
		StatusKey: task.StatusKey, StatusName: task.StatusName,
	}

	choice := storedChoice(task.OptionalStages)
	var out []Approval
	for _, t := range candidates {
		land, err := planLanding(g, choice, t)
		if err != nil {
			return nil, err
		}
		// Переход в выключенный этап недоступен: решать по нему нечего.
		if land.Unavailable != "" {
			continue
		}
		outcomes, _, err := e.evaluate(ctx, q, task, run, t, land, stageArtifacts, taskArtifacts)
		if err != nil {
			return nil, err
		}
		for _, gate := range AwaitsHuman(outcomes) {
			if gate.Type == domain.GateChecklist && runView.LeaseHeld {
				continue
			}
			out = append(out, Approval{
				Task: taskView,
				Transition: ApprovalTransition{
					Key: t.Key, Name: t.Name, ToStatus: g.byID[t.ToStatusID].Key,
					LandsIn: land.To.Key, LandsInName: land.To.Name,
				},
				Gate:     gate,
				StageRun: runView,
			})
		}
	}
	return out, nil
}

func hasBlockingHuman(t domain.Transition) bool {
	for _, g := range t.Gates {
		if g.Blocking && isHumanGate(g.Type) {
			return true
		}
	}
	return false
}
