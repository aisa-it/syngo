-- 0048_events_state_stream_index: частичный индекс state-событий по позиции.
--
-- Incremental /sync читает изменения состояния комнат после since
-- (store.StateDeltaForRooms: room_id = ANY($1) AND stream_ordering > $2 AND
-- state_key IS NOT NULL). pgx с шестого вызова переходит на generic-план, и
-- планировщик брал events_room_state_idx (room_id, type, state_key): все
-- state-события комнат пользователя (в комнате на 450 участников - тысячи
-- событий членства) с фильтром по позиции. Это верхний запрос pg_stat_statements
-- профиля M: 16 мс на каждый /sync. С индексом читаются только state-события
-- после since: 0,3 мс против 5,6 мс на синтетике 500 пользователей и 200 комнат.
--
-- CONCURRENTLY нельзя (миграция - одна транзакция, см. 0032); для большой базы
-- индекс можно построить заранее вручную с тем же именем и CONCURRENTLY.
CREATE INDEX IF NOT EXISTS events_room_state_stream_idx
    ON events (room_id, stream_ordering)
    WHERE state_key IS NOT NULL;
