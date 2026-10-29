package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
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
			            position, is_initial, is_terminal, is_optional, enabled_by_default,
			            wip_limit, color)
			SELECT $2, key, name, description, category, position,
			       is_initial, is_terminal, is_optional, enabled_by_default, wip_limit, color
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

// LockProject берёт строку проекта под FOR UPDATE до конца транзакции.
//
// Активация цикла и создание задачи спорят за то, какая версия действующая:
// создание берёт ключ из projects.task_counter под тем же замком, поэтому
// начатое до активации создание дождётся коммит активации и получит уже новую
// версию, а активация дождётся коммита начатого создания и перенесёт его
// задачу вместе с остальным бэклогом. Без замка задача, заведённая в момент
// активации, ложилась бы на только что снятую версию.
func (s *Store) LockProject(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) error {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM projects WHERE id = $1 FOR UPDATE`, projectID).Scan(&id); err != nil {
		return notFound(err, "проект не найден")
	}
	return nil
}

// RetiredWorkflows — снятые версии циклов проекта, которые вели те же типы
// задач, что и переданные. Нужны при активации: задача из бэклога могла
// остаться не на только что снятой версии, а на более ранней — если её
// никто не трогал две активации подряд или прежняя активация ещё не умела
// переносить бэклог (решение 0036).
func (s *Store) RetiredWorkflows(ctx context.Context, q Querier, projectID uuid.UUID, taskTypes []string) ([]uuid.UUID, error) {
	if len(taskTypes) == 0 {
		taskTypes = []string{domain.DefaultTaskType}
	}
	rows, err := q.Query(ctx, `
		SELECT id FROM workflows
		WHERE project_id = $1 AND state = 'retired' AND task_types && $2
		ORDER BY version`, projectID, taskTypes)
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

// MovedTask — задача, переехавшая на новую версию цикла при активации.
type MovedTask struct {
	Key        string `json:"task"`
	FromStatus string `json:"from_status"`
	ToStatus   string `json:"to_status"`
}

// MoveWaitingTasks переводит на новую версию цикла задачи, которые ещё не
// начинались: стоят на статусе категории backlog, и текущую попытку никто не
// забирал. Таким задачам нечего терять — ни артефактов этапа, ни решений по
// гейтам, — а правила им нужны актуальные (решение 0036).
//
// Статус выбирается по ключу, если в новой версии есть такой же статус той же
// категории; иначе — начальный статус новой версии. Старая попытка закрывается
// как abandoned/cancelled, на новом статусе открывается свежая с текущей
// версией роли, в ленту задачи ложатся события переезда и входа в статус.
//
// Всё — одним запросом: бэклог проекта может быть в тысячи задач, а перенос
// идёт в транзакции активации, и построчный цикл (десяток запросов на задачу)
// упирался бы в таймаут запроса уже на десятке тысяч. Задачи держатся
// FOR UPDATE до коммита: start и cancel по ним дождутся новой версии.
func (s *Store) MoveWaitingTasks(ctx context.Context, tx pgx.Tx, fromWorkflowID uuid.UUID, to domain.Workflow, actor uuid.UUID) ([]MovedTask, error) {
	initial, err := s.InitialStatus(ctx, tx, to.ID)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		WITH waiting AS (
			SELECT t.id AS task_id, t.key, t.seq, ws.key AS from_status, sr.id AS run_id
			FROM tasks t
			JOIN workflow_statuses ws ON ws.id = t.status_id
			JOIN LATERAL (
				SELECT id, state, claimed_by FROM stage_runs
				WHERE task_id = t.id AND status_id = t.status_id AND kind = 'stage'
				ORDER BY attempt DESC LIMIT 1
			) sr ON true
			WHERE t.workflow_id = $1 AND t.closed_at IS NULL
			  AND ws.category = 'backlog'
			  AND sr.state = 'pending' AND sr.claimed_by IS NULL
			FOR UPDATE OF t
		),
		dest AS MATERIALIZED (
			SELECT w.task_id, w.key, w.seq, w.from_status, w.run_id,
			       coalesce(same.id, $3::uuid) AS to_status_id,
			       coalesce(same.key, $4::text) AS to_status,
			       gen_random_uuid() AS new_run_id,
			       coalesce((SELECT max(attempt) FROM stage_runs x
			                 WHERE x.task_id = w.task_id
			                   AND x.status_id = coalesce(same.id, $3::uuid)
			                   AND x.kind = 'stage'), 0) + 1 AS attempt
			FROM waiting w
			LEFT JOIN workflow_statuses same
			       ON same.workflow_id = $2 AND same.key = w.from_status AND same.category = 'backlog'
		),
		closed AS (
			UPDATE stage_runs SET state = 'abandoned', outcome = 'cancelled',
			                      finished_at = now(), lease_expires_at = NULL
			WHERE id IN (SELECT run_id FROM dest)
			RETURNING id
		),
		moved AS (
			UPDATE tasks t SET workflow_id = $2, status_id = d.to_status_id, updated_at = now()
			FROM dest d WHERE t.id = d.task_id
			RETURNING t.id
		),
		status_role AS (
			SELECT sr.status_id, r.id AS role_id, rv.id AS role_version_id
			FROM status_roles sr
			JOIN roles r ON r.id = sr.role_id
			JOIN LATERAL (
				SELECT id FROM role_versions WHERE role_id = r.id ORDER BY version DESC LIMIT 1
			) rv ON true
			WHERE sr.is_primary AND sr.status_id IN (SELECT DISTINCT to_status_id FROM dest)
		),
		opened AS (
			INSERT INTO stage_runs (id, task_id, status_id, role_id, role_version_id, attempt, state)
			SELECT d.new_run_id, d.task_id, d.to_status_id, ro.role_id, ro.role_version_id,
			       d.attempt, 'pending'
			FROM dest d
			LEFT JOIN status_role ro ON ro.status_id = d.to_status_id
			RETURNING id
		),
		events AS (
			INSERT INTO task_events (task_id, stage_run_id, type, actor_id, payload, created_at)
			SELECT d.task_id, d.new_run_id, $5::text, $9::uuid,
			       jsonb_build_object(
			           'from_workflow_id', $1::text, 'to_workflow_id', $2::text,
			           'workflow', $7::text, 'version', $8::int,
			           'from_status', d.from_status, 'to_status', d.to_status),
			       clock_timestamp()
			FROM dest d
			UNION ALL
			SELECT d.task_id, d.new_run_id, $6::text, $9::uuid,
			       jsonb_build_object('status', d.to_status, 'attempt', d.attempt),
			       clock_timestamp()
			FROM dest d
			RETURNING id
		)
		SELECT key, from_status, to_status FROM dest ORDER BY seq`,
		fromWorkflowID, to.ID, initial.ID, initial.Key,
		domain.EventTaskWorkflowMoved, domain.EventStageEntered,
		to.Name, to.Version, actor)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []MovedTask{}
	for rows.Next() {
		var m MovedTask
		if err := rows.Scan(&m.Key, &m.FromStatus, &m.ToStatus); err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
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
	// IsOptional и EnabledByDefault согласует слой сервиса (решение 0066,
	// п. 2): сюда приходит уже итоговая пара, nil — не менять.
	IsOptional       *bool
	EnabledByDefault *bool
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
		            position, is_initial, is_terminal, wip_limit, color,
		            is_optional, enabled_by_default)
		SELECT $1, $2, $3, $4, $5,
		       coalesce($6, (SELECT coalesce(max(position), 0) + 1
		                     FROM workflow_statuses WHERE workflow_id = $1)),
		       coalesce($7, false), coalesce($8, false), $9, $10,
		       coalesce($11, false), coalesce($12, true)
		RETURNING id`,
		workflowID, in.Key, in.Name, in.Description, in.Category, in.Position,
		in.IsInitial, in.IsTerminal, in.WipLimit, in.Color,
		in.IsOptional, in.EnabledByDefault).Scan(&id)
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
			color       = coalesce($10, color),
			is_optional        = coalesce($11, is_optional),
			enabled_by_default = coalesce($12, enabled_by_default)
		WHERE id = $1`, st.ID, nullIfEmpty(in.Key), nullIfEmpty(in.Name), in.Description,
		nullIfEmpty(in.Category), in.Position, in.IsInitial, in.IsTerminal,
		in.WipLimit, in.Color, in.IsOptional, in.EnabledByDefault); err != nil {
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
