-- Откат 000047: снимок версии на прогонах и версии настройки агентов уходят
-- целиком. Прочие колонки прогонов и данные проектов не трогаются.
DROP INDEX IF EXISTS stage_runs_agent_settings_version_idx;
ALTER TABLE stage_runs DROP COLUMN IF EXISTS agent_settings_version_id;
DROP TABLE IF EXISTS agent_settings_versions;
