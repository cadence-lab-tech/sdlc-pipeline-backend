-- 000029_attempts_limit: одиннадцатый тип гейта — предел попыток целевого
-- статуса перехода (CLAB-217, решение 0061).
--
-- Предел задаётся в цикле, на переходе, а не в роли: петлю «проверка →
-- доработка» описывает путь задачи, а роль про поведение агента. Число
-- лежит в config гейта ({"max_attempts": N}), его проверяет валидатор цикла,
-- схема знает только тип. Существующие циклы гейта не получают и ведут себя
-- по-прежнему: миграция лишь расширяет набор допустимых типов, апгрейд с
-- данными идёт без ручных шагов.

ALTER TABLE gates DROP CONSTRAINT gates_type_check;
ALTER TABLE gates ADD CONSTRAINT gates_type_check CHECK (type IN (
    'artifacts_required',
    'schema_validation',
    'checklist',
    'human_approval',
    'agent_review',
    'external_check',
    'repo_ref_required',
    'drift_clear',
    'repo_scope',
    'dependencies_closed',
    'attempts_limit'));
