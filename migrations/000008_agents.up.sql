-- 000008_agents: аренды агентов и вложенный прогон перепроверки (гейт agent_review).
--
-- Аренда как таковая уже была в схеме (claimed_by, lease_expires_at,
-- agent_session_ref) — здесь появляется только отметка последнего heartbeat:
-- по ней видно, агент молчит или просто продлевает аренду редко.
ALTER TABLE stage_runs ADD COLUMN heartbeat_at timestamptz;

-- Гейт agent_review отдаёт результат этапа другой роли на перепроверку.
-- Ревьюер работает не «где-то сбоку», а таким же прогоном этапа: у него своя
-- аренда, свои артефакты и своя запись в истории. Отличается он тем, что
-- живёт внутри родительского прогона и принадлежит конкретному гейту.
--
-- Это не нарушает «внутри задачи нет параллельных этапов» (решение 0007):
-- пока идёт перепроверка, родительский прогон стоит в gating и работать
-- в нём некому — активная работа по-прежнему ровно одна.
ALTER TABLE stage_runs
    ADD COLUMN kind          text NOT NULL DEFAULT 'stage' CHECK (kind IN ('stage', 'review')),
    ADD COLUMN parent_run_id uuid REFERENCES stage_runs(id) ON DELETE CASCADE,
    ADD COLUMN gate_id       uuid REFERENCES gates(id) ON DELETE CASCADE;

ALTER TABLE stage_runs ADD CONSTRAINT stage_runs_review_shape CHECK (
    (kind = 'stage'  AND parent_run_id IS NULL     AND gate_id IS NULL) OR
    (kind = 'review' AND parent_run_id IS NOT NULL AND gate_id IS NOT NULL)
);

-- Прежняя уникальность (task_id, status_id, attempt) запрещала бы второй
-- прогон на том же статусе, а прогон ревью идёт именно там же. Уникальность
-- расходится по видам: у этапа она по попытке на статусе, у перепроверки —
-- по попытке внутри пары (родительский прогон, гейт), потому что после
-- доработки тот же гейт запускает ревью заново.
ALTER TABLE stage_runs DROP CONSTRAINT stage_runs_task_id_status_id_attempt_key;

CREATE UNIQUE INDEX stage_runs_stage_attempt_uniq
    ON stage_runs (task_id, status_id, attempt) WHERE kind = 'stage';

CREATE UNIQUE INDEX stage_runs_review_attempt_uniq
    ON stage_runs (parent_run_id, gate_id, attempt) WHERE kind = 'review';

-- Отбор «что сейчас можно забрать» идёт по проекту и упирается в это условие.
CREATE INDEX stage_runs_open ON stage_runs (task_id)
    WHERE state IN ('pending', 'claimed', 'in_progress');

CREATE INDEX stage_runs_review_parent ON stage_runs (parent_run_id) WHERE kind = 'review';
