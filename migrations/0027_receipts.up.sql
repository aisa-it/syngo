-- Отметки прочтения (m.read, m.read.private) переживают рестарт: раньше
-- они жили только в памяти процесса. Одна отметка на (комната, пользователь, тип, тред).
CREATE TABLE IF NOT EXISTS receipts (
    room_id      text   NOT NULL,
    user_id      text   NOT NULL,
    receipt_type text   NOT NULL,
    thread_id    text   NOT NULL DEFAULT '',
    event_id     text   NOT NULL,
    ts           bigint NOT NULL,
    PRIMARY KEY (room_id, user_id, receipt_type, thread_id)
);
