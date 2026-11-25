-- 0019_token_scope: отдельный scope у access-токена (ADR-0008 п.2).
-- Пустая строка — обычный пользовательский токен; 'admin' / 'admin:read' — admin-контур.
-- Admin-токен не принимается в Client-Server API и наоборот (проверка в middleware).
ALTER TABLE access_tokens
    ADD COLUMN scope text NOT NULL DEFAULT '';

CREATE INDEX access_tokens_scope_idx ON access_tokens (scope)
    WHERE scope <> '';
