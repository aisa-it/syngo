-- 0036_rooms_room_id_v12: room_id комнат версии 12.
--
-- В v12 room_id - «!» и reference hash события m.room.create (43 символа
-- URL-safe base64 без дополнения), серверной части нет. Прежний CHECK требовал
-- «!...:сервер», и комнаты v12 из Synapse не импортировались.
ALTER TABLE rooms DROP CONSTRAINT IF EXISTS rooms_room_id_format;
ALTER TABLE rooms ADD CONSTRAINT rooms_room_id_format
    CHECK (room_id ~ '^!.+:.+$' OR room_id ~ '^![A-Za-z0-9_-]{43}$');
