-- 0043: outlier - событие известно, но не входит в таймлайн.
--
-- state и auth_chain из ответа send_join - состояние комнаты до входа нашего
-- пользователя: они нужны состоянию и auth-цепочке, но не новые события
-- таймлайна (outlier в Synapse). Чтения таймлайна (/sync, /messages, prev_events
-- новых событий) их пропускают; состояние (room_state) ссылается на них как
-- обычно.
ALTER TABLE events ADD COLUMN IF NOT EXISTS outlier boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN events.outlier IS 'событие вне таймлайна (state/auth_chain из send_join)';
