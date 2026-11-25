DROP INDEX IF EXISTS events_body_tsv_idx;
ALTER TABLE events DROP COLUMN IF EXISTS body_tsv;
