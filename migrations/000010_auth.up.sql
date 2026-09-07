-- 000010_auth: аутентификация по токенам, права на проект, вебхуки GitHub.
--
-- Акторы заложены с первой миграции (решение 0010): аудит уже ссылается на
-- principals, поэтому включение аутентификации не трогает ни одной строки
-- истории. Здесь появляется только то, чего не хватало резолверу: чем
-- предъявляется токен, кто в каком проекте с какими правами, и что было
-- доставлено вебхуком.

-- Глобальный администратор: заводит проекты и первых участников. Первый
-- администратор появляется из окружения (SDLC_ADMIN_TOKEN) — иначе в базе
-- без участников некому было бы выдать первый токен.
ALTER TABLE principals ADD COLUMN is_admin boolean NOT NULL DEFAULT false;

-- agent_tokens задумывалась для агентов, но токен нужен и человеку: UI и CLI
-- предъявляют его одинаково. Таблица не переименовывается — применённые
-- миграции не редактируются, а смысл столбцов не изменился.
ALTER TABLE agent_tokens
    -- первые символы токена; по ним человек узнаёт свой токен в списке,
    -- не видя его целиком — целиком он показывается один раз при выдаче
    ADD COLUMN token_prefix text NOT NULL DEFAULT '',
    ADD COLUMN created_by   uuid REFERENCES principals(id);

CREATE INDEX agent_tokens_principal ON agent_tokens (principal_id) WHERE revoked_at IS NULL;

-- Участие в проекте. Роль здесь — уровень прав, а не роль специалиста из
-- roles: «агент» может забирать задачи, «сопровождающий» — править цикл и
-- ставить апрувы, «администратор» — управлять участниками и токенами.
-- Роли специалистов остаются данными проекта и на права не влияют.
CREATE TABLE project_members (
    project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    role         text NOT NULL CHECK (role IN ('viewer', 'agent', 'maintainer', 'admin')),
    added_by     uuid REFERENCES principals(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, principal_id)
);

CREATE INDEX project_members_principal ON project_members (principal_id);

-- GitHub повторяет доставку при таймауте, а обработчик обязан быть
-- идемпотентным: один и тот же push не должен дважды ставить прогон в
-- очередь. Ключ — идентификатор доставки от провайдера.
CREATE TABLE webhook_deliveries (
    provider    text NOT NULL,
    delivery_id text NOT NULL,
    event       text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, delivery_id)
);
