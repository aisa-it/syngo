-- Сохраняем hashes PDU при приёме: для чужих событий отдаём их как есть,
-- иначе пересчёт канонизации может разойтись с подписью отправителя.
ALTER TABLE events ADD COLUMN IF NOT EXISTS hashes jsonb;
