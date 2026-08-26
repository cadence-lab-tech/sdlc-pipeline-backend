-- 000005_knowledge: поисковый слой — векторизация артефактов и индекс кодовой базы.
--
-- Размерность вектора фиксируется на уровне колонки, поэтому она выбрана один
-- раз: 1024 = voyage-code-3 (рекомендуемая Anthropic модель для кода) и
-- text-embedding-3-large с dimensions=1024. Смена модели на другую размерность
-- = новая миграция с новой колонкой и переиндексацией.

CREATE EXTENSION IF NOT EXISTS vector;

-- Куски артефактов для семантического поиска по истории решений проекта.
CREATE TABLE artifact_chunks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    artifact_id     uuid NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    -- денормализовано ради дешёвой фильтрации векторного поиска по проекту
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id         uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    ord             integer NOT NULL,
    text            text NOT NULL,
    token_count     integer,
    embedding       vector(1024),
    embedding_model text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (artifact_id, ord)
);

-- Прогон индексации репозитория на конкретном коммите. Индексы неизменяемы:
-- новый коммит = новый прогон, старый остаётся, пока его не вычистят.
CREATE TABLE repo_index_runs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    commit_sha    text NOT NULL,
    state         text NOT NULL DEFAULT 'queued'
                      CHECK (state IN ('queued', 'running', 'completed', 'failed')),
    files_indexed integer NOT NULL DEFAULT 0,
    chunks_indexed integer NOT NULL DEFAULT 0,
    started_at    timestamptz,
    finished_at   timestamptz,
    error         text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repository_id, commit_sha)
);

CREATE TABLE code_files (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    index_run_id  uuid NOT NULL REFERENCES repo_index_runs(id) ON DELETE CASCADE,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    path          text NOT NULL,
    lang          text,
    size_bytes    bigint NOT NULL DEFAULT 0,
    -- содержимое файла в общем контент-адресуемом хранилище
    blob_sha      char(64) REFERENCES blobs(sha256),
    UNIQUE (index_run_id, path)
);

CREATE TABLE code_chunks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code_file_id    uuid NOT NULL REFERENCES code_files(id) ON DELETE CASCADE,
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    ord             integer NOT NULL,
    start_line      integer NOT NULL,
    end_line        integer NOT NULL,
    -- ближайший объемлющий символ, чтобы выдача в агента была осмысленной
    symbol_fqn      text,
    text            text NOT NULL,
    token_count     integer,
    embedding       vector(1024),
    embedding_model text,
    UNIQUE (code_file_id, ord)
);

-- Символьный граф. Извлекается tree-sitter'ом при индексации.
CREATE TABLE code_symbols (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code_file_id  uuid NOT NULL REFERENCES code_files(id) ON DELETE CASCADE,
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    kind          text NOT NULL,
    name          text NOT NULL,
    -- полное имя: 'internal/workflow.Engine.Advance'
    fqn           text NOT NULL,
    signature     text,
    doc           text,
    start_line    integer NOT NULL,
    end_line      integer NOT NULL,
    exported      boolean NOT NULL DEFAULT false
);

-- Рёбра графа. dst_symbol_id заполняется, когда цель разрешилась внутри
-- индекса; dst_fqn остаётся всегда — он держит связи между репозиториями,
-- которые невозможно разрешить локально.
CREATE TABLE code_edges (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    src_symbol_id uuid NOT NULL REFERENCES code_symbols(id) ON DELETE CASCADE,
    dst_symbol_id uuid REFERENCES code_symbols(id) ON DELETE CASCADE,
    dst_fqn       text NOT NULL,
    kind          text NOT NULL CHECK (kind IN (
                      'imports', 'calls', 'implements', 'extends',
                      'references', 'http_calls', 'publishes', 'subscribes'))
);

CREATE INDEX ON artifact_chunks (project_id);
CREATE INDEX ON artifact_chunks (task_id);
CREATE INDEX artifact_chunks_embedding_hnsw
    ON artifact_chunks USING hnsw (embedding vector_cosine_ops);

CREATE INDEX ON code_files (repository_id, path);
CREATE INDEX ON code_chunks (project_id);
CREATE INDEX code_chunks_embedding_hnsw
    ON code_chunks USING hnsw (embedding vector_cosine_ops);

CREATE INDEX ON code_symbols (repository_id, fqn);
CREATE INDEX ON code_symbols (code_file_id);
CREATE INDEX ON code_edges (src_symbol_id);
CREATE INDEX ON code_edges (dst_symbol_id);
CREATE INDEX ON code_edges (repository_id, dst_fqn);
