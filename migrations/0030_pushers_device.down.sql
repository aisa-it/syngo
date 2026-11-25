DROP INDEX IF EXISTS pushers_user_device_idx;
ALTER TABLE pushers DROP COLUMN IF EXISTS device_id;
