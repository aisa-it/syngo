-- 0004_device_keys: E2EE-ключи устройств (P1-скоуп).
-- Сервер хранит то, что прислало устройство, и раздаёт это другим устройствам;
-- расшифровок и приватных ключей у сервера нет по модели Matrix.

-- Ключи устройства: одна строка на (user_id, device_id). Храним присланный объект
-- целиком (device_keys + signatures), чтобы не терять поля, которые мы не разбираем.
CREATE TABLE device_keys (
    user_id     text        NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    device_id   text        NOT NULL,
    key_data    jsonb       NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, device_id)
);

-- One-time keys: выдаются ровно один раз (claimed_at), потом не отдаются никогда.
CREATE TABLE one_time_keys (
    user_id    text        NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    device_id  text        NOT NULL,
    key_id     text        NOT NULL,
    algorithm  text        NOT NULL,
    key_data   jsonb       NOT NULL,
    claimed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, device_id, key_id),
    CONSTRAINT one_time_keys_device_fk FOREIGN KEY (user_id, device_id)
        REFERENCES devices (user_id, device_id) ON DELETE CASCADE
);

-- Выборка неиспользованных ключей устройства при /keys/claim.
CREATE INDEX one_time_keys_unclaimed_idx ON one_time_keys (user_id, device_id, algorithm)
    WHERE claimed_at IS NULL;

-- Fallback keys (MSC2732) в этом срезе не поддерживаются: поле в ответе отдаём пустым.
-- Миграция появится вместе с их поддержкой.
