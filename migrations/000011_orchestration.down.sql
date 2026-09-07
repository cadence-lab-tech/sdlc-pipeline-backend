-- Откат 000011: эпики снова становятся отдельной сущностью, цикл — один на
-- проект, стоимость прогонов и гейт зависимостей исчезают.

DROP TABLE IF EXISTS stage_run_usage;

-- Сужающий CHECK не проходит валидацию на засиженной базе, пока в ней лежат
-- строки отменяемого типа.
DELETE FROM gates WHERE type = 'dependencies_closed';
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
    'repo_scope'));

CREATE TABLE epics (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key         text NOT NULL,
    title       text NOT NULL,
    description text,
    state       text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed')),
    target_date date,
    created_by  uuid REFERENCES principals(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, key)
);

ALTER TABLE tasks ADD COLUMN epic_id uuid REFERENCES epics(id) ON DELETE SET NULL;

-- Задача типа epic разбирается обратно на строку epics; её собственные
-- прогоны, артефакты и лента при этом теряются — цикла у эпика в прежней
-- схеме нет, хранить их негде.
DO $mig$
DECLARE
    t   record;
    eid uuid;
BEGIN
    FOR t IN SELECT * FROM tasks WHERE properties->>'type' = 'epic' ORDER BY created_at LOOP
        INSERT INTO epics (project_id, key, title, description, created_by, created_at)
        VALUES (t.project_id, coalesce(t.external_ref->>'legacy_epic_key', t.key),
                t.title, t.description, t.created_by, t.created_at)
        RETURNING id INTO eid;

        UPDATE tasks SET epic_id = eid WHERE parent_id = t.id;
        DELETE FROM tasks WHERE id = t.id;
    END LOOP;
END
$mig$;

CREATE INDEX ON tasks (epic_id);

DROP INDEX IF EXISTS tasks_type;
ALTER TABLE tasks DROP COLUMN parent_id;
ALTER TABLE tasks DROP CONSTRAINT tasks_type_string;
DELETE FROM task_property_defs WHERE key = 'type' AND is_system;

DROP TRIGGER workflows_one_active_per_type ON workflows;
DROP FUNCTION enforce_one_active_workflow_per_type();

-- Уникальный индекс не встанет, пока в проекте активны несколько циклов:
-- в прежней схеме действующий цикл ровно один, и остальные уходят в retired.
UPDATE workflows w SET state = 'retired', updated_at = now()
WHERE w.state = 'active'
  AND w.id <> (SELECT w2.id FROM workflows w2
               WHERE w2.project_id = w.project_id AND w2.state = 'active'
               ORDER BY 'task' = ANY (w2.task_types) DESC, w2.updated_at DESC
               LIMIT 1);

ALTER TABLE workflows DROP CONSTRAINT workflows_task_types_not_empty;
ALTER TABLE workflows DROP COLUMN task_types;

CREATE UNIQUE INDEX workflows_one_active_per_project
    ON workflows (project_id) WHERE state = 'active';
