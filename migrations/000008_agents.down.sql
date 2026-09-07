DROP INDEX IF EXISTS stage_runs_review_parent;
DROP INDEX IF EXISTS stage_runs_open;
DROP INDEX IF EXISTS stage_runs_review_attempt_uniq;
DROP INDEX IF EXISTS stage_runs_stage_attempt_uniq;

-- Возврат уникальности (task_id, status_id, attempt) невозможен, пока в
-- таблице лежат прогоны перепроверки: они делят статус с родительским этапом.
DELETE FROM stage_runs WHERE kind = 'review';

ALTER TABLE stage_runs ADD CONSTRAINT stage_runs_task_id_status_id_attempt_key
    UNIQUE (task_id, status_id, attempt);

ALTER TABLE stage_runs DROP CONSTRAINT IF EXISTS stage_runs_review_shape;
ALTER TABLE stage_runs
    DROP COLUMN IF EXISTS gate_id,
    DROP COLUMN IF EXISTS parent_run_id,
    DROP COLUMN IF EXISTS kind,
    DROP COLUMN IF EXISTS heartbeat_at;
