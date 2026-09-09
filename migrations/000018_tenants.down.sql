-- Откат 000018_tenants. Владельцы, проставленные миграцией, остаются:
-- команда-владелец — обычная команда, и снимать её незачем.
ALTER TABLE projects ALTER COLUMN owner_team_id DROP NOT NULL;
ALTER TABLE projects DROP CONSTRAINT projects_owner_team_id_fkey;
ALTER TABLE projects ADD CONSTRAINT projects_owner_team_id_fkey
    FOREIGN KEY (owner_team_id) REFERENCES teams(id) ON DELETE SET NULL;

DROP TABLE subscriptions;
DROP TABLE plans;

DROP INDEX teams_personal_of;
ALTER TABLE teams DROP CONSTRAINT teams_personal_of_kind;
ALTER TABLE teams DROP COLUMN personal_of;
ALTER TABLE teams DROP COLUMN kind;
