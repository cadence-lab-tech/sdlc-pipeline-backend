-- 000031_stage_time_limit: двенадцатый тип гейта — предел времени этапа
-- (CLAB-221, решение 0061).
--
-- Предел стоит на переходе, которым задача входит в статус, и действует на
-- этап, открытый этим переходом: {"max_duration": "4h"} в config гейта,
-- длительность проверяет валидатор цикла, схема знает только тип. Поэтому
-- прогон обязан помнить, каким переходом он открыт: до сих пор переход жил
-- лишь в payload task.advanced прошлого прогона, а доставать его из ленты на
-- каждом захвате и в выборке воркера — хрупко. NULL — честное «предела нет»:
-- начальный статус, перепроверки и все прогоны до этой миграции.

ALTER TABLE gates DROP CONSTRAINT gates_type_check;
ALTER TABLE gates ADD CONSTRAINT gates_type_check CHECK (type IN (
    'artifacts_required',
    'schema_validation',
    'checklist',
    'human_approval',
    'agent_review',
    'external_check',
    'repo_ref_required',
    'drift_clear',
    'repo_scope',
    'dependencies_closed',
    'attempts_limit',
    'stage_time_limit'));

-- SET NULL, а не CASCADE: прогон — история задачи, и пропажа перехода
-- (удалённый цикл) не должна уносить её с собой.
ALTER TABLE stage_runs
    ADD COLUMN entered_via_transition_id uuid
        REFERENCES workflow_transitions(id) ON DELETE SET NULL,
    -- Когда воркер заметил просрочку и написал stage.overdue. Отметка, а не
    -- состояние: по ней событие пишется один раз на прогон, а захват
    -- отбивается по часам и её не ждёт.
    ADD COLUMN overdue_at timestamptz;

-- Выборка воркера на каждом тике аренд: открытые этапы с переходом входа,
-- ещё не отмеченные. Остальные прогоны (а их подавляющее большинство —
-- закрытые) в индекс не попадают.
CREATE INDEX stage_runs_time_limited
    ON stage_runs (entered_at)
    WHERE kind = 'stage' AND finished_at IS NULL
      AND entered_via_transition_id IS NOT NULL AND overdue_at IS NULL;
