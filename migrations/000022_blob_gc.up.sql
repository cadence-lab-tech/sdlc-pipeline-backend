-- 000022_blob_gc: индексы под поиск блобов без ссылок (CLAB-11).
--
-- Сборщик мусора спрашивает у каждой строки blobs, ссылается ли на её sha256
-- хоть кто-нибудь. Ссылок в схеме ровно три — artifacts.blob_sha,
-- code_files.blob_sha, project_documents.blob_sha, — и ни по одной индекса не
-- было: внешний ключ индекса не создаёт. Без них каждый тик воркера
-- превращался бы в последовательный проход по code_files, самой большой
-- таблице схемы (файлы × удержанные прогоны × репозитории).
--
-- Индексы полезны не только сборщику: удаление строки blobs проверяет те же
-- внешние ключи, а удаление репозитория или проекта уносит ссылающиеся строки.

CREATE INDEX artifacts_blob_sha_idx ON artifacts (blob_sha);
CREATE INDEX code_files_blob_sha_idx ON code_files (blob_sha);
CREATE INDEX project_documents_blob_sha_idx ON project_documents (blob_sha);
