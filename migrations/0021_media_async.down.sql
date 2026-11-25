ALTER TABLE media DROP CONSTRAINT IF EXISTS media_storage_key_uploaded;
ALTER TABLE media DROP COLUMN IF EXISTS uploaded;
