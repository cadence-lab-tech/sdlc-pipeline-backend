package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Иерархия задач в один уровень (CLAB-196): задача либо имеет родителя, либо
// имеет детей. Правило держат заведение (CreateTask) и перенос
// (workflow.ReparentTask); обе операции идут под строкой проекта, поэтому
// встречные операции над общей задачей выполняются по очереди и проверки
// видят зафиксированное состояние. Старые цепочки глубже одного уровня
// (прежние версии их разрешали) не трогаются: правило — о новых операциях.

// ParentInProject — родитель по ключу: задача есть и из того же проекта.
// Отказы те же, что у заведения с таким parent_key: нет задачи — 404, ключ не
// ключ — 400, чужой проект — 400.
func (s *Store) ParentInProject(ctx context.Context, q Querier, projectID uuid.UUID, parentKey string) (domain.Task, error) {
	parent, err := s.GetTaskByKey(ctx, q, parentKey)
	if err != nil {
		return domain.Task{}, err
	}
	if parent.ProjectID != projectID {
		return domain.Task{}, apperr.InvalidInput("родительская задача %q из другого проекта", parentKey)
	}
	return parent, nil
}

// TopLevelParent — родителем бывает только задача без родителя: вложенность
// один уровень. Текст называет выбранного родителя и его родителя.
func TopLevelParent(parent domain.Task) error {
	if parent.ParentID == nil {
		return nil
	}
	grand := "другую задачу"
	if parent.ParentKey != nil {
		grand = *parent.ParentKey
	}
	return apperr.InvalidInput(
		"%s сама вложена в %s: вложенность — один уровень, родителем бывает только задача верхнего уровня",
		parent.Key, grand).WithDetails(map[string]any{"parent": parent.Key, "parent_of_parent": grand})
}

// LockProjectRow берёт строку проекта тем же замком, что UPDATE счётчика
// ключей в CreateTask (FOR NO KEY UPDATE): заведение задачи и перенос в одном
// проекте идут по очереди. Внешние ключи задач (FOR KEY SHARE) этот замок не
// ждут — переходы и сдачи переносу не мешают.
func (s *Store) LockProjectRow(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) error {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id = $1 FOR NO KEY UPDATE`,
		projectID).Scan(&id); err != nil {
		return notFound(err, "проект не найден")
	}
	return nil
}

// CountChildren — число детей задачи в любом статусе, закрытые и снятые тоже:
// снятый ребёнок остаётся в дереве и держит правило одного уровня.
func (s *Store) CountChildren(ctx context.Context, q Querier, taskID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE parent_id = $1`, taskID).Scan(&n)
	return n, translate(err)
}

// SetTaskParent ставит задаче родителя (nil — снимает) и ранг в конце группы
// нового родителя (решение 0075: тем же правилом, что новая задача). Обе
// группы ранга вызывающий уже держит. updated_at двигается — у задачи
// поменялось место в иерархии.
func (s *Store) SetTaskParent(ctx context.Context, tx pgx.Tx, task domain.Task, parentID *uuid.UUID) error {
	rank, err := s.appendRank(ctx, tx, task.ProjectID, parentID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET parent_id = $2, rank = $3, updated_at = now() WHERE id = $1`,
		task.ID, parentID, rank)
	return translate(err)
}
