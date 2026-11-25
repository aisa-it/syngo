-- Кросс-подписи по типам ключей и подписи по подписанту.
-- Раньше тело /keys/device_signing/upload лежало одним JSON на пользователя, а
-- /keys/signatures/upload складывал подписанные объекты целиком; /keys/query
-- ни то, ни другое не читал, поэтому Element считал свои устройства неподтвержденными.
CREATE TABLE e2e_cross_signing_keys (
    user_id    text        NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    key_type   text        NOT NULL CHECK (key_type IN ('master', 'self_signing', 'user_signing')),
    -- открытый ключ ed25519 (base64 без паддинга) - он же id ключа в /keys/signatures/upload
    key_id     text        NOT NULL,
    key_data   jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key_type)
);

-- Подпись ключа target (устройство или кросс-ключ) ключом signer.
CREATE TABLE e2e_signatures (
    target_user_id text        NOT NULL,
    target_key_id  text        NOT NULL,
    signer_user_id text        NOT NULL,
    signer_key_id  text        NOT NULL,
    signature      text        NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_user_id, target_key_id, signer_user_id, signer_key_id)
);

-- Перенос ключей из прежнего формата; старые подписи не переносятся (их нельзя
-- проверить без исходного объекта), клиент загрузит их заново.
INSERT INTO e2e_cross_signing_keys (user_id, key_type, key_id, key_data)
SELECT c.user_id, t.key_type,
       substr((SELECT k FROM jsonb_object_keys(c.key_data -> t.field -> 'keys') AS k LIMIT 1), 9),
       c.key_data -> t.field
FROM cross_signing_keys c
CROSS JOIN (VALUES ('master', 'master_key'), ('self_signing', 'self_signing_key'),
                   ('user_signing', 'user_signing_key')) AS t (key_type, field)
WHERE jsonb_typeof(c.key_data -> t.field -> 'keys') = 'object';

DROP TABLE cross_signing_keys;
DROP TABLE device_signatures;
