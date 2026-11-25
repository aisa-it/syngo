CREATE TABLE cross_signing_keys (
    user_id    text        NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    key_data   jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id)
);

CREATE TABLE device_signatures (
    user_id    text        NOT NULL,
    key_id     text        NOT NULL,
    signature  jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key_id)
);

INSERT INTO cross_signing_keys (user_id, key_data)
SELECT user_id, jsonb_object_agg(key_type || '_key', key_data)
FROM e2e_cross_signing_keys GROUP BY user_id;

DROP TABLE e2e_signatures;
DROP TABLE e2e_cross_signing_keys;
