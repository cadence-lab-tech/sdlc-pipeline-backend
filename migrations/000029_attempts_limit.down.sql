-- Откат 000029: предела попыток больше нет. Сужающий CHECK не проходит
-- валидацию, пока в базе лежат гейты отменяемого типа, поэтому сначала они
-- удаляются; их gate_results уходят каскадом.
DELETE FROM gates WHERE type = 'attempts_limit';
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
    'dependencies_closed'));
