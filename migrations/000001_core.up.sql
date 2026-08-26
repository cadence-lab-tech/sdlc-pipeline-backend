-- 000001_core: акторы, проекты, репозитории, контент-адресуемое хранилище.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Единая сущность "кто совершил действие": человек, агент или сам движок.
-- Все аудит-поля в системе ссылаются сюда, поэтому включение реальной
-- аутентификации в фазе 2 не требует миграции данных — меняется только
-- резолвер принципала в middleware.
CREATE TABLE principals (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL CHECK (kind IN ('user', 'agent', 'system')),
    handle       text NOT NULL UNIQUE,
    display_name text NOT NULL,
    email        text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    disabled_at  timestamptz
);

INSERT INTO principals (kind, handle, display_name)
VALUES ('system', 'system', 'SDLC Engine');

CREATE TABLE projects (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key          text NOT NULL UNIQUE CHECK (key ~ '^[A-Z][A-Z0-9_]{1,15}$'),
    name         text NOT NULL,
    description  text,
    -- счётчик для выдачи человекочитаемых ключей задач (SDLC-42)
    task_counter integer NOT NULL DEFAULT 0,
    settings     jsonb NOT NULL DEFAULT '{}',
    created_by   uuid REFERENCES principals(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    archived_at  timestamptz
);

-- Токены для локальных агентов. В фазе 1 таблица не проверяется middleware,
-- но структура уже позволяет ограничить агента проектом и набором ролей.
CREATE TABLE agent_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    project_id   uuid REFERENCES projects(id) ON DELETE CASCADE,
    name         text NOT NULL,
    token_hash   text NOT NULL UNIQUE,
    role_keys    text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz,
    revoked_at   timestamptz
);

-- Контент-адресуемое хранилище. Один и тот же артефакт, приложенный дважды,
-- физически хранится один раз. Мелкое лежит inline в bytea, крупное
-- выносится в объектное хранилище по external_uri — интерфейс BlobStore
-- в Go скрывает эту разницу от вызывающего кода.
CREATE TABLE blobs (
    sha256       char(64) PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes   bigint NOT NULL CHECK (size_bytes >= 0),
    media_type   text NOT NULL DEFAULT 'text/markdown',
    storage      text NOT NULL CHECK (storage IN ('inline', 'external')),
    content      bytea,
    external_uri text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT blobs_storage_shape CHECK (
        (storage = 'inline'   AND content IS NOT NULL AND external_uri IS NULL) OR
        (storage = 'external' AND content IS NULL     AND external_uri IS NOT NULL)
    )
);

CREATE TABLE repositories (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    provider        text NOT NULL DEFAULT 'github' CHECK (provider IN ('github')),
    owner           text NOT NULL,
    name            text NOT NULL,
    default_branch  text NOT NULL DEFAULT 'main',
    clone_url       text NOT NULL,
    visibility      text NOT NULL DEFAULT 'private' CHECK (visibility IN ('public', 'private')),
    -- пока система только читает репозитории; write добавится отдельным релизом
    access_mode     text NOT NULL DEFAULT 'read' CHECK (access_mode IN ('read')),
    -- имя секрета во внешнем хранилище, не сам токен
    credential_ref  text,
    -- подсказка агенту, за что отвечает репозиторий: 'backend', 'frontend', 'infra'
    role_hint       text,
    last_indexed_at timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, provider, owner, name)
);

-- Проектные материалы, не привязанные к конкретной задаче:
-- API-registry, блок-схемы взаимодействия, общая документация, глоссарий.
CREATE TABLE project_documents (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind          text NOT NULL,
    title         text NOT NULL,
    source        text NOT NULL CHECK (source IN ('inline', 'url', 'repo_path')),
    url           text,
    repository_id uuid REFERENCES repositories(id) ON DELETE SET NULL,
    repo_path     text,
    blob_sha      char(64) REFERENCES blobs(sha256),
    meta          jsonb NOT NULL DEFAULT '{}',
    created_by    uuid REFERENCES principals(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT project_documents_source_shape CHECK (
        (source = 'inline'    AND blob_sha IS NOT NULL) OR
        (source = 'url'       AND url IS NOT NULL) OR
        (source = 'repo_path' AND repository_id IS NOT NULL AND repo_path IS NOT NULL)
    )
);

CREATE INDEX ON repositories (project_id);
CREATE INDEX ON project_documents (project_id, kind);
