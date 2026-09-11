-- 000019_github_installations: установка GitHub App принадлежит арендатору
-- (решение 0034, уточняет 0028).
--
-- До сих пор установка была числом, которое администратор проекта вписывал
-- в настройки руками. На общем инстансе человек нажимает «Подключить GitHub»,
-- выбирает репозитории на стороне GitHub и возвращается с installation_id —
-- этот идентификатор должен помнить, чей он, чтобы его нельзя было приписать
-- чужой команде и чтобы вебхуки шли только к арендатору установки.

-- Установка приложения на аккаунт GitHub (человека или организацию).
-- Принадлежит команде: организация ставит приложение один раз, а проектов
-- у неё сколько угодно. У одной команды установок может быть несколько —
-- по числу аккаунтов GitHub, репозитории которых она подключила.
-- Секретов здесь нет: токен по установке выписывает ключ приложения.
CREATE TABLE github_installations (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id bigint NOT NULL UNIQUE,
    account_login   text NOT NULL,
    account_id      bigint NOT NULL,
    account_type    text NOT NULL CHECK (account_type IN ('User', 'Organization')),
    owner_team_id   uuid NOT NULL REFERENCES teams(id) ON DELETE RESTRICT,
    connected_by    uuid NOT NULL REFERENCES principals(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- Снятая или приостановленная на стороне GitHub установка остаётся в
    -- таблице: индекс её репозиториев не удаляется, потому что отзыв по
    -- ошибке — обычное дело, а переиндексация с нуля стоит денег.
    suspended_at    timestamptz,
    removed_at      timestamptz
);

CREATE INDEX github_installations_team ON github_installations (owner_team_id);

-- Репозиторий помнит, какой установкой он читается. Проект перестаёт быть
-- ограничен одним аккаунтом GitHub: репозиторий из личного аккаунта и
-- репозиторий организации живут в одной области задачи, каждый со своей
-- установкой. Установка проекта (000014) остаётся установкой по умолчанию.
ALTER TABLE repositories ADD COLUMN installation_id bigint
    REFERENCES github_installations(installation_id) ON DELETE SET NULL;

-- Репозиторий, убранный из установки на стороне GitHub: индексация стоит,
-- поиск по уже собранному индексу продолжает работать.
ALTER TABLE repositories ADD COLUMN unavailable_at timestamptz;

CREATE INDEX repositories_installation ON repositories (installation_id)
    WHERE installation_id IS NOT NULL;
