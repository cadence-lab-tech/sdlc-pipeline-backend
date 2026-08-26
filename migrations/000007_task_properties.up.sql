-- 000007_task_properties: свойства задач и область задачи по репозиториям.
--
-- Задача может законно затрагивать несколько репозиториев: «добавить поле в
-- ручку бэкенда и отобразить его на фронте» — это одна аналитика и одно
-- архитектурное решение. Но не всякая задача такая, и разрешение трогать всё
-- подряд размывает границы этапов.
--
-- Решение: у задачи есть свойства, одно из которых — системное `multi_repo`.
-- Выключено (по умолчанию) — задача живёт в одном репозитории. Включено —
-- область задачи может охватывать несколько.
--
-- Это НЕ параллельность этапов: задача по-прежнему находится ровно в одном
-- статусе и имеет ровно один активный stage_run. Меняется только то, сколько
-- репозиториев видит агент внутри своего этапа.

-- Определения свойств: задаются на проект, драйвят валидацию и форму в UI.
CREATE TABLE task_property_defs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key             text NOT NULL,
    name            text NOT NULL,
    description     text,
    value_type      text NOT NULL CHECK (value_type IN ('boolean', 'enum', 'string', 'number')),
    default_value   jsonb,
    enum_values     text[],
    -- Свойства, поведение которых зашито в движок. Их нельзя удалить и нельзя
    -- сменить им тип; проект может поменять только default_value.
    is_system       boolean NOT NULL DEFAULT false,
    -- отдавать ли значение агенту в контексте захвата задачи
    expose_to_agent boolean NOT NULL DEFAULT true,
    position        integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, key),
    CONSTRAINT task_property_defs_enum_shape CHECK (
        value_type <> 'enum' OR (enum_values IS NOT NULL AND cardinality(enum_values) > 0)
    )
);

-- Значения свойств задачи. Хранятся jsonb, а не таблицей ключ-значение:
-- свойства читаются вместе с задачей на каждой карточке и в каждом захвате.
ALTER TABLE tasks ADD COLUMN properties jsonb NOT NULL DEFAULT '{}';

-- Единственное свойство, тип которого движок обязан знать заранее.
ALTER TABLE tasks ADD CONSTRAINT tasks_multi_repo_boolean CHECK (
    properties->'multi_repo' IS NULL OR jsonb_typeof(properties->'multi_repo') = 'boolean'
);

-- Область задачи по репозиториям.
ALTER TABLE tasks        ADD CONSTRAINT tasks_project_id_id_key        UNIQUE (project_id, id);
ALTER TABLE repositories ADD CONSTRAINT repositories_project_id_id_key UNIQUE (project_id, id);

CREATE TABLE task_repositories (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id            uuid NOT NULL,
    task_id               uuid NOT NULL,
    repository_id         uuid NOT NULL,
    -- где живёт основное изменение; для однорепозиторной задачи — единственный
    is_primary            boolean NOT NULL DEFAULT false,
    -- этап, на котором репозиторий попал в область: аналитика может её расширить
    added_in_stage_run_id uuid REFERENCES stage_runs(id) ON DELETE SET NULL,
    added_by              uuid REFERENCES principals(id),
    added_at              timestamptz NOT NULL DEFAULT now(),
    note                  text,
    UNIQUE (task_id, repository_id),
    -- задача и репозиторий обязаны быть из одного проекта
    FOREIGN KEY (project_id, task_id)       REFERENCES tasks (project_id, id) ON DELETE CASCADE,
    FOREIGN KEY (project_id, repository_id) REFERENCES repositories (project_id, id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX task_repositories_one_primary
    ON task_repositories (task_id) WHERE is_primary;

-- Инвариант: у задачи с выключенным multi_repo в области не больше одного
-- репозитория. Проверка отложенная — она должна пережить транзакцию, которая
-- одновременно включает флаг и добавляет второй репозиторий.
CREATE OR REPLACE FUNCTION enforce_task_repo_scope() RETURNS trigger AS $fn$
DECLARE
    tid   uuid;
    multi boolean;
    cnt   integer;
    tkey  text;
BEGIN
    IF TG_TABLE_NAME = 'tasks' THEN
        tid := NEW.id;
    ELSE
        tid := NEW.task_id;
    END IF;

    SELECT coalesce((t.properties->>'multi_repo')::boolean, false), t.key
      INTO multi, tkey
      FROM tasks t WHERE t.id = tid;

    SELECT count(*) INTO cnt FROM task_repositories WHERE task_id = tid;

    IF NOT multi AND cnt > 1 THEN
        RAISE EXCEPTION
            'задача % однорепозиторная (multi_repo=false), но в её области % репозитория(ев)', tkey, cnt
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END
$fn$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER task_repositories_scope_check
    AFTER INSERT OR UPDATE ON task_repositories
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_task_repo_scope();

CREATE CONSTRAINT TRIGGER tasks_repo_scope_check
    AFTER UPDATE OF properties ON tasks
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_task_repo_scope();

-- Новый гейт: артефакты этапа не вышли за объявленную область задачи.
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

-- Задача, разделённая надвое, сохраняет происхождение.
ALTER TABLE task_links DROP CONSTRAINT task_links_rel_check;
ALTER TABLE task_links ADD CONSTRAINT task_links_rel_check CHECK (rel IN (
    'blocks', 'relates_to', 'duplicates', 'shares_surface', 'split_from'));

CREATE INDEX ON task_repositories (repository_id);
CREATE INDEX ON task_property_defs (project_id, position);
-- быстрый отбор мультирепозиторных задач для доски и метрик
CREATE INDEX tasks_multi_repo ON tasks (project_id)
    WHERE (properties->>'multi_repo')::boolean IS TRUE;
