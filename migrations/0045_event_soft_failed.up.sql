-- 0045: soft-failed события федерации.
--
-- Событие прошло auth по состоянию до него, но не по текущему состоянию комнаты
-- (soft-fail Synapse): хранится, получает состояние после себя (на него могут
-- ссылаться prev_events), но не попадает в таймлайн (outlier), forward extremities
-- и текущее состояние. Отклоненные события - outlier с rejected_reason.
ALTER TABLE events ADD COLUMN IF NOT EXISTS soft_failed boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN events.soft_failed IS 'событие федерации не прошло auth по текущему состоянию комнаты (soft-fail)';
