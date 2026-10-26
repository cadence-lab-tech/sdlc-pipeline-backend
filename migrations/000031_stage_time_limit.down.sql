-- Откат 000031: предела времени этапа больше нет. Сужающий CHECK не проходит
-- валидацию, пока в базе лежат гейты отменяемого типа, поэтому сначала они
-- удаляются; их gate_results уходят каскадом.
DROP INDEX IF EXISTS stage_runs_time_limited;
ALTER TABLE stage_runs
    DROP COLUMN IF EXISTS overdue_at,
    DROP COLUMN IF EXISTS entered_via_transition_id;
DELETE FROM gates WHERE type = 'stage_time_limit';
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
    'attempts_limit'));
