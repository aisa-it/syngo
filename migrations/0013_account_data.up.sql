-- Account data: глобальные и комнатные настройки клиента (m.direct и др.).
CREATE TABLE IF NOT EXISTS account_data (
    user_id  text NOT NULL,
    room_id  text NOT NULL DEFAULT '',
    type     text NOT NULL,
    content  jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, room_id, type)
);
