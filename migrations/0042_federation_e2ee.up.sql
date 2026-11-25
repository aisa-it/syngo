-- 0044_federation_e2ee: ключи и устройства пользователей других серверов,
-- поток изменений своих device lists для федерации и дедуп входящих
-- to-device.
--
-- Кэш удаленных устройств ведется только для пользователей, которые делят
-- комнату с нашими (их сервер шлет нам m.device_list_update). Строка в
-- remote_device_lists - пользователь отслеживается, кэш авторитетен; stream_id -
-- последний примененный stream_id его сервера; stale - кэш нужно пересинхронизировать
-- через /_matrix/federation/v1/user/devices (дыра в prev_id, сбой ресинка).
CREATE TABLE IF NOT EXISTS remote_device_lists (
    user_id    text        PRIMARY KEY,
    stream_id  bigint      NOT NULL DEFAULT 0,
    stale      boolean     NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS remote_devices (
    user_id      text        NOT NULL REFERENCES remote_device_lists (user_id) ON DELETE CASCADE,
    device_id    text        NOT NULL,
    display_name text,
    key_data     jsonb,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, device_id)
);

-- Ключи кросс-подписи отслеживаемых удаленных пользователей (master, self_signing):
-- в /keys/query отдаются вместе с локальными (e2e_cross_signing_keys ссылается на users).
CREATE TABLE IF NOT EXISTS remote_cross_signing_keys (
    user_id    text        NOT NULL REFERENCES remote_device_lists (user_id) ON DELETE CASCADE,
    key_type   text        NOT NULL CHECK (key_type IN ('master', 'self_signing')),
    key_id     text        NOT NULL,
    key_data   jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key_type)
);

-- Поток изменений device list своих пользователей: stream_id последнего
-- m.device_list_update (prev_id следующего) и /user/devices.
CREATE TABLE IF NOT EXISTS device_lists_outbound (
    user_id   text   PRIMARY KEY,
    stream_id bigint NOT NULL
);

-- Дедуп входящих m.direct_to_device по (origin, message_id): повтор EDU в другой
-- транзакции не доставляется устройству дважды.
CREATE TABLE IF NOT EXISTS device_federation_inbox (
    origin      text        NOT NULL,
    message_id  text        NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (origin, message_id)
);

-- Членство пользователя по room_state (удаленные участники из /send есть только там).
CREATE INDEX IF NOT EXISTS room_state_member_key_idx ON room_state (state_key) WHERE type = 'm.room.member';
