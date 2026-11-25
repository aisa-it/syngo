-- Асинхронная загрузка медиа (MSC2246): placeholder до загрузки байтов.
ALTER TABLE media ADD COLUMN IF NOT EXISTS uploaded boolean NOT NULL DEFAULT true;
ALTER TABLE media DROP CONSTRAINT IF EXISTS media_storage_key_not_empty;
ALTER TABLE media ADD CONSTRAINT media_storage_key_uploaded CHECK (uploaded OR storage_key = '');
