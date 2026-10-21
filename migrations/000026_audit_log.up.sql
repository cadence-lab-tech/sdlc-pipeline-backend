-- 000026_audit_log: журнал действий — кто, каким токеном, через какой канал,
-- каким тулом или ручкой, с какими параметрами и с каким исходом (CLAB-210,
-- решение 0049, п. 6).
--
-- Отдельная таблица, а не task_events: у журнала есть строки без задачи
-- (правила проекта, токены, участники, отказы до разбора задачи), у него срок
-- хранения, а лента — бессрочная история, по которой движок считает апрувы,
-- и объём здесь на порядок больше: пишется каждый вызов MCP-тула.
CREATE TABLE audit_log (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- NULL — действие уровня инстанса: команды, принципалы, токены без
    -- проекта. Удаление проекта уносит его журнал (решение 0037); след
    -- самого удаления остаётся в team_events и в журнале инстанса.
    project_id   uuid REFERENCES projects(id) ON DELETE CASCADE,
    -- NULL — проектное действие. Задача сама по себе не удаляется, только
    -- вместе с проектом, но журнал не должен зависеть от того, что лента
    -- задачи уходит с ней: запись остаётся при проекте.
    task_id      uuid REFERENCES tasks(id) ON DELETE SET NULL,
    -- Ссылка, а не строка (решение 0010). NULL — отказ 401 по токену,
    -- которого нет в таблице: доказанного актора у такого вызова нет.
    principal_id uuid REFERENCES principals(id),
    -- Токен ссылкой и снимком: отозванный и даже удалённый вместе с
    -- принципалом токен остаётся узнаваемым по имени и префиксу.
    token_id     uuid REFERENCES agent_tokens(id) ON DELETE SET NULL,
    token_name   text,
    token_prefix text,
    -- Вид без CHECK намеренно: вид токена задачи (CLAB-212) и следующие
    -- ложатся в журнал без миграции журнала. Перечень видов держит
    -- agent_tokens.kind.
    token_kind   text,
    channel      text NOT NULL CHECK (channel IN ('mcp', 'rest')),
    -- Имя тула MCP или «МЕТОД шаблон-маршрута» REST: шаблон, а не путь, —
    -- по действию фильтруют, а ключи задач в нём разбили бы его на тысячи.
    action       text NOT NULL,
    -- Параметры после чистки: тела артефактов и документов — размер и
    -- sha256, секреты вырезаны, предел размера (workflow/audit.go).
    params       jsonb NOT NULL DEFAULT '{}',
    -- 'ok' или код apperr (forbidden, unauthenticated, conflict...).
    outcome      text NOT NULL,
    -- HTTP-статус ответа REST; у вызова тула MCP его нет.
    http_status  integer,
    -- Связь с логом процесса (X-Request-Id).
    request_id   text
);

-- Чтение — новые сверху с курсором (created_at, id): по задаче, по проекту
-- и записи инстанса. Индекс по created_at заодно держит уборку по сроку.
CREATE INDEX audit_log_task_idx ON audit_log (task_id, created_at DESC, id DESC)
    WHERE task_id IS NOT NULL;
CREATE INDEX audit_log_project_idx ON audit_log (project_id, created_at DESC, id DESC);
CREATE INDEX audit_log_created_idx ON audit_log (created_at, id);
