package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// LockStageRun читает прогон под FOR UPDATE.
//
// Захват обязан идти под блокировкой строки: без неё два агента, пришедшие
// одновременно, прочитают одну и ту же свободную аренду и оба решат, что
// задача досталась им.
func (s *Store) LockStageRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (domain.StageRun, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT id FROM stage_runs WHERE id = $1 FOR UPDATE`, runID).Scan(&id); err != nil {
		return domain.StageRun{}, notFound(err, "прогон этапа не найден")
	}
	r, err := scanStageRun(tx.QueryRow(ctx, stageRunSelect+` WHERE sr.id = $1`, id))
	if err != nil {
		return r, translate(err)
	}
	return r, nil
}

func (s *Store) GetStageRun(ctx context.Context, q Querier, runID uuid.UUID) (domain.StageRun, error) {
	r, err := scanStageRun(q.QueryRow(ctx, stageRunSelect+` WHERE sr.id = $1`, runID))
	if err != nil {
		return r, notFound(err, "прогон этапа не найден")
	}
	return r, nil
}

// OpenReviewRuns — незакрытые прогоны перепроверки внутри этапа.
// Пока хоть один открыт, работать в самом этапе некому: задача стоит в gating.
func (s *Store) OpenReviewRuns(ctx context.Context, q Querier, parentRunID uuid.UUID) ([]domain.StageRun, error) {
	rows, err := q.Query(ctx, stageRunSelect+`
		WHERE sr.parent_run_id = $1 AND sr.kind = 'review'
		  AND sr.state IN ('pending', 'claimed', 'in_progress', 'submitted')
		ORDER BY sr.entered_at`, parentRunID)
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

// LatestReviewRun — последняя попытка перепроверки по конкретному гейту.
// Возвращает nil без ошибки, если ревью ещё не запускалось.
func (s *Store) LatestReviewRun(ctx context.Context, q Querier, parentRunID, gateID uuid.UUID) (*domain.StageRun, error) {
	r, err := scanStageRun(q.QueryRow(ctx, stageRunSelect+`
		WHERE sr.parent_run_id = $1 AND sr.gate_id = $2 AND sr.kind = 'review'
		ORDER BY sr.attempt DESC LIMIT 1`, parentRunID, gateID))
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, translate(err)
	}
	return &r, nil
}

// CreateReviewRun открывает попытку перепроверки внутри этапа.
//
// Статус тот же, что у родителя: ревьюер проверяет результат там же, где он
// сделан, — переход ещё не состоялся. Номер попытки считается по паре
// (родительский прогон, гейт): после доработки тот же гейт запускает ревью
// заново, и обе попытки должны остаться в истории.
func (s *Store) CreateReviewRun(ctx context.Context, tx pgx.Tx, parent domain.StageRun, gateID, roleID, roleVersionID uuid.UUID) (domain.StageRun, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO stage_runs (task_id, status_id, kind, parent_run_id, gate_id,
		                        role_id, role_version_id, attempt, state)
		SELECT $1, $2, 'review', $3, $4, $5, $6,
		       coalesce(max(attempt), 0) + 1, 'pending'
		FROM stage_runs WHERE parent_run_id = $3 AND gate_id = $4 AND kind = 'review'
		RETURNING id`,
		parent.TaskID, parent.StatusID, parent.ID, gateID, roleID, roleVersionID,
	).Scan(&id); err != nil {
		return domain.StageRun{}, translate(err)
	}

	r, err := scanStageRun(tx.QueryRow(ctx, stageRunSelect+` WHERE sr.id = $1`, id))
	if err != nil {
		return r, translate(err)
	}
	return r, nil
}

// ClaimStageRun выдаёт аренду на прогон.
//
// Условие в WHERE повторяет проверки, сделанные вызывающим кодом, намеренно:
// оно и есть точка, в которой захват становится атомарным. Ноль изменённых
// строк означает, что состояние поменялось между чтением и записью.
func (s *Store) ClaimStageRun(ctx context.Context, tx pgx.Tx, runID, actor uuid.UUID, sessionRef string, expires time.Time) (domain.StageRun, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE stage_runs
		SET state             = 'claimed',
		    claimed_by        = $2,
		    claimed_at        = now(),
		    lease_expires_at  = $3,
		    heartbeat_at      = now(),
		    agent_session_ref = nullif($4, '')
		WHERE id = $1
		  AND finished_at IS NULL
		  AND state IN ('pending', 'claimed', 'in_progress', 'submitted')
		  AND (claimed_by IS NULL OR claimed_by = $2
		       OR lease_expires_at IS NULL OR lease_expires_at <= now())`,
		runID, actor, expires, sessionRef)
	if err != nil {
		return domain.StageRun{}, translate(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.StageRun{}, apperr.New(apperr.CodeLeaseHeldByOther,
			"аренда перехвачена другим агентом, пока шёл захват")
	}
	return s.GetStageRun(ctx, tx, runID)
}

// ExtendLease продлевает аренду. Продлить может только держатель: heartbeat
// чужого агента не должен удерживать задачу за тем, кто её уже потерял.
func (s *Store) ExtendLease(ctx context.Context, tx pgx.Tx, runID, actor uuid.UUID, expires time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE stage_runs
		SET lease_expires_at = $3, heartbeat_at = now(),
		    state = CASE WHEN state = 'claimed' THEN 'in_progress' ELSE state END
		WHERE id = $1 AND claimed_by = $2 AND finished_at IS NULL
		  AND lease_expires_at > now()`, runID, actor, expires)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseStageRun снимает аренду, не завершая этап: работа осталась
// незаконченной и прогон возвращается в pending, чтобы его мог забрать другой.
func (s *Store) ReleaseStageRun(ctx context.Context, tx pgx.Tx, runID, actor uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE stage_runs
		SET state = CASE WHEN state = 'submitted' THEN 'submitted' ELSE 'pending' END,
		    claimed_by = NULL, claimed_at = NULL,
		    lease_expires_at = NULL, agent_session_ref = NULL
		WHERE id = $1 AND claimed_by = $2 AND finished_at IS NULL`, runID, actor)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() > 0, nil
}

// ExpireLeases возвращает в pending всё, чья аренда истекла.
//
// Один запрос вместо «прочитать и обновить»: воркер может работать в
// нескольких экземплярах, и RETURNING отдаёт ровно те строки, которые забрал
// именно этот прогон — по ним и пишутся события.
func (s *Store) ExpireLeases(ctx context.Context, q Querier, limit int) ([]domain.StageRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `
		WITH expired AS (
			SELECT id, claimed_by FROM stage_runs
			WHERE state IN ('claimed', 'in_progress')
			  AND lease_expires_at IS NOT NULL
			  AND lease_expires_at <= now()
			ORDER BY lease_expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE stage_runs sr
		SET state = 'pending', claimed_by = NULL, claimed_at = NULL,
		    lease_expires_at = NULL, agent_session_ref = NULL
		FROM expired e
		WHERE sr.id = e.id
		-- Держатель берётся из CTE, а не из sr: RETURNING видит уже
		-- обновлённую строку, где claimed_by обнулён, и событие "аренда
		-- протухла" осталось бы без имени того, чья она была.
		RETURNING sr.id, sr.task_id, sr.kind, sr.attempt,
		          (SELECT handle FROM principals WHERE id = e.claimed_by)`, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.StageRun{}
	for rows.Next() {
		var r domain.StageRun
		if err := rows.Scan(&r.ID, &r.TaskID, &r.Kind, &r.Attempt, &r.ClaimedHandle); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// ListRunArtifacts — то, что сдано именно в этом прогоне.
// Гейт agent_review смотрит вердикт ревьюера, а не всё, что накопила задача.
//
// includeSuperseded нужен как раз ему: вердикт закрытой перепроверки пинится
// прогоном, и более поздняя версия того же kind в другом прогоне не должна
// делать вид, что вердикта не было.
func (s *Store) ListRunArtifacts(ctx context.Context, q Querier, runID uuid.UUID, includeSuperseded bool) ([]domain.Artifact, error) {
	rows, err := q.Query(ctx, artifactSelect+`
		WHERE a.stage_run_id = $1 AND ($2 OR a.status <> 'superseded')
		ORDER BY a.created_at`, runID, includeSuperseded)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Artifact{}
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, a)
	}
	return out, translate(rows.Err())
}

// OpenRun — открытый прогон вместе с задачей, к которой он относится.
// Это то, что агент видит в списке «что можно забрать».
type OpenRun struct {
	Task     domain.Task     `json:"task"`
	Run      domain.StageRun `json:"stage_run"`
	RoleKey  string          `json:"role_key,omitempty"`
	RoleName string          `json:"role_name,omitempty"`
	// Свободна ли аренда и есть ли кому работать. Статус без роли (вроде
	// "new") в очереди виден, но забрать его нельзя — задачу с него двигает
	// человек.
	Claimable bool `json:"claimable"`
}

// OpenRunsFilter — рамки выборки «что сейчас в работе».
type OpenRunsFilter struct {
	StatusKey string
	RoleKey   string
	// Кто спрашивает: задача, назначенная другому, для него не claimable.
	// uuid.Nil — назначение не учитывается.
	Viewer uuid.UUID
	Limit  int
}

// ListOpenRuns — незакрытые прогоны проекта: и этапы, и перепроверки.
//
// Фильтр по роли нужен агенту, подключённому одной ролью: список «что могу
// забрать я» иначе пришлось бы собирать на его стороне.
func (s *Store) ListOpenRuns(ctx context.Context, q Querier, projectID uuid.UUID, f OpenRunsFilter) ([]OpenRun, error) {
	statusKey, roleKey, limit := f.StatusKey, f.RoleKey, f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT sr.id
		FROM stage_runs sr
		JOIN tasks t ON t.id = sr.task_id
		JOIN workflow_statuses ws ON ws.id = sr.status_id
		LEFT JOIN roles ro ON ro.id = sr.role_id
		WHERE t.project_id = $1
		  AND sr.finished_at IS NULL
		  AND sr.state IN ('pending', 'claimed', 'in_progress', 'submitted')
		  AND ($2 = '' OR ws.key = $2)
		  AND ($3 = '' OR ro.key = $3)
		ORDER BY ws.position, t.seq
		LIMIT $4`, projectID, statusKey, roleKey, limit)
	if err != nil {
		return nil, translate(err)
	}
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, translate(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}

	out := make([]OpenRun, 0, len(ids))
	for _, id := range ids {
		run, err := s.GetStageRun(ctx, q, id)
		if err != nil {
			return nil, err
		}
		task, err := s.GetTaskByID(ctx, q, run.TaskID)
		if err != nil {
			return nil, err
		}
		// Назначенную задачу забирает только тот, на кого она назначена
		// (решение 0025): для остальных она в списке видна, но не claimable.
		mine := task.AssigneeID == nil || (f.Viewer != uuid.Nil && *task.AssigneeID == f.Viewer)
		item := OpenRun{
			Task: task, Run: run, RoleKey: run.RoleKey,
			Claimable: run.RoleID != nil && !run.LeaseHeld(time.Now()) && mine,
		}
		if run.RoleID != nil {
			if err := q.QueryRow(ctx, `SELECT name FROM roles WHERE id = $1`, *run.RoleID).
				Scan(&item.RoleName); err != nil && !isNoRows(err) {
				return nil, translate(err)
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *Store) GetTaskByID(ctx context.Context, q Querier, taskID uuid.UUID) (domain.Task, error) {
	t, err := scanTask(q.QueryRow(ctx, taskSelect+` WHERE t.id = $1`, taskID))
	if err != nil {
		return t, notFound(err, "задача не найдена")
	}
	if t.Repositories, err = s.TaskRepositories(ctx, q, t.ID); err != nil {
		return t, err
	}
	return t, nil
}

// LastRunClaimedBy — последний прогон задачи, который забирал этот актор.
// Нужен отчёту о стоимости: раннер отчитывается, когда этап уже закрыт
// переходом и текущий прогон принадлежит следующему статусу.
func (s *Store) LastRunClaimedBy(ctx context.Context, q Querier, taskID, actor uuid.UUID) (domain.StageRun, error) {
	run, err := scanStageRun(q.QueryRow(ctx, stageRunSelect+`
		WHERE sr.task_id = $1 AND sr.claimed_by = $2
		ORDER BY sr.claimed_at DESC LIMIT 1`, taskID, actor))
	if err != nil {
		return run, notFound(err, "вы не забирали ни одного этапа этой задачи")
	}
	return run, nil
}
