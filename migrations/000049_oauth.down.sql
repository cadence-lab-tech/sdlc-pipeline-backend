-- Откат 000049: таблицы OAuth уходят, строки agent_tokens выдач остаются
-- обычными токенами вида api — отзывать их откат не берётся, это решение
-- человека (список токенов, DELETE /v1/auth/tokens/:id).
DROP TABLE IF EXISTS oauth_refresh_tokens;
DROP TABLE IF EXISTS oauth_authorization_requests;
DROP TABLE IF EXISTS oauth_grants;
DROP TABLE IF EXISTS oauth_clients;
