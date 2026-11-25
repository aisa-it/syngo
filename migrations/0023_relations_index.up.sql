-- 0023_relations_index: индекс под выборку /relations по m.relates_to.event_id.
-- Без него каждый запрос связей — Seq Scan по всем событиям комнаты.
CREATE INDEX IF NOT EXISTS events_relates_to_idx
    ON events ((content->'m.relates_to'->>'event_id'))
    WHERE content ? 'm.relates_to';
