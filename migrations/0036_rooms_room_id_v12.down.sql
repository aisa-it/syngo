-- Откат 0036: прежний формат room_id для новых комнат. NOT VALID - уже
-- перенесенные комнаты v12 не проверяются: откат схемы не должен падать на них и
-- не удаляет данные молча.
ALTER TABLE rooms DROP CONSTRAINT IF EXISTS rooms_room_id_format;
ALTER TABLE rooms ADD CONSTRAINT rooms_room_id_format CHECK (room_id ~ '^!.+:.+$') NOT VALID;
