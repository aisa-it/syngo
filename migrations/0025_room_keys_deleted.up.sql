-- Удаление версии бэкапа ключей (DELETE /room_keys/version/{version}):
-- версия помечается удаленной, а не стирается, чтобы номер не выдавался повторно.
-- etag - счетчик изменений ключей версии.
ALTER TABLE room_keys_versions ADD COLUMN IF NOT EXISTS deleted boolean NOT NULL DEFAULT false;
UPDATE room_keys_versions SET etag = '0' WHERE etag = '';
