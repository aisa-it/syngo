package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// importToDevice сохраняет каждую доставку Synapse один раз. Сервер назначает
// собственный stream_ordering для /sync, а исходный stream_id служит для повторов.
func (im *importer) importToDevice(ctx context.Context, record *dumpRecord) error {
	var message struct {
		Type    string          `json:"type"`
		Sender  string          `json:"sender"`
		Content json.RawMessage `json:"content"`
	}

	if err := json.Unmarshal(record.Message, &message); err != nil {
		return fmt.Errorf("to_device: некорректное сообщение: %w", err)
	}
	if message.Type == "" {
		return fmt.Errorf("to_device: отсутствует тип сообщения")
	}

	content := rawJSON(message.Content)

	return pgx.BeginFunc(ctx, im.db.Pool(), func(tx pgx.Tx) error {
		// Оба формата могут импортироваться одновременно. После взятия одной
		// блокировки на получателя следующая команда видит результат предшественника.
		var hash []byte
		err := tx.QueryRow(ctx, `
			SELECT sha256(convert_to(jsonb_build_array($1::text, $2::text, $3::text, $4::text, $5::jsonb)::text, 'UTF8'))
			FROM pg_advisory_xact_lock(hashtextextended(jsonb_build_array($1::text, $2::text)::text, 179))`,
			record.UserID, record.DeviceID, message.Type, message.Sender, content).Scan(&hash)
		if err != nil {
			return err
		}

		seen, err := im.matchImportedDelivery(ctx, tx, record, hash)
		if err != nil || seen {
			return err
		}

		// При повторе старого дампа могла остаться строка от предыдущей версии
		// импортера. Привязываем её только один раз. Для записи с исходным ID
		// одинаковое сообщение в обычной очереди всегда остаётся отдельной доставкой.
		_, err = tx.Exec(ctx, `
			WITH pending AS (
				SELECT i.id FROM device_inbox i
				WHERE $6::bigint IS NULL AND i.user_id = $1 AND i.device_id = $2
				  AND i.event_type = $3 AND i.sender = $4 AND i.content = $5::jsonb
				  AND NOT EXISTS (SELECT 1 FROM synapse_inbox_imports WHERE inbox_id = i.id)
				ORDER BY i.id LIMIT 1
			), queued AS (
				INSERT INTO device_inbox (user_id, device_id, event_type, sender, content)
				SELECT $1, $2, $3, $4, $5::jsonb WHERE NOT EXISTS (SELECT 1 FROM pending)
				RETURNING id
			)
			INSERT INTO synapse_inbox_imports (inbox_id, user_id, device_id, source_stream_id, message_hash)
			SELECT id, $1, $2, $6, $7 FROM (SELECT id FROM pending UNION ALL SELECT id FROM queued) AS delivery`,
			record.UserID, record.DeviceID, message.Type, message.Sender, content, record.SourceStreamID, hash)

		return err
	})
}

func (im *importer) matchImportedDelivery(ctx context.Context, tx pgx.Tx, record *dumpRecord, hash []byte) (bool, error) {
	if record.SourceStreamID != nil {
		// Точный ID имеет приоритет. При переходе со старого экспорта каждая
		// ранее учтённая копия может получить исходный ID лишь один раз.
		var seen bool
		err := tx.QueryRow(ctx, `
			WITH known AS (
				SELECT 1 FROM synapse_inbox_imports WHERE user_id = $1 AND device_id = $2 AND source_stream_id = $3
			), linked AS (
				UPDATE synapse_inbox_imports SET source_stream_id = $3
				WHERE NOT EXISTS (SELECT 1 FROM known) AND inbox_id = (
					SELECT inbox_id FROM synapse_inbox_imports WHERE source_stream_id IS NULL AND message_hash = $4
					ORDER BY inbox_id LIMIT 1)
				RETURNING 1
			)
			SELECT EXISTS (SELECT 1 FROM known) OR EXISTS (SELECT 1 FROM linked)`,
			record.UserID, record.DeviceID, record.SourceStreamID, hash).Scan(&seen)

		return seen, err
	}

	// Старый экспорт различает одинаковые строки только по их числу в снимке.
	if im.legacyInboxOccurrences == nil {
		im.legacyInboxOccurrences = make(map[string]int64)
	}
	offset := im.legacyInboxOccurrences[string(hash)]
	im.legacyInboxOccurrences[string(hash)]++

	var seen bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM synapse_inbox_imports
		WHERE message_hash = $1 ORDER BY inbox_id LIMIT 1 OFFSET $2)`, hash, offset).Scan(&seen)

	return seen, err
}
