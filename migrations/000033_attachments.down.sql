-- Откат 000033: отметки вложения больше нет. Вложения остаются строками
-- artifacts без прогона с техническим kind — удалять загруженное откат не
-- вправе; откат версии с вложениями — вместе с базой из бэкапа.
DROP INDEX IF EXISTS artifacts_attachments;
ALTER TABLE artifacts
    DROP CONSTRAINT IF EXISTS artifacts_file_name_len,
    DROP CONSTRAINT IF EXISTS artifacts_attachment_shape,
    DROP COLUMN IF EXISTS indexed_at,
    DROP COLUMN IF EXISTS media_type,
    DROP COLUMN IF EXISTS file_name,
    DROP COLUMN IF EXISTS is_attachment;
