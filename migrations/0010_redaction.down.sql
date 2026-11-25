DROP INDEX IF EXISTS events_redacted_by_idx;
ALTER TABLE events DROP COLUMN IF EXISTS redacted_by;
