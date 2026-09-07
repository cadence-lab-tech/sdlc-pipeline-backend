-- 000011_orchestration: эпик как задача своего типа, иерархия задач, цикл по
-- типу задачи, гейт зависимостей и стоимость прогонов
-- (решения 0024, 0025, 0026).
--
-- Отдельная сущность `epics` упраздняется. Эпик, у которого нет ни статуса,
-- ни гейтов, ни артефактов, — метка на задаче; оркестратору нужен эпик,
-- который проходит цикл: планирование, декомпозиция, ожидание детей.
-- Дать ему всё это как второй сущности означало бы второй движок рядом с
-- первым, поэтому эпик становится задачей типа `epic`.

-- ── иерархия ────────────────────────────────────────────────────────────────

-- Ссылка на родителя, а не префикс в ключе: ключи задач остаются плоскими
-- (SDLC-42), и переподчинение задачи не переименовывает её.
ALTER TABLE tasks ADD COLUMN parent_id uuid REFERENCES tasks(id) ON DELETE SET NULL;

-- Схема допускает любую глубину; практический предел — эпик и его задачи,
-- и это правило проекта, а не движка.
CREATE INDEX ON tasks (parent_id);

-- ── тип задачи ──────────────────────────────────────────────────────────────

-- Тип — системное свойство: набор типов расширяет проект (bug, spike), а
-- движок знает про тип ровно одно — по нему выбирается цикл.
ALTER TABLE tasks ADD CONSTRAINT tasks_type_string CHECK (
    properties->'type' IS NULL OR jsonb_typeof(properties->'type') = 'string'
);

INSERT INTO task_property_defs
    (project_id, key, name, description, value_type, default_value, enum_values,
     is_system, expose_to_agent, position)
SELECT id, 'type', 'Тип задачи',
       'Определяет, по какому циклу идёт задача. Эпик оркестратор нарезает на '
       'дочерние задачи, обычная задача проходит цикл разработки.',
       'enum', '"task"'::jsonb, ARRAY['task', 'epic'], true, true, 0
FROM projects
ON CONFLICT (project_id, key) DO NOTHING;

CREATE INDEX tasks_type ON tasks (project_id, (properties->>'type'));

-- ── цикл по типу задачи ─────────────────────────────────────────────────────

-- Эпик и задача разные по смыслу переходов: один цикл с ветвлением по типу
-- означал бы условие внутри данных цикла. Активных циклов в проекте столько,
-- сколько типов, но на один тип — ровно один.
ALTER TABLE workflows ADD COLUMN task_types text[] NOT NULL DEFAULT '{task}';

ALTER TABLE workflows ADD CONSTRAINT workflows_task_types_not_empty
    CHECK (cardinality(task_types) > 0);

DROP INDEX workflows_one_active_per_project;

-- Пересечение массивов частичным уникальным индексом не выражается, а gist по
-- text[] в ядре нет — поэтому инвариант держит отложенный constraint-триггер.
-- Отложенность принципиальна: активация уводит прежнюю версию в retired и
-- поднимает новую одной транзакцией, и в середине неё активны обе.
CREATE OR REPLACE FUNCTION enforce_one_active_workflow_per_type() RETURNS trigger AS $fn$
DECLARE
    clash text;
BEGIN
    IF NEW.state <> 'active' THEN
        RETURN NULL;
    END IF;

    SELECT string_agg(DISTINCT tt, ', ' ORDER BY tt) INTO clash
    FROM workflows w, unnest(w.task_types) AS tt
    WHERE w.project_id = NEW.project_id
      AND w.state = 'active'
      AND w.id <> NEW.id
      AND tt = ANY (NEW.task_types);

    IF clash IS NOT NULL THEN
        RAISE EXCEPTION
            'в проекте уже есть активный цикл для типов задач: %', clash
            USING ERRCODE = 'unique_violation';
    END IF;

    RETURN NULL;
END
$fn$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER workflows_one_active_per_type
    AFTER INSERT OR UPDATE OF state, task_types, project_id ON workflows
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_one_active_workflow_per_type();

-- ── перенос эпиков в задачи ─────────────────────────────────────────────────

-- Эпик встаёт на начальный статус активного цикла проекта: цикла эпиков в
-- проекте ещё нет, и заводить его за проект миграция не вправе. Прежний ключ
-- эпика остаётся во внешних ссылках — на него ссылаются люди и тикеты.
DO $mig$
DECLARE
    e     record;
    wfid  uuid;
    stid  uuid;
    rid   uuid;
    rvid  uuid;
    tid   uuid;
    seqn  integer;
    pkey  text;
BEGIN
    FOR e IN SELECT * FROM epics ORDER BY created_at LOOP
        wfid := NULL;
        stid := NULL;
        rid  := NULL;
        rvid := NULL;

        SELECT id INTO wfid FROM workflows
        WHERE project_id = e.project_id AND state = 'active' LIMIT 1;
        IF wfid IS NULL THEN
            RAISE EXCEPTION 'эпик % не перенести: у проекта нет активного цикла', e.key;
        END IF;

        SELECT id INTO stid FROM workflow_statuses
        WHERE workflow_id = wfid AND is_initial;

        UPDATE projects SET task_counter = task_counter + 1, updated_at = now()
        WHERE id = e.project_id
        RETURNING key, task_counter INTO pkey, seqn;

        INSERT INTO tasks (project_id, key, seq, title, description, workflow_id,
                           status_id, properties, external_ref, created_by, created_at)
        VALUES (e.project_id, pkey || '-' || seqn, seqn, e.title, e.description,
                wfid, stid, jsonb_build_object('type', 'epic'),
                jsonb_build_object('legacy_epic_key', e.key), e.created_by, e.created_at)
        RETURNING id INTO tid;

        SELECT r.id, rv.id INTO rid, rvid
        FROM status_roles sr
        JOIN roles r ON r.id = sr.role_id
        JOIN LATERAL (
            SELECT id FROM role_versions WHERE role_id = r.id ORDER BY version DESC LIMIT 1
        ) rv ON true
        WHERE sr.status_id = stid AND sr.is_primary;

        INSERT INTO stage_runs (task_id, status_id, role_id, role_version_id, attempt, state)
        VALUES (tid, stid, rid, rvid, 1, 'pending');

        INSERT INTO task_events (task_id, type, actor_id, payload)
        VALUES (tid, 'task.created', e.created_by,
                jsonb_build_object('key', pkey || '-' || seqn, 'title', e.title,
                                   'migrated_from_epic', e.key));

        UPDATE tasks SET parent_id = tid, updated_at = now() WHERE epic_id = e.id;
    END LOOP;
END
$mig$;

ALTER TABLE tasks DROP COLUMN epic_id;
DROP TABLE epics;

-- ── гейт зависимостей ───────────────────────────────────────────────────────

-- Порядок между задачами задан связями (task_links.blocks) и иерархией, но
-- движок их до сих пор не читал: заблокированную задачу можно было забрать.
-- Десятый тип гейта закрывает это — и остаётся данными цикла: проекту, которому
-- порядок не нужен, гейт ставить незачем.
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

-- ── стоимость прогона ───────────────────────────────────────────────────────

-- Строк на прогон может быть несколько: агент отчитывается по ходу работы и
-- может сменить модель внутри этапа. Поэтому таблица, а не колонка в
-- stage_runs; стоимость прогона — сумма.
--
-- Числа nullable намеренно: прогон, о котором никто не отчитался, стоит
-- «неизвестно», а не ноль — иначе метрика по стоимости молча занизится на всех,
-- кого запускали руками.
CREATE TABLE stage_run_usage (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id             uuid NOT NULL REFERENCES stage_runs(id) ON DELETE CASCADE,
    -- модель хранится строкой как пришла: перечень моделей меняется чаще схемы
    model              text NOT NULL,
    input_tokens       bigint,
    output_tokens      bigint,
    cache_read_tokens  bigint,
    cache_write_tokens bigint,
    cost_usd           numeric(12, 6),
    duration_ms        bigint,
    -- кто отчитался: сам агент или раннер, который его запускал
    reported_by        uuid REFERENCES principals(id),
    reported_at        timestamptz NOT NULL DEFAULT now(),
    details            jsonb NOT NULL DEFAULT '{}',
    CONSTRAINT stage_run_usage_non_negative CHECK (
        coalesce(input_tokens, 0) >= 0 AND coalesce(output_tokens, 0) >= 0 AND
        coalesce(cache_read_tokens, 0) >= 0 AND coalesce(cache_write_tokens, 0) >= 0 AND
        coalesce(cost_usd, 0) >= 0 AND coalesce(duration_ms, 0) >= 0
    )
);

CREATE INDEX ON stage_run_usage (run_id);
CREATE INDEX ON stage_run_usage (model, reported_at DESC);
