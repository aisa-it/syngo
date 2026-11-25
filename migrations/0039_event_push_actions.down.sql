ALTER TABLE receipts DROP COLUMN IF EXISTS event_stream;
DROP TABLE IF EXISTS event_push_backfill;
DROP TABLE IF EXISTS event_push_stream;
DROP TABLE IF EXISTS event_push_actions;
