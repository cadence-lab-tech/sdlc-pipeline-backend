-- Откат 000041: колонка кода снова to_tsvector('simple', text), как в 000009.
-- Таблица переписывается так же, как при up; функция удаляется после
-- колонки, которая от неё зависит.
DROP INDEX IF EXISTS code_chunks_tsv;
ALTER TABLE code_chunks DROP COLUMN IF EXISTS tsv;
ALTER TABLE code_chunks
    ADD COLUMN tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', text)) STORED;
CREATE INDEX code_chunks_tsv ON code_chunks USING gin (tsv);
DROP FUNCTION IF EXISTS sdlc_code_tsvector(text);
