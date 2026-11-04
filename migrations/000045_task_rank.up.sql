-- 000045_task_rank: ранг бэклога — ручной порядок задач (CLAB-399, эпик
-- CLAB-368, решение 0075).
--
-- Ключ дробный лексикографический (domain.RankBetween): перемещение пишет
-- ранг одной задачи, соседей не перенумеровывает. COLLATE "C" — сравнение
-- байтами, как у строк Go: генератор и ORDER BY обязаны видеть один порядок,
-- а лингвистическое сравнение локали базы его ломает (регистр, «0» и «a»).
-- Группа ранга — (project_id, parent_id): верхний уровень проекта или дети
-- одного родителя; ранг за задачей сохраняется и в работе, и после закрытия.
ALTER TABLE tasks ADD COLUMN rank text COLLATE "C";

-- Заполнение — в прежнем порядке order=backlog (CLAB-363): без незакрытых
-- блокирующих выше, затем приоритет, затем номер. Выражение заморожено
-- копией: миграция не зависит от того, каким станет порядок в Go.
-- Ключи — тот же набор, что domain.RankSpread(n) для группы из n задач:
-- одна длина на группу с шагом не меньше основания 62 (между соседями
-- поместится вставка без роста длины), ключ на младшую цифру «0» сдвинут на
-- единицу.
WITH ordered AS (
    SELECT t.id,
           row_number() OVER w AS r,
           count(*) OVER (PARTITION BY t.project_id, t.parent_id) AS n
    FROM tasks t
    WINDOW w AS (
        PARTITION BY t.project_id, t.parent_id
        ORDER BY EXISTS (SELECT 1 FROM task_links bl
                         JOIN tasks blk ON blk.id = bl.from_task_id
                         JOIN workflow_statuses bws ON bws.id = blk.status_id
                         WHERE bl.to_task_id = t.id AND bl.rel = 'blocks' AND bws.category <> 'done'),
                 CASE t.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
                 t.seq)
),
sized AS (
    SELECT o.id, o.r, s.len, s.step
    FROM ordered o
    CROSS JOIN LATERAL (
        SELECT l AS len, div(62::numeric ^ l, o.n + 1) AS step
        FROM generate_series(1, 16) l
        WHERE 62::numeric ^ l >= 62 * (o.n + 1)
        ORDER BY l
        LIMIT 1) s
),
valued AS (
    SELECT id, len,
           step * r + CASE WHEN mod(step * r, 62) = 0 THEN 1 ELSE 0 END AS v
    FROM sized
)
UPDATE tasks t
SET rank = (
    SELECT string_agg(substr('0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz',
                             mod(div(v.v, 62::numeric ^ k), 62)::int + 1, 1), '' ORDER BY k DESC)
    FROM generate_series(0, v.len - 1) k)
FROM valued v
WHERE v.id = t.id;

-- Ключ из алфавита генератора и не на младшую цифру: иначе между «a» и
-- «a0» не вставить. Прямая вставка в обход CreateTask (сиды, фикстуры)
-- обязана назвать ранг сама.
ALTER TABLE tasks
    ALTER COLUMN rank SET NOT NULL,
    ADD CONSTRAINT tasks_rank_key_check CHECK (rank ~ '^[0-9A-Za-z]*[1-9A-Za-z]$');

-- Соседи в группе: перемещение и заведение ищут край и соседей по группе,
-- порядок бэклога листает по (rank, seq). Не уникальный: при равных рангах
-- порядок детерминирует номер.
CREATE INDEX tasks_rank_group ON tasks (project_id, parent_id, rank, seq);
