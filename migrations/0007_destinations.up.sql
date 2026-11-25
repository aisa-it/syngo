-- 0007_destinations: состояние ретраев до чужих серверов (ADR-0007 §5).

CREATE TABLE destinations (
    destination                    text PRIMARY KEY,
    failure_ts                     timestamptz,
    retry_last_ts                  timestamptz,
    retry_interval                 bigint NOT NULL DEFAULT 0, -- миллисекунды до следующей попытки
    last_successful_stream_ordering bigint
);

COMMENT ON TABLE destinations IS 'Backoff-состояние федеративных destination (retry/backoff)';
