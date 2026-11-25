-- 0037_sso_external_ids: вход через OIDC/SSO.
--
-- user_external_ids - привязка учетки провайдера SSO к учетной записи, как одноименная
-- таблица Synapse: auth_provider - idp_id с префиксом oidc-, external_id - subject
-- (claim sub). Переносится из Synapse (export.sql, syngo-import), чтобы после переезда
-- пользователи SSO входили в те же учетные записи.
CREATE TABLE IF NOT EXISTS user_external_ids (
    auth_provider text NOT NULL,
    external_id   text NOT NULL,
    user_id       text NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (auth_provider, external_id)
);

CREATE INDEX IF NOT EXISTS user_external_ids_user_idx ON user_external_ids (user_id);

-- login_tokens - одноразовые короткоживущие loginToken (m.login.token): выдаются после
-- успешного SSO и обмениваются на сессию в POST /login. Хранится только SHA-256.
CREATE TABLE IF NOT EXISTS login_tokens (
    token_hash    bytea PRIMARY KEY,
    user_id       text NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    auth_provider text NOT NULL DEFAULT '',
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS login_tokens_expires_idx ON login_tokens (expires_at);
