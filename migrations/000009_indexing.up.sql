-- 000009_indexing: инкрементальная индексация репозиториев, кэш эмбеддингов и
-- полнотекстовая половина гибридного поиска.
--
-- Отправная точка: кодовая база под задачами двигается непрерывно, и куда
-- чаще, чем задачи по циклу. Полная переиндексация на каждый коммит означала
-- бы выкачивать репозиторий целиком и заново платить за эмбеддинги всех его
-- файлов ради десятка изменившихся строк. Поэтому прогон индексации
-- наследует у родительского всё, что не изменилось, и знает, сколько он
-- унаследовал: это и есть его смысл.

-- ── Прогон индексации ───────────────────────────────────────────────────────

ALTER TABLE repo_index_runs
    -- 'full' — первый прогон ветки: наследовать не у кого.
    -- 'incremental' — есть родитель, выкачиваются только изменившиеся файлы.
    ADD COLUMN kind          text NOT NULL DEFAULT 'full'
                                 CHECK (kind IN ('full', 'incremental')),
    -- Унаследовано у родительского прогона без сети и без эмбеддингов.
    -- Отношение reused к indexed — цена инкрементальности в чистом виде.
    ADD COLUMN files_reused   integer NOT NULL DEFAULT 0,
    ADD COLUMN chunks_reused  integer NOT NULL DEFAULT 0,
    -- Сколько кусков реально ушло в модель: остальное закрыл кэш.
    ADD COLUMN chunks_embedded integer NOT NULL DEFAULT 0,
    -- Повторные попытки: сеть отвалилась, у провайдера лимит. Прогон не
    -- удаляется, потому что UNIQUE (repository_id, commit_sha) — это ещё и
    -- защита от повторной индексации того же коммита.
    ADD COLUMN attempt         integer NOT NULL DEFAULT 0,
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN trigger         text NOT NULL DEFAULT 'schedule'
                                 CHECK (trigger IN ('schedule', 'manual', 'webhook'));

-- Очередь воркера: забирает самый старый готовый к попытке прогон.
CREATE INDEX repo_index_runs_queue
    ON repo_index_runs (next_attempt_at, created_at) WHERE state = 'queued';

-- «Актуальный индекс репозитория» — это последний завершённый прогон ветки.
-- Его ищут и поиск по коду, и детектор дрейфа, и простановка базовой линии.
CREATE INDEX repo_index_runs_current
    ON repo_index_runs (repository_id, branch, finished_at DESC) WHERE state = 'completed';

-- Прогон, застрявший в running (воркер убит посреди выкачки), возвращается в
-- очередь по started_at — отдельного индекса ради него не нужно.

-- ── Файлы и куски ───────────────────────────────────────────────────────────

ALTER TABLE code_files
    -- sha объекта у провайдера (git blob sha). Именно по нему прогон решает,
    -- изменился ли файл: сравнение листинга дерева с предыдущим прогоном
    -- стоит один запрос к API, а не выкачивание содержимого.
    ADD COLUMN source_sha text;

-- Кусок адресуется содержимым: тот же текст не эмбеддится второй раз ни в
-- следующем прогоне, ни в соседнем репозитории.
ALTER TABLE code_chunks     ADD COLUMN text_sha char(64);
ALTER TABLE artifact_chunks ADD COLUMN text_sha char(64);

-- ── Кэш эмбеддингов ─────────────────────────────────────────────────────────
--
-- Ключ — модель и sha256 текста. Модель в ключе, потому что вектор от другой
-- модели несравним с этими: смена модели обязана промахиваться мимо кэша, а
-- не отдавать чужие координаты.
CREATE TABLE embedding_cache (
    model      text     NOT NULL,
    text_sha   char(64) NOT NULL,
    embedding  vector(1024) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (model, text_sha)
);

-- ── Куски проектных документов ──────────────────────────────────────────────
--
-- Артефакты принадлежат задаче, документы — проекту: API-registry и глоссарий
-- нужны агенту на любом этапе любой задачи, и в artifact_chunks им не место
-- (там NOT NULL task_id).
CREATE TABLE document_chunks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id     uuid NOT NULL REFERENCES project_documents(id) ON DELETE CASCADE,
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    ord             integer NOT NULL,
    text            text NOT NULL,
    token_count     integer,
    text_sha        char(64),
    embedding       vector(1024),
    embedding_model text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (document_id, ord)
);

-- ── Полнотекстовая половина поиска ──────────────────────────────────────────
--
-- Поиск гибридный: вектор находит смысл, полнотекст — точное имя функции,
-- ключ конфига или номер ошибки, по которым вектор промахивается. Конфигурация
-- разная намеренно: код — 'simple' (стеммить идентификаторы нечем и незачем),
-- артефакты и документы пишутся по-русски.
ALTER TABLE code_chunks
    ADD COLUMN tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', text)) STORED;
ALTER TABLE artifact_chunks
    ADD COLUMN tsv tsvector GENERATED ALWAYS AS (to_tsvector('russian', text)) STORED;
ALTER TABLE document_chunks
    ADD COLUMN tsv tsvector GENERATED ALWAYS AS (to_tsvector('russian', text)) STORED;

-- Запрос агента — это фраза («как устроена ротация токенов»), а не набор
-- обязательных слов. websearch_to_tsquery склеивает слова через AND, и в
-- режиме без векторов такой запрос не находит ничего, если хоть одного слова
-- в куске нет. Здесь связка меняется на OR, а за релевантность отвечает
-- ранжирование: лучше выдать отсортированное «похожее», чем пустоту.
CREATE FUNCTION sdlc_tsquery(config regconfig, query text) RETURNS tsquery
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
        SELECT replace(websearch_to_tsquery(config, query)::text, '&', '|')::tsquery
    $$;

CREATE INDEX code_chunks_tsv     ON code_chunks     USING gin (tsv);
CREATE INDEX artifact_chunks_tsv ON artifact_chunks USING gin (tsv);
CREATE INDEX document_chunks_tsv ON document_chunks USING gin (tsv);
CREATE INDEX document_chunks_embedding_hnsw
    ON document_chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX ON document_chunks (project_id);

-- ── Что индексировать ───────────────────────────────────────────────────────

ALTER TABLE repositories
    -- Репозиторий можно оставить в проекте (он часть области задач), но не
    -- индексировать: зеркало, архив, гигантский монорепозиторий.
    ADD COLUMN index_enabled boolean NOT NULL DEFAULT true,
    -- Дополнение к общему списку исключений индексатора, на случай генерённых
    -- каталогов, о которых знает только этот проект.
    ADD COLUMN exclude_globs text[] NOT NULL DEFAULT '{}';

-- ── Состояние PR со стороны GitHub ──────────────────────────────────────────
--
-- Гейт external_check не ходит в сеть: он исполняется внутри транзакции
-- перехода вместе с остальными гейтами, и сетевой вызов там означал бы
-- открытую транзакцию на время таймаута чужого API. Состояние проверок
-- приносит воркер, гейт читает готовое.
ALTER TABLE task_repo_refs
    ADD COLUMN head_sha  text,
    -- {"conclusion":"success","runs":[{"name":"build","conclusion":"success"}]}
    ADD COLUMN checks    jsonb NOT NULL DEFAULT '{}',
    ADD COLUMN synced_at timestamptz;
