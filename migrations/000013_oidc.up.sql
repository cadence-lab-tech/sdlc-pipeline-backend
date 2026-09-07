-- 000013_oidc: вход людей через корпоративный провайдер (решение 0027).
--
-- Учётки людей уже есть у компании, и вторая на каждого — это и неудобно, и
-- небезопасно: токен, выданный руками, не отзывается вместе с увольнением.
-- Транспорт при этом не меняется: после проверки ID-токена бэкенд выдаёт
-- обычный токен той же таблицы, только короткоживущий.

-- Ключ принципала у провайдера. Не email: почту в IdP меняют, `sub` — нет,
-- и переименование сотрудника не должно заводить второго принципала.
ALTER TABLE principals ADD COLUMN idp_subject text;

CREATE UNIQUE INDEX principals_idp_subject ON principals (idp_subject)
    WHERE idp_subject IS NOT NULL;

-- Вид токена. Сессия отличается от токена агента сроком и происхождением:
-- её выдаёт вход через провайдера, и живёт она часы, а не месяцы. Ветка в
-- проверке прав от этого не появляется — токен остаётся токеном.
ALTER TABLE agent_tokens ADD COLUMN kind text NOT NULL DEFAULT 'api'
    CHECK (kind IN ('api', 'session'));

-- Сопоставление групп провайдера с командами: состав синхронизируется при
-- каждом входе, руками его править не нужно. Одна группа может вести в
-- несколько команд, и в одну команду — несколько групп.
CREATE TABLE team_idp_groups (
    team_id   uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    idp_group text NOT NULL,
    added_by  uuid REFERENCES principals(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, idp_group)
);

CREATE INDEX team_idp_groups_group ON team_idp_groups (idp_group);
