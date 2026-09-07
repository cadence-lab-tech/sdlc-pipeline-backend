-- Откат 000014: доступ к GitHub снова общий на инстанс.

DROP INDEX IF EXISTS projects_github_installation;
ALTER TABLE projects DROP COLUMN github_credential_ref;
ALTER TABLE projects DROP COLUMN github_installation_id;
