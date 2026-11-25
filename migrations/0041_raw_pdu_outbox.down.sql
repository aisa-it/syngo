-- jsonb не принимает \u0000: такие PDU в старой схеме не могли быть в очереди.
DELETE FROM federation_outbox WHERE strpos(pdu_json, '\u0000') > 0;
DELETE FROM federation_outbox_dead WHERE strpos(pdu_json, '\u0000') > 0;

ALTER TABLE federation_outbox ALTER COLUMN pdu_json TYPE jsonb USING pdu_json::jsonb;
ALTER TABLE federation_outbox_dead ALTER COLUMN pdu_json TYPE jsonb USING pdu_json::jsonb;

COMMENT ON COLUMN events.pdu_json IS NULL;
