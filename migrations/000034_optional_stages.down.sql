-- Откат 000034: отметки необязательных этапов теряются, все статусы снова
-- обязательны. Выбор этапов на задачах (CLAB-315) откатывается своей
-- миграцией раньше этой.
ALTER TABLE workflow_statuses
    DROP CONSTRAINT IF EXISTS workflow_statuses_default_needs_optional,
    DROP COLUMN IF EXISTS enabled_by_default,
    DROP COLUMN IF EXISTS is_optional;
