-- Откат 000013: вход только по токенам, выданным руками.
--
-- Сессии удаляются, а не оставляются протухать: без вида токена отличить их
-- от токенов агентов будет нечем.

DELETE FROM agent_tokens WHERE kind = 'session';
ALTER TABLE agent_tokens DROP COLUMN kind;

DROP TABLE IF EXISTS team_idp_groups;

DROP INDEX IF EXISTS principals_idp_subject;
ALTER TABLE principals DROP COLUMN idp_subject;
