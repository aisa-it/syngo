-- 0048_event_push_summary: сводка счетчиков непрочитанного, аналог
-- event_push_summary Synapse.
--
-- Раньше счетчики /sync считались count(*) по строкам event_push_actions на
-- каждую сборку ответа, а receipt удалял прочитанные строки в пути запроса: в
-- комнате на 450 участников сообщение дает до 449 строк, и оба запроса росли с
-- числом непрочитанного. Теперь счетчики (пользователь, комната, тред) хранятся
-- готовыми: порция разбора их увеличивает в той же транзакции, что вставляет
-- действия, receipt поднимает позицию прочтения и пересчитывает только строки
-- выше нее, а прочитанные строки удаляются в фоне.
--
-- Инвариант: notif_count и highlight_count - число строк event_push_actions
-- того же ключа с stream_ordering > read_upto.
CREATE TABLE IF NOT EXISTS event_push_summary (
    user_id         text    NOT NULL,
    room_id         text    NOT NULL,
    -- thread_id - корень треда ('' - основная лента), как в event_push_actions.
    thread_id       text    NOT NULL DEFAULT '',
    notif_count     integer NOT NULL DEFAULT 0,
    highlight_count integer NOT NULL DEFAULT 0,
    -- read_upto - позиция прочтения ленты (receipt m.read/m.read.private или
    -- свое событие): строки не выше нее прочитаны и не считаются.
    read_upto       bigint  NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, room_id, thread_id)
);

-- До миграции прочитанные строки удалялись сразу: все текущие строки - непрочитанные.
INSERT INTO event_push_summary (user_id, room_id, thread_id, notif_count, highlight_count)
SELECT user_id, room_id, thread_id, count(*), count(*) FILTER (WHERE highlight)
FROM event_push_actions
GROUP BY user_id, room_id, thread_id
ON CONFLICT (user_id, room_id, thread_id) DO NOTHING;
