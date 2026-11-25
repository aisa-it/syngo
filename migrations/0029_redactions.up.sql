-- redactions - связь m.room.redaction с целью, как одноименная таблица
-- Synapse. Нужна, чтобы применить redaction, пришедшую раньше цели, и хранить
-- оригинальный content цели отдельно от events: в events после redaction лежит
-- вырезанный content (его видят клиенты, поиск и связи), оригинал - здесь, до
-- истечения redaction.retention_period.
CREATE TABLE IF NOT EXISTS redactions (
    event_id         text PRIMARY KEY REFERENCES events (event_id) ON DELETE CASCADE,
    room_id          text NOT NULL,
    redacts          text NOT NULL,
    received_at      timestamptz NOT NULL DEFAULT now(),
    -- original_content - content цели до вырезания (только у примененной redaction).
    original_content jsonb,
    -- pruned - content цели в events уже вырезан этой redaction.
    pruned           boolean NOT NULL DEFAULT false
);

CREATE INDEX IF NOT EXISTS redactions_redacts_idx ON redactions (redacts);
CREATE INDEX IF NOT EXISTS redactions_original_idx ON redactions (received_at)
    WHERE original_content IS NOT NULL;
CREATE INDEX IF NOT EXISTS redactions_unpruned_idx ON redactions (received_at) WHERE NOT pruned;

-- Уже примененные redaction (до 0029 content вырезался только при чтении):
-- оригинал переносится сюда, вырезание в events делает syngod при старте.
INSERT INTO redactions (event_id, room_id, redacts, received_at, original_content, pruned)
SELECT t.redacted_by, t.room_id, t.event_id, r.received_at, t.content, false
FROM events t
JOIN events r ON r.event_id = t.redacted_by
WHERE t.redacted_by IS NOT NULL
ON CONFLICT (event_id) DO NOTHING;

-- Остальные redaction (цель неизвестна или скрыта другой redaction).
INSERT INTO redactions (event_id, room_id, redacts, received_at, pruned)
SELECT e.event_id, e.room_id, e.content->>'redacts', e.received_at, true
FROM events e
WHERE e.type = 'm.room.redaction' AND e.content->>'redacts' IS NOT NULL
ON CONFLICT (event_id) DO NOTHING;
