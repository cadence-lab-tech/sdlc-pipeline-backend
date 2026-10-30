package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// ApprovalCandidates — задачи проекта, которым может грозить решение
// человека (CLAB-326, «Ждут меня»): статус не терминальный и не отменённый, и
// из него выходит переход вперёд с блокирующим гейтом одного из типов
// gateTypes. Отбор по графу цикла идёт здесь, одним запросом: задачи без
// таких гейтов до вычисления гейтов не доходят, и выборка не растёт с ними.
//
// Порядок — номер задачи, предела нет: задач с человеческим гейтом на
// переходе вперёд в проекте десятки, а предел ответа держит движок по
// элементам, а не по задачам. Области задачи (repositories) здесь нет:
// строке выборки она не нужна, а лишний запрос на задачу — нужен.
func (s *Store) ApprovalCandidates(ctx context.Context, q Querier, projectID uuid.UUID, gateTypes []string) ([]domain.Task, error) {
	rows, err := q.Query(ctx, taskSelect+`
		WHERE t.project_id = $1
		  AND NOT ws.is_terminal
		  AND ws.category NOT IN ('done', 'cancelled')
		  AND EXISTS (
		        SELECT 1
		        FROM workflow_transitions tr
		        JOIN gates g ON g.transition_id = tr.id
		        WHERE tr.workflow_id = t.workflow_id
		          AND tr.kind = 'advance'
		          AND (tr.from_status_id = t.status_id OR tr.from_status_id IS NULL)
		          AND g.blocking
		          AND g.type = ANY($2))
		ORDER BY t.seq`, projectID, gateTypes)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, t)
	}
	return out, translate(rows.Err())
}
