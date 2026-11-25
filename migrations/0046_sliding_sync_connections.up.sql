-- 0047: состояние соединений simplified sliding sync (MSC4186).
--
-- Соединение - (пользователь, устройство, conn_id) клиента. На каждую выданную
-- позицию pos хранится состояние: какие комнаты отданы клиенту и до какой
-- позиции, с какими timeline_limit и required_state, каких lazy-участников он уже
-- знает. По нему incremental отдает только изменения. Как в Synapse
-- (sliding_sync_connections, sliding_sync_connection_positions), но состояние
-- позиции - один документ jsonb, а не нормализованные строки.
--
-- Когда клиент приходит с позицией, остальные позиции соединения удаляются: он
-- подтвердил получение этой (повтор запроса с ней же остается возможным).
-- Соединения без запросов 7 дней удаляются при создании новых.
CREATE TABLE IF NOT EXISTS sliding_sync_connections (
    connection_key bigserial PRIMARY KEY,
    user_id        text NOT NULL,
    device_id      text NOT NULL,
    conn_id        text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_used_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS sliding_sync_connections_conn_idx
    ON sliding_sync_connections (user_id, device_id, conn_id);
CREATE INDEX IF NOT EXISTS sliding_sync_connections_used_idx ON sliding_sync_connections (last_used_at);

CREATE TABLE IF NOT EXISTS sliding_sync_connection_positions (
    connection_position bigserial PRIMARY KEY,
    connection_key      bigint NOT NULL REFERENCES sliding_sync_connections (connection_key) ON DELETE CASCADE,
    created_at          timestamptz NOT NULL DEFAULT now(),
    state               jsonb NOT NULL
);

CREATE INDEX IF NOT EXISTS sliding_sync_connection_positions_conn_idx
    ON sliding_sync_connection_positions (connection_key);

COMMENT ON TABLE sliding_sync_connections IS 'соединения simplified sliding sync (MSC4186): пользователь, устройство, conn_id';
COMMENT ON TABLE sliding_sync_connection_positions IS 'состояние соединения sliding sync на выданную позицию pos';
