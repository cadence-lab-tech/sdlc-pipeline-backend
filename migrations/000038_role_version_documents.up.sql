-- 000038_role_version_documents: версия роли — упорядоченный набор
-- документов (CLAB-343, решение 0068, пп. 1 и 6).
--
-- Документ принадлежит версии роли и иммутабелен вместе с ней: правка одного
-- документа — новая версия роли со всем набором. Байты лежат в blobs по
-- sha256, как вложения (решение 0064): неизменённый документ новой версии и
-- одинаковый текст в двух ролях ссылаются на одну строку blobs. Размер и sha
-- — свойства байтов, их отдаёт blobs; имя, тип и позиция — на ссылке, потому
-- что блоб общий. Создатель и время — у версии роли.
--
-- Имя — имя файла: документ ложится файлом в каталог роли связки и в
-- --instructions-dir, поэтому только нижний регистр (macOS и Windows не
-- различают регистр), без «/» и без ведущей точки, не длиннее 64 символов:
-- 1 + до 60 + «.md». Тип — данные: список в сервере не закрыт, форму
-- проверяет сервис, а не схема.
CREATE TABLE role_version_documents (
    role_version_id uuid NOT NULL REFERENCES role_versions(id) ON DELETE CASCADE,
    position        integer NOT NULL CHECK (position >= 0),
    name            text NOT NULL
        CONSTRAINT role_version_documents_name_shape
        CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,60}\.md$'),
    kind            text NOT NULL,
    blob_sha        char(64) NOT NULL REFERENCES blobs(sha256),
    PRIMARY KEY (role_version_id, position),
    UNIQUE (role_version_id, name)
);

-- Четвёртая ссылка на blobs: сборщик мусора спрашивает о ней у каждого
-- кандидата (blobRefsExist), а внешний ключ индекса не создаёт (как 000022).
CREATE INDEX role_version_documents_blob_sha_idx ON role_version_documents (blob_sha);

-- Перенос: у каждой версии роли — один документ main.md, равный её
-- instructions байт в байт. Внешнее хранилище из SQL недоступно, поэтому
-- даже крупная инструкция ложится inline: blobs хранит inline любой размер.
-- Пустая instructions (сиды и ручные вставки — сервис её не пропускает)
-- даёт пустой блоб: миграция не падает ни на одной строке, а версия
-- читается так же, как до неё.
INSERT INTO blobs (sha256, size_bytes, media_type, storage, content)
SELECT DISTINCT ON (h.sha) h.sha, octet_length(s.content), 'text/markdown', 'inline', s.content
FROM (SELECT convert_to(instructions, 'UTF8') AS content FROM role_versions) s
CROSS JOIN LATERAL (SELECT encode(sha256(s.content), 'hex') AS sha) h
ON CONFLICT (sha256) DO NOTHING;

INSERT INTO role_version_documents (role_version_id, position, name, kind, blob_sha)
SELECT id, 0, 'main.md', 'instructions', encode(sha256(convert_to(instructions, 'UTF8')), 'hex')
FROM role_versions;
