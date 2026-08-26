-- 000006_drift: отслеживание расхождения задач в полёте с движущейся кодовой базой.
--
-- Задачи исполняются параллельно, а main продолжает двигаться. Задача, чья
-- аналитика сделана на коммите X, может к моменту разработки опираться на код,
-- которого уже нет. Отдельный случай — две задачи в полёте, объявившие своей
-- зоной изменений одни и те же файлы: конфликт возникнет не в git, а в замысле.
--
-- Механизм: у задачи есть базовая линия (коммит, на котором построены её
-- утверждённые артефакты); индексатор считает, что изменилось между прогонами;
-- воркер пересекает изменения с кодом, на который задача ссылается, и заводит
-- сигнал. Блокирует движение обычный гейт drift_clear — отдельной машинерии нет.

-- Индексируем не произвольные коммиты, а ветки, за которыми следим.
ALTER TABLE repositories
    ADD COLUMN watch_branches text[] NOT NULL DEFAULT '{main}';

ALTER TABLE repo_index_runs
    ADD COLUMN branch text NOT NULL DEFAULT 'main',
    ADD COLUMN parent_index_run_id uuid REFERENCES repo_index_runs(id) ON DELETE SET NULL;

-- Что изменилось между родительским прогоном индексации и этим.
CREATE TABLE code_file_changes (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    index_run_id    uuid NOT NULL REFERENCES repo_index_runs(id) ON DELETE CASCADE,
    repository_id   uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    path            text NOT NULL,
    change_type     text NOT NULL CHECK (change_type IN ('added', 'modified', 'deleted', 'renamed')),
    previous_path   text,
    -- fqn символов, затронутых изменением: даёт куда более точное пересечение,
    -- чем совпадение по имени файла
    changed_symbols text[] NOT NULL DEFAULT '{}',
    additions       integer NOT NULL DEFAULT 0,
    deletions       integer NOT NULL DEFAULT 0,
    commit_sha      text NOT NULL,
    UNIQUE (index_run_id, path)
);

-- Нормализованные ссылки артефактов на код. Дублирует artifact_links.target_uri
-- намеренно: пересечение с изменениями main должно быть индексируемым джойном,
-- а не разбором строк URI на каждом прогоне.
CREATE TABLE artifact_code_refs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    artifact_id   uuid NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    task_id       uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    path          text NOT NULL,
    symbol_fqn    text,
    start_line    integer,
    end_line      integer,
    -- на каком коммите агент смотрел этот код
    commit_sha    text,
    -- 'reads'      — задача опирается на этот код
    -- 'modifies'   — задача собирается его менять (источник пересечений задач)
    -- 'depends_on' — контракт, поломка которого ломает задачу
    ref_kind      text NOT NULL CHECK (ref_kind IN ('reads', 'modifies', 'depends_on')),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Базовая линия задачи: коммит, на котором построены её текущие артефакты.
-- Ставится при первом захвате и переставляется, когда дрейф разобран.
CREATE TABLE task_baselines (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id       uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    branch        text NOT NULL DEFAULT 'main',
    commit_sha    text NOT NULL,
    index_run_id  uuid REFERENCES repo_index_runs(id) ON DELETE SET NULL,
    stage_run_id  uuid REFERENCES stage_runs(id) ON DELETE SET NULL,
    set_by        uuid REFERENCES principals(id),
    set_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, repository_id, branch)
);

-- Сигнал: код под задачей уехал, либо задача пересеклась с другой задачей.
CREATE TABLE task_drift_signals (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id                uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository_id          uuid REFERENCES repositories(id) ON DELETE SET NULL,
    kind                   text NOT NULL CHECK (kind IN ('baseline_drift', 'task_overlap')),
    severity               text NOT NULL CHECK (severity IN ('low', 'medium', 'high')),
    state                  text NOT NULL DEFAULT 'open'
                               CHECK (state IN ('open', 'acknowledged', 'resolved', 'dismissed')),
    from_commit            text,
    to_commit              text,
    -- для kind='task_overlap': вторая задача, объявившая ту же зону изменений
    related_task_id        uuid REFERENCES tasks(id) ON DELETE CASCADE,
    detected_by_run_id     uuid REFERENCES repo_index_runs(id) ON DELETE SET NULL,
    -- в каком этапе задачу застало — попадает в контекст следующего захвата
    stage_run_id           uuid REFERENCES stage_runs(id) ON DELETE SET NULL,
    -- {"paths":[…],"symbols":[…],"artifact_ids":[…]}
    overlap                jsonb NOT NULL DEFAULT '{}',
    detected_at            timestamptz NOT NULL DEFAULT now(),
    resolution             text,
    -- разбор дрейфа сам является артефактом задачи
    resolution_artifact_id uuid REFERENCES artifacts(id) ON DELETE SET NULL,
    resolved_by            uuid REFERENCES principals(id),
    resolved_at            timestamptz,
    CONSTRAINT task_drift_signals_overlap_shape CHECK (
        (kind = 'task_overlap' AND related_task_id IS NOT NULL) OR
        (kind = 'baseline_drift' AND to_commit IS NOT NULL)
    ),
    CONSTRAINT task_drift_signals_not_self CHECK (related_task_id IS DISTINCT FROM task_id)
);

-- Воркер прогоняется многократно; повторно тот же незакрытый сигнал не заводится.
CREATE UNIQUE INDEX task_drift_signals_open_uniq
    ON task_drift_signals (
        task_id, kind,
        coalesce(to_commit, ''),
        coalesce(related_task_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE state IN ('open', 'acknowledged');

-- Связи между задачами. Разбиение бизнес-фичи на бэкенд и фронтенд даёт две
-- задачи одного эпика, между которыми почти всегда есть порядок.
-- 'shares_surface' проставляется автоматически при обнаружении task_overlap.
CREATE TABLE task_links (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    from_task_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    to_task_id   uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    rel          text NOT NULL CHECK (rel IN ('blocks', 'relates_to', 'duplicates', 'shares_surface')),
    note         text,
    created_by   uuid REFERENCES principals(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT task_links_not_self CHECK (from_task_id <> to_task_id),
    UNIQUE (from_task_id, to_task_id, rel)
);

-- Два новых типа гейта:
--   repo_ref_required — к задаче привязан PR (заменяет требование сдавать сам diff)
--   drift_clear       — нет открытых сигналов дрейфа выше порога
ALTER TABLE gates DROP CONSTRAINT gates_type_check;
ALTER TABLE gates ADD CONSTRAINT gates_type_check CHECK (type IN (
    'artifacts_required',
    'schema_validation',
    'checklist',
    'human_approval',
    'agent_review',
    'external_check',
    'repo_ref_required',
    'drift_clear'));

CREATE INDEX ON code_file_changes (repository_id, path);
CREATE INDEX ON code_file_changes (index_run_id);
-- главный джойн воркера дрейфа: изменённые пути против кода под задачами
CREATE INDEX ON artifact_code_refs (repository_id, path);
CREATE INDEX ON artifact_code_refs (task_id);
CREATE INDEX artifact_code_refs_modifies
    ON artifact_code_refs (repository_id, path) WHERE ref_kind = 'modifies';
CREATE INDEX ON task_baselines (repository_id, commit_sha);
CREATE INDEX ON task_drift_signals (task_id) WHERE state IN ('open', 'acknowledged');
CREATE INDEX ON task_drift_signals (related_task_id);
CREATE INDEX ON task_links (to_task_id);
