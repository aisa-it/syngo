-- 0005_device_inbox: to-device сообщения и изменения ключей устройств.
--
-- To-device не попадает в timeline/DAG: это отдельная очередь доставки.
-- stream_ordering берём из общего монотонного счётчика (как у событий), чтобы /sync
-- мог одним числом описать «докуда доставлено»; позиция синхронизации — максимум по
-- events, device_inbox и device_lists_changes (см. store.SyncPosition).

CREATE TABLE device_inbox (
    id              bigserial PRIMARY KEY,
    user_id         text   NOT NULL,
    device_id       text   NOT NULL,
    event_type      text   NOT NULL,
    sender          text   NOT NULL,
    content         jsonb  NOT NULL,
    stream_ordering bigint NOT NULL DEFAULT nextval('events_stream_ordering_seq'),
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Основной запрос: «что не доставлено этому устройству после позиции since».
CREATE INDEX device_inbox_device_idx ON device_inbox (user_id, device_id, stream_ordering);

COMMENT ON TABLE device_inbox IS 'Очередь to-device сообщений: удаляется после выдачи в /sync';

-- Изменения ключей устройств для GET /keys/changes.
CREATE TABLE device_lists_changes (
    id              bigserial PRIMARY KEY,
    user_id         text   NOT NULL,
    device_id       text   NOT NULL,
    stream_ordering bigint NOT NULL DEFAULT nextval('events_stream_ordering_seq'),
    changed_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX device_lists_changes_stream_idx ON device_lists_changes (user_id, stream_ordering);

-- Кросс-подписывающие ключи пользователя (POST /keys/device_signing/upload).
-- Храним присланный объект целиком: сервер не проверяет криптографию, только раздаёт.
CREATE TABLE cross_signing_keys (
    user_id    text        NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    key_data   jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id)
);

-- Подписи устройств/мастер-ключей (POST /keys/signatures/upload).
CREATE TABLE device_signatures (
    user_id    text        NOT NULL,
    key_id     text        NOT NULL,
    signature  jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key_id)
);
