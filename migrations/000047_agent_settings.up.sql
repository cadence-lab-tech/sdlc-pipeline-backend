-- 000047_agent_settings: настройка агентов проекта версиями и её снимок на
-- прогоне (CLAB-425, решение 0077 п. 4).
--
-- Таблица «агент → модель и усилие на уровне, прайс» — правило команды, как
-- цикл и роли, поэтому живёт версиями по образцу цикла (решение 0016):
-- правится черновик, активная и снятые версии не меняются. Прогон помнит,
-- по какой версии его захватили, — как пару уровня (000028): активация
-- новой версии идущий этап не трогает, а стоимость (CLAB-426) считается по
-- прайсу той версии, что была при захвате.
--
-- Миграция только добавляет: новая таблица и колонка без значения по
-- умолчанию (ADD COLUMN без DEFAULT таблицу не переписывает). Апгрейд с
-- данными идёт без ручных шагов, у старых прогонов колонка NULL — «настройки
-- не было».

CREATE TABLE agent_settings_versions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    version      integer NOT NULL
        CONSTRAINT agent_settings_versions_version_check CHECK (version >= 1),
    state        text NOT NULL
        CONSTRAINT agent_settings_versions_state_check
        CHECK (state IN ('draft', 'active', 'retired')),
    -- Форму документа проверяет движок одной функцией на все входы: схема
    -- держит только «объект», иначе каждое новое поле документа было бы
    -- миграцией.
    document     jsonb NOT NULL DEFAULT '{}'
        CONSTRAINT agent_settings_versions_document_object
        CHECK (jsonb_typeof(document) = 'object'),
    changelog    text,
    created_by   uuid REFERENCES principals(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    activated_by uuid REFERENCES principals(id),
    activated_at timestamptz,
    CONSTRAINT agent_settings_versions_project_version_uniq UNIQUE (project_id, version)
);

-- Не больше одного черновика и одной активной версии на проект держит схема,
-- а не сервис: два одновременных заведения черновика или активации иначе
-- оставили бы проект с двумя. Снятых (retired) — сколько угодно.
CREATE UNIQUE INDEX agent_settings_one_draft_per_project
    ON agent_settings_versions (project_id) WHERE state = 'draft';
CREATE UNIQUE INDEX agent_settings_one_active_per_project
    ON agent_settings_versions (project_id) WHERE state = 'active';

-- Снимок версии при захвате. Без ON DELETE: версии удаляются только вместе с
-- проектом, а его задачи и прогоны DeleteProject удаляет раньше; черновик,
-- который можно удалить, прогоны не снимают — снимок берёт только активную.
ALTER TABLE stage_runs ADD COLUMN agent_settings_version_id uuid
    CONSTRAINT stage_runs_agent_settings_version_fkey
    REFERENCES agent_settings_versions(id);

-- Проверка внешнего ключа при удалении версий проекта и выборка прогонов по
-- версии прайса (CLAB-426) идут по индексу; у старых прогонов NULL, в индекс
-- они не попадают.
CREATE INDEX stage_runs_agent_settings_version_idx
    ON stage_runs (agent_settings_version_id) WHERE agent_settings_version_id IS NOT NULL;
