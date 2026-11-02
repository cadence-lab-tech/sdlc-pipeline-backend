package workflow

import (
	"context"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Дети в карточке родителя (CLAB-324): категория статуса, «ждёт апрува» и
// входящие blocks. Карточка эпика — та же карточка задачи, у которой есть
// дети; без этих полей клиенту пришлось бы читать каждого ребёнка и версию
// его цикла отдельным запросом.

// AwaitsApproval — правило «ждёт апрува» для гейтов одного перехода вперёд:
// блокирующий human_approval не прошёл, а все остальные блокирующие прошли
// (passed, waived, skipped). То же правило, что у карточки интерфейса
// (frontend/src/components/cycle/state.ts) и у плагина Claude: тип гейта —
// словарь движка, а не данные проекта.
func AwaitsApproval(gates []GateOutcome) bool {
	approval := false
	for _, g := range gates {
		if !g.Blocking {
			continue
		}
		if g.Type == domain.GateHumanApproval {
			if !g.Passed() {
				approval = true
			}
			continue
		}
		if !g.Passed() {
			return false
		}
	}
	return approval
}

// TransitionsAwaitApproval — ждёт ли задача апрува хотя бы на одном переходе
// вперёд из предпросмотра.
func TransitionsAwaitApproval(ts []TransitionPreview) bool {
	for _, t := range ts {
		if t.Kind == "advance" && AwaitsApproval(t.Gates) {
			return true
		}
	}
	return false
}

// ChildCards — дети задачи для карточки родителя, в порядке заведения.
//
// Запросов на всех детей два (задачи и факты). «Ждёт апрува» считается только
// у детей в работе (active, waiting) и только по переходам вперёд, на которых
// стоит блокирующий human_approval: остальным апрув не грозит, и гейты их
// переходов не вычисляются. Граф цикла читается один раз на версию.
func (e *Engine) ChildCards(ctx context.Context, q postgres.Querier, parentID uuid.UUID) ([]domain.ChildTask, error) {
	kids, err := e.store.ChildTasks(ctx, q, parentID)
	if err != nil {
		return nil, err
	}
	facts, err := e.store.ChildFacts(ctx, q, parentID)
	if err != nil {
		return nil, err
	}
	probe := e.newApprovalProbe()
	out := make([]domain.ChildTask, 0, len(kids))
	for _, kid := range kids {
		f := facts[kid.ID]
		card := domain.ChildTask{Task: kid, StatusCategory: f.Category, BlockedBy: f.BlockedBy}
		if card.BlockedBy == nil {
			card.BlockedBy = []string{}
		}
		if card.AwaitingApproval, err = probe.awaiting(ctx, q, kid, f.Category); err != nil {
			return nil, err
		}
		out = append(out, card)
	}
	return out, nil
}

// approvalProbe — общая часть детей карточки и списка задач (TaskOverviews):
// графы версий цикла читаются один раз на версию, гейты вычисляются только у
// задач в работе.
type approvalProbe struct {
	e      *Engine
	graphs map[uuid.UUID]cycleGraph
}

func (e *Engine) newApprovalProbe() *approvalProbe {
	return &approvalProbe{e: e, graphs: map[uuid.UUID]cycleGraph{}}
}

func (p *approvalProbe) graph(ctx context.Context, q postgres.Querier, workflowID uuid.UUID) (cycleGraph, error) {
	if g, ok := p.graphs[workflowID]; ok {
		return g, nil
	}
	g, err := p.e.cycleGraph(ctx, q, workflowID)
	if err != nil {
		return cycleGraph{}, err
	}
	p.graphs[workflowID] = g
	return g, nil
}

// awaiting — «ждёт апрува» задачи в категории статуса category: апрув
// грозит только задаче в работе (active, waiting).
func (p *approvalProbe) awaiting(ctx context.Context, q postgres.Querier, task domain.Task, category string) (bool, error) {
	if category != "active" && category != "waiting" {
		return false, nil
	}
	g, err := p.graph(ctx, q, task.WorkflowID)
	if err != nil {
		return false, err
	}
	return p.e.awaitsApproval(ctx, q, task, g)
}

// awaitsApproval — то же, что TransitionsAwaitApproval(Preview(task)), но
// вычисляет гейты только у переходов вперёд с блокирующим human_approval.
func (e *Engine) awaitsApproval(ctx context.Context, q postgres.Querier, task domain.Task, g cycleGraph) (bool, error) {
	if !hasCandidates(g, task.StatusID, hasBlockingApproval) {
		return false, nil
	}
	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		// Без прогона на статусе нечего апрувить: задача не ждёт решения, а
		// список задач не падает из-за одной такой строки (CLAB-327).
		if apperr.As(err).Code == apperr.CodeNotFound {
			return false, nil
		}
		return false, err
	}
	verdicts, err := e.humanVerdicts(ctx, q, task, g, run, hasBlockingApproval, nil)
	if err != nil {
		return false, err
	}
	return verdictsAwaitApproval(verdicts), nil
}

// transitionVerdict — вердикт гейтов одного перехода вперёд и где по нему
// встанет задача.
type transitionVerdict struct {
	Transition domain.Transition
	Landing    landing
	Outcomes   []GateOutcome
}

// hasCandidates — есть ли из статуса переход вперёд, подходящий под keep.
func hasCandidates(g cycleGraph, statusID uuid.UUID, keep func(domain.Transition) bool) bool {
	for _, t := range g.available(statusID) {
		if t.Kind == "advance" && keep(t) {
			return true
		}
	}
	return false
}

// humanVerdicts — гейты переходов вперёд из статуса задачи, отобранных keep,
// на прогоне run. Одна функция на «ждёт апрува» (awaiting_approval списка и
// children[] карточки), «Ждут меня» (/approvals) и причину awaits_human
// списка задач (CLAB-363): строка списка вычисляет гейты один раз и берёт
// из одного вердикта оба признака. arts — уже прочитанные артефакты этапа и
// задачи; nil — прочитать.
func (e *Engine) humanVerdicts(ctx context.Context, q postgres.Querier, task domain.Task, g cycleGraph, run domain.StageRun, keep func(domain.Transition) bool, arts *taskArtifacts) ([]transitionVerdict, error) {
	var candidates []domain.Transition
	for _, t := range g.available(task.StatusID) {
		if t.Kind == "advance" && keep(t) {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if arts == nil {
		stage, all, err := e.artifacts(ctx, q, task, run)
		if err != nil {
			return nil, err
		}
		arts = &taskArtifacts{stage: stage, all: all}
	}
	choice := storedChoice(task.OptionalStages)
	out := make([]transitionVerdict, 0, len(candidates))
	for _, t := range candidates {
		land, err := planLanding(g, choice, t)
		if err != nil {
			return nil, err
		}
		outcomes, _, err := e.evaluate(ctx, q, task, run, t, land, arts.stage, arts.all)
		if err != nil {
			return nil, err
		}
		out = append(out, transitionVerdict{Transition: t, Landing: land, Outcomes: outcomes})
	}
	return out, nil
}

// taskArtifacts — артефакты этапа и задачи, прочитанные один раз на задачу.
type taskArtifacts struct {
	stage, all []domain.Artifact
}

// verdictsAwaitApproval — «ждёт апрува» по вердиктам: хотя бы на одном
// переходе с блокирующим human_approval правило AwaitsApproval выполнено.
func verdictsAwaitApproval(verdicts []transitionVerdict) bool {
	for _, v := range verdicts {
		if hasBlockingApproval(v.Transition) && AwaitsApproval(v.Outcomes) {
			return true
		}
	}
	return false
}

// humanWait — человеческий гейт, который ждёт решения, с переходом и
// посадкой.
type humanWait struct {
	Transition domain.Transition
	Landing    landing
	Gate       GateOutcome
}

// humanWaits — гейты «Ждут меня» по вердиктам: AwaitsHuman на доступных
// переходах; чек-лист при живой аренде прогона не ждёт человека — его, по
// циклу, отмечает исполнитель этапа.
func humanWaits(verdicts []transitionVerdict, leaseHeld bool) []humanWait {
	var out []humanWait
	for _, v := range verdicts {
		// Переход в выключенный этап недоступен: решать по нему нечего.
		if v.Landing.Unavailable != "" {
			continue
		}
		for _, gate := range AwaitsHuman(v.Outcomes) {
			if gate.Type == domain.GateChecklist && leaseHeld {
				continue
			}
			out = append(out, humanWait{Transition: v.Transition, Landing: v.Landing, Gate: gate})
		}
	}
	return out
}

func hasBlockingApproval(t domain.Transition) bool {
	for _, g := range t.Gates {
		if g.Blocking && g.Type == domain.GateHumanApproval {
			return true
		}
	}
	return false
}
