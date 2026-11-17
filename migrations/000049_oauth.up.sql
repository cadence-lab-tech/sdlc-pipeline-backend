-- 000049_oauth: инстанс — сервер авторизации OAuth 2.1 для удалённых
-- MCP-клиентов (CLAB-481, решение 0079).
--
-- Новый вид токена не заводится (решения 0021, 0049, 0076): выдача
-- («подключённый клиент») ссылается на одну строку agent_tokens вида api, у
-- которой на каждом refresh меняются хеш, префикс и срок. Состояние
-- «отозван» одно — agent_tokens.revoked_at этой строки; здесь только то, чего
-- в токене нет: клиент, его метка, привязка через resource, refresh и
-- причина отзыва.
--
-- Секретов в базе нет: коды и refresh — sha256 (колонки *_hash), секретов
-- клиентов не бывает — клиенты только публичные. Миграция только добавляет
-- таблицы, данных не переносит, agent_tokens.kind не трогает.

-- Клиенты, зарегистрированные по DCR (RFC 7591). Клиенты CIMD строк не
-- имеют: их документ живёт по адресу client_id и кэшируется в памяти api.
CREATE TABLE oauth_clients (
    -- client_id, выданный при регистрации: 32 случайных байта в base64url.
    -- Текст, а не uuid: у CIMD client_id — URL, и выдача хранит оба вида
    -- одной колонкой.
    id               text PRIMARY KEY,
    client_name      text NOT NULL,
    redirect_uris    text[] NOT NULL CHECK (cardinality(redirect_uris) > 0),
    software_id      text,
    software_version text,
    -- Пределы DCR (1000 без живой выдачи, 60 за скользящий час) считаются по
    -- этой колонке в базе, а не в памяти: реплики api видят одно число.
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX oauth_clients_created_idx ON oauth_clients (created_at);

-- Выдача: человек разрешил клиенту действовать от своего имени. Строка
-- живёт, пока жив её токен; отозванная остаётся следом с причиной.
CREATE TABLE oauth_grants (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Одна строка токена на всё подключение: token_id в журнале один на весь
    -- срок клиента, и отзыв этой строки гасит выдачу целиком.
    token_id      uuid NOT NULL UNIQUE REFERENCES agent_tokens(id) ON DELETE CASCADE,
    principal_id  uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    -- id DCR или URL документа CIMD. Без внешнего ключа: у CIMD строки
    -- клиента нет, а клиент DCR уходит заданием воркера только без живой
    -- выдачи.
    client_id     text NOT NULL,
    client_kind   text NOT NULL CHECK (client_kind IN ('dcr', 'cimd')),
    -- Снимок на момент согласия: имя клиент заявляет сам и может сменить, а
    -- человек разрешал именно это.
    client_name   text NOT NULL,
    redirect_host text NOT NULL,
    -- Адрес MCP из authorize; по нему проект привязки (NULL — инстансовый).
    resource      text NOT NULL,
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE,
    -- Столбец заведён сразу, чтобы сужение скоупов позже не требовало
    -- миграции данных (решение 0079, п. 7).
    scope         text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    refreshed_at  timestamptz,
    -- Причина отзыва; сам отзыв — agent_tokens.revoked_at.
    revoke_reason text CHECK (revoke_reason IN ('user', 'refresh_reuse', 'code_reuse', 'expired'))
);
CREATE INDEX oauth_grants_principal_idx ON oauth_grants (principal_id, created_at DESC);
CREATE INDEX oauth_grants_client_idx ON oauth_grants (client_id);

-- Запрос авторизации: от authorize до обмена кода. Код живёт здесь же — его
-- sha256, срок и отметка погашения: повтор погашенного кода отзывает
-- выданную по нему выдачу (grant_id).
CREATE TABLE oauth_authorization_requests (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id             text NOT NULL,
    client_kind           text NOT NULL CHECK (client_kind IN ('dcr', 'cimd')),
    client_name           text NOT NULL,
    redirect_uri          text NOT NULL,
    -- state клиента возвращается как есть: это его значение, не наш секрет.
    state                 text,
    code_challenge        text NOT NULL,
    scope                 text NOT NULL,
    resource              text NOT NULL,
    -- Ключ проекта из resource; проект и участие проверяются на согласии.
    project_key           text,
    created_at            timestamptz NOT NULL DEFAULT now(),
    expires_at            timestamptz NOT NULL,
    decision              text CHECK (decision IN ('allow', 'deny')),
    decided_at            timestamptz,
    decided_by            uuid REFERENCES principals(id) ON DELETE CASCADE,
    code_hash             text UNIQUE,
    code_expires_at       timestamptz,
    code_used_at          timestamptz,
    grant_id              uuid REFERENCES oauth_grants(id) ON DELETE SET NULL,
    CONSTRAINT oauth_requests_code_needs_allow CHECK (code_hash IS NULL OR decision = 'allow')
);
CREATE INDEX oauth_requests_expires_idx ON oauth_authorization_requests (expires_at);

-- Refresh выдачи: sha256 каждого выпущенного с отметкой использования.
-- Ротация на каждом использовании; погашенные хранятся, пока жива выдача, —
-- по ним узнаётся повтор, который отзывает выдачу.
CREATE TABLE oauth_refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    grant_id    uuid NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
    token_hash  text NOT NULL UNIQUE,
    -- Предыдущий refresh цепочки: повтор непосредственно предыдущего в
    -- окне гонки параллельных запросов выдачу не отзывает.
    parent_id   uuid REFERENCES oauth_refresh_tokens(id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- Срок без использования: созданный + SDLC_OAUTH_REFRESH_TTL.
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz
);
-- Действующий refresh у выдачи всегда один: два параллельных обмена одного
-- refresh не выпустят две пары, второй упрётся в индекс.
CREATE UNIQUE INDEX oauth_refresh_one_live ON oauth_refresh_tokens (grant_id) WHERE used_at IS NULL;
