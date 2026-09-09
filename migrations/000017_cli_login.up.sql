-- 000017_cli_login: вход CLI через браузер (решение 0032).
--
-- CLI на машине разработчика не может принять код авторизации от GitHub:
-- callback приложения ведёт на интерфейс, а не на localhost, и человек может
-- работать по ssh, где браузера нет вовсе. Поэтому вход устроен как device
-- flow: CLI заводит запрос и показывает короткий код, человек подтверждает его
-- в уже открытом интерфейсе, CLI опрашивает запрос и получает обычный
-- долгоживущий токен.
--
-- Секретов в таблице нет: poll_hash — sha256 секрета, который держит только
-- CLI; сам токен выдаётся в момент опроса и здесь не появляется.
CREATE TABLE cli_login_requests (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Короткий код для человека: он сверяет его с тем, что показал CLI.
    user_code    text NOT NULL UNIQUE,
    poll_hash    text NOT NULL UNIQUE,
    -- Подпись запроса — «clab на macbook»: так же будет называться токен.
    label        text NOT NULL,
    -- Срок токена, о котором просит CLI; 0 — бессрочно. Человек видит его
    -- перед подтверждением.
    token_days   int NOT NULL DEFAULT 0 CHECK (token_days >= 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    approved_by  uuid REFERENCES principals(id) ON DELETE CASCADE,
    approved_at  timestamptz,
    denied_at    timestamptz,
    -- Токен выдан и забран: повторный опрос ничего не вернёт.
    consumed_at  timestamptz
);
