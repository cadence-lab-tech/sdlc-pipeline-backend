package workflow

import (
	"context"

	"github.com/google/uuid"

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

// TasksAwaitingApproval — признак «ждёт апрува» для строк списка задач
// проекта (CLAB-326): то же правило и та же функция, что у детей в карточке
// родителя, поэтому признак списка, children[] и карточки совпадают.
// Категория статуса берётся из графа версии цикла, который нужен правилу и
// так: лишнего запроса на задачу нет. Ключ ответа — задачи, которые ждут.
func (e *Engine) TasksAwaitingApproval(ctx context.Context, q postgres.Querier, tasks []domain.Task) (map[uuid.UUID]bool, error) {
	probe := e.newApprovalProbe()
	out := map[uuid.UUID]bool{}
	for _, t := range tasks {
		g, err := probe.graph(ctx, q, t.WorkflowID)
		if err != nil {
			return nil, err
		}
		awaiting, err := probe.awaiting(ctx, q, t, g.byID[t.StatusID].Category)
		if err != nil {
			return nil, err
		}
		if awaiting {
			out[t.ID] = true
		}
	}
	return out, nil
}

// approvalProbe — общая часть детей карточки и списка задач: графы версий
// цикла читаются один раз на версию, гейты вычисляются только у задач в
// работе.
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
	var candidates []domain.Transition
	for _, t := range g.available(task.StatusID) {
		if t.Kind == "advance" && hasBlockingApproval(t) {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return false, nil
	}
	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return false, err
	}
	stageArtifacts, taskArtifacts, err := e.artifacts(ctx, q, task, run)
	if err != nil {
		return false, err
	}
	choice := storedChoice(task.OptionalStages)
	for _, t := range candidates {
		land, err := planLanding(g, choice, t)
		if err != nil {
			return false, err
		}
		outcomes, _, err := e.evaluate(ctx, q, task, run, t, land, stageArtifacts, taskArtifacts)
		if err != nil {
			return false, err
		}
		if AwaitsApproval(outcomes) {
			return true, nil
		}
	}
	return false, nil
}

func hasBlockingApproval(t domain.Transition) bool {
	for _, g := range t.Gates {
		if g.Blocking && g.Type == domain.GateHumanApproval {
			return true
		}
	}
	return false
}
