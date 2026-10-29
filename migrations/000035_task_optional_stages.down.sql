-- Откат 000035: выбор необязательных этапов у задач теряется, движок снова
-- проходит все статусы. Отметки в версиях цикла (000034) остаются.
ALTER TABLE tasks
    DROP CONSTRAINT IF EXISTS tasks_optional_stages_object,
    DROP COLUMN IF EXISTS optional_stages;
