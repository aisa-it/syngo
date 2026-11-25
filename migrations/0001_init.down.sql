-- 0001_init.down.sql — полный откат базовой схемы (порядок обратен зависимостям).

DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS room_state;
DROP TABLE IF EXISTS room_memberships;
DROP TABLE IF EXISTS events;
DROP SEQUENCE IF EXISTS events_stream_ordering_seq;
DROP TABLE IF EXISTS room_aliases;
DROP TABLE IF EXISTS rooms;
DROP TABLE IF EXISTS access_tokens;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS users;
