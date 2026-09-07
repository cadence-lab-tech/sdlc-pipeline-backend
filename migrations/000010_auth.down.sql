DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS project_members;
DROP INDEX IF EXISTS agent_tokens_principal;
ALTER TABLE agent_tokens DROP COLUMN IF EXISTS created_by, DROP COLUMN IF EXISTS token_prefix;
ALTER TABLE principals DROP COLUMN IF EXISTS is_admin;
