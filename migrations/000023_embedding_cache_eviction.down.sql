-- Откат 000023: кэш эмбеддингов снова не помнит, когда им пользовались, и
-- вытеснять его не по чему.

DROP INDEX embedding_cache_last_used_at_idx;
ALTER TABLE embedding_cache DROP COLUMN last_used_at;
