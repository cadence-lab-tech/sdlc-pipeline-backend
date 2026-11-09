-- Откат 000048: источник суммы, версия прайса и оценка агента уходят, суммы
-- cost_usd и остальные колонки строк учёта не трогаются.
DROP INDEX IF EXISTS stage_run_usage_pricing_version_idx;
ALTER TABLE stage_run_usage DROP CONSTRAINT IF EXISTS stage_run_usage_pricing_has_version;
ALTER TABLE stage_run_usage DROP COLUMN IF EXISTS reported_cost_usd;
ALTER TABLE stage_run_usage DROP COLUMN IF EXISTS pricing_version_id;
ALTER TABLE stage_run_usage DROP COLUMN IF EXISTS cost_source;
