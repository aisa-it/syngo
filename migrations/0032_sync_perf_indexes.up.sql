-- 0032_sync_perf_indexes: индексы под /sync и запись событий.
--
-- Замеры на синтетике 1,1 млн событий, 200 тыс. изменений ключей (EXPLAIN ANALYZE):
--   * SyncPosition - max(stream_ordering) по events/device_inbox/device_lists_changes
--     без индекса по одной колонке: три Seq Scan, 190 мс на каждый /sync;
--   * safePosition (stream_ordering = ANY) - Seq Scan по events, 235 мс;
--   * изменения ключей после since - Seq Scan по device_lists_changes;
--   * GIN events_prev_events_idx не используется ни одним запросом (поиск по
--     prev_events нигде не делается), но обновляется на каждую вставку события.
--
-- CONCURRENTLY здесь нельзя: golang-migrate выполняет файл одним запросом, то есть
-- в одной (неявной) транзакции, а CREATE INDEX CONCURRENTLY в транзакции запрещен.
-- Обычный CREATE INDEX держит SHARE-блокировку на events (чтение работает, запись
-- ждет) на время построения - секунды на миллион строк. Для большой боевой базы
-- индексы можно построить заранее вручную теми же именами с CONCURRENTLY, тогда
-- IF NOT EXISTS превращает эту миграцию в мгновенную.
CREATE INDEX IF NOT EXISTS events_stream_ordering_idx ON events (stream_ordering);
CREATE INDEX IF NOT EXISTS device_inbox_stream_idx ON device_inbox (stream_ordering);
CREATE INDEX IF NOT EXISTS device_lists_changes_pos_idx ON device_lists_changes (stream_ordering);

DROP INDEX IF EXISTS events_prev_events_idx;
