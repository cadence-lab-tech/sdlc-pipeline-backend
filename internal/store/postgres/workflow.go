package postgres

import (
	"context"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// GetActiveWorkflow отдаёт действующий цикл обычных задач. Оставлен для тех,
// кому тип не важен: доска по умолчанию, экспорт связки, каталог ролей.
func (s *Store) GetActiveWorkflow(ctx context.Context, q Querier, projectID uuid.UUID) (domain.Workflow, error) {
	return s.GetActiveWorkflowForType(ctx, q, projectID, domain.DefaultTaskType)
}

// GetActiveWorkflowForType отдаёт действующий цикл того типа задач целиком:
// статусы с ролями, переходы с гейтами. Собирается несколькими запросами, а не
// одним join'ом — иначе строки размножаются по декартову произведению гейтов
// и ролей.
//
// Активных циклов в проекте столько, сколько типов задач, но на один тип —
// ровно один (решение 0024), поэтому выбор однозначен.
func (s *Store) GetActiveWorkflowForType(ctx context.Context, q Querier, projectID uuid.UUID, taskType string) (domain.Workflow, error) {
	if taskType == "" {
		taskType = domain.DefaultTaskType
	}
	wf, err := scanWorkflow(q.QueryRow(ctx, `SELECT `+workflowCols+`
		FROM workflows
		WHERE project_id = $1 AND state = 'active' AND $2 = ANY (task_types)`,
		projectID, taskType))
	if err != nil {
		return wf, notFound(err, "у проекта нет активного цикла для задач типа %q", taskType)
	}

	if wf.Statuses, err = s.ListStatuses(ctx, q, wf.ID); err != nil {
		return wf, err
	}
	if wf.Transitions, err = s.ListTransitions(ctx, q, wf.ID); err != nil {
		return wf, err
	}
	return wf, nil
}

// ListActiveWorkflows — все действующие циклы проекта: по одному на тип задач.
func (s *Store) ListActiveWorkflows(ctx context.Context, q Querier, projectID uuid.UUID) ([]domain.Workflow, error) {
	rows, err := q.Query(ctx, `SELECT `+workflowCols+`
		FROM workflows WHERE project_id = $1 AND state = 'active'
		ORDER BY task_types, name`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Workflow{}
	for rows.Next() {
		wf, err := scanWorkflow(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, wf)
	}
	return out, translate(rows.Err())
}

func (s *Store) ListStatuses(ctx context.Context, q Querier, workflowID uuid.UUID) ([]domain.Status, error) {
	rows, err := q.Query(ctx, `
		SELECT id, workflow_id, key, name, description, category, position,
		       is_initial, is_terminal, is_optional, enabled_by_default, wip_limit, color
		FROM workflow_statuses WHERE workflow_id = $1 ORDER BY position`, workflowID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	statuses := []domain.Status{}
	byID := map[uuid.UUID]int{}
	for rows.Next() {
		var st domain.Status
		if err := rows.Scan(&st.ID, &st.WorkflowID, &st.Key, &st.Name, &st.Description,
			&st.Category, &st.Position, &st.IsInitial, &st.IsTerminal, &st.IsOptional, &st.EnabledByDefault, &st.WipLimit, &st.Color); err != nil {
			return nil, translate(err)
		}
		byID[st.ID] = len(statuses)
		statuses = append(statuses, st)
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}

	roleRows, err := q.Query(ctx, `
		SELECT sr.status_id, sr.role_id, r.key, r.name, sr.is_primary, sr.position
		FROM status_roles sr
		JOIN roles r ON r.id = sr.role_id
		JOIN workflow_statuses ws ON ws.id = sr.status_id
		WHERE ws.workflow_id = $1
		ORDER BY sr.is_primary DESC, sr.position`, workflowID)
	if err != nil {
		return nil, translate(err)
	}
	defer roleRows.Close()

	for roleRows.Next() {
		var statusID uuid.UUID
		var role domain.StatusRole
		if err := roleRows.Scan(&statusID, &role.RoleID, &role.RoleKey, &role.RoleName,
			&role.IsPrimary, &role.Position); err != nil {
			return nil, translate(err)
		}
		if idx, ok := byID[statusID]; ok {
			statuses[idx].Roles = append(statuses[idx].Roles, role)
		}
	}
	return statuses, translate(roleRows.Err())
}

func (s *Store) ListTransitions(ctx context.Context, q Querier, workflowID uuid.UUID) ([]domain.Transition, error) {
	rows, err := q.Query(ctx, `
		SELECT id, workflow_id, key, name, from_status_id, to_status_id, kind, auto, position
		FROM workflow_transitions WHERE workflow_id = $1 ORDER BY position, key`, workflowID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	transitions := []domain.Transition{}
	byID := map[uuid.UUID]int{}
	for rows.Next() {
		var t domain.Transition
		if err := rows.Scan(&t.ID, &t.WorkflowID, &t.Key, &t.Name, &t.FromStatusID,
			&t.ToStatusID, &t.Kind, &t.Auto, &t.Position); err != nil {
			return nil, translate(err)
		}
		byID[t.ID] = len(transitions)
		transitions = append(transitions, t)
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}

	gateRows, err := q.Query(ctx, `
		SELECT g.id, g.transition_id, g.key, g.name, g.type, g.config, g.blocking, g.position
		FROM gates g
		JOIN workflow_transitions t ON t.id = g.transition_id
		WHERE t.workflow_id = $1
		ORDER BY g.position, g.key`, workflowID)
	if err != nil {
		return nil, translate(err)
	}
	defer gateRows.Close()

	for gateRows.Next() {
		var g domain.Gate
		if err := gateRows.Scan(&g.ID, &g.TransitionID, &g.Key, &g.Name, &g.Type,
			&g.Config, &g.Blocking, &g.Position); err != nil {
			return nil, translate(err)
		}
		if idx, ok := byID[g.TransitionID]; ok {
			transitions[idx].Gates = append(transitions[idx].Gates, g)
		}
	}
	return transitions, translate(gateRows.Err())
}

// AvailableTransitions — переходы, доступные из текущего статуса задачи.
// from_status_id IS NULL означает «из любого статуса» (например, отмена).
func (s *Store) AvailableTransitions(ctx context.Context, q Querier, workflowID, statusID uuid.UUID) ([]domain.Transition, error) {
	all, err := s.ListTransitions(ctx, q, workflowID)
	if err != nil {
		return nil, err
	}
	out := []domain.Transition{}
	for _, t := range all {
		if t.FromStatusID == nil || *t.FromStatusID == statusID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *Store) GetTransitionByKey(ctx context.Context, q Querier, workflowID uuid.UUID, key string) (domain.Transition, error) {
	var t domain.Transition
	err := q.QueryRow(ctx, `
		SELECT id, workflow_id, key, name, from_status_id, to_status_id, kind, auto, position
		FROM workflow_transitions WHERE workflow_id = $1 AND key = $2`, workflowID, key,
	).Scan(&t.ID, &t.WorkflowID, &t.Key, &t.Name, &t.FromStatusID,
		&t.ToStatusID, &t.Kind, &t.Auto, &t.Position)
	if err != nil {
		return t, notFound(err, "переход %q не найден в цикле", key)
	}
	return s.withGates(ctx, q, t)
}

// GetTransition — переход по идентификатору вместе с гейтами. Нужен пределу
// этапа: прогон помнит переход входа идентификатором (CLAB-221).
func (s *Store) GetTransition(ctx context.Context, q Querier, id uuid.UUID) (domain.Transition, error) {
	var t domain.Transition
	err := q.QueryRow(ctx, `
		SELECT id, workflow_id, key, name, from_status_id, to_status_id, kind, auto, position
		FROM workflow_transitions WHERE id = $1`, id,
	).Scan(&t.ID, &t.WorkflowID, &t.Key, &t.Name, &t.FromStatusID,
		&t.ToStatusID, &t.Kind, &t.Auto, &t.Position)
	if err != nil {
		return t, notFound(err, "переход не найден")
	}
	return s.withGates(ctx, q, t)
}

func (s *Store) withGates(ctx context.Context, q Querier, t domain.Transition) (domain.Transition, error) {
	rows, err := q.Query(ctx, `
		SELECT id, transition_id, key, name, type, config, blocking, position
		FROM gates WHERE transition_id = $1 ORDER BY position, key`, t.ID)
	if err != nil {
		return t, translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var g domain.Gate
		if err := rows.Scan(&g.ID, &g.TransitionID, &g.Key, &g.Name, &g.Type,
			&g.Config, &g.Blocking, &g.Position); err != nil {
			return t, translate(err)
		}
		t.Gates = append(t.Gates, g)
	}
	return t, translate(rows.Err())
}

func (s *Store) GetStatus(ctx context.Context, q Querier, statusID uuid.UUID) (domain.Status, error) {
	var st domain.Status
	err := q.QueryRow(ctx, `
		SELECT id, workflow_id, key, name, description, category, position,
		       is_initial, is_terminal, is_optional, enabled_by_default, wip_limit, color
		FROM workflow_statuses WHERE id = $1`, statusID,
	).Scan(&st.ID, &st.WorkflowID, &st.Key, &st.Name, &st.Description, &st.Category,
		&st.Position, &st.IsInitial, &st.IsTerminal, &st.IsOptional, &st.EnabledByDefault, &st.WipLimit, &st.Color)
	if err != nil {
		return st, notFound(err, "статус не найден")
	}
	return st, nil
}

func (s *Store) InitialStatus(ctx context.Context, q Querier, workflowID uuid.UUID) (domain.Status, error) {
	var st domain.Status
	err := q.QueryRow(ctx, `
		SELECT id, workflow_id, key, name, description, category, position,
		       is_initial, is_terminal, is_optional, enabled_by_default, wip_limit, color
		FROM workflow_statuses WHERE workflow_id = $1 AND is_initial`, workflowID,
	).Scan(&st.ID, &st.WorkflowID, &st.Key, &st.Name, &st.Description, &st.Category,
		&st.Position, &st.IsInitial, &st.IsTerminal, &st.IsOptional, &st.EnabledByDefault, &st.WipLimit, &st.Color)
	if err != nil {
		return st, notFound(err, "у цикла не задан начальный статус")
	}
	return st, nil
}

// PrimaryRole — исполнитель статуса и последняя версия его инструкции.
// Возвращает nil-ы, если на статусе никто не работает: у служебных статусов
// вроде "new" или "cancelled" роли нет, и это нормально.
func (s *Store) PrimaryRole(ctx context.Context, q Querier, statusID uuid.UUID) (roleID, roleVersionID *uuid.UUID, err error) {
	var rid, rvid uuid.UUID
	err = q.QueryRow(ctx, `
		SELECT r.id, rv.id
		FROM status_roles sr
		JOIN roles r ON r.id = sr.role_id
		JOIN LATERAL (
			SELECT id FROM role_versions WHERE role_id = r.id ORDER BY version DESC LIMIT 1
		) rv ON true
		WHERE sr.status_id = $1 AND sr.is_primary`, statusID,
	).Scan(&rid, &rvid)
	if err != nil {
		if isNoRows(err) {
			return nil, nil, nil
		}
		return nil, nil, translate(err)
	}
	return uuidPtr(rid), uuidPtr(rvid), nil
}

func (s *Store) GetRoleVersion(ctx context.Context, q Querier, id uuid.UUID) (domain.RoleVersion, error) {
	rv, err := scanRoleVersion(q.QueryRow(ctx,
		`SELECT `+roleVersionCols+` FROM role_versions WHERE id = $1`, id))
	if err != nil {
		return rv, notFound(err, "версия роли не найдена")
	}
	return rv, nil
}
