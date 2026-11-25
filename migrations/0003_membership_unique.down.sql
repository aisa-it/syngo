-- Откат 0003_membership_unique.
ALTER TABLE room_memberships DROP CONSTRAINT IF EXISTS room_memberships_room_user_key;
