-- 0009_event_auth: auth-цепочка событий (ADR-0007 §4).
-- Храним ссылки event → auth_event, чтобы проверять авторизацию без разбора jsonb.

CREATE TABLE event_auth (
    event_id      text NOT NULL,
    auth_event_id text NOT NULL,
    PRIMARY KEY (event_id, auth_event_id)
);

CREATE INDEX event_auth_auth_idx ON event_auth (auth_event_id);
