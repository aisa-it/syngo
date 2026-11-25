-- 0038_thread_replies_index: индекс под список тредов комнаты.
-- GET /rooms/{roomId}/threads группирует ответы m.thread комнаты по корню; без
-- частичного индекса это проход по всем событиям комнаты. Запрос должен
-- содержать тот же предикат rel_type = 'm.thread', иначе индекс не выбирается.
CREATE INDEX IF NOT EXISTS events_thread_replies_idx
    ON events (room_id, stream_ordering)
    WHERE content->'m.relates_to'->>'rel_type' = 'm.thread';
