-- Возвращаем оригинал в events: до 0029 content вырезался только при чтении.
UPDATE events e SET content = r.original_content
FROM redactions r
WHERE r.redacts = e.event_id AND e.redacted_by = r.event_id AND r.original_content IS NOT NULL;

DROP TABLE IF EXISTS redactions;
