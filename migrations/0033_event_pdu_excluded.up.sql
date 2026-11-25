-- 0033: исходный PDU событий и события, не попадающие в timeline.
--
-- pdu_json - исходный PDU текстом, как его хранит Synapse (event_json.json):
-- подписи, hashes, unsigned, origin, redacts и точный JSON для reference hash.
-- Текст, а не jsonb: в content бывает \u0000, который jsonb не принимает.
ALTER TABLE events ADD COLUMN IF NOT EXISTS pdu_json text;

-- excluded_events - события источника, которые не должны быть видны клиентам и
-- участвовать в состоянии: отклоненные (rejections Synapse), soft-failed и
-- outlier. Хранятся отдельно от events, поэтому ни один путь чтения timeline,
-- состояния, поиска и связей их не видит; PDU сохранен для auth chain и сверки.
CREATE TABLE IF NOT EXISTS excluded_events (
    event_id         text PRIMARY KEY,
    room_id          text NOT NULL REFERENCES rooms (room_id) ON DELETE CASCADE,
    type             text NOT NULL,
    sender           text NOT NULL,
    state_key        text,
    reason           text NOT NULL,
    rejected_reason  text,
    depth            bigint NOT NULL,
    origin_server_ts bigint NOT NULL,
    stream_ordering  bigint,
    pdu_json         text,
    imported_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT excluded_events_reason CHECK (reason IN ('rejected', 'soft_failed', 'outlier'))
);

CREATE INDEX IF NOT EXISTS excluded_events_room_idx ON excluded_events (room_id);
