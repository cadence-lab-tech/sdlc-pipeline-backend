DROP INDEX IF EXISTS tasks_multi_repo;

-- Сужение CHECK требует, чтобы данных отменяемого типа не осталось.
DELETE FROM task_links WHERE rel = 'split_from';
ALTER TABLE task_links DROP CONSTRAINT IF EXISTS task_links_rel_check;
ALTER TABLE task_links ADD CONSTRAINT task_links_rel_check CHECK (rel IN (
    'blocks', 'relates_to', 'duplicates', 'shares_surface'));

DELETE FROM gates WHERE type = 'repo_scope';
ALTER TABLE gates DROP CONSTRAINT IF EXISTS gates_type_check;
ALTER TABLE gates ADD CONSTRAINT gates_type_check CHECK (type IN (
    'artifacts_required', 'schema_validation', 'checklist', 'human_approval',
    'agent_review', 'external_check', 'repo_ref_required', 'drift_clear'));

DROP TRIGGER IF EXISTS tasks_repo_scope_check ON tasks;
DROP TABLE IF EXISTS task_repositories;
DROP FUNCTION IF EXISTS enforce_task_repo_scope();

ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repositories_project_id_id_key;
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_project_id_id_key;
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_multi_repo_boolean;
ALTER TABLE tasks DROP COLUMN IF EXISTS properties;

DROP TABLE IF EXISTS task_property_defs;
