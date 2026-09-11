-- Откат 000019: установка снова число в настройках проекта.

DROP INDEX IF EXISTS repositories_installation;
ALTER TABLE repositories DROP COLUMN unavailable_at;
ALTER TABLE repositories DROP COLUMN installation_id;
DROP TABLE github_installations;
