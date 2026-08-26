-- 000002_workflow: кастомный SDLC-цикл проекта — статусы, переходы, роли, гейты.

-- Workflow версионируется целиком. Активный ровно один на проект; старые
-- версии остаются, потому что на них ссылаются уже прошедшие задачи.
CREATE TABLE workflows (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       text NOT NULL,
    version    integer NOT NULL DEFAULT 1,
    state      text NOT NULL DEFAULT 'draft' CHECK (state IN ('draft', 'active', 'retired')),
    created_by uuid REFERENCES principals(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name, version)
);

CREATE UNIQUE INDEX workflows_one_active_per_project
    ON workflows (project_id) WHERE state = 'active';

CREATE TABLE workflow_statuses (
    id          uuid NOT NULL DEFAULT gen_random_uuid(),
    workflow_id uuid NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    key         text NOT NULL,
    name        text NOT NULL,
    description text,
    category    text NOT NULL CHECK (category IN ('backlog', 'active', 'waiting', 'done', 'cancelled')),
    position    integer NOT NULL,
    is_initial  boolean NOT NULL DEFAULT false,
    is_terminal boolean NOT NULL DEFAULT false,
    wip_limit   integer,
    color       text,
    PRIMARY KEY (id),
    UNIQUE (workflow_id, key),
    -- позволяет ссылаться композитным FK и гарантировать, что статус задачи
    -- принадлежит именно её workflow
    UNIQUE (workflow_id, id),
    -- deferrable, чтобы переупорядочивать статусы одной транзакцией
    CONSTRAINT workflow_statuses_position_uniq UNIQUE (workflow_id, position)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE UNIQUE INDEX workflow_statuses_one_initial
    ON workflow_statuses (workflow_id) WHERE is_initial;

-- Роль — описание поведения агента-специалиста. Сама роль стабильна,
-- меняется её версия.
CREATE TABLE roles (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key         text NOT NULL,
    name        text NOT NULL,
    summary     text,
    created_by  uuid REFERENCES principals(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz,
    UNIQUE (project_id, key)
);

-- Иммутабельная версия роли. stage_runs всегда ссылается на конкретную версию,
-- поэтому редактирование роли не переписывает историю уже пройденных этапов.
CREATE TABLE role_versions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    role_id         uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    version         integer NOT NULL,
    -- markdown: как ведёт себя специалист, что проверяет, чего не делает
    instructions    text NOT NULL,
    -- какие артефакты предыдущих этапов подать на вход: ['analysis.brief', ...]
    input_kinds     text[] NOT NULL DEFAULT '{}',
    -- что роль обязана произвести:
    -- [{"kind":"analysis.brief","title":"...","format":"markdown","required":true,"schema":{...}}]
    output_contract jsonb NOT NULL DEFAULT '[]',
    -- подсказка агенту по инструментам: {"allow":["Read","Grep"],"deny":["Bash(git push:*)"]}
    tool_policy     jsonb NOT NULL DEFAULT '{}',
    model_hint      text,
    -- пусто = все репозитории проекта; иначе список repositories.role_hint или owner/name
    repo_scope      text[] NOT NULL DEFAULT '{}',
    -- {"max_prior_artifacts":20,"rag_top_k":12,"include_project_docs":true}
    context_budget  jsonb NOT NULL DEFAULT '{}',
    changelog       text,
    created_by      uuid REFERENCES principals(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (role_id, version)
);

-- Какая роль работает на статусе. Основная — исполнитель, дополнительные
-- используются гейтом agent_review как перепроверяющие.
CREATE TABLE status_roles (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    status_id  uuid NOT NULL REFERENCES workflow_statuses(id) ON DELETE CASCADE,
    role_id    uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    position   integer NOT NULL DEFAULT 0,
    is_primary boolean NOT NULL DEFAULT true,
    UNIQUE (status_id, role_id)
);

CREATE UNIQUE INDEX status_roles_one_primary
    ON status_roles (status_id) WHERE is_primary;

-- Переход между статусами. Откат назад при найденной проблеме — это обычный
-- переход с kind='reject', на который навешан гейт, требующий артефакт-проблему.
CREATE TABLE workflow_transitions (
    id             uuid NOT NULL DEFAULT gen_random_uuid(),
    workflow_id    uuid NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    key            text NOT NULL,
    name           text NOT NULL,
    -- NULL = переход доступен из любого статуса (например, 'cancel')
    from_status_id uuid,
    to_status_id   uuid NOT NULL,
    kind           text NOT NULL CHECK (kind IN ('advance', 'reject', 'reassign', 'cancel')),
    -- выполнять автоматически, как только все блокирующие гейты пройдены
    auto           boolean NOT NULL DEFAULT false,
    position       integer NOT NULL DEFAULT 0,
    PRIMARY KEY (id),
    UNIQUE (workflow_id, key),
    UNIQUE (workflow_id, id),
    FOREIGN KEY (workflow_id, from_status_id)
        REFERENCES workflow_statuses (workflow_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workflow_id, to_status_id)
        REFERENCES workflow_statuses (workflow_id, id) ON DELETE CASCADE
);

-- Гейт — условие, которое обязано выполниться, чтобы переход состоялся.
-- Гейты висят на переходе, а не на статусе: у "вперёд" и "назад" из одного
-- статуса требования принципиально разные.
CREATE TABLE gates (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    transition_id uuid NOT NULL REFERENCES workflow_transitions(id) ON DELETE CASCADE,
    key           text NOT NULL,
    name          text NOT NULL,
    type          text NOT NULL CHECK (type IN (
                      'artifacts_required',  -- присутствуют артефакты нужных kind
                      'schema_validation',   -- артефакт валиден по JSON Schema
                      'checklist',           -- агент/человек отметил пункты
                      'human_approval',      -- живой апрув в UI
                      'agent_review',        -- перепроверка другой ролью
                      'external_check')),    -- CI, вебхук, внешняя система
    config        jsonb NOT NULL DEFAULT '{}',
    blocking      boolean NOT NULL DEFAULT true,
    position      integer NOT NULL DEFAULT 0,
    UNIQUE (transition_id, key)
);

CREATE INDEX ON workflows (project_id);
CREATE INDEX ON workflow_statuses (workflow_id, position);
CREATE INDEX ON role_versions (role_id, version DESC);
CREATE INDEX ON workflow_transitions (workflow_id, from_status_id);
