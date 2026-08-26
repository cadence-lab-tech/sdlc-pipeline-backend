-- 000003_tasks: задачи, прохождение этапов, результаты гейтов, лента событий.

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

CREATE TABLE tasks (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    epic_id        uuid REFERENCES epics(id) ON DELETE SET NULL,
    -- человекочитаемый ключ вида SDLC-42, выдаётся из projects.task_counter
    key            text NOT NULL,
    seq            integer NOT NULL,
    title          text NOT NULL,
    description    text,
    workflow_id    uuid NOT NULL REFERENCES workflows(id),
    status_id      uuid NOT NULL,
    priority       text NOT NULL DEFAULT 'normal'
                       CHECK (priority IN ('low', 'normal', 'high', 'urgent')),
    -- человек, отвечающий за задачу; агент фиксируется отдельно в stage_runs
    assignee_id    uuid REFERENCES principals(id),
    -- {"jira": {"key": "ABC-1", "url": "https://..."}}
    external_ref   jsonb NOT NULL DEFAULT '{}',
    labels         text[] NOT NULL DEFAULT '{}',
    blocked_reason text,
    created_by     uuid REFERENCES principals(id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    closed_at      timestamptz,
    UNIQUE (project_id, key),
    UNIQUE (project_id, seq),
    -- статус задачи обязан принадлежать её же workflow
    FOREIGN KEY (workflow_id, status_id)
        REFERENCES workflow_statuses (workflow_id, id)
);

-- Одна попытка прохождения задачи через один статус. Повторный заход после
-- отката создаёт новый stage_run с attempt+1, а не переписывает старый —
-- поэтому история "аналитика делалась дважды, второй раз после отката от
-- архитектора" сохраняется целиком.
CREATE TABLE stage_runs (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id               uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    status_id             uuid NOT NULL REFERENCES workflow_statuses(id),
    role_id               uuid REFERENCES roles(id),
    -- версия роли на момент захвата: инструкции этапа воспроизводимы
    role_version_id       uuid REFERENCES role_versions(id),
    attempt               integer NOT NULL DEFAULT 1,
    state                 text NOT NULL DEFAULT 'pending' CHECK (state IN (
                              'pending',      -- задача вошла в статус, агент не забрал
                              'claimed',      -- агент забрал, держит аренду
                              'in_progress',  -- агент работает
                              'submitted',    -- артефакты сданы, ждём прогон гейтов
                              'gating',       -- гейты выполняются / ждут человека
                              'passed',
                              'failed',
                              'abandoned')),
    claimed_by            uuid REFERENCES principals(id),
    claimed_at            timestamptz,
    -- аренда: агент локальный и может умереть, не отпустив задачу
    lease_expires_at      timestamptz,
    -- идентификатор сессии/машины агента для трассировки
    agent_session_ref     text,
    entered_at            timestamptz NOT NULL DEFAULT now(),
    finished_at           timestamptz,
    outcome               text CHECK (outcome IN ('advanced', 'rejected', 'cancelled')),
    -- если этап начался из-за отката, здесь ссылка на этап, который откатил
    rejected_from_run_id  uuid REFERENCES stage_runs(id),
    summary               text,
    UNIQUE (task_id, status_id, attempt)
);

-- Результат одного гейта в рамках одной попытки прохождения.
CREATE TABLE gate_results (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    stage_run_id  uuid NOT NULL REFERENCES stage_runs(id) ON DELETE CASCADE,
    gate_id       uuid NOT NULL REFERENCES gates(id) ON DELETE CASCADE,
    transition_id uuid NOT NULL REFERENCES workflow_transitions(id) ON DELETE CASCADE,
    attempt       integer NOT NULL DEFAULT 1,
    status        text NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'passed', 'failed', 'waived', 'skipped')),
    -- чего именно не хватило, вердикт ревьюера, отмеченные пункты чек-листа
    details       jsonb NOT NULL DEFAULT '{}',
    decided_by    uuid REFERENCES principals(id),
    decided_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (stage_run_id, gate_id, attempt)
);

-- Ветки, PR и коммиты, относящиеся к задаче. Заполняется на этапе разработки.
CREATE TABLE task_repo_refs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id       uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    ref_type      text NOT NULL CHECK (ref_type IN ('branch', 'pull_request', 'commit')),
    ref           text NOT NULL,
    url           text,
    state         text,
    meta          jsonb NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, repository_id, ref_type, ref)
);

-- Append-only лента: единственный источник правды для таймлайна задачи в UI
-- и для разбора "почему задача откатилась".
CREATE TABLE task_events (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id      uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    stage_run_id uuid REFERENCES stage_runs(id) ON DELETE SET NULL,
    -- 'task.created', 'stage.claimed', 'artifact.submitted', 'gate.failed',
    -- 'task.advanced', 'task.rejected', 'lease.expired', ...
    type         text NOT NULL,
    actor_id     uuid REFERENCES principals(id),
    payload      jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON tasks (project_id, status_id);
CREATE INDEX ON tasks (project_id, updated_at DESC);
CREATE INDEX ON tasks (epic_id);
CREATE INDEX ON stage_runs (task_id, entered_at);
CREATE INDEX ON stage_runs (status_id, state);
-- для воркера, возвращающего задачи с протухшей арендой
CREATE INDEX stage_runs_expiring_leases
    ON stage_runs (lease_expires_at)
    WHERE state IN ('claimed', 'in_progress');
CREATE INDEX ON gate_results (stage_run_id);
CREATE INDEX ON task_events (task_id, created_at DESC);
