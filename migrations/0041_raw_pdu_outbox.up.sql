-- 0041: исходный PDU событий и исходящая очередь федерации без перекодирования.
--
-- events.pdu_json (0033) теперь заполняется для всех событий: свое - подписанный
-- PDU, как мы его подписали при создании; чужое - PDU, как он пришел (/send,
-- send_join, invite, send_leave, get_missing_events, ответ send_join). Федерация
-- (/event, /state, /backfill, /get_missing_events, send_join, воркер outbox)
-- отдает ровно его; события без pdu_json (до 0041, импорт без json) собираются
-- из колонок.
COMMENT ON COLUMN events.pdu_json IS
    'исходный PDU: свое событие - как подписано при создании, чужое - как пришло; NULL - собирается из колонок';

-- federation_outbox.pdu_json и federation_outbox_dead.pdu_json (готовый PDU
-- вместо ссылки event_id, 0040): jsonb переупорядочивал ключи, менял запись чисел
-- и не принимал \u0000. Текст хранит PDU байт-в-байт. room_id (0040) у старых
-- строк проставляется из PDU: purge комнаты удаляет по нему, без разбора текста.
UPDATE federation_outbox SET room_id = pdu_json->>'room_id' WHERE room_id IS NULL AND pdu_json IS NOT NULL;
UPDATE federation_outbox_dead SET room_id = pdu_json->>'room_id' WHERE room_id IS NULL AND pdu_json IS NOT NULL;

ALTER TABLE federation_outbox ALTER COLUMN pdu_json TYPE text USING pdu_json::text;
ALTER TABLE federation_outbox_dead ALTER COLUMN pdu_json TYPE text USING pdu_json::text;
