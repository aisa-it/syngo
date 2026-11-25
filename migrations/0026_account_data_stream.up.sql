-- Позиция account data в общем потоке /sync: без нее запись,
-- пришедшая между двумя /sync, не сдвигала позицию, и следующий long-poll ждал
-- полный таймаут. Element ждет каждый секрет восстановления через /sync - настройка
-- ключа восстановления растягивалась на минуты.
ALTER TABLE account_data
    ADD COLUMN IF NOT EXISTS stream_ordering bigint NOT NULL DEFAULT nextval('events_stream_ordering_seq');
CREATE INDEX IF NOT EXISTS account_data_stream_idx ON account_data (stream_ordering);
