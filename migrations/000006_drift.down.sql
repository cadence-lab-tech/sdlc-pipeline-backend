-- Гейты отменяемых типов удаляются вместе с их результатами (FK ON DELETE CASCADE),
-- иначе сужение CHECK не проходит валидацию.
DELETE FROM gates WHERE type IN ('repo_ref_required', 'drift_clear');
ALTER TABLE gates DROP CONSTRAINT IF EXISTS gates_type_check;
ALTER TABLE gates ADD CONSTRAINT gates_type_check CHECK (type IN (
    'artifacts_required', 'schema_validation', 'checklist',
    'human_approval', 'agent_review', 'external_check'));

DROP TABLE IF EXISTS task_links;
DROP TABLE IF EXISTS task_drift_signals;
DROP TABLE IF EXISTS task_baselines;
DROP TABLE IF EXISTS artifact_code_refs;
DROP TABLE IF EXISTS code_file_changes;

ALTER TABLE repo_index_runs DROP COLUMN IF EXISTS parent_index_run_id;
ALTER TABLE repo_index_runs DROP COLUMN IF EXISTS branch;
ALTER TABLE repositories DROP COLUMN IF EXISTS watch_branches;
