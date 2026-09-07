package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

// Редактирование цикла. Правится только черновик: активная версия иммутабельна,
// потому что по ней прямо сейчас идут задачи, а stage_runs и gate_results
// ссылаются на её статусы и гейты. Изменение цикла — это новая версия
// (workflows.version), ровно как правка артефакта — новая версия артефакта.

const workflowCols = `id, project_id, name, version, state, task_types`

func scanWorkflow(row interface{ Scan(...any) error }) (domain.Workflow, error) {
	var wf domain.Workflow
	err := row.Scan(&wf.ID, &wf.ProjectID, &wf.Name, &wf.Version, &wf.State, &wf.TaskTypes)
	return wf, err
}

func (s *Store) ListWorkflows(ctx context.Context, q Querier, projectID uuid.UUID) ([]domain.Workflow, error) {
	rows, err := q.Query(ctx, `SELECT `+workflowCols+`
		FROM workflows WHERE project_id = $1
		ORDER BY (state = 'active') DESC, name, version DESC`, projectID)
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

// WorkflowHead — версия цикла без содержимого. Отдельно от GetWorkflow, потому
// что проверка «это черновик?» перед каждой правкой не должна тащить статусы,
// переходы и гейты.
func (s *Store) WorkflowHead(ctx context.Context, q Querier, id uuid.UUID) (domain.Workflow, error) {
	wf, err := scanWorkflow(q.QueryRow(ctx,
		`SELECT `+workflowCols+` FROM workflows WHERE id = $1`, id))
	if err != nil {
		return wf, notFound(err, "версия цикла не найдена")
	}
	return wf, nil
}

func (s *Store) GetWorkflow(ctx context.Context, q Querier, id uuid.UUID) (domain.Workflow, error) {
	wf, err := s.WorkflowHead(ctx, q, id)
	if err != nil {
		return wf, err
	}
	if wf.Statuses, err = s.ListStatuses(ctx, q, wf.ID); err != nil {
		return wf, err
	}
	if wf.Transitions, err = s.ListTransitions(ctx, q, wf.ID); err != nil {
		return wf, err
	}
	return wf, nil
}

// CreateWorkflow заводит черновик. Номер версии — следующий для этого имени:
// «Стандартный цикл» v2 читается человеком лучше, чем сквозная нумерация.
func (s *Store) CreateWorkflow(ctx context.Context, q Querier, projectID uuid.UUID, name string, taskTypes []string, createdBy uuid.UUID) (domain.Workflow, error) {
	if len(taskTypes) == 0 {
		taskTypes = []string{domain.DefaultTaskType}
	}
	wf, err := scanWorkflow(q.QueryRow(ctx, `
		INSERT INTO workflows (project_id, name, version, state, task_types, created_by)
		SELECT $1, $2, coalesce(max(version), 0) + 1, 'draft', $3, $4
		FROM workflows WHERE project_id = $1 AND name = $2
		RETURNING `+workflowCols, projectID, name, taskTypes, createdBy))
	if err != nil {
		return wf, translate(err)
	}
	return wf, nil
}

// RenameWorkflow переименовывает черновик. Версия при этом не пересчитывается:
// нумерация идёт по имени, и переименование черновика — редкий случай правки
// заголовка, а не заведение новой ветки версий.
func (s *Store) RenameWorkflow(ctx context.Context, q Querier, id uuid.UUID, name string) error {
	_, err := q.Exec(ctx, `
		UPDATE workflows SET name = $2, updated_at = now() WHERE id = $1`, id, name)
	return translate(err)
}

// SetWorkflowTaskTypes меняет типы задач черновика. У активной версии их не
// поменять: задачи уже привязаны к ней своим типом.
func (s *Store) SetWorkflowTaskTypes(ctx context.Context, q Querier, id uuid.UUID, taskTypes []string) error {
	_, err := q.Exec(ctx, `
		UPDATE workflows SET task_types = $2, updated_at = now() WHERE id = $1`, id, taskTypes)
	return translate(err)
}

// CopyWorkflowContent переносит содержимое одной версии цикла в другую.
//
// Идентификаторы статусов и переходов новые, поэтому связи пересобираются по
// ключам: ключ уникален внутри версии цикла и переживает копирование.
func (s *Store) CopyWorkflowContent(ctx context.Context, q Querier, srcID, dstID uuid.UUID) error {
	if _, err := q.Exec(ctx, `
		WITH src AS (
			SELECT id, key FROM workflow_statuses WHERE workflow_id = $1
		), ins AS (
			INSERT INTO workflow_statuses (workflow_id, key, name, description, category,
			            position, is_initial, is_terminal, wip_limit, color)
			SELECT $2, key, name, description, category, position,
			       is_initial, is_terminal, wip_limit, color
			FROM workflow_statuses WHERE workflow_id = $1
			RETURNING id, key
		)
		INSERT INTO status_roles (status_id, role_id, position, is_primary)
		SELECT ins.id, sr.role_id, sr.position, sr.is_primary
		FROM ins
		JOIN src ON src.key = ins.key
		JOIN status_roles sr ON sr.status_id = src.id`, srcID, dstID); err != nil {
		return translate(err)
	}

	_, err := q.Exec(ctx, `
		WITH ins AS (
			INSERT INTO workflow_transitions (workflow_id, key, name, from_status_id,
			            to_status_id, kind, auto, position)
			SELECT $2, t.key, t.name, fs.id, ts.id, t.kind, t.auto, t.position
			FROM workflow_transitions t
			JOIN workflow_statuses ots ON ots.id = t.to_status_id
			JOIN workflow_statuses ts  ON ts.workflow_id = $2 AND ts.key = ots.key
			-- from_status_id IS NULL («из любого статуса») обязан остаться NULL
			LEFT JOIN workflow_statuses ofs ON ofs.id = t.from_status_id
			LEFT JOIN workflow_statuses fs  ON fs.workflow_id = $2 AND fs.key = ofs.key
			WHERE t.workflow_id = $1
			RETURNING id, key
		)
		INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
		SELECT ins.id, g.key, g.name, g.type, g.config, g.blocking, g.position
		FROM ins
		JOIN workflow_transitions ot ON ot.workflow_id = $1 AND ot.key = ins.key
		JOIN gates g ON g.transition_id = ot.id`, srcID, dstID)
	return translate(err)
}

func (s *Store) DeleteWorkflow(ctx context.Context, q Querier, id uuid.UUID) error {
	_, err := q.Exec(ctx, `DELETE FROM workflows WHERE id = $1`, id)
	return translate(err)
}

func (s *Store) SetWorkflowState(ctx context.Context, q Querier, id uuid.UUID, state string) error {
	_, err := q.Exec(ctx, `
		UPDATE workflows SET state = $2, updated_at = now() WHERE id = $1`, id, state)
	return translate(err)
}

// RetireActiveWorkflow отправляет текущую активную версию на покой.
// Вызывается только вместе с активацией новой и в той же транзакции:
// частичный уникальный индекс не даёт двум версиям быть активными.
// RetireActiveWorkflows уводит на покой действующие циклы, которые ведут те же
// типы задач, что и активируемая версия. Циклы других типов не трогаются: в
// проекте с эпиками их несколько, и активация цикла эпиков не должна оставить
// обычные задачи без цикла.
func (s *Store) RetireActiveWorkflows(ctx context.Context, q Querier, projectID uuid.UUID, taskTypes []string) ([]uuid.UUID, error) {
	if len(taskTypes) == 0 {
		taskTypes = []string{domain.DefaultTaskType}
	}
	rows, err := q.Query(ctx, `
		UPDATE workflows SET state = 'retired', updated_at = now()
		WHERE project_id = $1 AND state = 'active' AND task_types && $2
		RETURNING id`, projectID, taskTypes)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, translate(err)
		}
		out = append(out, id)
	}
	return out, translate(rows.Err())
}

// OpenTaskOnWorkflow — незакрытая задача, оставшаяся на прежней версии цикла.
type OpenTaskOnWorkflow struct {
	Key       string `json:"task"`
	StatusKey string `json:"status"`
}

// OpenTasksOnWorkflow — задачи, которые доходят по этой версии цикла.
//
// Активация новой версии их не трогает: задача идёт по тому циклу, на котором
// начата (см. Engine.Execute). Список нужен, чтобы сказать человеку, что часть
// доски ещё живёт по старым правилам.
func (s *Store) OpenTasksOnWorkflow(ctx context.Context, q Querier, workflowID uuid.UUID) ([]OpenTaskOnWorkflow, error) {
	rows, err := q.Query(ctx, `
		SELECT t.key, ws.key
		FROM tasks t
		JOIN workflow_statuses ws ON ws.id = t.status_id
		WHERE t.workflow_id = $1 AND t.closed_at IS NULL
		ORDER BY t.seq`, workflowID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []OpenTaskOnWorkflow{}
	for rows.Next() {
		var t OpenTaskOnWorkflow
		if err := rows.Scan(&t.Key, &t.StatusKey); err != nil {
			return nil, translate(err)
		}
		out = append(out, t)
	}
	return out, translate(rows.Err())
}

// ── статусы ─────────────────────────────────────────────────────────────────

type StatusInput struct {
	Key         string
	Name        string
	Description *string
	Category    string
	Position    *int
	IsInitial   *bool
	IsTerminal  *bool
	WipLimit    *int
	Color       *string
}

func (s *Store) CreateStatus(ctx context.Context, q Querier, workflowID uuid.UUID, in StatusInput) (domain.Status, error) {
	if in.IsInitial != nil && *in.IsInitial {
		if err := s.clearInitial(ctx, q, workflowID); err != nil {
			return domain.Status{}, err
		}
	}
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO workflow_statuses (workflow_id, key, name, description, category,
		            position, is_initial, is_terminal, wip_limit, color)
		SELECT $1, $2, $3, $4, $5,
		       coalesce($6, (SELECT coalesce(max(position), 0) + 1
		                     FROM workflow_statuses WHERE workflow_id = $1)),
		       coalesce($7, false), coalesce($8, false), $9, $10
		RETURNING id`,
		workflowID, in.Key, in.Name, in.Description, in.Category, in.Position,
		in.IsInitial, in.IsTerminal, in.WipLimit, in.Color).Scan(&id)
	if err != nil {
		return domain.Status{}, translate(err)
	}
	return s.GetStatus(ctx, q, id)
}

func (s *Store) UpdateStatus(ctx context.Context, q Querier, workflowID uuid.UUID, key string, in StatusInput) (domain.Status, error) {
	st, err := s.GetStatusByKey(ctx, q, workflowID, key)
	if err != nil {
		return st, err
	}
	if in.IsInitial != nil && *in.IsInitial {
		if err := s.clearInitial(ctx, q, workflowID); err != nil {
			return st, err
		}
	}
	if _, err := q.Exec(ctx, `
		UPDATE workflow_statuses SET
			key         = coalesce($2, key),
			name        = coalesce($3, name),
			description = coalesce($4, description),
			category    = coalesce($5, category),
			position    = coalesce($6, position),
			is_initial  = coalesce($7, is_initial),
			is_terminal = coalesce($8, is_terminal),
			wip_limit   = coalesce($9, wip_limit),
			color       = coalesce($10, color)
		WHERE id = $1`, st.ID, nullIfEmpty(in.Key), nullIfEmpty(in.Name), in.Description,
		nullIfEmpty(in.Category), in.Position, in.IsInitial, in.IsTerminal,
		in.WipLimit, in.Color); err != nil {
		return st, translate(err)
	}
	return s.GetStatus(ctx, q, st.ID)
}

// clearInitial снимает признак начального статуса со всех остальных.
// Иначе частичный уникальный индекс отдаёт нарушение уникальности вместо
// понятного «начальный статус теперь другой».
func (s *Store) clearInitial(ctx context.Context, q Querier, workflowID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE workflow_statuses SET is_initial = false
		WHERE workflow_id = $1 AND is_initial`, workflowID)
	return translate(err)
}

// StatusRoles — роли одного статуса. ListStatuses собирает их для всего цикла
// сразу, но после правки набора нужен ровно один статус.
func (s *Store) StatusRoles(ctx context.Context, q Querier, statusID uuid.UUID) ([]domain.StatusRole, error) {
	rows, err := q.Query(ctx, `
		SELECT sr.role_id, r.key, r.name, sr.is_primary, sr.position
		FROM status_roles sr
		JOIN roles r ON r.id = sr.role_id
		WHERE sr.status_id = $1
		ORDER BY sr.is_primary DESC, sr.position`, statusID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.StatusRole{}
	for rows.Next() {
		var r domain.StatusRole
		if err := rows.Scan(&r.RoleID, &r.RoleKey, &r.RoleName, &r.IsPrimary, &r.Position); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

func (s *Store) GetStatusByKey(ctx context.Context, q Querier, workflowID uuid.UUID, key string) (domain.Status, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT id FROM workflow_statuses WHERE workflow_id = $1 AND key = $2`,
		workflowID, key).Scan(&id)
	if err != nil {
		return domain.Status{}, notFound(err, "статус %q не найден в этой версии цикла", key)
	}
	return s.GetStatus(ctx, q, id)
}

func (s *Store) DeleteStatus(ctx context.Context, q Querier, workflowID uuid.UUID, key string) error {
	tag, err := q.Exec(ctx, `
		DELETE FROM workflow_statuses WHERE workflow_id = $1 AND key = $2`, workflowID, key)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("статус %q не найден в этой версии цикла", key)
	}
	return nil
}

// ReorderStatuses переставляет колонки одной транзакцией.
//
// По одному запросу на статус переставить нельзя: уникальность (workflow_id,
// position) нарушается на любом промежуточном шаге. Ради этого ограничение
// объявлено DEFERRABLE — проверка откладывается до конца транзакции.
func (s *Store) ReorderStatuses(ctx context.Context, q Querier, workflowID uuid.UUID, keys []string) error {
	for i, key := range keys {
		tag, err := q.Exec(ctx, `
			UPDATE workflow_statuses SET position = $3
			WHERE workflow_id = $1 AND key = $2`, workflowID, key, i+1)
		if err != nil {
			return translate(err)
		}
		if tag.RowsAffected() == 0 {
			return apperr.InvalidInput("статуса %q нет в этой версии цикла", key)
		}
	}
	return nil
}

type StatusRoleInput struct {
	RoleKey   string
	IsPrimary bool
	Position  int
}

// SetStatusRoles заменяет набор ролей статуса целиком: исполнитель и
// проверяющие задаются вместе, потому что «основная роль ровно одна» —
// свойство набора, а не отдельной строки.
func (s *Store) SetStatusRoles(ctx context.Context, q Querier, projectID uuid.UUID, statusID uuid.UUID, roles []StatusRoleInput) error {
	if _, err := q.Exec(ctx, `DELETE FROM status_roles WHERE status_id = $1`, statusID); err != nil {
		return translate(err)
	}
	for _, r := range roles {
		role, err := s.GetRoleByKey(ctx, q, projectID, r.RoleKey)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO status_roles (status_id, role_id, position, is_primary)
			VALUES ($1, $2, $3, $4)`, statusID, role.ID, r.Position, r.IsPrimary); err != nil {
			return translate(err)
		}
	}
	return nil
}

// ── переходы и гейты ────────────────────────────────────────────────────────

type TransitionInput struct {
	Key  string
	Name string
	// Ключ статуса-источника; пустая строка — «из любого статуса».
	FromStatus *string
	ToStatus   string
	Kind       string
	Auto       *bool
	Position   *int
}

func (s *Store) CreateTransition(ctx context.Context, q Querier, workflowID uuid.UUID, in TransitionInput) (domain.Transition, error) {
	from, to, err := s.resolveEnds(ctx, q, workflowID, in)
	if err != nil {
		return domain.Transition{}, err
	}
	var key string
	err = q.QueryRow(ctx, `
		INSERT INTO workflow_transitions (workflow_id, key, name, from_status_id,
		            to_status_id, kind, auto, position)
		VALUES ($1, $2, $3, $4, $5, $6, coalesce($7, false), coalesce($8, 0))
		RETURNING key`,
		workflowID, in.Key, in.Name, from, to, in.Kind, in.Auto, in.Position).Scan(&key)
	if err != nil {
		return domain.Transition{}, translate(err)
	}
	return s.GetTransitionByKey(ctx, q, workflowID, key)
}

func (s *Store) UpdateTransition(ctx context.Context, q Querier, workflowID uuid.UUID, key string, in TransitionInput) (domain.Transition, error) {
	t, err := s.GetTransitionByKey(ctx, q, workflowID, key)
	if err != nil {
		return t, err
	}
	from, to, err := s.resolveEnds(ctx, q, workflowID, in)
	if err != nil {
		return t, err
	}
	// from_status_id меняется только если о нём сказали: NULL здесь —
	// осмысленное значение «из любого статуса», и coalesce его не различает.
	if _, err := q.Exec(ctx, `
		UPDATE workflow_transitions SET
			key            = coalesce($2, key),
			name           = coalesce($3, name),
			from_status_id = CASE WHEN $4 THEN $5 ELSE from_status_id END,
			to_status_id   = coalesce($6, to_status_id),
			kind           = coalesce($7, kind),
			auto           = coalesce($8, auto),
			position       = coalesce($9, position)
		WHERE id = $1`, t.ID, nullIfEmpty(in.Key), nullIfEmpty(in.Name),
		in.FromStatus != nil, from, to, nullIfEmpty(in.Kind), in.Auto, in.Position); err != nil {
		return t, translate(err)
	}
	newKey := t.Key
	if in.Key != "" {
		newKey = in.Key
	}
	return s.GetTransitionByKey(ctx, q, workflowID, newKey)
}

// resolveEnds переводит ключи статусов в идентификаторы этой же версии цикла.
func (s *Store) resolveEnds(ctx context.Context, q Querier, workflowID uuid.UUID, in TransitionInput) (from, to *uuid.UUID, err error) {
	if in.FromStatus != nil && *in.FromStatus != "" {
		st, err := s.GetStatusByKey(ctx, q, workflowID, *in.FromStatus)
		if err != nil {
			return nil, nil, err
		}
		from = &st.ID
	}
	if in.ToStatus != "" {
		st, err := s.GetStatusByKey(ctx, q, workflowID, in.ToStatus)
		if err != nil {
			return nil, nil, err
		}
		to = &st.ID
	}
	return from, to, nil
}

func (s *Store) DeleteTransition(ctx context.Context, q Querier, workflowID uuid.UUID, key string) error {
	tag, err := q.Exec(ctx, `
		DELETE FROM workflow_transitions WHERE workflow_id = $1 AND key = $2`, workflowID, key)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("перехода %q нет в этой версии цикла", key)
	}
	return nil
}

type GateInput struct {
	Key      string
	Name     string
	Type     string
	Config   json.RawMessage
	Blocking *bool
	Position *int
}

func (s *Store) CreateGate(ctx context.Context, q Querier, transitionID uuid.UUID, in GateInput) (domain.Gate, error) {
	if len(in.Config) == 0 {
		in.Config = json.RawMessage(`{}`)
	}
	var g domain.Gate
	err := q.QueryRow(ctx, `
		INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
		VALUES ($1, $2, $3, $4, $5, coalesce($6, true), coalesce($7, 0))
		RETURNING id, transition_id, key, name, type, config, blocking, position`,
		transitionID, in.Key, in.Name, in.Type, in.Config, in.Blocking, in.Position,
	).Scan(&g.ID, &g.TransitionID, &g.Key, &g.Name, &g.Type, &g.Config, &g.Blocking, &g.Position)
	if err != nil {
		return g, translate(err)
	}
	return g, nil
}

func (s *Store) UpdateGate(ctx context.Context, q Querier, transitionID uuid.UUID, key string, in GateInput) (domain.Gate, error) {
	var g domain.Gate
	err := q.QueryRow(ctx, `
		UPDATE gates SET
			key      = coalesce($3, key),
			name     = coalesce($4, name),
			type     = coalesce($5, type),
			config   = coalesce($6, config),
			blocking = coalesce($7, blocking),
			position = coalesce($8, position)
		WHERE transition_id = $1 AND key = $2
		RETURNING id, transition_id, key, name, type, config, blocking, position`,
		transitionID, key, nullIfEmpty(in.Key), nullIfEmpty(in.Name), nullIfEmpty(in.Type),
		nullIfEmptyJSON(in.Config), in.Blocking, in.Position,
	).Scan(&g.ID, &g.TransitionID, &g.Key, &g.Name, &g.Type, &g.Config, &g.Blocking, &g.Position)
	if err != nil {
		return g, notFound(err, "гейта %q нет на этом переходе", key)
	}
	return g, nil
}

func (s *Store) DeleteGate(ctx context.Context, q Querier, transitionID uuid.UUID, key string) error {
	tag, err := q.Exec(ctx, `
		DELETE FROM gates WHERE transition_id = $1 AND key = $2`, transitionID, key)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("гейта %q нет на этом переходе", key)
	}
	return nil
}

// nullIfEmpty отправляет в запрос NULL вместо пустой строки, чтобы coalesce в
// UPDATE означал «поле не прислали», а не «поле обнулили».
func nullIfEmpty(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func nullIfEmptyJSON(v json.RawMessage) *json.RawMessage {
	if len(v) == 0 {
		return nil
	}
	return &v
}
