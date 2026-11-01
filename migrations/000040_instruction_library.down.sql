-- Откат 000040: снимается только добавленное. Документы ролей остаются с
-- теми же байтами, теряя закрепление; блобы инструкций, на которые больше
-- никто не ссылается, уберёт сборщик мусора.
DROP INDEX IF EXISTS role_version_documents_pin_idx;
ALTER TABLE role_version_documents DROP CONSTRAINT IF EXISTS role_version_documents_pin_fkey;
ALTER TABLE role_version_documents DROP COLUMN IF EXISTS instruction_version_id;
DROP TABLE IF EXISTS instruction_versions;
DROP TABLE IF EXISTS instructions;
