-- 000048_usage_pricing: откуда взялась сумма строки учёта — источник, версия
-- прайса и оценка самого агента (CLAB-426, решение 0077 п. 4.11).
--
-- Раньше cost_usd — всегда самоотчёт агента (решение 0026). Теперь, если у
-- прогона есть снимок настройки агентов (000047) и в её прайсе есть модель
-- строки, сумму считает сервер по токенам; названная агентом сумма не
-- пропадает, а ложится рядом как reported_cost_usd. Колонки:
--   cost_source        — 'pricing' (посчитано по прайсу) или 'reported' (как
--                        названа агентом); NULL — суммы нет или строка до
--                        миграции;
--   pricing_version_id — версия настройки агентов, по прайсу которой
--                        посчитана сумма; только при 'pricing';
--   reported_cost_usd  — сумма, названная агентом; NULL — не называл.
--
-- Миграция только добавляет: ADD COLUMN без значения по умолчанию таблицу не
-- переписывает, апгрейд инстанса с данными идёт без ручных шагов. Старые
-- строки получают NULL и не пересчитываются: сумма прошлого не плывёт от
-- смены прайса.

ALTER TABLE stage_run_usage ADD COLUMN cost_source text
    CONSTRAINT stage_run_usage_cost_source_check
    CHECK (cost_source IN ('pricing', 'reported'));

-- Без ON DELETE: версии настройки уходят только вместе с проектом, а его
-- задачи и прогоны (и строки учёта каскадом) DeleteProject удаляет раньше.
ALTER TABLE stage_run_usage ADD COLUMN pricing_version_id uuid
    CONSTRAINT stage_run_usage_pricing_version_fkey
    REFERENCES agent_settings_versions(id);

ALTER TABLE stage_run_usage ADD COLUMN reported_cost_usd numeric(12, 6)
    CONSTRAINT stage_run_usage_reported_cost_check
    CHECK (reported_cost_usd IS NULL OR reported_cost_usd >= 0);

-- Источник «по прайсу» без версии не бывает: сумму нечем объяснить.
ALTER TABLE stage_run_usage ADD CONSTRAINT stage_run_usage_pricing_has_version
    CHECK (cost_source IS DISTINCT FROM 'pricing' OR pricing_version_id IS NOT NULL);

-- Проверка внешнего ключа при удалении версий проекта и выборка строк по
-- версии прайса идут по индексу; у строк не по прайсу NULL, в индекс они не
-- попадают.
CREATE INDEX stage_run_usage_pricing_version_idx
    ON stage_run_usage (pricing_version_id) WHERE pricing_version_id IS NOT NULL;
