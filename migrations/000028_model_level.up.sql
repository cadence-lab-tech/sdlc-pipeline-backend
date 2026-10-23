-- 000028_model_level: рекомендованный уровень модели на роли, задаче и этапе
-- задачи, снимок на прогоне (CLAB-259, решение 0060).
--
-- Уровень, а не строка модели: написание модели зависит от провайдера и
-- исполнителя, а три уровня переносятся между ними. Сервер моделей не знает;
-- модель под уровень выбирает исполнитель. model_hint остаётся ради clab и
-- раннера прежних версий.
--
-- Миграция только добавляет: ADD COLUMN без значения и с постоянным DEFAULT
-- в Postgres 11+ таблицу не переписывает, апгрейд инстанса с данными идёт
-- без ручных шагов. Уровня на статусе цикла нет — цикл описывает путь
-- задачи, а не сложность работы (решение 0060, «Альтернативы»).

ALTER TABLE role_versions ADD COLUMN model_level text
    CONSTRAINT role_versions_model_level_check
    CHECK (model_level IN ('low', 'medium', 'high'));

ALTER TABLE tasks ADD COLUMN model_level text
    CONSTRAINT tasks_model_level_check
    CHECK (model_level IN ('low', 'medium', 'high'));

-- Объект «ключ статуса → объект настроек этапа», а не колонка уровня: рядом с
-- model_level встанут поля профиля исполнения (CLAB-64). Ключи сверяет движок
-- по версии цикла задачи — схема их не знает, а цикл задачи может быть старым.
ALTER TABLE tasks ADD COLUMN stage_settings jsonb NOT NULL DEFAULT '{}'
    CONSTRAINT tasks_stage_settings_object
    CHECK (jsonb_typeof(stage_settings) = 'object');

-- Снимок при захвате: уровни задачи правятся, а прогон помнит, что ему
-- рекомендовали. У прогонов до миграции обе колонки NULL — «снимка не было»,
-- это не источник none.
ALTER TABLE stage_runs ADD COLUMN model_level text
    CONSTRAINT stage_runs_model_level_check
    CHECK (model_level IN ('low', 'medium', 'high'));
ALTER TABLE stage_runs ADD COLUMN model_level_source text
    CONSTRAINT stage_runs_model_level_source_check
    CHECK (model_level_source IN ('stage', 'task', 'role', 'none'));

-- Пара согласована: none — ровно при пустом уровне, иначе источник назван.
-- IS NOT NULL явно: сравнение с NULL в CHECK — не ложь, и уровень без
-- источника прошёл бы.
ALTER TABLE stage_runs ADD CONSTRAINT stage_runs_model_level_pair CHECK (
    (model_level IS NULL AND (model_level_source IS NULL OR model_level_source = 'none'))
    OR (model_level IS NOT NULL AND model_level_source IS NOT NULL
        AND model_level_source <> 'none')
);
