ALTER TABLE stage_runs DROP CONSTRAINT IF EXISTS stage_runs_model_level_pair;
ALTER TABLE stage_runs DROP COLUMN IF EXISTS model_level_source;
ALTER TABLE stage_runs DROP COLUMN IF EXISTS model_level;
ALTER TABLE tasks DROP COLUMN IF EXISTS stage_settings;
ALTER TABLE tasks DROP COLUMN IF EXISTS model_level;
ALTER TABLE role_versions DROP COLUMN IF EXISTS model_level;
