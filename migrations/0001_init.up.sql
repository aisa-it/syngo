-- 0001_init.up.sql — базовая схема Matrix-homeserver'а.
-- Референс поведения и хранения: Synapse, full_schemas/72/full.sql.postgres + delta-миграции.
-- Отличия и причины — docs/adr/0003-db-schema.md.
--
-- Соглашения:
--   * все идентификаторы Matrix (user_id, room_id, event_id, alias) — text как есть, без нормализации;
--   * время — timestamptz (UTC), кроме origin_server_ts (ms since epoch, как в протоколе);
--   * JSON-содержимое событий — jsonb (в Synapse text, разбор на каждом чтении);
--   * секреты (пароли, токены) — только хеши.

-- Пользователи локального сервера.
CREATE TABLE users (
    user_id       text PRIMARY KEY,
    localpart     text NOT NULL,
    password_hash text NOT NULL,
    display_name  text NOT NULL DEFAULT '',
    avatar_url    text,
    is_admin      boolean NOT NULL DEFAULT false,
    deactivated   boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_user_id_format CHECK (user_id ~ '^@[^:]+:.+$'),
    CONSTRAINT users_localpart_format CHECK (localpart <> ''),
    CONSTRAINT users_localpart_unique UNIQUE (localpart)
);

COMMENT ON COLUMN users.password_hash IS 'argon2id PHC-строка ($argon2id$v=19$...)';

-- Устройства (одно устройство = один клиент, ключ E2EE привязан к устройству).
CREATE TABLE devices (
    user_id       text NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    device_id     text NOT NULL,
    display_name  text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_ip  text,
    last_seen_at  timestamptz,
    PRIMARY KEY (user_id, device_id)
);

-- Access/refresh токены. Хранится только хеш: утечка БД не даёт войти.
CREATE TABLE access_tokens (
    id               bigserial PRIMARY KEY,
    token_hash       bytea NOT NULL,
    user_id          text NOT NULL,
    device_id        text NOT NULL,
    refresh_hash     bytea,
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz,
    last_validated_at timestamptz,
    revoked_at       timestamptz,
    CONSTRAINT access_tokens_token_hash_unique UNIQUE (token_hash),
    CONSTRAINT access_tokens_refresh_hash_unique UNIQUE (refresh_hash),
    CONSTRAINT access_tokens_device_fk FOREIGN KEY (user_id, device_id)
        REFERENCES devices (user_id, device_id) ON DELETE CASCADE
);

CREATE INDEX access_tokens_user_idx ON access_tokens (user_id) WHERE revoked_at IS NULL;

-- Комнаты.
CREATE TABLE rooms (
    room_id      text PRIMARY KEY,
    room_version text NOT NULL DEFAULT '10',
    creator      text NOT NULL,
    is_public    boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT rooms_room_id_format CHECK (room_id ~ '^!.+:.+$')
);

-- Алиасы комнат (уникальны глобально в пределах сервера).
CREATE TABLE room_aliases (
    room_alias text PRIMARY KEY,
    room_id    text NOT NULL REFERENCES rooms (room_id) ON DELETE CASCADE,
    creator    text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT room_aliases_format CHECK (room_alias ~ '^#[^:]+:.+$')
);

CREATE INDEX room_aliases_room_idx ON room_aliases (room_id);

-- Порядок поступления событий на сервер: монотонный, сквозной для всех комнат.
-- /sync и /messages отдают события именно в stream_ordering (см. ADR-0003).
CREATE SEQUENCE events_stream_ordering_seq AS bigint;

-- События DAG. state_key = NULL у обычных сообщений, задан у state-событий.
CREATE TABLE events (
    event_id         text PRIMARY KEY,
    room_id          text NOT NULL REFERENCES rooms (room_id) ON DELETE CASCADE,
    type             text NOT NULL,
    sender           text NOT NULL,
    state_key        text,
    content          jsonb NOT NULL,
    prev_events      text[] NOT NULL DEFAULT '{}',
    auth_events      text[] NOT NULL DEFAULT '{}',
    depth            bigint NOT NULL,
    origin_server_ts bigint NOT NULL,
    origin           text NOT NULL,
    received_at      timestamptz NOT NULL DEFAULT now(),
    stream_ordering  bigint NOT NULL DEFAULT nextval('events_stream_ordering_seq'),
    rejected_reason  text
);

-- Главный индекс для /sync и /messages: выборка по комнате в stream-порядке.
CREATE INDEX events_room_stream_idx ON events (room_id, stream_ordering);
-- История по DAG (топологический порядок) и выборка соседей.
CREATE INDEX events_room_depth_idx ON events (room_id, depth);
-- Материализация state: поиск текущего события по (type, state_key).
CREATE INDEX events_room_state_idx ON events (room_id, type, state_key) WHERE state_key IS NOT NULL;
-- Обход DAG: поиск событий по prev_events.
CREATE INDEX events_prev_events_idx ON events USING gin (prev_events);

COMMENT ON COLUMN events.stream_ordering IS 'сквозной порядок поступления; /sync since-токен';
COMMENT ON COLUMN events.depth IS 'топологическая глубина в DAG (max(prev.depth)+1)';

-- История membership (как в Synapse room_memberships). user_id без FK: федерация приносит
-- удалённых пользователей, которых в users не будет.
CREATE TABLE room_memberships (
    event_id     text PRIMARY KEY REFERENCES events (event_id) ON DELETE CASCADE,
    room_id      text NOT NULL REFERENCES rooms (room_id) ON DELETE CASCADE,
    user_id      text NOT NULL,
    sender       text NOT NULL,
    membership   text NOT NULL,
    display_name text,
    avatar_url   text,
    forgotten    boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT room_memberships_kind CHECK (membership IN ('join', 'invite', 'leave', 'ban', 'knock'))
);

CREATE INDEX room_memberships_user_idx ON room_memberships (user_id, room_id);
CREATE INDEX room_memberships_room_idx ON room_memberships (room_id, membership);

-- Материализованное текущее состояние комнаты (то, что Synapse держит в
-- current_state_events + state_groups_state). Пересчитывается при записи state-события.
CREATE TABLE room_state (
    room_id   text NOT NULL REFERENCES rooms (room_id) ON DELETE CASCADE,
    type      text NOT NULL,
    state_key text NOT NULL,
    event_id  text NOT NULL REFERENCES events (event_id) ON DELETE CASCADE,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, type, state_key)
);

CREATE INDEX room_state_event_idx ON room_state (event_id);

-- Идемпотентность клиентских PUT-ов с txn_id (отправка событий, to-device, redaction).
CREATE TABLE transactions (
    user_id     text NOT NULL,
    device_id   text NOT NULL,
    endpoint    text NOT NULL,
    txn_id      text NOT NULL,
    room_id     text,
    event_id    text REFERENCES events (event_id) ON DELETE SET NULL,
    inserted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, device_id, endpoint, txn_id)
);

CREATE INDEX transactions_event_idx ON transactions (event_id) WHERE event_id IS NOT NULL;
