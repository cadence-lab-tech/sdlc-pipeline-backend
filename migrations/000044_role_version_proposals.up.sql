-- 000044_role_version_proposals: предложение новой версии роли участником
-- уровня agent, которое утверждает сопровождающий (CLAB-381, решение 0072,
-- п. 5).
--
-- Отдельная таблица, а не состояние в role_versions: последняя версия роли —
-- действующая, на этом стоят захват, перепроверка, связка и «обновить в
-- ролях». Черновик в role_versions пришлось бы отфильтровывать во всех этих
-- местах, и одна пропущенная проверка выдала бы его в работу. Утверждение
-- заводит версию тем же путём, что и все (Authoring.addVersion).
--
-- Поля версии — те же, что у role_versions, с теми же значениями по
-- умолчанию: утверждение переносит их в версию как есть. Уникального «одно
-- ожидающее на роль» нет: предложений от разных авторов несколько, они
-- конкурируют за одну базу, и выигрывает первое утверждённое — остальные
-- получают stale_role_base.
CREATE TABLE role_version_proposals (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    role_id         uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    -- Последняя версия роли на момент подачи: утверждение сверяет её с
    -- последней на момент решения («апрув того, что видел», 0042).
    base_version    integer NOT NULL CHECK (base_version >= 1),
    input_kinds     text[] NOT NULL DEFAULT '{}',
    output_contract jsonb NOT NULL DEFAULT '[]',
    tool_policy     jsonb NOT NULL DEFAULT '{}',
    model_hint      text,
    model_level     text
        CONSTRAINT role_version_proposals_model_level_check
        CHECK (model_level IN ('low', 'medium', 'high')),
    repo_scope      text[] NOT NULL DEFAULT '{}',
    context_budget  jsonb NOT NULL DEFAULT '{}',
    -- Обязателен, в отличие от версии: сопровождающий решает по нему, что
    -- и зачем меняется.
    changelog       text NOT NULL CHECK (btrim(changelog) <> ''),
    state           text NOT NULL DEFAULT 'pending'
        CONSTRAINT role_version_proposals_state_check
        CHECK (state IN ('pending', 'approved', 'rejected', 'withdrawn')),
    created_by      uuid NOT NULL REFERENCES principals(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    decided_by      uuid REFERENCES principals(id),
    decided_at      timestamptz,
    decision_note   text,
    result_version  integer,
    -- Решение и его след согласованы: ожидающее без решившего, решённое —
    -- с ним; номер версии — ровно у утверждённого; отклонение — с заметкой.
    CONSTRAINT role_version_proposals_decided CHECK (
        (state = 'pending' AND decided_by IS NULL AND decided_at IS NULL)
        OR (state <> 'pending' AND decided_by IS NOT NULL AND decided_at IS NOT NULL)),
    CONSTRAINT role_version_proposals_result CHECK (
        (state = 'approved') = (result_version IS NOT NULL)),
    CONSTRAINT role_version_proposals_reject_note CHECK (
        state <> 'rejected' OR btrim(coalesce(decision_note, '')) <> '')
);

-- Список предложений роли с фильтром по состоянию, новые первыми.
CREATE INDEX role_version_proposals_role_idx
    ON role_version_proposals (role_id, state, created_at);

-- Документы предложения — та же форма, что role_version_documents (000038,
-- 000040): имя, тип, позиция, байты в blobs и закрепление за версией
-- инструкции. Ссылка на библиотеку разрешается при подаче: утверждающий
-- видит те байты, которые станут версией, и новая версия инструкции после
-- подачи на них не влияет. Составной ключ закрепления держит «закреплённый
-- документ — те же байты», как у версии роли.
CREATE TABLE role_version_proposal_documents (
    proposal_id            uuid NOT NULL REFERENCES role_version_proposals(id) ON DELETE CASCADE,
    position               integer NOT NULL CHECK (position >= 0),
    name                   text NOT NULL
        CONSTRAINT role_version_proposal_documents_name_shape
        CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,60}\.md$'),
    kind                   text NOT NULL,
    blob_sha               char(64) NOT NULL REFERENCES blobs(sha256),
    instruction_version_id uuid,
    PRIMARY KEY (proposal_id, position),
    UNIQUE (proposal_id, name),
    CONSTRAINT role_version_proposal_documents_pin_fkey
        FOREIGN KEY (instruction_version_id, blob_sha)
        REFERENCES instruction_versions (id, blob_sha)
);

-- Шестая ссылка на blobs: сборщик мусора спрашивает о ней у каждого
-- кандидата (blobRefsExist), а внешний ключ индекса не создаёт. Блоб
-- предложения живёт, пока живо предложение в любом состоянии.
CREATE INDEX role_version_proposal_documents_blob_sha_idx
    ON role_version_proposal_documents (blob_sha);
