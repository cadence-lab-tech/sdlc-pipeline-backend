-- 000046_stage_runs_outcome_moved: исход прогона «переведён» (CLAB-408, эпик
-- CLAB-404).
--
-- Перевод задачи в статус — операция движка вне переходов цикла: сотрудник
-- или сопровождающий ставит задачу в любой статус её версии цикла, гейты не
-- вычисляются. Прогон, из которого задачу перевели, закрывается abandoned с
-- отдельным исходом: это не возврат (rejected) и не законченный этап
-- (advanced), и метрики статуса держат его отдельным счётом moves, а не в
-- возвратах и часах. Миграция только расширяет набор: старые строки не
-- трогаются, апгрейд с данными идёт без ручных шагов.

ALTER TABLE stage_runs DROP CONSTRAINT stage_runs_outcome_check;
ALTER TABLE stage_runs ADD CONSTRAINT stage_runs_outcome_check
    CHECK (outcome IN ('advanced', 'rejected', 'cancelled', 'moved'));
