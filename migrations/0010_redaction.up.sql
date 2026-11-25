-- redacted_by — событие m.room.redaction, скрывшее content.
-- Оригинальный content не сохраняем: redaction применяется сразу, как в клиентском
-- представлении Synapse (расхождение зафиксировано в docs/api-scope.md).
ALTER TABLE events ADD COLUMN IF NOT EXISTS redacted_by text;

CREATE INDEX IF NOT EXISTS events_redacted_by_idx ON events (room_id, redacted_by)
    WHERE redacted_by IS NOT NULL;
