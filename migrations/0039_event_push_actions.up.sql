-- 0039_event_push_actions: хранимые действия push-правил и счетчики
-- непрочитанного, аналог event_push_actions Synapse.
--
-- Раньше unread_notifications считались на каждом /sync: до 100 событий на
-- комнату с вычислением push-правил (6,4 с initial sync у пользователя с 1000
-- комнат). Теперь правила вычисляются один раз после записи события
-- для каждого локального участника, строка хранится только для notify, а
-- прочитанное удаляется по receipt. Счетчики комнаты - count(*) по строкам
-- пользователя одним запросом.
CREATE TABLE IF NOT EXISTS event_push_actions (
    user_id         text    NOT NULL,
    room_id         text    NOT NULL,
    event_id        text    NOT NULL,
    stream_ordering bigint  NOT NULL,
    -- thread_id - корень треда ('' - основная лента), для receipts тредов.
    thread_id       text    NOT NULL DEFAULT '',
    highlight       boolean NOT NULL DEFAULT false,
    PRIMARY KEY (user_id, event_id)
);

CREATE INDEX IF NOT EXISTS event_push_actions_user_room_idx
    ON event_push_actions (user_id, room_id, thread_id, stream_ordering);
-- Удаление по redaction.
CREATE INDEX IF NOT EXISTS event_push_actions_event_idx ON event_push_actions (event_id);

-- Позиция обработки событий (одна строка): события с stream_ordering не выше
-- processed уже разобраны. backfill_upto - позиция на момент миграции: для
-- более старых событий действия досчитываются по пользователю при первом
-- обращении (event_push_backfill), как считались раньше.
CREATE TABLE IF NOT EXISTS event_push_stream (
    singleton     boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    processed     bigint  NOT NULL,
    backfill_upto bigint  NOT NULL
);

INSERT INTO event_push_stream (singleton, processed, backfill_upto)
SELECT true, coalesce(max(stream_ordering), 0), coalesce(max(stream_ordering), 0) FROM events
ON CONFLICT (singleton) DO NOTHING;

-- Пользователи, для которых действия до backfill_upto уже досчитаны.
CREATE TABLE IF NOT EXISTS event_push_backfill (
    user_id text PRIMARY KEY
);

-- Позиция события receipt: обработчик не создает действия для событий, которые
-- пользователь уже отметил прочитанными (receipt мог прийти раньше обработки).
ALTER TABLE receipts ADD COLUMN IF NOT EXISTS event_stream bigint;

UPDATE receipts r SET event_stream = e.stream_ordering
FROM events e
WHERE e.event_id = r.event_id AND r.event_stream IS NULL;
