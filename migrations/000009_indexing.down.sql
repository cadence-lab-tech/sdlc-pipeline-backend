-- Откат 000009. Кэш эмбеддингов и куски документов сносятся вместе с
-- таблицами: это производные данные, восстанавливаются переиндексацией.

DROP FUNCTION IF EXISTS sdlc_tsquery(regconfig, text);
DROP TABLE IF EXISTS document_chunks;
DROP TABLE IF EXISTS embedding_cache;

DROP INDEX IF EXISTS code_chunks_tsv;
DROP INDEX IF EXISTS artifact_chunks_tsv;
ALTER TABLE code_chunks     DROP COLUMN IF EXISTS tsv;
ALTER TABLE artifact_chunks DROP COLUMN IF EXISTS tsv;

ALTER TABLE code_chunks     DROP COLUMN IF EXISTS text_sha;
ALTER TABLE artifact_chunks DROP COLUMN IF EXISTS text_sha;
ALTER TABLE code_files      DROP COLUMN IF EXISTS source_sha;

ALTER TABLE task_repo_refs
    DROP COLUMN IF EXISTS head_sha,
    DROP COLUMN IF EXISTS checks,
    DROP COLUMN IF EXISTS synced_at;

ALTER TABLE repositories
    DROP COLUMN IF EXISTS index_enabled,
    DROP COLUMN IF EXISTS exclude_globs;

DROP INDEX IF EXISTS repo_index_runs_queue;
DROP INDEX IF EXISTS repo_index_runs_current;
ALTER TABLE repo_index_runs
    DROP COLUMN IF EXISTS kind,
    DROP COLUMN IF EXISTS files_reused,
    DROP COLUMN IF EXISTS chunks_reused,
    DROP COLUMN IF EXISTS chunks_embedded,
    DROP COLUMN IF EXISTS attempt,
    DROP COLUMN IF EXISTS next_attempt_at,
    DROP COLUMN IF EXISTS trigger;
