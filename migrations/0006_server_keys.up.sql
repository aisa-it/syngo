-- 0006_server_keys: кэш публичных ключей чужих серверов (ADR-0007 §3).
-- Свои ключи в БД не храним: приватный ключ живёт только в файле (chmod 600).

CREATE TABLE server_signing_keys (
    server_name    text   NOT NULL,
    key_id         text   NOT NULL,
    verify_key     text   NOT NULL,
    valid_until_ts bigint NOT NULL DEFAULT 0,
    fetched_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (server_name, key_id)
);

COMMENT ON TABLE server_signing_keys IS 'Кэш ed25519-ключей чужих серверов для проверки X-Matrix';
