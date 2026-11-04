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

// Approvals — ответ выборки. CanDecide — есть ли у вызывающего зона решений
// вообще: maintainer и выше — весь проект, agent — этапы его ролей (решение
// 0072), если роль хоть одна; без зоны список пуст. Сотрудник решает гейты
// любого этапа, и у него поле true при любом наборе ролей. Поле для клиентов
// прежней версии: решать ли по элементу, говорит его CanDecide.
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
	// Может ли вызывающий вынести по гейту passed — та же mayDecide, что у
	// самого решения (решение 0072, п. 9).
	CanDecide bool `json:"can_decide"`
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

// approvalZone — чьи гейты видит актор в «Ждут меня»: весь проект
// (maintainer и выше), этапы своих ролей (agent) или ничего. Зону agent
// выражает фильтр выборки (ApprovalFilter), поэтому задачи чужих этапов до
// вычисления гейтов не доходят и число запросов от них не растёт.
// Сотрудник решает гейты любого этапа (CLAB-410), поэтому can_decide у него
// true и без ролей; список остаётся зоной его ролей и при [] пуст.
func approvalZone(actor Actor) (canDecide bool, filter postgres.ApprovalFilter) {
	switch {
	case actor.Level.Covers(domain.MemberMaintainer):
		return true, postgres.ApprovalFilter{}
	case !actor.Level.Covers(domain.MemberAgent):
		return false, postgres.ApprovalFilter{}
	case actor.Roles != nil && len(actor.Roles) == 0:
		return actor.Class() == ClassEmployee, postgres.ApprovalFilter{OwnStage: true, Roles: []string{}}
	}
	return true, postgres.ApprovalFilter{OwnStage: true, Roles: actor.Roles}
}

// Approvals — гейты проекта, ждущие решения, в зоне актора.
//
// Задачи отбирает один запрос по графу цикла (ApprovalCandidates): дальше
// идут только те, у чьего статуса есть переход вперёд с блокирующим апрувом
// или чек-листом, а для agent — ещё и статус с основной ролью из его набора
// и переход не глобальный. Граф версии читается один раз на версию, гейты —
// только у таких переходов. Чек-лист при живой аренде прогона в выборку не
// входит: его, по циклу, отмечает исполнитель этапа, и человеку он
// достаётся, только когда исполнителя нет.
func (e *Engine) Approvals(ctx context.Context, q postgres.Querier, projectID uuid.UUID, actor Actor, limit int) (Approvals, error) {
	canDecide, filter := approvalZone(actor)
	out := Approvals{Approvals: []Approval{}, CanDecide: canDecide}
	// Зона без ролей пуста и без запроса к базе.
	if !canDecide || (filter.OwnStage && filter.Roles != nil && len(filter.Roles) == 0) {
		return out, nil
	}
	if limit <= 0 {
		limit = ApprovalsDefaultLimit
	}
	limit = min(limit, ApprovalsMaxLimit)

	tasks, err := e.store.ApprovalCandidates(ctx, q, projectID, humanGateTypes, filter)
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
		items, err := e.taskApprovals(ctx, q, task, g, now, actor, filter.OwnStage)
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

// taskApprovals — элементы выборки одной задачи, по переходам в порядке
// цикла. ownStage — зона agent: глобальные переходы не показываются.
func (e *Engine) taskApprovals(ctx context.Context, q postgres.Querier, task domain.Task, g cycleGraph, now time.Time, actor Actor, ownStage bool) ([]Approval, error) {
	if !hasCandidates(g, task.StatusID, hasBlockingHuman) {
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
	stageArtifacts, allArtifacts, err := e.artifacts(ctx, q, task, run)
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

	verdicts, err := e.humanVerdicts(ctx, q, task, g, run, hasBlockingHuman,
		&taskArtifacts{stage: stageArtifacts, all: allArtifacts})
	if err != nil {
		return nil, err
	}
	stageRole := g.stageRole(task.StatusID)
	var out []Approval
	for _, w := range humanWaits(verdicts, runView.LeaseHeld) {
		if ownStage && w.Transition.FromStatusID == nil {
			continue
		}
		var canDecide bool
		for _, gate := range w.Transition.Gates {
			if gate.Key == w.Gate.GateKey {
				_, err := mayDecide(actor, actor.Principal, task, run, w.Transition, gate, stageRole, domain.GateResultPassed)
				canDecide = err == nil
				break
			}
		}
		out = append(out, Approval{
			CanDecide: canDecide,
			Task:      taskView,
			Transition: ApprovalTransition{
				Key: w.Transition.Key, Name: w.Transition.Name, ToStatus: g.byID[w.Transition.ToStatusID].Key,
				LandsIn: w.Landing.To.Key, LandsInName: w.Landing.To.Name,
			},
			Gate:     w.Gate,
			StageRun: runView,
		})
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
