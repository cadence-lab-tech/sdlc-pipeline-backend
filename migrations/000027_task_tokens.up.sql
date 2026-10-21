-- 000027_task_tokens: токен задачи — учётные данные исполнителя этапа
-- (CLAB-212, решение 0049).
--
-- Аренда привязана к принципалу, а пул раннеров — один принципал на много
-- задач (решение 0025): его токен работает с любой задачей, которую пул
-- держит. Исполнителю этапа нужен токен уже аренды — привязанный к прогону.
-- Своего срока у него нет: живёт он, пока жива аренда прогона у того же
-- принципала, и это проверяется на каждом запросе по stage_runs, а не
-- копией срока в токене, которая разошлась бы с heartbeat.
--
-- Номер 000027, а не следующий за main 000026: его заняла миграция журнала
-- CLAB-210, которая идёт параллельно. Мигратор применяет всё, чего нет в
-- schema_migrations, поэтому порядок слияния двух задач не важен.
ALTER TABLE agent_tokens DROP CONSTRAINT agent_tokens_kind_check;
ALTER TABLE agent_tokens ADD CONSTRAINT agent_tokens_kind_check
    CHECK (kind IN ('api', 'session', 'task'));

-- Прогон уходит вместе с задачей — и токены задачи вместе с ним: висящий
-- токен на несуществующую работу ничего, кроме вопросов, не даёт.
ALTER TABLE agent_tokens
    ADD COLUMN stage_run_id uuid REFERENCES stage_runs(id) ON DELETE CASCADE;

-- Токен задачи без прогона — токен ни на что; прогон у личного токена —
-- ошибка, которая молча сузила бы его до одной задачи.
ALTER TABLE agent_tokens ADD CONSTRAINT agent_tokens_task_run
    CHECK ((kind = 'task') = (stage_run_id IS NOT NULL));

-- Отзыв по событиям аренды ищет живые токены прогона: release, протухание,
-- переход идут под транзакцией прогона, и полный проход по таблице там лишний.
CREATE INDEX agent_tokens_stage_run ON agent_tokens (stage_run_id)
    WHERE revoked_at IS NULL;
