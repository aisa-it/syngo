-- Прочитанные строки, которые еще не удалены фоном, до сводки не хранились.
DELETE FROM event_push_actions a
USING event_push_summary s
WHERE a.user_id = s.user_id AND a.room_id = s.room_id AND a.thread_id = s.thread_id
  AND a.stream_ordering <= s.read_upto;

DROP TABLE IF EXISTS event_push_summary;
