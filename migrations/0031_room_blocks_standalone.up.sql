-- Блокировка комнаты переживает purge: раньше room_blocks ссылалась на
-- rooms с ON DELETE CASCADE, и удаление комнаты снимало блокировку. Без внешнего
-- ключа можно заблокировать и еще не известную комнату - как block в Synapse admin API.
ALTER TABLE room_blocks DROP CONSTRAINT IF EXISTS room_blocks_room_id_fkey;

-- Транзакции клиента ищутся по комнате при purge.
CREATE INDEX IF NOT EXISTS transactions_room_idx ON transactions (room_id) WHERE room_id IS NOT NULL;
