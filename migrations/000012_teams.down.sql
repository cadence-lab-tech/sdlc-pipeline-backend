-- Откат 000012: доступ снова поимённый, проект ничей.
--
-- Права, выданные через команды, при откате не размазываются по
-- project_members: угадывать, кому из состава команды они предназначались,
-- миграция не вправе.

DROP INDEX IF EXISTS projects_owner_team;
ALTER TABLE projects DROP COLUMN owner_team_id;

DROP TABLE IF EXISTS project_teams;
DROP TABLE IF EXISTS team_members;
DROP TABLE IF EXISTS teams;
