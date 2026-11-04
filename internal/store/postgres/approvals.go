package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// ApprovalFilter — зона «Ждут меня» участника уровня agent (решение 0072):
// OwnStage — только задачи, у текущего статуса которых есть основная роль
// из Roles (nil — любая), и только переходы из этого статуса, без
// глобальных. Пустой фильтр — весь проект, как у maintainer.
type ApprovalFilter struct {
	OwnStage bool
	Roles    []string
}

// ApprovalCandidates — задачи проекта, которым может грозить решение
// человека (CLAB-326, «Ждут меня»): статус не терминальный и не отменённый, и
// из него выходит переход вперёд с блокирующим гейтом одного из типов
// gateTypes. Отбор по графу цикла идёт здесь, одним запросом: задачи без
// таких гейтов до вычисления гейтов не доходят, и выборка не растёт с ними.
// Зона agent (filter) — условие того же запроса: задачи чужих этапов тоже
// до гейтов не доходят.
//
// Порядок — номер задачи, предела нет: задач с человеческим гейтом на
// переходе вперёд в проекте десятки, а предел ответа держит движок по
// элементам, а не по задачам. Область задачи (repositories) грузится
// пакетом, одним запросом на всю выборку (collectTasks): строке выборки она
// не нужна, но гейт repo_scope вычисляется на этих задачах, и без области он
// всегда failed («область не объявлена») — задача пропадала из «Ждут меня»
// при awaits_human в списке (CLAB-396).
func (s *Store) ApprovalCandidates(ctx context.Context, q Querier, projectID uuid.UUID, gateTypes []string, filter ApprovalFilter) ([]domain.Task, error) {
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
		          AND (tr.from_status_id = t.status_id OR (tr.from_status_id IS NULL AND NOT $3))
		          AND g.blocking
		          AND g.type = ANY($2))
		  AND (NOT $3 OR EXISTS (
		        SELECT 1
		        FROM status_roles sr
		        JOIN roles r ON r.id = sr.role_id
		        WHERE sr.status_id = t.status_id
		          AND sr.is_primary
		          AND ($4::text[] IS NULL OR r.key = ANY($4))))
		ORDER BY t.seq`, projectID, gateTypes, filter.OwnStage, filter.Roles)
	if err != nil {
		return nil, translate(err)
	}
	return s.collectTasks(ctx, q, rows)
}
