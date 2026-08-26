-- Референсная реализация детектора дрейфа. Эти два запроса воркер прогоняет
-- после каждого успешного прогона индексации отслеживаемой ветки.
-- Проверены на сценарии из backend/migrations/seed/drift_demo.sql.

-- 1. Дрейф базовой линии: код, на который опирается задача в полёте, изменился
--    в main после того, как была зафиксирована базовая линия задачи.
--
--    Уровень: high  — изменился именно тот символ, на который есть прямая ссылка;
--             medium — изменился файл, который задача собирается менять или от
--                      контракта которого зависит;
--             low    — изменился файл, который задача только читала.
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
    -- этап, в котором задачу застало: попадёт в контекст следующего захвата
    (SELECT sr.id FROM stage_runs sr
      WHERE sr.task_id = h.task_id AND sr.state <> 'passed'
      ORDER BY sr.entered_at DESC LIMIT 1),
    jsonb_build_object(
        'paths',        to_jsonb(array_agg(DISTINCT h.path)),
        'symbols',      to_jsonb(array_remove(array_agg(DISTINCT h.symbol_fqn), NULL)),
        'artifact_ids', to_jsonb(array_agg(DISTINCT h.artifact_id)))
FROM (
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
    JOIN task_baselines tb      ON tb.task_id = acr.task_id
                              AND tb.repository_id = acr.repository_id
                              AND tb.branch = rir.branch
    JOIN tasks t                ON t.id = acr.task_id
    JOIN workflow_statuses ws   ON ws.id = t.status_id
    WHERE rir.id = $1
      -- задача ещё в полёте
      AND ws.is_terminal = false
      AND t.closed_at IS NULL
      -- и её базовая линия отстаёт от только что проиндексированного коммита
      AND tb.commit_sha <> rir.commit_sha
) h
GROUP BY h.task_id, h.repository_id, h.baseline_commit, h.run_commit, h.run_id
ON CONFLICT DO NOTHING;

-- 2. Пересечение задач: две задачи в полёте объявили своей зоной изменений один
--    и тот же файл. Конфликт возникнет не в git, а в замысле, поэтому ловить
--    его нужно до того, как обе уйдут в разработку.
--    Сигнал заводится обеим задачам — каждая видит его у себя.
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
JOIN tasks ta              ON ta.id = a.task_id
JOIN tasks tb              ON tb.id = b.task_id
JOIN workflow_statuses wsa ON wsa.id = ta.status_id
JOIN workflow_statuses wsb ON wsb.id = tb.status_id
WHERE a.ref_kind = 'modifies'
  AND ta.project_id = $1
  AND wsa.is_terminal = false AND ta.closed_at IS NULL
  AND wsb.is_terminal = false AND tb.closed_at IS NULL
GROUP BY a.task_id, a.repository_id, b.task_id
ON CONFLICT DO NOTHING;

-- 3. Проверка гейта drift_clear перед переходом.
--    Возвращает сигналы, которые держат переход.
SELECT s.id, s.kind, s.severity, s.overlap, s.detected_at
FROM task_drift_signals s
WHERE s.task_id = $1
  AND s.state IN ('open', 'acknowledged')
  AND CASE $2                    -- min_severity из конфига гейта
        WHEN 'high'   THEN s.severity = 'high'
        WHEN 'medium' THEN s.severity IN ('high', 'medium')
        ELSE true
      END
ORDER BY array_position(ARRAY['high','medium','low'], s.severity), s.detected_at;
