-- 0003_membership_unique: материализованное членство — одна актуальная строка на пару
-- комната-пользователь. В Synapse актуальное членство живёт в current_state_events, а
-- room_memberships хранит историю по event_id; у нас room_memberships — материализация,
-- поэтому нужен уникальный ключ для UPSERT.
--
-- Перед добавлением ключа убираем возможные дубли (оставляем строку с самым новым
-- событием по stream_ordering).
DELETE FROM room_memberships rm
WHERE EXISTS (
    SELECT 1
    FROM room_memberships newer
    JOIN events newer_event ON newer_event.event_id = newer.event_id
    LEFT JOIN events older_event ON older_event.event_id = rm.event_id
    WHERE rm.room_id = newer.room_id
      AND rm.user_id = newer.user_id
      AND rm.event_id <> newer.event_id
      AND (older_event.stream_ordering IS NULL
           OR newer_event.stream_ordering > older_event.stream_ordering)
);

ALTER TABLE room_memberships
    ADD CONSTRAINT room_memberships_room_user_key UNIQUE (room_id, user_id);
