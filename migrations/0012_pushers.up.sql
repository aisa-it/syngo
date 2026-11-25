-- Pushers: устройства доставки push-уведомлений.
CREATE TABLE IF NOT EXISTS pushers (
    user_id    text NOT NULL,
    app_id     text NOT NULL,
    pushkey    text NOT NULL,
    kind       text NOT NULL,
    app_display_name text NOT NULL DEFAULT '',
    device_display_name text NOT NULL DEFAULT '',
    profile_tag text NOT NULL DEFAULT '',
    lang       text NOT NULL DEFAULT '',
    data       jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, app_id, pushkey)
);

CREATE INDEX IF NOT EXISTS pushers_pushkey_idx ON pushers (pushkey);
