-- 0044: состояние на событие и forward extremities.
--
-- state resolution v2 разрешает наборы состояний: состояние после каждого forward
-- extremity (текущее состояние комнаты) и после каждого prev_event (состояние до
-- входящего события). Поэтому состояние хранится на событие - как state groups
-- Synapse: группа - дельта к prev_group (не длиннее 50 звеньев) или полный снимок.

CREATE TABLE IF NOT EXISTS state_groups (
    id         bigserial PRIMARY KEY,
    room_id    text NOT NULL REFERENCES rooms (room_id) ON DELETE CASCADE,
    prev_group bigint REFERENCES state_groups (id) ON DELETE CASCADE,
    chain_len  integer NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS state_groups_room_idx ON state_groups (room_id);
CREATE INDEX IF NOT EXISTS state_groups_prev_idx ON state_groups (prev_group) WHERE prev_group IS NOT NULL;

-- Записи группы: для дельты - измененные ключи, для снимка - все.
CREATE TABLE IF NOT EXISTS state_group_entries (
    state_group bigint NOT NULL REFERENCES state_groups (id) ON DELETE CASCADE,
    type        text NOT NULL,
    state_key   text NOT NULL,
    event_id    text NOT NULL,
    PRIMARY KEY (state_group, type, state_key)
);

-- Состояние ПОСЛЕ события (для отклоненного и не-state события - состояние до него).
CREATE TABLE IF NOT EXISTS event_state_groups (
    event_id    text PRIMARY KEY REFERENCES events (event_id) ON DELETE CASCADE,
    room_id     text NOT NULL REFERENCES rooms (room_id) ON DELETE CASCADE,
    state_group bigint NOT NULL REFERENCES state_groups (id) ON DELETE CASCADE,
    -- rejected - событие не прошло auth по состоянию до него: в состояние и
    -- extremities не попадает, в auth-проверках не участвует.
    rejected    boolean NOT NULL DEFAULT false
);

CREATE INDEX IF NOT EXISTS event_state_groups_group_idx ON event_state_groups (state_group);

-- Forward extremities - события комнаты без потомков (prev_events новых событий).
CREATE TABLE IF NOT EXISTS room_forward_extremities (
    room_id  text NOT NULL REFERENCES rooms (room_id) ON DELETE CASCADE,
    event_id text NOT NULL REFERENCES events (event_id) ON DELETE CASCADE,
    PRIMARY KEY (room_id, event_id)
);

CREATE INDEX IF NOT EXISTS room_forward_extremities_event_idx ON room_forward_extremities (event_id);

-- Группа, совпадающая с room_state: от нее локальное событие строит свою дельту.
CREATE TABLE IF NOT EXISTS room_state_groups (
    room_id     text PRIMARY KEY REFERENCES rooms (room_id) ON DELETE CASCADE,
    state_group bigint NOT NULL REFERENCES state_groups (id) ON DELETE CASCADE
);

-- Любая запись room_state снимает room_state_groups: пути, которые знают группу
-- (локальная запись события, разрешение состояния), ставят ее заново после записи,
-- остальные (импорт, пересчет, прямые upsert) - нет, и следующее событие строит
-- снимок из room_state вместо дельты к устаревшей группе.
CREATE OR REPLACE FUNCTION room_state_groups_invalidate() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM room_state_groups WHERE room_id = OLD.room_id;
    ELSE
        DELETE FROM room_state_groups WHERE room_id = NEW.room_id;
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS room_state_groups_invalidate ON room_state;

CREATE TRIGGER room_state_groups_invalidate
    AFTER INSERT OR UPDATE OR DELETE ON room_state
    FOR EACH ROW EXECUTE FUNCTION room_state_groups_invalidate();
