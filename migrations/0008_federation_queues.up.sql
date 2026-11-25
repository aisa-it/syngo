-- 0008_federation_queues: исходящая/входящая очереди федерации и маршрутизация по комнатам
-- (ADR-0007 §5–§6).

CREATE TABLE federation_outbox (
    id              bigserial PRIMARY KEY,
    destination     text   NOT NULL,
    stream_ordering bigint NOT NULL DEFAULT nextval('events_stream_ordering_seq'),
    pdu_json        jsonb,
    edu_json        jsonb,
    queued_at       timestamptz NOT NULL DEFAULT now(),
    attempts        int    NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX federation_outbox_destination_idx
    ON federation_outbox (destination, next_attempt_at, id);

COMMENT ON TABLE federation_outbox IS 'Очередь исходящих федеративных транзакций: удаляется после 200 от получателя';

-- Дедупликация входящих транзакций: UNIQUE (origin, txn_id) переживает рестарт.
CREATE TABLE federation_inbox (
    origin      text NOT NULL,
    txn_id      text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (origin, txn_id)
);

-- Какие комнаты отправлять какому серверу (для выбора получателей).
CREATE TABLE destination_rooms (
    destination     text   NOT NULL,
    room_id         text   NOT NULL,
    stream_ordering bigint NOT NULL DEFAULT nextval('events_stream_ordering_seq'),
    PRIMARY KEY (destination, room_id)
);

CREATE INDEX destination_rooms_room_idx ON destination_rooms (room_id);
