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
	graphs := map[uuid.UUID]cycleGraph{}
	out := make([]domain.ChildTask, 0, len(kids))
	for _, kid := range kids {
		f := facts[kid.ID]
		card := domain.ChildTask{Task: kid, StatusCategory: f.Category, BlockedBy: f.BlockedBy}
		if card.BlockedBy == nil {
			card.BlockedBy = []string{}
		}
		if f.Category == "active" || f.Category == "waiting" {
			g, ok := graphs[kid.WorkflowID]
			if !ok {
				if g, err = e.cycleGraph(ctx, q, kid.WorkflowID); err != nil {
					return nil, err
				}
				graphs[kid.WorkflowID] = g
			}
			if card.AwaitingApproval, err = e.awaitsApproval(ctx, q, kid, g); err != nil {
				return nil, err
			}
		}
		out = append(out, card)
	}
	return out, nil
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
