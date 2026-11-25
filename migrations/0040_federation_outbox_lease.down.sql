DROP TABLE IF EXISTS federation_outbox_dead;
DROP INDEX IF EXISTS federation_outbox_room_idx;
DROP INDEX IF EXISTS federation_outbox_txn_idx;
DROP INDEX IF EXISTS federation_outbox_ready_idx;
ALTER TABLE federation_outbox
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS isolate,
    DROP COLUMN IF EXISTS claimed_until,
    DROP COLUMN IF EXISTS txn_id,
    DROP COLUMN IF EXISTS room_id,
    DROP COLUMN IF EXISTS event_id;
