-- 000041_code_tsvector_parts: полнотекст кода находит имя внутри записи
-- через точку (CLAB-362).
--
-- Стандартный парсер Postgres отдаёт запись через точку одним токеном:
-- ts_debug('simple', …) — host|e.store.RevokeRunTokens, host|api.listAttachments,
-- host|context.Context, file|pkg.X, file|internal/workflow.Engine.Advance.
-- Голого имени в tsvector нет, и запрос RevokeRunTokens не находит ни одного
-- вызова метода: в Go и TS почти каждый вызов записан так. JSX-тег со
-- строковым атрибутом — тоже один токен (tag|<Button variant="x">), и имя
-- компонента в tsvector не попадает.
--
-- sdlc_code_tsvector — прежний to_tsvector('simple', text) плюс части
-- составных токенов: url (5), host (6), tag (13), url_path (18), file (19)
-- парсера default (oid 3722; номера типов и oid парсера зашиты в исходники
-- Postgres и одинаковы в 15–18). Знаки, кроме букв, цифр и _, становятся
-- пробелами, части разбираются тем же 'simple'. Запись целиком остаётся
-- лексемой, как была: запрос e.store.RevokeRunTokens находит её по-прежнему.
-- obj?.x и user_id парсер уже режет сам — им ничего не добавляется.
--
-- camelCase не дробится: RevokeRunTokens остаётся одним словом. Части
-- (run, tokens, get, context) есть почти в каждом куске, а запрос в
-- OR-семантике (sdlc_tsquery) поднял бы по слову run весь код; смысловой
-- запрос — дело векторной половины.
--
-- ts_parse по oid парсера IMMUTABLE (по имени — STABLE), поэтому функция
-- годится для генерируемой колонки. Функции и конфигурация — с pg_catalog
-- явно (операторы pg_catalog находит при любом search_path): pg_dump пишет
-- выражение колонки как public.sdlc_code_tsvector(text) и грузит дамп при
-- пустом search_path, колонка пересчитывается при загрузке.
CREATE FUNCTION sdlc_code_tsvector(body text) RETURNS tsvector
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
        SELECT pg_catalog.to_tsvector('pg_catalog.simple', body)
            || pg_catalog.to_tsvector('pg_catalog.simple', coalesce(
                (SELECT pg_catalog.string_agg(
                            pg_catalog.regexp_replace(p.token, '[^[:alnum:]_]+', ' ', 'g'), ' ')
                 FROM pg_catalog.ts_parse(3722::pg_catalog.oid, body) p
                 WHERE p.tokid = ANY ('{5,6,13,18,19}'::pg_catalog.int4[])),
                ''))
    $$;

-- Новое выражение — новая колонка: ALTER COLUMN … SET EXPRESSION есть только
-- с PostgreSQL 17, а внешняя база клиента бывает 15–18. ADD COLUMN … STORED
-- переписывает таблицу под эксклюзивной блокировкой и пересобирает её индексы,
-- HNSW векторов тоже: около 0,3 мс на кусок без векторов и до 2 мс с ними
-- (замер CLAB-362; заметка апгрейда в self-hosted/docs/admin.md). Куски —
-- производные данные, переиндексация после миграции не нужна: колонка
-- считается из текста сама. maintenance_work_mem здесь не поднимается: в
-- контейнере с /dev/shm по умолчанию (64 МБ) пересборка HNSW с 256 МБ падает.
DROP INDEX code_chunks_tsv;
ALTER TABLE code_chunks DROP COLUMN tsv;
ALTER TABLE code_chunks
    ADD COLUMN tsv tsvector GENERATED ALWAYS AS (sdlc_code_tsvector(text)) STORED;
CREATE INDEX code_chunks_tsv ON code_chunks USING gin (tsv);
