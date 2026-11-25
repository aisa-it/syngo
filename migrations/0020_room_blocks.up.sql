-- Блокировка комнаты (admin API): вместо физического удаления
-- Synapse блокирует комнату; join в неё запрещён, история/состояние сохраняются.
CREATE TABLE IF NOT EXISTS room_blocks (
    room_id    text PRIMARY KEY REFERENCES rooms (room_id) ON DELETE CASCADE,
    blocked_by text NOT NULL,
    blocked_at timestamptz NOT NULL DEFAULT now()
);
