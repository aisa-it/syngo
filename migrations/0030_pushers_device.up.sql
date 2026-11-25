-- Привязка pusher к устройству: logout, удаление устройства и смена
-- пароля с выходом удаляют pushers этого устройства. Старые записи без
-- устройства (NULL) удаляются только при logout/all и деактивации.
ALTER TABLE pushers ADD COLUMN IF NOT EXISTS device_id text;

CREATE INDEX IF NOT EXISTS pushers_user_device_idx ON pushers (user_id, device_id);
