package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

// TaskRepoRef — ветка, PR или коммит, привязанные к задаче.
type TaskRepoRef struct {
	ID           uuid.UUID       `json:"id"`
	TaskID       uuid.UUID       `json:"task_id"`
	RepositoryID uuid.UUID       `json:"repository_id"`
	Repository   string          `json:"repository"`
	RefType      string          `json:"ref_type"`
	Ref          string          `json:"ref"`
	URL          *string         `json:"url,omitempty"`
	State        *string         `json:"state,omitempty"`
	Meta         json.RawMessage `json:"meta"`
}

type LinkRepoRefInput struct {
	Task         domain.Task
	RepositoryID uuid.UUID
	RefType      string
	Ref          string
	URL          string
	State        string
	Meta         json.RawMessage
}

// LinkRepoRef привязывает PR (ветку, коммит) к задаче.
//
// Повторная привязка того же ref обновляет состояние, а не заводит вторую
// строку: PR один, а его состояние меняется — гейт repo_ref_required смотрит
// именно на него.
func (s *Store) LinkRepoRef(ctx context.Context, q Querier, in LinkRepoRefInput) (TaskRepoRef, error) {
	inScope := false
	for _, r := range in.Task.Repositories {
		if r.RepositoryID == in.RepositoryID {
			inScope = true
			break
		}
	}
	if !inScope {
		return TaskRepoRef{}, apperr.New(apperr.CodeOutOfTaskScope,
			"репозиторий не входит в область задачи %s: сам код изменений не должен уезжать за её пределы",
			in.Task.Key)
	}

	meta := in.Meta
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO task_repo_refs (task_id, repository_id, ref_type, ref, url, state, meta)
		VALUES ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''), $7)
		ON CONFLICT (task_id, repository_id, ref_type, ref) DO UPDATE
		SET url = coalesce(excluded.url, task_repo_refs.url),
		    state = coalesce(excluded.state, task_repo_refs.state),
		    meta = excluded.meta
		RETURNING id`,
		in.Task.ID, in.RepositoryID, in.RefType, in.Ref, in.URL, in.State, meta,
	).Scan(&id); err != nil {
		return TaskRepoRef{}, translate(err)
	}

	refs, err := s.ListRepoRefs(ctx, q, in.Task.ID)
	if err != nil {
		return TaskRepoRef{}, err
	}
	for _, r := range refs {
		if r.ID == id {
			return r, nil
		}
	}
	return TaskRepoRef{}, apperr.Internal(nil, "привязка не найдена сразу после записи")
}

func (s *Store) ListRepoRefs(ctx context.Context, q Querier, taskID uuid.UUID) ([]TaskRepoRef, error) {
	rows, err := q.Query(ctx, `
		SELECT rr.id, rr.task_id, rr.repository_id, r.owner || '/' || r.name,
		       rr.ref_type, rr.ref, rr.url, rr.state, rr.meta
		FROM task_repo_refs rr
		JOIN repositories r ON r.id = rr.repository_id
		WHERE rr.task_id = $1
		ORDER BY r.owner, r.name, rr.ref_type, rr.ref`, taskID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []TaskRepoRef{}
	for rows.Next() {
		var r TaskRepoRef
		if err := rows.Scan(&r.ID, &r.TaskID, &r.RepositoryID, &r.Repository,
			&r.RefType, &r.Ref, &r.URL, &r.State, &r.Meta); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// TaskBaseline — коммит, на котором построены текущие артефакты задачи.
type TaskBaseline struct {
	Repository string    `json:"repository"`
	Branch     string    `json:"branch"`
	CommitSHA  string    `json:"commit"`
	SetAt      time.Time `json:"set_at"`
}

func (s *Store) ListBaselines(ctx context.Context, q Querier, taskID uuid.UUID) ([]TaskBaseline, error) {
	rows, err := q.Query(ctx, `
		SELECT r.owner || '/' || r.name, b.branch, b.commit_sha, b.set_at
		FROM task_baselines b
		JOIN repositories r ON r.id = b.repository_id
		WHERE b.task_id = $1
		ORDER BY r.owner, r.name, b.branch`, taskID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []TaskBaseline{}
	for rows.Next() {
		var b TaskBaseline
		if err := rows.Scan(&b.Repository, &b.Branch, &b.CommitSHA, &b.SetAt); err != nil {
			return nil, translate(err)
		}
		out = append(out, b)
	}
	return out, translate(rows.Err())
}

// DriftSignal — расхождение зоны задачи с тем, что уехало в main, или
// пересечение с другой задачей в полёте.
type DriftSignal struct {
	ID           uuid.UUID       `json:"id"`
	TaskID       uuid.UUID       `json:"task_id"`
	Kind         string          `json:"kind"`
	Severity     string          `json:"severity"`
	State        string          `json:"state"`
	Repository   *string         `json:"repository,omitempty"`
	FromCommit   *string         `json:"from_commit,omitempty"`
	ToCommit     *string         `json:"to_commit,omitempty"`
	RelatedTask  *string         `json:"related_task,omitempty"`
	Overlap      json.RawMessage `json:"overlap"`
	DetectedAt   time.Time       `json:"detected_at"`
	Resolution   *string         `json:"resolution,omitempty"`
	ResolvedAt   *time.Time      `json:"resolved_at,omitempty"`
	ResolvedByID *uuid.UUID      `json:"resolved_by,omitempty"`
}

func (s *Store) ListDriftSignals(ctx context.Context, q Querier, taskID uuid.UUID, states []string) ([]DriftSignal, error) {
	if states == nil {
		states = []string{}
	}
	rows, err := q.Query(ctx, `
		SELECT d.id, d.task_id, d.kind, d.severity, d.state,
		       r.owner || '/' || r.name, d.from_commit, d.to_commit, rt.key,
		       d.overlap, d.detected_at, d.resolution, d.resolved_at, d.resolved_by
		FROM task_drift_signals d
		LEFT JOIN repositories r ON r.id = d.repository_id
		LEFT JOIN tasks rt       ON rt.id = d.related_task_id
		WHERE d.task_id = $1
		  AND (cardinality($2::text[]) = 0 OR d.state = ANY($2))
		ORDER BY d.detected_at DESC`, taskID, states)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []DriftSignal{}
	for rows.Next() {
		var d DriftSignal
		if err := rows.Scan(&d.ID, &d.TaskID, &d.Kind, &d.Severity, &d.State,
			&d.Repository, &d.FromCommit, &d.ToCommit, &d.RelatedTask,
			&d.Overlap, &d.DetectedAt, &d.Resolution, &d.ResolvedAt, &d.ResolvedByID); err != nil {
			return nil, translate(err)
		}
		out = append(out, d)
	}
	return out, translate(rows.Err())
}

func (s *Store) GetDriftSignal(ctx context.Context, q Querier, id uuid.UUID) (DriftSignal, error) {
	var taskID uuid.UUID
	if err := q.QueryRow(ctx,
		`SELECT task_id FROM task_drift_signals WHERE id = $1`, id).Scan(&taskID); err != nil {
		return DriftSignal{}, notFound(err, "сигнал дрейфа не найден")
	}
	all, err := s.ListDriftSignals(ctx, q, taskID, nil)
	if err != nil {
		return DriftSignal{}, err
	}
	for _, d := range all {
		if d.ID == id {
			return d, nil
		}
	}
	return DriftSignal{}, apperr.NotFound("сигнал дрейфа не найден")
}

// ResolveDriftSignal закрывает сигнал разбором или отклоняет его.
//
// Базовая линия при resolved переставляется на коммит, относительно которого
// дрейф разбирался: иначе следующий прогон детектора заведёт тот же сигнал
// заново, а разбор окажется бессмысленным.
func (s *Store) ResolveDriftSignal(ctx context.Context, q Querier, signalID, actor uuid.UUID, resolution, note string, artifactID *uuid.UUID) error {
	tag, err := q.Exec(ctx, `
		UPDATE task_drift_signals
		SET state = $2,
		    resolution = nullif($3, ''),
		    resolution_artifact_id = $4,
		    resolved_by = $5,
		    resolved_at = now()
		WHERE id = $1 AND state IN ('open', 'acknowledged')`,
		signalID, resolution, note, artifactID, actor)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("сигнал дрейфа уже закрыт")
	}

	if resolution != "resolved" {
		return nil
	}
	_, err = q.Exec(ctx, `
		UPDATE task_baselines b
		SET commit_sha = d.to_commit, set_by = $2, set_at = now()
		FROM task_drift_signals d
		WHERE d.id = $1
		  AND b.task_id = d.task_id
		  AND b.repository_id = d.repository_id
		  AND d.to_commit IS NOT NULL`, signalID, actor)
	return translate(err)
}

// RefToSync — привязанный к задаче PR, состояние которого пора обновить.
type RefToSync struct {
	ID      uuid.UUID
	TaskID  uuid.UUID
	TaskKey string
	Owner   string
	Name    string
	RefType string
	Ref     string
	State   *string
}

// RepoRefsToSync — PR задач в полёте, начиная с тех, о которых система знает
// меньше всего.
//
// Закрытые и влитые не опрашиваются: их состояние уже конечное, а лимит
// запросов к провайдеру общий с индексацией.
func (s *Store) RepoRefsToSync(ctx context.Context, q Querier, limit int) ([]RefToSync, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT rr.id, rr.task_id, t.key, r.owner, r.name, rr.ref_type, rr.ref, rr.state
		FROM task_repo_refs rr
		JOIN repositories r       ON r.id = rr.repository_id
		JOIN tasks t              ON t.id = rr.task_id
		JOIN workflow_statuses ws ON ws.id = t.status_id
		WHERE rr.ref_type = 'pull_request'
		  AND t.closed_at IS NULL AND ws.is_terminal = false
		  AND (rr.state IS NULL OR rr.state NOT IN ('closed', 'merged', 'declined'))
		ORDER BY rr.synced_at NULLS FIRST
		LIMIT $1`, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []RefToSync{}
	for rows.Next() {
		var r RefToSync
		if err := rows.Scan(&r.ID, &r.TaskID, &r.TaskKey, &r.Owner, &r.Name,
			&r.RefType, &r.Ref, &r.State); err != nil {
			return nil, translate(err)
		}
		out = append(out, r)
	}
	return out, translate(rows.Err())
}

// UpdateRepoRefChecks кладёт снимок состояния PR и его проверок.
//
// Снимок, а не отдельные сущности: это состояние чужой системы на момент
// опроса, и хранить его нормализованно значило бы делать вид, что оно наше.
// Гейт external_check читает именно эту строку — в сеть он не ходит.
func (s *Store) UpdateRepoRefChecks(ctx context.Context, q Querier, id uuid.UUID, state, headSHA, url string, checks json.RawMessage) error {
	if len(checks) == 0 {
		checks = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `
		UPDATE task_repo_refs
		SET state = nullif($2, ''),
		    head_sha = nullif($3, ''),
		    url = coalesce(nullif($4, ''), url),
		    checks = $5,
		    synced_at = now()
		WHERE id = $1`, id, state, headSHA, url, checks)
	return translate(err)
}
