-- Полнотекстовый поиск по m.room.message: tsvector по body с GIN-индексом.
ALTER TABLE events ADD COLUMN IF NOT EXISTS body_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('simple', coalesce(content->>'body', ''))) STORED;

CREATE INDEX IF NOT EXISTS events_body_tsv_idx ON events USING gin (body_tsv);
