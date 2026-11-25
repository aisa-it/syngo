CREATE TABLE IF NOT EXISTS room_keys_versions (
    user_id    text NOT NULL,
    version    text NOT NULL,
    algorithm  text NOT NULL,
    auth_data  jsonb NOT NULL DEFAULT '{}'::jsonb,
    etag       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, version)
);

CREATE TABLE IF NOT EXISTS room_keys_current (
    user_id text PRIMARY KEY,
    version text NOT NULL
);

CREATE TABLE IF NOT EXISTS room_keys (
    user_id    text NOT NULL,
    version    text NOT NULL,
    room_id    text NOT NULL,
    session_id text NOT NULL,
    key        jsonb NOT NULL,
    PRIMARY KEY (user_id, version, room_id, session_id)
);
