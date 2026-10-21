DROP INDEX IF EXISTS agent_tokens_stage_run;
DELETE FROM agent_tokens WHERE kind = 'task';
ALTER TABLE agent_tokens DROP CONSTRAINT IF EXISTS agent_tokens_task_run;
ALTER TABLE agent_tokens DROP COLUMN IF EXISTS stage_run_id;
ALTER TABLE agent_tokens DROP CONSTRAINT agent_tokens_kind_check;
ALTER TABLE agent_tokens ADD CONSTRAINT agent_tokens_kind_check
    CHECK (kind IN ('api', 'session'));
