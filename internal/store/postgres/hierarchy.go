package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

// Иерархия задач и связи между ними: то, из чего гейт dependencies_closed
// делает вывод «эпик можно закрывать» (решение 0024).

// TaskLink — связь между задачами в том виде, в каком её показывают карточка
// и агент: с ключом задачи на другом конце и направлением.
type TaskLink struct {
	Rel string `json:"rel"`
	// Направление: outgoing — эта задача блокирует, incoming — блокируют её.
	Direction string    `json:"direction"`
	TaskID    uuid.UUID `json:"task_id"`
	TaskKey   string    `json:"task_key"`
	TaskTitle string    `json:"task_title"`
	StatusKey string    `json:"status_key"`
	Note      *string   `json:"note,omitempty"`
}

// LinkTasks заводит связь. Повторная связь того же вида — не ошибка: агент,
// повторивший вызов после обрыва, не должен получать конфликт.
func (s *Store) LinkTasks(ctx context.Context, q Querier, fromID, toID uuid.UUID, rel, note string, actor uuid.UUID) (bool, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO task_links (from_task_id, to_task_id, rel, note, created_by)
		VALUES ($1, $2, $3, nullif($4, ''), $5)
		ON CONFLICT (from_task_id, to_task_id, rel) DO NOTHING
		RETURNING id`, fromID, toID, rel, note, actor).Scan(&id)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, translate(err)
	}
	return true, nil
}

func (s *Store) UnlinkTasks(ctx context.Context, q Querier, fromID, toID uuid.UUID, rel string) error {
	_, err := q.Exec(ctx, `
		DELETE FROM task_links
		WHERE from_task_id = $1 AND to_task_id = $2 AND rel = $3`, fromID, toID, rel)
	return translate(err)
}

// ListTaskLinks отдаёт связи в обе стороны: «эта задача блокирует» и «эту
// задачу блокируют» — для агента это разные новости.
func (s *Store) ListTaskLinks(ctx context.Context, q Querier, taskID uuid.UUID) ([]TaskLink, error) {
	rows, err := q.Query(ctx, `
		SELECT l.rel, 'outgoing', t.id, t.key, t.title, ws.key, l.note
		FROM task_links l
		JOIN tasks t ON t.id = l.to_task_id
		JOIN workflow_statuses ws ON ws.id = t.status_id
		WHERE l.from_task_id = $1
		UNION ALL
		SELECT l.rel, 'incoming', t.id, t.key, t.title, ws.key, l.note
		FROM task_links l
		JOIN tasks t ON t.id = l.from_task_id
		JOIN workflow_statuses ws ON ws.id = t.status_id
		WHERE l.to_task_id = $1
		ORDER BY 1, 2, 4`, taskID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []TaskLink{}
	for rows.Next() {
		var l TaskLink
		if err := rows.Scan(&l.Rel, &l.Direction, &l.TaskID, &l.TaskKey,
			&l.TaskTitle, &l.StatusKey, &l.Note); err != nil {
			return nil, translate(err)
		}
		out = append(out, l)
	}
	return out, translate(rows.Err())
}

// TaskDependency — задача, от которой зависит проверяемая: ребёнок в дереве
// или блокирующая связь.
type TaskDependency struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	Type      string `json:"type"`
	StatusKey string `json:"status_key"`
	// Категория статуса: гейт судит по ней, а не по ключу статуса — какие
	// статусы в проекте, движок не знает.
	Category string `json:"category"`
	// child | blocker
	Relation string `json:"relation"`
}

// TaskDependencies собирает то, чего задача ждёт.
//
// Дети берутся всем поддеревом: эпик, у которого закрыты дети, но открыты
// внуки, закрытым не является. Блокирующие — только прямые связи `blocks`:
// транзитивность здесь означала бы, что гейт объясняет провал задачей, о
// которой человек в этой задаче никогда не слышал.
func (s *Store) TaskDependencies(ctx context.Context, q Querier, taskID uuid.UUID, children, blockers bool) ([]TaskDependency, error) {
	rows, err := q.Query(ctx, `
		WITH RECURSIVE descendants AS (
			SELECT id FROM tasks WHERE parent_id = $1
			UNION ALL
			SELECT t.id FROM tasks t JOIN descendants d ON t.parent_id = d.id
		)
		SELECT t.key, t.title, coalesce(t.properties->>'type', $4), ws.key, ws.category, 'child'
		FROM descendants d
		JOIN tasks t ON t.id = d.id
		JOIN workflow_statuses ws ON ws.id = t.status_id
		WHERE $2
		UNION ALL
		SELECT t.key, t.title, coalesce(t.properties->>'type', $4), ws.key, ws.category, 'blocker'
		FROM task_links l
		JOIN tasks t ON t.id = l.from_task_id
		JOIN workflow_statuses ws ON ws.id = t.status_id
		WHERE $3 AND l.to_task_id = $1 AND l.rel = 'blocks'
		ORDER BY 6, 1`, taskID, children, blockers, domain.DefaultTaskType)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []TaskDependency{}
	for rows.Next() {
		var d TaskDependency
		if err := rows.Scan(&d.Key, &d.Title, &d.Type, &d.StatusKey, &d.Category, &d.Relation); err != nil {
			return nil, translate(err)
		}
		out = append(out, d)
	}
	return out, translate(rows.Err())
}

// SetAssignee меняет исполнителя задачи. Назначение адресное: назначенную
// задачу заберёт только этот принципал (решение 0025).
func (s *Store) SetAssignee(ctx context.Context, q Querier, taskID uuid.UUID, assignee *uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE tasks SET assignee_id = $2, updated_at = now() WHERE id = $1`, taskID, assignee)
	return translate(err)
}
