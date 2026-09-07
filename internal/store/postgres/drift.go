package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// NewDriftSignal — только что заведённый сигнал. Возвращается из детектора,
// чтобы воркер написал о нём в ленту задачи: расхождение, о котором никто не
// узнал, ничем не отличается от незамеченного.
type NewDriftSignal struct {
	ID         uuid.UUID
	TaskID     uuid.UUID
	TaskKey    string
	Kind       string
	Severity   string
	Repository string
	Overlap    json.RawMessage
}

// DetectBaselineDrift — первый запрос детектора (docs/drift_detection.sql).
//
// Пересекает изменения только что проиндексированного коммита с кодом, на
// который ссылаются артефакты задач в полёте, чья базовая линия отстала.
// Уровень: high — изменился тот самый символ, medium — файл, который задача
// собирается менять или от контракта которого зависит, low — файл, который
// она только читала.
func (s *Store) DetectBaselineDrift(ctx context.Context, q Querier, runID uuid.UUID) ([]NewDriftSignal, error) {
	rows, err := q.Query(ctx, `
		WITH hits AS (
		    SELECT acr.task_id, acr.repository_id, acr.artifact_id, acr.path,
		           acr.symbol_fqn, acr.ref_kind,
		           cfc.changed_symbols,
		           tb.commit_sha AS baseline_commit,
		           rir.commit_sha AS run_commit,
		           rir.id AS run_id
		    FROM repo_index_runs rir
		    JOIN code_file_changes cfc  ON cfc.index_run_id = rir.id
		    JOIN artifact_code_refs acr ON acr.repository_id = cfc.repository_id
		                              AND acr.path = cfc.path
		    JOIN artifacts a            ON a.id = acr.artifact_id
		                              AND a.status <> 'superseded'
		    JOIN task_baselines tb      ON tb.task_id = acr.task_id
		                              AND tb.repository_id = acr.repository_id
		                              AND tb.branch = rir.branch
		    JOIN tasks t                ON t.id = acr.task_id
		    JOIN workflow_statuses ws   ON ws.id = t.status_id
		    WHERE rir.id = $1
		      AND ws.is_terminal = false
		      AND t.closed_at IS NULL
		      AND tb.commit_sha <> rir.commit_sha
		), inserted AS (
		    INSERT INTO task_drift_signals (
		        task_id, repository_id, kind, severity,
		        from_commit, to_commit, detected_by_run_id, stage_run_id, overlap)
		    SELECT
		        h.task_id,
		        h.repository_id,
		        'baseline_drift',
		        CASE
		            WHEN bool_or(h.symbol_fqn IS NOT NULL AND h.symbol_fqn = ANY (h.changed_symbols)) THEN 'high'
		            WHEN bool_or(h.ref_kind IN ('modifies', 'depends_on'))                            THEN 'medium'
		            ELSE 'low'
		        END,
		        h.baseline_commit,
		        h.run_commit,
		        h.run_id,
		        (SELECT sr.id FROM stage_runs sr
		          WHERE sr.task_id = h.task_id AND sr.state <> 'passed'
		          ORDER BY sr.entered_at DESC LIMIT 1),
		        jsonb_build_object(
		            'paths',        to_jsonb(array_agg(DISTINCT h.path)),
		            'symbols',      to_jsonb(array_remove(array_agg(DISTINCT h.symbol_fqn), NULL)),
		            'artifact_ids', to_jsonb(array_agg(DISTINCT h.artifact_id)))
		    FROM hits h
		    GROUP BY h.task_id, h.repository_id, h.baseline_commit, h.run_commit, h.run_id
		    ON CONFLICT DO NOTHING
		    RETURNING id, task_id, repository_id, kind, severity, overlap)
		SELECT i.id, i.task_id, t.key, i.kind, i.severity,
		       coalesce(r.owner || '/' || r.name, ''), i.overlap
		FROM inserted i
		JOIN tasks t ON t.id = i.task_id
		LEFT JOIN repositories r ON r.id = i.repository_id`, runID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	return scanNewSignals(rows)
}

// DetectTaskOverlap — второй запрос детектора: две задачи в полёте объявили
// своей зоной изменений один и тот же файл. Конфликт возникнет не в git, а в
// замысле, поэтому ловить его надо до того, как обе уйдут в разработку.
func (s *Store) DetectTaskOverlap(ctx context.Context, q Querier, projectID uuid.UUID) ([]NewDriftSignal, error) {
	rows, err := q.Query(ctx, `
		WITH inserted AS (
		    INSERT INTO task_drift_signals (
		        task_id, repository_id, kind, severity, related_task_id, overlap)
		    SELECT
		        a.task_id,
		        a.repository_id,
		        'task_overlap',
		        CASE WHEN count(DISTINCT a.symbol_fqn) FILTER (WHERE a.symbol_fqn IS NOT NULL) > 0
		             THEN 'high' ELSE 'medium' END,
		        b.task_id,
		        jsonb_build_object('paths', to_jsonb(array_agg(DISTINCT a.path)))
		    FROM artifact_code_refs a
		    JOIN artifact_code_refs b ON b.repository_id = a.repository_id
		                             AND b.path = a.path
		                             AND b.task_id <> a.task_id
		                             AND b.ref_kind = 'modifies'
		    JOIN artifacts aa          ON aa.id = a.artifact_id AND aa.status <> 'superseded'
		    JOIN artifacts ab          ON ab.id = b.artifact_id AND ab.status <> 'superseded'
		    JOIN tasks ta              ON ta.id = a.task_id
		    JOIN tasks tb              ON tb.id = b.task_id
		    JOIN workflow_statuses wsa ON wsa.id = ta.status_id
		    JOIN workflow_statuses wsb ON wsb.id = tb.status_id
		    WHERE a.ref_kind = 'modifies'
		      AND ta.project_id = $1
		      AND wsa.is_terminal = false AND ta.closed_at IS NULL
		      AND wsb.is_terminal = false AND tb.closed_at IS NULL
		    GROUP BY a.task_id, a.repository_id, b.task_id
		    ON CONFLICT DO NOTHING
		    RETURNING id, task_id, related_task_id, repository_id, kind, severity, overlap),
		linked AS (
		    -- Пересечение зон — это факт про пару задач, а не только про одну:
		    -- в карточке обеих должно быть видно, с кем они делят поверхность.
		    INSERT INTO task_links (from_task_id, to_task_id, rel, note)
		    SELECT i.task_id, i.related_task_id, 'shares_surface',
		           'общая зона изменений, найдена детектором дрейфа'
		    FROM inserted i
		    ON CONFLICT (from_task_id, to_task_id, rel) DO NOTHING
		    RETURNING 1)
		SELECT i.id, i.task_id, t.key, i.kind, i.severity,
		       coalesce(r.owner || '/' || r.name, ''), i.overlap
		FROM inserted i
		JOIN tasks t ON t.id = i.task_id
		LEFT JOIN repositories r ON r.id = i.repository_id`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	return scanNewSignals(rows)
}

func scanNewSignals(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]NewDriftSignal, error) {
	out := []NewDriftSignal{}
	for rows.Next() {
		var s NewDriftSignal
		if err := rows.Scan(&s.ID, &s.TaskID, &s.TaskKey, &s.Kind, &s.Severity,
			&s.Repository, &s.Overlap); err != nil {
			return nil, translate(err)
		}
		out = append(out, s)
	}
	return out, translate(rows.Err())
}

// EnsureBaselines ставит задаче базовые линии по актуальному индексу её
// репозиториев.
//
// Момент — первый захват задачи агентом: с этого коммита начинается работа, и
// именно от него считается, что код под задачей уехал. Уже стоящая линия не
// трогается: её двигает только разбор дрейфа.
//
// Нулевой актор означает «поставлено системой»: линию ставит и фоновый проход,
// а аудит ссылается на principals, и выдумывать принципала ради этого незачем.
func (s *Store) EnsureBaselines(ctx context.Context, q Querier, taskID uuid.UUID, stageRunID *uuid.UUID, actor uuid.UUID) (int, error) {
	tag, err := q.Exec(ctx, `
		INSERT INTO task_baselines
		    (task_id, repository_id, branch, commit_sha, index_run_id, stage_run_id, set_by)
		SELECT $1, tr.repository_id, ir.branch, ir.commit_sha, ir.id, $2,
		       nullif($3, '00000000-0000-0000-0000-000000000000'::uuid)
		FROM task_repositories tr
		JOIN repositories r ON r.id = tr.repository_id
		JOIN LATERAL (
		    SELECT id, branch, commit_sha
		    FROM repo_index_runs
		    WHERE repository_id = tr.repository_id
		      AND state = 'completed'
		      AND branch = r.default_branch
		    ORDER BY finished_at DESC
		    LIMIT 1) ir ON true
		WHERE tr.task_id = $1
		ON CONFLICT (task_id, repository_id, branch) DO NOTHING`,
		taskID, stageRunID, actor)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

// ProjectsWithOpenTasks — проекты, в которых есть смысл искать пересечения
// задач. Гонять запрос по проектам без задач в полёте незачем.
func (s *Store) ProjectsWithOpenTasks(ctx context.Context, q Querier) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT t.project_id
		FROM tasks t
		JOIN workflow_statuses ws ON ws.id = t.status_id
		WHERE t.closed_at IS NULL AND ws.is_terminal = false`)
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
