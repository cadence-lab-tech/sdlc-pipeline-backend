-- Откат 000022: поиск блобов без ссылок снова идёт последовательным проходом.

DROP INDEX project_documents_blob_sha_idx;
DROP INDEX code_files_blob_sha_idx;
DROP INDEX artifacts_blob_sha_idx;
