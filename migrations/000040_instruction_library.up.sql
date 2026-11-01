-- 000040_instruction_library: библиотека инструкций проекта и закрепление
-- документа версии роли за версией инструкции (CLAB-351, решение 0069).
--
-- Инструкция — общее правило проекта (git, self-hosted, формат отчёта),
-- которое роли берут версией. Её версии иммутабельны, как версии роли
-- (0016): новый текст — новая строка, а роли переходят на неё только новой
-- версией роли. Плавающей ссылки «последняя версия» нет (0068, п. 2): текст
-- под идущим этапом не меняется.
--
-- Ключ — имя документа в ролях без «.md» (<ключ>.md), поэтому форма — та
-- же, что у имени документа роли (000038), без точки: 1 + до 60 символов, и
-- «<ключ>.md» укладывается в 64. Ключ не меняется: по нему инструкцию
-- узнают связка и отбор документов на статусе (000039).
CREATE TABLE instructions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key        text NOT NULL
        CONSTRAINT instructions_key_shape CHECK (key ~ '^[a-z0-9][a-z0-9_-]{0,60}$'),
    name       text NOT NULL CHECK (btrim(name) <> ''),
    summary    text,
    created_by uuid REFERENCES principals(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, key)
);

-- Версия инструкции: байты в blobs по sha256, как документ роли (0064,
-- 000038); тип и changelog — на версии. Номер выдаётся из максимума в
-- транзакции заведения. UNIQUE (id, blob_sha) — цель составного внешнего
-- ключа закрепления: схема сама держит «закреплённый документ — те же байты».
CREATE TABLE instruction_versions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instruction_id uuid NOT NULL REFERENCES instructions(id) ON DELETE CASCADE,
    version        integer NOT NULL CHECK (version >= 1),
    kind           text NOT NULL,
    blob_sha       char(64) NOT NULL REFERENCES blobs(sha256),
    changelog      text,
    created_by     uuid REFERENCES principals(id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (instruction_id, version),
    UNIQUE (id, blob_sha)
);

-- Пятая ссылка на blobs: сборщик мусора спрашивает о ней у каждого
-- кандидата (blobRefsExist), а внешний ключ индекса не создаёт.
CREATE INDEX instruction_versions_blob_sha_idx ON instruction_versions (blob_sha);

-- Закрепление: NULL — обычный документ (все строки до этой миграции).
-- MATCH SIMPLE пропускает строку с NULL в любой колонке ключа, поэтому
-- незакреплённые документы ссылкой не проверяются. Удаления без каскада:
-- версия инструкции уходит только вместе с проектом, а с ним — и роли.
ALTER TABLE role_version_documents ADD COLUMN instruction_version_id uuid;
ALTER TABLE role_version_documents ADD CONSTRAINT role_version_documents_pin_fkey
    FOREIGN KEY (instruction_version_id, blob_sha)
    REFERENCES instruction_versions (id, blob_sha);

-- «Где используется» и отставание ролей ищут документы по версии
-- инструкции; незакреплённых строк в индексе нет.
CREATE INDEX role_version_documents_pin_idx ON role_version_documents (instruction_version_id)
    WHERE instruction_version_id IS NOT NULL;
