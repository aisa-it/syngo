DROP INDEX IF EXISTS transactions_room_idx;

-- Блокировки уже удаленных комнат не пережили бы возврат внешнего ключа.
DELETE FROM room_blocks b WHERE NOT EXISTS (SELECT 1 FROM rooms r WHERE r.room_id = b.room_id);
ALTER TABLE room_blocks
    ADD CONSTRAINT room_blocks_room_id_fkey FOREIGN KEY (room_id) REFERENCES rooms (room_id) ON DELETE CASCADE;
