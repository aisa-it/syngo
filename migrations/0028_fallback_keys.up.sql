-- Fallback keys (спецификация «One-time and fallback keys»): один ключ на
-- (устройство, алгоритм). Выдается в /keys/claim, когда one-time keys кончились, и
-- остается доступен до замены устройством; used - выдавался ли ключ (по нему
-- клиент видит в device_unused_fallback_key_types, что пора загрузить новый).
-- Раньше fallback_keys из /keys/upload терялись, и после исчерпания one-time keys
-- открыть Olm-сессию с устройством было нельзя.
CREATE TABLE IF NOT EXISTS e2e_fallback_keys (
    user_id    text        NOT NULL,
    device_id  text        NOT NULL,
    algorithm  text        NOT NULL,
    key_id     text        NOT NULL,
    key_data   jsonb       NOT NULL,
    used       boolean     NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, device_id, algorithm),
    CONSTRAINT e2e_fallback_keys_device_fk FOREIGN KEY (user_id, device_id)
        REFERENCES devices (user_id, device_id) ON DELETE CASCADE
);
