-- Сохраняем происхождение импортированной доставки и после подтверждения в /sync.
-- Поэтому inbox_id не ссылается внешним ключом на очищаемую очередь device_inbox.
CREATE TABLE synapse_inbox_imports (
    inbox_id bigint PRIMARY KEY,
    user_id text NOT NULL,
    device_id text NOT NULL,
    source_stream_id bigint,
    message_hash bytea NOT NULL,
    UNIQUE (user_id, device_id, source_stream_id)
);

-- В старых JSONL нет stream_id. Хеш включает получателя, устройство и всё сообщение.
CREATE INDEX synapse_inbox_imports_message_idx ON synapse_inbox_imports (message_hash, inbox_id);
