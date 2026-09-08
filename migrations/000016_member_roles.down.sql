-- Откат 000016: роли специалистов снова ограничиваются только токеном.

ALTER TABLE project_teams DROP COLUMN IF EXISTS role_keys;
ALTER TABLE project_members DROP COLUMN IF EXISTS role_keys;
