package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

const taskSelect = `
	SELECT t.id, t.project_id, p.key, t.parent_id, parent.key, t.key, t.seq, t.title, t.description,
	       t.workflow_id, t.status_id, ws.key, ws.name, t.priority, t.assignee_id, assignee.handle,
	       t.external_ref, t.labels, t.properties, t.created_at, t.updated_at, t.closed_at,
	       t.model_level, t.stage_settings, t.optional_stages
	FROM tasks t
	JOIN projects p ON p.id = t.project_id
	JOIN workflow_statuses ws ON ws.id = t.status_id
	LEFT JOIN tasks parent ON parent.id = t.parent_id
	LEFT JOIN principals assignee ON assignee.id = t.assignee_id`

func scanTask(row interface{ Scan(...any) error }) (domain.Task, error) {
	var t domain.Task
	err := row.Scan(&t.ID, &t.ProjectID, &t.ProjectKey, &t.ParentID, &t.ParentKey, &t.Key, &t.Seq,
		&t.Title, &t.Description, &t.WorkflowID, &t.StatusID, &t.StatusKey, &t.StatusName,
		&t.Priority, &t.AssigneeID, &t.AssigneeHandle, &t.ExternalRef, &t.Labels, &t.Properties,
		&t.CreatedAt, &t.UpdatedAt, &t.ClosedAt, &t.ModelLevel, &t.StageSettings, &t.OptionalStages)
	return t, err
}

type ListTasksFilter struct {
	StatusKey string
	// Ключ родительской задачи: дети эпика. Пусто — фильтра нет.
	ParentKey string
	// Тип задачи. Пусто — все типы: доска показывает и эпики, и задачи.
	Type string
	// Ссылка на тикет основного трекера: «КЛЮЧ» в любой системе или
	// «система:КЛЮЧ». Регистр ключа не важен. Пусто — фильтра нет.
	External string
	Limit    int
}

func (s *Store) ListTasks(ctx context.Context, q Querier, projectID uuid.UUID, f ListTasksFilter) ([]domain.Task, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	// Поиск по ссылке идёт без индекса: он всегда внутри проекта, где задач
	// сотни, а не миллионы. Записи чужой формы (legacy_epic_key из 000011 —
	// строка, а не объект) отсеивает jsonb_typeof; то же правило в Go —
	// domain.ExternalRefMatches.
	extSystem, extKey := domain.ParseExternalQuery(f.External)
	rows, err := q.Query(ctx, taskSelect+`
		WHERE t.project_id = $1
		  AND ($2 = '' OR ws.key = $2)
		  AND ($3 = '' OR parent.key = $3)
		  AND ($4 = '' OR coalesce(t.properties->>'type', $5) = $4)
		  AND ($7 = '' OR EXISTS (
		        SELECT 1
		        FROM jsonb_each(CASE WHEN jsonb_typeof(t.external_ref) = 'object'
		                             THEN t.external_ref ELSE '{}'::jsonb END) e
		        WHERE jsonb_typeof(e.value) = 'object'
		          AND lower(e.value->>'key') = lower($7)
		          AND ($8 = '' OR e.key = $8)))
		ORDER BY ws.position, t.seq
		LIMIT $6`, projectID, f.StatusKey, f.ParentKey, f.Type, domain.DefaultTaskType, limit,
		extKey, extSystem)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	tasks := []domain.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, translate(err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}

	for i := range tasks {
		if tasks[i].Repositories, err = s.TaskRepositories(ctx, q, tasks[i].ID); err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

// GetTaskByKey ищет задачу по человекочитаемому ключу SDLC-42. Ключ уникален
// внутри проекта, поэтому проект вычисляется из префикса самого ключа.
func (s *Store) GetTaskByKey(ctx context.Context, q Querier, taskKey string) (domain.Task, error) {
	projectKey, err := splitTaskKey(taskKey)
	if err != nil {
		return domain.Task{}, err
	}
	t, err := scanTask(q.QueryRow(ctx, taskSelect+`
		WHERE p.key = $1 AND t.key = $2`, projectKey, taskKey))
	if err != nil {
		return t, notFound(err, "задача %q не найдена", taskKey)
	}
	if t.Repositories, err = s.TaskRepositories(ctx, q, t.ID); err != nil {
		return t, err
	}
	return t, nil
}

// LockTaskByKey читает задачу под FOR UPDATE. Переход обязан идти под
// блокировкой строки: без неё два одновременных запроса прогонят гейты на
// одном и том же состоянии и оба переведут задачу дальше.
func (s *Store) LockTaskByKey(ctx context.Context, tx pgx.Tx, taskKey string) (domain.Task, error) {
	projectKey, err := splitTaskKey(taskKey)
	if err != nil {
		return domain.Task{}, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT t.id FROM tasks t
		JOIN projects p ON p.id = t.project_id
		WHERE p.key = $1 AND t.key = $2
		FOR UPDATE OF t`, projectKey, taskKey).Scan(&id)
	if err != nil {
		return domain.Task{}, notFound(err, "задача %q не найдена", taskKey)
	}

	t, err := scanTask(tx.QueryRow(ctx, taskSelect+` WHERE t.id = $1`, id))
	if err != nil {
		return t, translate(err)
	}
	if t.Repositories, err = s.TaskRepositories(ctx, tx, t.ID); err != nil {
		return t, err
	}
	return t, nil
}

// KeyShareTask берёт строку задачи FOR KEY SHARE — тот же замок, что
// внешний ключ ссылающейся строки, только раньше. Запись, которая потом
// возьмёт замок по sha блоба (вложение), обязана сначала взять задачу:
// сдача этапа берёт их в том же порядке (FOR UPDATE задачи, затем sha), и
// обратный порядок при одинаковых байтах — взаимная блокировка. Вложения
// друг друга этим замком не ждут, переход ждёт конца короткой транзакции.
func (s *Store) KeyShareTask(ctx context.Context, tx pgx.Tx, taskID uuid.UUID) error {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id = $1 FOR KEY SHARE`, taskID).Scan(&id); err != nil {
		return notFound(err, "задача не найдена")
	}
	return nil
}

func (s *Store) TaskRepositories(ctx context.Context, q Querier, taskID uuid.UUID) ([]domain.TaskRepository, error) {
	rows, err := q.Query(ctx, `
		SELECT tr.repository_id, r.owner, r.name, r.role_hint, tr.is_primary
		FROM task_repositories tr
		JOIN repositories r ON r.id = tr.repository_id
		WHERE tr.task_id = $1
		ORDER BY tr.is_primary DESC, r.owner, r.name`, taskID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.TaskRepository{}
	for rows.Next() {
		var tr domain.TaskRepository
		if err := rows.Scan(&tr.RepositoryID, &tr.Owner, &tr.Name, &tr.RoleHint, &tr.IsPrimary); err != nil {
			return nil, translate(err)
		}
		out = append(out, tr)
	}
	return out, translate(rows.Err())
}

type CreateTaskInput struct {
	ProjectID uuid.UUID
	// Ключ родительской задачи: так оркестратор вешает нарезку на эпик.
	ParentKey string
	// Тип задачи. Пусто — тип из properties, а если и там пусто — обычная
	// задача. По типу выбирается цикл (решение 0024).
	TaskType    string
	Title       string
	Description *string
	Priority    string
	// Адресное назначение: назначенную задачу заберёт только этот принципал.
	AssigneeID *uuid.UUID
	Labels     []string
	Properties json.RawMessage
	// Ссылка на основной трекер, уже проверенная движком. Пусто — ссылок нет.
	ExternalRef json.RawMessage
	// Ссылки на репозитории: owner/name, просто name или role_hint.
	// Область задачи — не украшение: на ней стоит гейт repo_scope и по ней
	// детектор дрейфа понимает, за какими репозиториями следить.
	Repositories []string
	// Уровень модели задачи и настройки этапов (решение 0060), уже
	// проверенные движком. Ключи этапов движок сверяет с циклом, который
	// выберет CreateTask, в той же транзакции. Пусто — не задано.
	ModelLevel    *string
	StageSettings json.RawMessage
	CreatedBy     uuid.UUID
}

// CreateTask заводит задачу и сразу ставит её на начальный статус цикла.
//
// Счётчик ключей живёт в projects.task_counter и берётся под FOR UPDATE:
// последовательность в отдельном объекте дала бы дыры в нумерации при откате
// транзакции, а ключи задач читают люди.
func (s *Store) CreateTask(ctx context.Context, tx pgx.Tx, in CreateTaskInput) (domain.Task, error) {
	var projectKey string
	var seq int
	if err := tx.QueryRow(ctx, `
		UPDATE projects SET task_counter = task_counter + 1, updated_at = now()
		WHERE id = $1 RETURNING key, task_counter`, in.ProjectID,
	).Scan(&projectKey, &seq); err != nil {
		return domain.Task{}, notFound(err, "проект не найден")
	}

	props, taskType, err := taskTypeInProperties(in.Properties, in.TaskType)
	if err != nil {
		return domain.Task{}, err
	}
	if err := s.checkTaskType(ctx, tx, in.ProjectID, taskType); err != nil {
		return domain.Task{}, err
	}

	// Цикл выбирается типом задачи: эпик идёт по циклу планирования, задача —
	// по циклу разработки, и это два разных набора статусов.
	wf, err := s.GetActiveWorkflowForType(ctx, tx, in.ProjectID, taskType)
	if err != nil {
		return domain.Task{}, err
	}
	initial, err := s.InitialStatus(ctx, tx, wf.ID)
	if err != nil {
		return domain.Task{}, err
	}

	var parentID *uuid.UUID
	if in.ParentKey != "" {
		parent, err := s.GetTaskByKey(ctx, tx, in.ParentKey)
		if err != nil {
			return domain.Task{}, err
		}
		if parent.ProjectID != in.ProjectID {
			return domain.Task{}, apperr.InvalidInput(
				"родительская задача %q из другого проекта", in.ParentKey)
		}
		parentID = &parent.ID
	}

	priority := in.Priority
	if priority == "" {
		priority = "normal"
	}
	labels := in.Labels
	if labels == nil {
		labels = []string{}
	}
	externalRef := in.ExternalRef
	if len(externalRef) == 0 {
		externalRef = json.RawMessage(`{}`)
	}
	stageSettings := in.StageSettings
	if len(stageSettings) == 0 {
		stageSettings = json.RawMessage(`{}`)
	}

	var taskID uuid.UUID
	key := fmt.Sprintf("%s-%d", projectKey, seq)
	if err := tx.QueryRow(ctx, `
		INSERT INTO tasks (project_id, parent_id, key, seq, title, description,
		                   workflow_id, status_id, priority, assignee_id, labels,
		                   properties, created_by, external_ref, model_level, stage_settings)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, nullif($15::text, ''), $16)
		RETURNING id`,
		in.ProjectID, parentID, key, seq, in.Title, in.Description,
		wf.ID, initial.ID, priority, in.AssigneeID, labels, props, in.CreatedBy, externalRef,
		in.ModelLevel, stageSettings,
	).Scan(&taskID); err != nil {
		return domain.Task{}, translate(err)
	}

	for i, ref := range in.Repositories {
		repo, err := s.FindRepository(ctx, tx, in.ProjectID, ref)
		if err != nil {
			return domain.Task{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO task_repositories (project_id, task_id, repository_id, is_primary, added_by)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (task_id, repository_id) DO NOTHING`,
			in.ProjectID, taskID, repo.ID, i == 0, in.CreatedBy); err != nil {
			return domain.Task{}, translate(err)
		}
	}

	if _, err := s.EnterStatus(ctx, tx, taskID, initial.ID, nil, nil); err != nil {
		return domain.Task{}, err
	}

	t, err := scanTask(tx.QueryRow(ctx, taskSelect+` WHERE t.id = $1`, taskID))
	if err != nil {
		return t, translate(err)
	}
	if t.Repositories, err = s.TaskRepositories(ctx, tx, taskID); err != nil {
		return t, err
	}
	return t, nil
}

// nextAttemptSQL — номер попытки, который получит следующий вход задачи $1 в
// статус $2. Одно выражение на EnterStatus и гейт attempts_limit: разойдись
// они, гейт пускал бы задачу на попытку, которой не будет, или держал бы ту,
// что ещё в пределе. Прогоны перепроверки (kind = 'review') делят статус с
// этапом, но входом в статус не являются и в счёт не идут.
const nextAttemptSQL = `
	SELECT coalesce(max(attempt), 0) + 1
	FROM stage_runs WHERE task_id = $1 AND status_id = $2 AND kind = 'stage'`

// NextStageAttempt — номер попытки следующего входа задачи в статус: тот же,
// что выдаст EnterStatus, если переход состоится сейчас.
func (s *Store) NextStageAttempt(ctx context.Context, q Querier, taskID, statusID uuid.UUID) (int, error) {
	var n int
	if err := q.QueryRow(ctx, nextAttemptSQL, taskID, statusID).Scan(&n); err != nil {
		return 0, translate(err)
	}
	return n, nil
}

// EnterStatus открывает новую попытку прохождения статуса.
//
// attempt считается от уже существующих попыток на этой паре (задача, статус):
// повторный заход после отката — это новый stage_run, а не переписанный старый,
// поэтому история "аналитика делалась дважды" остаётся целой (решение 0003).
//
// via — переход, которым задача входит в статус; nil у начального статуса.
// По нему считается предел этапа stage_time_limit (CLAB-221).
func (s *Store) EnterStatus(ctx context.Context, tx pgx.Tx, taskID, statusID uuid.UUID, rejectedFrom, via *uuid.UUID) (domain.StageRun, error) {
	roleID, roleVersionID, err := s.PrimaryRole(ctx, tx, statusID)
	if err != nil {
		return domain.StageRun{}, err
	}

	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO stage_runs (task_id, status_id, role_id, role_version_id, attempt,
		                        state, rejected_from_run_id, entered_via_transition_id)
		VALUES ($1, $2, $3, $4, (`+nextAttemptSQL+`), 'pending', $5, $6)
		RETURNING id`,
		taskID, statusID, roleID, roleVersionID, rejectedFrom, via).Scan(&id); err != nil {
		return domain.StageRun{}, translate(err)
	}

	run, err := scanStageRun(tx.QueryRow(ctx, stageRunSelect+` WHERE sr.id = $1`, id))
	if err != nil {
		return run, translate(err)
	}
	return run, nil
}

const stageRunSelect = `
	SELECT sr.id, sr.task_id, sr.status_id, ws.key, sr.kind, sr.role_id, ro.key,
	       sr.role_version_id, sr.attempt, sr.state, sr.claimed_by, pr.handle,
	       sr.claimed_at, sr.lease_expires_at, sr.heartbeat_at, sr.agent_session_ref,
	       sr.parent_run_id, sr.gate_id, sr.rejected_from_run_id,
	       sr.entered_at, sr.finished_at, sr.outcome, sr.summary,
	       sr.model_level, sr.model_level_source,
	       sr.entered_via_transition_id, sr.overdue_at
	FROM stage_runs sr
	JOIN workflow_statuses ws ON ws.id = sr.status_id
	LEFT JOIN roles ro       ON ro.id = sr.role_id
	LEFT JOIN principals pr  ON pr.id = sr.claimed_by`

func scanStageRun(row interface{ Scan(...any) error }) (domain.StageRun, error) {
	var r domain.StageRun
	var roleKey *string
	err := row.Scan(&r.ID, &r.TaskID, &r.StatusID, &r.StatusKey, &r.Kind, &r.RoleID, &roleKey,
		&r.RoleVersionID, &r.Attempt, &r.State, &r.ClaimedBy, &r.ClaimedHandle,
		&r.ClaimedAt, &r.LeaseExpires, &r.HeartbeatAt, &r.SessionRef,
		&r.ParentRunID, &r.GateID, &r.RejectedFromRunID,
		&r.EnteredAt, &r.FinishedAt, &r.Outcome, &r.Summary,
		&r.ModelLevel, &r.ModelLevelSource,
		&r.EnteredViaTransitionID, &r.OverdueAt)
	if roleKey != nil {
		r.RoleKey = *roleKey
	}
	return r, err
}

// CurrentStageRun — активная попытка на текущем статусе задачи.
// Внутри задачи всегда ровно одна (решение 0007), поэтому выбор без
// неоднозначности: последняя попытка на статусе, где задача стоит сейчас.
func (s *Store) CurrentStageRun(ctx context.Context, q Querier, taskID, statusID uuid.UUID) (domain.StageRun, error) {
	r, err := scanStageRun(q.QueryRow(ctx, stageRunSelect+`
		WHERE sr.task_id = $1 AND sr.status_id = $2 AND sr.kind = 'stage'
		ORDER BY sr.attempt DESC LIMIT 1`, taskID, statusID))
	if err != nil {
		return r, notFound(err, "у задачи нет активной попытки на текущем статусе")
	}
	return r, nil
}

// ListStageRuns отдаёт и этапы, и вложенные прогоны перепроверки: карточка
// задачи показывает их вместе, а различает по kind и parent_run_id.
func (s *Store) ListStageRuns(ctx context.Context, q Querier, taskID uuid.UUID) ([]domain.StageRun, error) {
	rows, err := q.Query(ctx, stageRunSelect+`
		WHERE sr.task_id = $1 ORDER BY sr.entered_at`, taskID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.StageRun{}
	for rows.Next() {
		r, err := scanStageRun(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// RoleVersionsByTask — номер версии роли (role_versions.version) каждого
// прогона задачи одним запросом (CLAB-283). Прогона без роли в ответе нет.
func (s *Store) RoleVersionsByTask(ctx context.Context, q Querier, taskID uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := q.Query(ctx, `
		SELECT sr.id, rv.version
		FROM stage_runs sr
		JOIN role_versions rv ON rv.id = sr.role_version_id
		WHERE sr.task_id = $1`, taskID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := map[uuid.UUID]int{}
	for rows.Next() {
		var (
			id uuid.UUID
			v  int
		)
		if err := rows.Scan(&id, &v); err != nil {
			return nil, translate(err)
		}
		out[id] = v
	}
	return out, translate(rows.Err())
}

func (s *Store) FinishStageRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID, state, outcome string) error {
	_, err := tx.Exec(ctx, `
		UPDATE stage_runs
		SET state = $2, outcome = $3, finished_at = now(), lease_expires_at = NULL
		WHERE id = $1`, runID, state, outcome)
	if err != nil {
		return translate(err)
	}
	// Закрытый этап — погасшие токены задачи (решение 0049): переход вперёд,
	// возврат и закрытие перепроверки проходят здесь.
	_, err = s.RevokeRunTokens(ctx, tx, runID)
	return err
}

// SetTaskStatus переводит задачу в статус и возвращает то, что база проставила
// сама: updated_at и closed_at (nil для нетерминального статуса). Копия задачи,
// прочитанная до перехода, получает их отсюда (CLAB-331).
func (s *Store) SetTaskStatus(ctx context.Context, tx pgx.Tx, taskID, statusID uuid.UUID, terminal bool) (updatedAt time.Time, closedAt *time.Time, err error) {
	err = tx.QueryRow(ctx, `
		UPDATE tasks
		SET status_id = $2,
		    updated_at = now(),
		    closed_at = CASE WHEN $3 THEN now() ELSE NULL END
		WHERE id = $1
		RETURNING updated_at, closed_at`, taskID, statusID, terminal).Scan(&updatedAt, &closedAt)
	return updatedAt, closedAt, translate(err)
}

// UpdateTaskProperties пишет свойства задачи целиком.
//
// Проверку "multi_repo выключили, а в области два репозитория" делает не этот
// метод, а отложенный триггер enforce_task_repo_scope() — он сработает на
// коммите и увидит согласованное состояние, даже если флаг и репозитории
// меняются одной транзакцией.
func (s *Store) UpdateTaskProperties(ctx context.Context, tx pgx.Tx, taskID uuid.UUID, props json.RawMessage) error {
	_, err := tx.Exec(ctx, `
		UPDATE tasks SET properties = $2, updated_at = now() WHERE id = $1`, taskID, props)
	return translate(err)
}

// UpdateTaskFieldsInput — постановка задачи: заголовок, описание, приоритет,
// метки и ссылка на основной трекер. nil означает «не трогать»: правка одного
// поля не должна затирать остальные.
type UpdateTaskFieldsInput struct {
	Title       *string
	Description *string
	Priority    *string
	Labels      *[]string
	// Ссылка заменяется целиком, как метки; `{}` снимает все ссылки.
	ExternalRef *json.RawMessage
	// Уровень модели задачи; пустая строка снимает его (решение 0060).
	ModelLevel *string
	// Настройки этапов целиком — уже слитые движком с прежними.
	StageSettings *json.RawMessage
	// Выбор необязательных этапов полным набором — уже слитый и проверенный
	// движком (решение 0066).
	OptionalStages *json.RawMessage
}

// Any отвечает на вопрос, есть ли что писать.
func (in UpdateTaskFieldsInput) Any() bool {
	return in.Title != nil || in.Description != nil || in.Priority != nil || in.Labels != nil ||
		in.ExternalRef != nil || in.ModelLevel != nil || in.StageSettings != nil ||
		in.OptionalStages != nil
}

// UpdateTaskFields пишет переданные поля постановки и двигает updated_at.
//
// Пустое описание — это очистка: NULL и пустая строка для читателя одно и то
// же, а хранить два способа сказать «описания нет» незачем.
func (s *Store) UpdateTaskFields(ctx context.Context, tx pgx.Tx, taskID uuid.UUID, in UpdateTaskFieldsInput) error {
	var description *string
	if in.Description != nil && *in.Description != "" {
		description = in.Description
	}
	// Метки едут значением, а не указателем на срез: nil-срез драйвер кладёт
	// как NULL, и coalesce оставляет прежний набор; пустой срез его очищает.
	var labels []string
	if in.Labels != nil {
		labels = *in.Labels
		if labels == nil {
			labels = []string{}
		}
	}
	// Ссылка, как и метки, едет значением: nil драйвер кладёт как NULL, и
	// coalesce оставляет прежнюю.
	var externalRef []byte
	if in.ExternalRef != nil {
		externalRef = *in.ExternalRef
		if len(externalRef) == 0 {
			externalRef = []byte(`{}`)
		}
	}
	var stageSettings []byte
	if in.StageSettings != nil {
		stageSettings = *in.StageSettings
		if len(stageSettings) == 0 {
			stageSettings = []byte(`{}`)
		}
	}
	var optionalStages []byte
	if in.OptionalStages != nil {
		optionalStages = *in.OptionalStages
		if len(optionalStages) == 0 {
			optionalStages = []byte(`{}`)
		}
	}
	_, err := tx.Exec(ctx, `
		UPDATE tasks
		SET title           = coalesce($2, title),
		    description     = CASE WHEN $3::boolean THEN $4 ELSE description END,
		    priority        = coalesce($5, priority),
		    labels          = coalesce($6::text[], labels),
		    external_ref    = coalesce($7::jsonb, external_ref),
		    model_level     = CASE WHEN $8::boolean THEN nullif($9::text, '') ELSE model_level END,
		    stage_settings  = coalesce($10::jsonb, stage_settings),
		    optional_stages = coalesce($11::jsonb, optional_stages),
		    updated_at      = now()
		WHERE id = $1`,
		taskID, in.Title, in.Description != nil, description, in.Priority, labels, externalRef,
		in.ModelLevel != nil, in.ModelLevel, stageSettings, optionalStages)
	return translate(err)
}

func (s *Store) AddTaskRepository(ctx context.Context, tx pgx.Tx, t domain.Task, repoID uuid.UUID, stageRunID *uuid.UUID, actor uuid.UUID, note string) error {
	if !t.MultiRepo() && len(t.Repositories) > 0 {
		return apperr.New(apperr.CodeScopeExpansionForbidden,
			"задача %s однорепозиторная: расширить область — решение человека, "+
				"либо включите свойство multi_repo, либо заведите отдельную задачу", t.Key)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO task_repositories (project_id, task_id, repository_id, is_primary,
		                               added_in_stage_run_id, added_by, note)
		VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''))
		ON CONFLICT (task_id, repository_id) DO NOTHING`,
		t.ProjectID, t.ID, repoID, len(t.Repositories) == 0, stageRunID, actor, note)
	return translate(err)
}

// RemoveTaskRepository убирает репозиторий из области задачи и отвечает,
// был ли он основным. Строки в области нет — ошибки здесь нет: что
// убирать можно, решает движок, хранилище только исполняет.
func (s *Store) RemoveTaskRepository(ctx context.Context, tx pgx.Tx, taskID, repoID uuid.UUID) (wasPrimary bool, err error) {
	err = tx.QueryRow(ctx, `
		DELETE FROM task_repositories WHERE task_id = $1 AND repository_id = $2
		RETURNING is_primary`, taskID, repoID).Scan(&wasPrimary)
	if isNoRows(err) {
		return false, nil
	}
	return wasPrimary, translate(err)
}

// PromoteTaskRepository делает репозиторий основным в области задачи.
// Прежний основной к этому моменту уже снят: уникальный индекс
// task_repositories_one_primary второго основного не пропустит.
func (s *Store) PromoteTaskRepository(ctx context.Context, tx pgx.Tx, taskID, repoID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE task_repositories SET is_primary = true
		WHERE task_id = $1 AND repository_id = $2`, taskID, repoID)
	return translate(err)
}

// taskTypeInProperties сводит тип задачи и её свойства в одно место: тип —
// системное свойство, и хранить его отдельной колонкой значило бы держать два
// источника правды об одном.
func taskTypeInProperties(props json.RawMessage, taskType string) (json.RawMessage, string, error) {
	values := map[string]json.RawMessage{}
	if len(props) > 0 {
		if err := json.Unmarshal(props, &values); err != nil {
			return nil, "", apperr.InvalidInput("свойства задачи не объект JSON")
		}
	}

	if taskType == "" {
		if raw, ok := values["type"]; ok {
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, "", apperr.InvalidInput("свойство type должно быть строкой")
			}
			taskType = v
		}
	}
	if taskType == "" {
		taskType = domain.DefaultTaskType
	}

	encoded, err := json.Marshal(taskType)
	if err != nil {
		return nil, "", apperr.Internal(err, "тип задачи")
	}
	values["type"] = encoded

	out, err := json.Marshal(values)
	if err != nil {
		return nil, "", apperr.Internal(err, "свойства задачи")
	}
	return out, taskType, nil
}

// DeclareTaskTypes объявляет типы задач в проекте: системное свойство type
// получает их в перечень значений, сохраняя порядок и не теряя уже
// объявленных. Определения может не быть вовсе — миграция 000011 завела его
// только проектам, существовавшим на тот момент, а новый проект живёт без него
// до первого активированного цикла. Тип по умолчанию объявлен всегда: задача
// без названного типа должна куда-то деваться.
func (s *Store) DeclareTaskTypes(ctx context.Context, q Querier, projectID uuid.UUID, types []string) error {
	declared := append([]string{domain.DefaultTaskType}, types...)
	_, err := q.Exec(ctx, `
		INSERT INTO task_property_defs
		    (project_id, key, name, description, value_type, default_value, enum_values,
		     is_system, expose_to_agent, position)
		VALUES ($1, 'type', 'Тип задачи',
		        'Определяет, по какому циклу идёт задача. Перечень типов пополняется активацией цикла.',
		        'enum', to_jsonb($2::text), $3::text[], true, true, 0)
		ON CONFLICT (project_id, key) DO UPDATE SET enum_values = (
		    SELECT array_agg(v ORDER BY first)
		    FROM (SELECT v, min(ord) AS first
		          FROM unnest(task_property_defs.enum_values || EXCLUDED.enum_values)
		               WITH ORDINALITY AS u(v, ord)
		          GROUP BY v) AS merged)`,
		projectID, domain.DefaultTaskType, declared)
	return translate(err)
}

// checkTaskType сверяет тип с перечнем, объявленным проектом.
func (s *Store) checkTaskType(ctx context.Context, q Querier, projectID uuid.UUID, taskType string) error {
	values, err := s.TaskTypes(ctx, q, projectID)
	if err != nil {
		return err
	}
	for _, v := range values {
		if v == taskType {
			return nil
		}
	}
	return apperr.InvalidInput("тип задачи %q не объявлен в проекте; заведены: %v", taskType, values)
}

// TaskTypes — типы задач, объявленные проектом. Набор типов — данные
// (task_property_defs), а не список в коде: проект расширяет его сам.
func (s *Store) TaskTypes(ctx context.Context, q Querier, projectID uuid.UUID) ([]string, error) {
	var values []string
	err := q.QueryRow(ctx, `
		SELECT enum_values FROM task_property_defs
		WHERE project_id = $1 AND key = 'type'`, projectID).Scan(&values)
	if isNoRows(err) {
		return []string{domain.DefaultTaskType}, nil
	}
	if err != nil {
		return nil, translate(err)
	}
	if len(values) == 0 {
		return []string{domain.DefaultTaskType}, nil
	}
	return values, nil
}

// ChildFact — то, что карточка родителя знает о ребёнке сверх самой задачи:
// категория статуса и ключи задач, которые его блокируют.
type ChildFact struct {
	Category  string
	BlockedBy []string
}

// ChildFacts — категория статуса и входящие blocks каждого ребёнка одним
// запросом на всех: число запросов карточки эпика не растёт с детьми.
func (s *Store) ChildFacts(ctx context.Context, q Querier, parentID uuid.UUID) (map[uuid.UUID]ChildFact, error) {
	rows, err := q.Query(ctx, `
		SELECT t.id, ws.category,
		       coalesce(array_agg(src.key ORDER BY src.seq) FILTER (WHERE src.id IS NOT NULL), '{}')
		FROM tasks t
		JOIN workflow_statuses ws ON ws.id = t.status_id
		LEFT JOIN task_links l ON l.to_task_id = t.id AND l.rel = $2
		LEFT JOIN tasks src ON src.id = l.from_task_id
		WHERE t.parent_id = $1
		GROUP BY t.id, ws.category`, parentID, domain.TaskLinkBlocks)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := map[uuid.UUID]ChildFact{}
	for rows.Next() {
		var id uuid.UUID
		var f ChildFact
		if err := rows.Scan(&id, &f.Category, &f.BlockedBy); err != nil {
			return nil, translate(err)
		}
		out[id] = f
	}
	return out, translate(rows.Err())
}

// ChildTasks — дочерние задачи, в порядке заведения. На них стоит гейт
// dependencies_closed и по ним карточка эпика рисует дерево.
func (s *Store) ChildTasks(ctx context.Context, q Querier, parentID uuid.UUID) ([]domain.Task, error) {
	rows, err := q.Query(ctx, taskSelect+` WHERE t.parent_id = $1 ORDER BY t.seq`, parentID)
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
