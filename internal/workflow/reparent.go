// Перенос задачи к другому родителю и снятие с родителя (CLAB-196): операция
// движка вне правки постановки. Меняются только parent_id и ранг задачи —
// тип, цикл, статус, прогоны, артефакты, связи, область и назначение прежние.
//
// Вложенность — ровно один уровень: родителем бывает только задача без
// родителя, а задачу с детьми под родителя не кладут. Отдельного обхода
// потомков ради циклов нет и не нужно: при одном уровне потомки задачи — её
// прямые дети, а им родителем не стать (TopLevelParent), поэтому цикл
// сводится к «не сама себе» и двум правилам уровня. Область при переносе не
// проверяется: перенос — действие человека, а рамка области (childInScope)
// — правило бота и токена задачи, которым перенос закрыт правом.
package workflow

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// ReparentResult — исход переноса: задача после него, ключи прежнего и нового
// родителя (nil — верхний уровень) и changed=false, если родитель уже был
// этим: тогда ни ранг, ни лента не менялись.
type ReparentResult struct {
	Task    domain.Task `json:"task"`
	From    *string     `json:"from"`
	To      *string     `json:"to"`
	Changed bool        `json:"changed"`
}

// reparentedPayload — нагрузка task.reparented: одна на ленты задачи и
// обоих родителей.
type reparentedPayload struct {
	Task string  `json:"task"`
	From *string `json:"from"`
	To   *string `json:"to"`
}

// ReparentTask ставит задаче taskKey родителя parentKey; пустой ключ снимает
// родителя.
//
// Блокировки по порядку: строка проекта (та же, что у заведения задачи, —
// заведение ребёнка, перенос и встречный перенос в проекте идут по очереди),
// затем группы ранга прежнего и нового родителя в порядке идентификаторов
// (тот же замок, что у перемещения в бэклоге), затем строка задачи. Группы
// раньше строки задачи: перемещение в бэклоге держит группу и пишет ранги
// строк, обратный порядок дал бы взаимную блокировку.
//
// Порядок отказов: задачи нет (404) → право актора (TaskRight, ActReparent:
// 403) → родитель есть и из того же проекта (как при заведении) → не сама
// задача (400) → повтор с тем же родителем (200 без изменений) → родитель без
// родителя (400) → у задачи нет детей (400). Снятие родителя проверок уровня
// не проходит: глубина от него не растёт. Ни один отказ ничего не меняет.
func (e *Engine) ReparentTask(ctx context.Context, taskKey, parentKey string, who Actor) (ReparentResult, error) {
	var res ReparentResult
	parentKey = strings.TrimSpace(parentKey)
	actor := who.Principal.ID

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.GetTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		if err := e.store.LockProjectRow(ctx, tx, task.ProjectID); err != nil {
			return err
		}
		// Под строкой проекта parent_id задачи и её родителя никто не
		// меняет: читается заново, это и есть состояние, которое проверяется.
		if task, err = e.store.GetTaskByKey(ctx, tx, taskKey); err != nil {
			return err
		}
		if err := e.mayTask(ctx, tx, who, ActReparent, task); err != nil {
			return err
		}

		var parent *domain.Task
		if parentKey != "" {
			p, err := e.store.ParentInProject(ctx, tx, task.ProjectID, parentKey)
			if err != nil {
				return err
			}
			if p.ID == task.ID {
				return apperr.InvalidInput("задачу %s нельзя сделать родителем самой себя", task.Key)
			}
			parent = &p
		}

		var to *uuid.UUID
		if parent != nil {
			to = &parent.ID
		}
		res.From = task.ParentKey
		if parent != nil {
			res.To = &parent.Key
		}
		if sameParent(task.ParentID, to) {
			res.Task = task
			return nil
		}

		if parent != nil {
			if err := postgres.TopLevelParent(*parent); err != nil {
				return err
			}
			n, err := e.store.CountChildren(ctx, tx, task.ID)
			if err != nil {
				return err
			}
			if n > 0 {
				return apperr.InvalidInput(
					"у %s есть дочерние задачи (%d): задачу с детьми нельзя положить под родителя — "+
						"вложенность один уровень; снять родителя можно всегда", task.Key, n).
					WithDetails(map[string]any{"children": n})
			}
		}

		for _, g := range rankGroupsInOrder(task.ParentID, to) {
			if err := e.store.LockRankGroup(ctx, tx, task.ProjectID, g); err != nil {
				return err
			}
		}
		if _, err := e.store.LockTaskByKey(ctx, tx, taskKey); err != nil {
			return err
		}
		if err := e.store.SetTaskParent(ctx, tx, task, to); err != nil {
			return err
		}

		payload := reparentedPayload{Task: task.Key, From: res.From, To: res.To}
		feeds := []uuid.UUID{task.ID}
		if task.ParentID != nil {
			feeds = append(feeds, *task.ParentID)
		}
		if to != nil {
			feeds = append(feeds, *to)
		}
		for _, id := range feeds {
			if err := e.store.AppendEvent(ctx, tx, id, nil, domain.EventTaskReparented, actor, payload); err != nil {
				return err
			}
		}

		res.Changed = true
		res.Task, err = e.store.GetTaskByKey(ctx, tx, taskKey)
		return err
	})
	return res, err
}

// rankGroupsInOrder — группы ранга прежнего и нового родителя в одном порядке
// для всех вызовов (верхний уровень первым, затем по идентификатору): два
// переноса между одними группами в разные стороны берут замки одинаково.
func rankGroupsInOrder(a, b *uuid.UUID) []*uuid.UUID {
	key := func(p *uuid.UUID) string {
		if p == nil {
			return ""
		}
		return p.String()
	}
	if key(a) > key(b) {
		a, b = b, a
	}
	return []*uuid.UUID{a, b}
}
