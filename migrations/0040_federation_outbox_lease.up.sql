-- 0041_federation_outbox_lease: захват пачки outbox с арендой, стабильный txn_id
-- ретраев, изоляция ядовитых элементов и dead-letter, постановка
-- события в outbox в транзакции его записи.
--
-- event_id/room_id - ссылка на событие: PDU собирается и подписывается при
-- отправке, поэтому строку можно вставить в транзакции записи события.
-- txn_id - транзакция, в которую попал элемент: после сетевой ошибки или 5xx
-- пачка повторяется с тем же txn_id (получатель дедуплицирует), после 4xx
-- txn_id снимается. claimed_until - аренда на время отправки: другой инстанс
-- не берет пачку, пока аренда не истекла. isolate - элемент был в отвергнутой
-- пачке и отправляется отдельно, чтобы не блокировать остальные.
ALTER TABLE federation_outbox
    ADD COLUMN IF NOT EXISTS event_id      text,
    ADD COLUMN IF NOT EXISTS room_id       text,
    ADD COLUMN IF NOT EXISTS txn_id        text,
    ADD COLUMN IF NOT EXISTS claimed_until timestamptz,
    ADD COLUMN IF NOT EXISTS isolate       boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS last_error    text;

CREATE INDEX IF NOT EXISTS federation_outbox_ready_idx
    ON federation_outbox (destination, id) WHERE txn_id IS NULL;
CREATE INDEX IF NOT EXISTS federation_outbox_txn_idx
    ON federation_outbox (destination, txn_id) WHERE txn_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS federation_outbox_room_idx ON federation_outbox (room_id);

-- Элементы, которые не будут доставлены: предел попыток, срок, отказ получателя
-- по PDU, destination вне политики. Хранятся для разбора, повторно не шлются.
CREATE TABLE IF NOT EXISTS federation_outbox_dead (
    id              bigint      PRIMARY KEY,
    destination     text        NOT NULL,
    event_id        text,
    room_id         text,
    pdu_json        jsonb,
    edu_json        jsonb,
    attempts        int         NOT NULL,
    queued_at       timestamptz NOT NULL,
    failed_at       timestamptz NOT NULL DEFAULT now(),
    reason          text        NOT NULL
);

CREATE INDEX IF NOT EXISTS federation_outbox_dead_destination_idx
    ON federation_outbox_dead (destination, failed_at);
CREATE INDEX IF NOT EXISTS federation_outbox_dead_room_idx ON federation_outbox_dead (room_id);

COMMENT ON TABLE federation_outbox_dead IS 'Недоставленные элементы outbox федерации (dead-letter)';
