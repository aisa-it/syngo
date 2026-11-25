CREATE INDEX IF NOT EXISTS events_prev_events_idx ON events USING gin (prev_events);

DROP INDEX IF EXISTS device_lists_changes_pos_idx;
DROP INDEX IF EXISTS device_inbox_stream_idx;
DROP INDEX IF EXISTS events_stream_ordering_idx;
