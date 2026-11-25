DELETE FROM room_keys k USING room_keys_versions v
WHERE v.deleted AND k.user_id = v.user_id AND k.version = v.version;
DELETE FROM room_keys_versions WHERE deleted;
ALTER TABLE room_keys_versions DROP COLUMN IF EXISTS deleted;
