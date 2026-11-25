DROP TRIGGER IF EXISTS room_state_groups_invalidate ON room_state;
DROP FUNCTION IF EXISTS room_state_groups_invalidate();
DROP TABLE IF EXISTS room_state_groups;
DROP TABLE IF EXISTS room_forward_extremities;
DROP TABLE IF EXISTS event_state_groups;
DROP TABLE IF EXISTS state_group_entries;
DROP TABLE IF EXISTS state_groups;
