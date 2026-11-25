package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/auth"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// redactionType - тип события удаления.
const redactionType = "m.room.redaction"

// redactionRef - то, что нужно о записанном событии для применения redaction.
type redactionRef struct {
	EventID string
	RoomID  string
	Type    string
	Sender  string
	// Redacts - цель, если событие - m.room.redaction.
	Redacts string
}

func redactionRefOf(ev core.Event) redactionRef {
	ref := redactionRef{EventID: ev.EventID, RoomID: ev.RoomID, Type: ev.Type, Sender: ev.Sender}
	if ev.Type == redactionType {
		ref.Redacts = contentRedacts(ev.Content)
	}

	return ref
}

func redactionRefOfPDU(pdu *FedPDU) redactionRef {
	ref := redactionRef{EventID: pdu.EventID, RoomID: pdu.RoomID, Type: pdu.Type, Sender: pdu.Sender}
	if pdu.Type == redactionType {
		ref.Redacts = RedactionTarget(pdu)
	}

	return ref
}

// RedactionTarget достает цель redaction: top-level `redacts` (комнаты до v11) или
// content.redacts (v11 и наши собственные события).
func RedactionTarget(pdu *FedPDU) string {
	if pdu.Redacts != "" {
		return pdu.Redacts
	}

	return contentRedacts(pdu.Content)
}

func contentRedacts(content []byte) string {
	var body struct {
		Redacts string `json:"redacts"`
	}

	if err := json.Unmarshal(content, &body); err != nil {
		return ""
	}

	return body.Redacts
}

// SetRedactionRetention задает срок хранения оригинального content удаленных
// событий (redaction.retention_period). 0 - оригинал не сохраняется.
func (db *DB) SetRedactionRetention(retention time.Duration) {
	db.keepRedactedOriginal = retention > 0
}

// linkRedactions применяет redaction в транзакции записи события:
// если событие - m.room.redaction, помечает и вырезает цель; если на событие уже
// есть redaction (пришла раньше цели), применяет ее к нему. Раньше пометка шла
// отдельным запросом после коммита: при обрыве между ними redaction оставалась
// записанной, а цель - нетронутой.
func (db *DB) linkRedactions(ctx context.Context, tx pgx.Tx, ref redactionRef) error {
	// redaction и ее цель могут записываться параллельно (федерация): без общей
	// блокировки по id цели каждая транзакция не видит незакоммиченную другую.
	if err := lockRedactionKey(ctx, tx, ref.EventID); err != nil {
		return err
	}

	if ref.Type == redactionType && ref.Redacts != "" && ref.Redacts != ref.EventID {
		if err := lockRedactionKey(ctx, tx, ref.Redacts); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO redactions (event_id, room_id, redacts) VALUES ($1, $2, $3)
			ON CONFLICT (event_id) DO NOTHING`, ref.EventID, ref.RoomID, ref.Redacts); err != nil {
			return fmt.Errorf("store: record redaction %s: %w", ref.EventID, err)
		}

		if _, err := db.applyRedactionTx(ctx, tx, ref.RoomID, ref.Redacts, ref.EventID, ref.Sender); err != nil {
			return err
		}
	}

	rows, err := tx.Query(ctx, `
		SELECT r.event_id, e.sender FROM redactions r
		JOIN events e ON e.event_id = r.event_id
		WHERE r.redacts = $1 AND r.room_id = $2 AND r.event_id <> $1
		ORDER BY e.stream_ordering`, ref.EventID, ref.RoomID)
	if err != nil {
		return fmt.Errorf("store: pending redactions %s: %w", ref.EventID, err)
	}

	type pending struct{ id, sender string }

	var early []pending

	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.sender); err != nil {
			rows.Close()

			return fmt.Errorf("store: scan pending redaction: %w", err)
		}

		early = append(early, p)
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: pending redactions %s: %w", ref.EventID, err)
	}

	for _, p := range early {
		applied, err := db.applyRedactionTx(ctx, tx, ref.RoomID, ref.EventID, p.id, p.sender)
		if err != nil {
			return err
		}

		if applied {
			break
		}
	}

	return nil
}

func lockRedactionKey(ctx context.Context, tx pgx.Tx, eventID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('redaction:' || $1))`, eventID); err != nil {
		return fmt.Errorf("store: lock redaction %s: %w", eventID, err)
	}

	return nil
}

// applyRedactionTx помечает цель redaction и вырезает ее content по правилам версии
// комнаты; оригинал (если хранится) переносится в redactions.original_content.
// Возвращает true, если цель удалена (этой или более ранней redaction).
//
// Правило применения - как в Synapse для комнат v3+: redaction действует, если
// домены отправителей совпадают или у отправителя redaction есть уровень redact.
// Для локальных событий права (свое - всегда, чужое - по redact) проверяет
// core.RoomService.Redact до записи.
func (db *DB) applyRedactionTx(ctx context.Context, tx pgx.Tx, roomID, targetID, redactionID, sender string) (bool, error) {
	var (
		eventType, targetSender, by, version string
		content, hashes                      []byte
		pdu                                  *string
		target                               core.Event
	)

	// FOR UPDATE: параллельные redaction одной цели сериализуются, первая побеждает.
	err := tx.QueryRow(ctx, `
		SELECT t.type, t.sender, coalesce(t.redacted_by, ''), t.content, coalesce(rm.room_version, ''), t.pdu_json,
		       t.hashes, t.state_key, t.prev_events, t.auth_events, t.depth, t.origin_server_ts
		FROM events t
		LEFT JOIN rooms rm ON rm.room_id = t.room_id
		WHERE t.room_id = $1 AND t.event_id = $2
		FOR UPDATE OF t`, roomID, targetID).Scan(&eventType, &targetSender, &by, &content, &version, &pdu,
		&hashes, &target.StateKey, &target.PrevEvents, &target.AuthEvents, &target.Depth, &target.OriginServerTS)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // цели еще нет: применится при ее записи
	}

	if err != nil {
		return false, fmt.Errorf("store: redaction target %s: %w", targetID, err)
	}

	if by != "" {
		return true, nil // уже удалено: redacted_because остается у первой redaction
	}

	if serverOf(sender) != serverOf(targetSender) {
		allowed, err := canRedactOthers(ctx, tx, roomID, sender)
		if err != nil || !allowed {
			return false, err
		}
	}

	roomVersion, convErr := strconv.Atoi(version)
	if convErr != nil {
		roomVersion = core.DefaultRoomVersion
	}

	pruned := core.RedactContentFor(eventType, content, roomVersion)

	// Событие без исходного PDU (до миграции 0041) после redaction собирается из
	// вырезанного content: hashes оригинала надо сохранить сейчас, иначе изменится
	// и event_id, который считается по ним.
	if pdu == nil && len(hashes) == 0 {
		target.RoomID, target.Type, target.Sender, target.Content, target.RoomVersion =
			roomID, eventType, targetSender, content, roomVersion
		hashes = originalHashes(target)
	}

	// Исходный PDU (импорт из Synapse) тоже вырезается: в нем content цели.
	if _, err := tx.Exec(ctx, `
		UPDATE events SET redacted_by = $3, content = $4, pdu_json = $5, hashes = coalesce(hashes, $6)
		WHERE room_id = $1 AND event_id = $2`,
		roomID, targetID, redactionID, string(pruned), prunePDUContent(pdu, pruned), nullJSON(hashes)); err != nil {
		return false, fmt.Errorf("store: redact %s: %w", targetID, err)
	}

	var original any
	if db.keepRedactedOriginal {
		original = string(content)
	}

	if _, err := tx.Exec(ctx, `UPDATE redactions SET pruned = true, original_content = $2 WHERE event_id = $1`,
		redactionID, original); err != nil {
		return false, fmt.Errorf("store: redaction original %s: %w", redactionID, err)
	}

	return true, nil
}

// originalHashes - hashes события, собранного из колонок (core.PDUObject), до
// вырезания content; nil - не посчитать.
func originalHashes(event core.Event) []byte {
	hash, err := signing.ContentHash(core.PDUObject(event))
	if err != nil {
		return nil
	}

	raw, err := json.Marshal(map[string]string{"sha256": hash})
	if err != nil {
		return nil
	}

	return raw
}

// canRedactOthers - уровень отправителя >= redact по текущему m.room.power_levels;
// создатели комнаты v12 (отправитель create и additional_creators) - всегда:
// их уровень бесконечен и в power_levels не пишется.
func canRedactOthers(ctx context.Context, tx pgx.Tx, roomID, sender string) (bool, error) {
	creator, err := isV12Creator(ctx, tx, roomID, sender)
	if err != nil || creator {
		return creator, err
	}

	var content []byte

	err = tx.QueryRow(ctx, `
		SELECT e.content FROM room_state rs JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = $1 AND rs.type = 'm.room.power_levels' AND rs.state_key = ''`, roomID).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("store: power levels %s: %w", roomID, err)
	}

	var body struct {
		Users        map[string]json.RawMessage `json:"users"`
		UsersDefault json.RawMessage            `json:"users_default"`
		Redact       json.RawMessage            `json:"redact"`
	}

	if err := json.Unmarshal(content, &body); err != nil {
		return false, nil //nolint:nilerr // битые power_levels - прав нет
	}

	level := powerNumber(body.UsersDefault, 0)
	if raw, ok := body.Users[sender]; ok {
		level = powerNumber(raw, level)
	}

	return level >= powerNumber(body.Redact, 50), nil
}

// isV12Creator - sender - создатель комнаты с правилами v12 по ее текущему
// m.room.create.
func isV12Creator(ctx context.Context, tx pgx.Tx, roomID, sender string) (bool, error) {
	var create auth.Event

	err := tx.QueryRow(ctx, `
		SELECT e.sender, e.content FROM room_state rs JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = $1 AND rs.type = 'm.room.create' AND rs.state_key = ''`, roomID).
		Scan(&create.Sender, &create.Content)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("store: create %s: %w", roomID, err)
	}

	_, creator := auth.Creators(auth.State{Create: &create})[sender]

	return creator, nil
}

// powerNumber - уровень из power_levels: число или строка с числом (старые комнаты).
func powerNumber(raw json.RawMessage, fallback int64) int64 {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" {
		return fallback
	}

	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fallback
	}

	return n
}

// serverOf - имя сервера из @user:server (все после первого двоеточия).
func serverOf(id string) string {
	_, server, _ := strings.Cut(id, ":")

	return server
}

// PruneRedactedContent вырезает content событий, удаленных до миграции 0029 (тогда
// вырезание было только при чтении). Вызывается при старте syngod до приема
// запросов; возвращает число обработанных событий.
func (db *DB) PruneRedactedContent(ctx context.Context) (int, error) {
	if db == nil || db.pool == nil {
		return 0, ErrNotConfigured
	}

	total := 0

	for {
		n, err := db.pruneRedactedBatch(ctx, 500)
		if err != nil {
			return total, err
		}

		total += n

		if n == 0 {
			return total, nil
		}
	}
}

func (db *DB) pruneRedactedBatch(ctx context.Context, limit int) (int, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: begin prune: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT r.event_id, t.event_id, t.type, t.content, coalesce(rm.room_version, '')
		FROM redactions r
		JOIN events t ON t.event_id = r.redacts AND t.redacted_by = r.event_id
		LEFT JOIN rooms rm ON rm.room_id = t.room_id
		WHERE NOT r.pruned
		LIMIT $1
		FOR UPDATE OF r, t`, limit)
	if err != nil {
		return 0, fmt.Errorf("store: select unpruned: %w", err)
	}

	type item struct {
		redactionID, targetID, eventType, version string
		content                                   []byte
	}

	var items []item

	for rows.Next() {
		var it item
		if err := rows.Scan(&it.redactionID, &it.targetID, &it.eventType, &it.content, &it.version); err != nil {
			rows.Close()

			return 0, fmt.Errorf("store: scan unpruned: %w", err)
		}

		items = append(items, it)
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: select unpruned: %w", err)
	}

	for _, it := range items {
		version, convErr := strconv.Atoi(it.version)
		if convErr != nil {
			version = core.DefaultRoomVersion
		}

		pruned := core.RedactContentFor(it.eventType, it.content, version)

		if _, err := tx.Exec(ctx, `UPDATE events SET content = $2 WHERE event_id = $1`, it.targetID, string(pruned)); err != nil {
			return 0, fmt.Errorf("store: prune %s: %w", it.targetID, err)
		}

		if _, err := tx.Exec(ctx, `UPDATE redactions SET pruned = true WHERE event_id = $1`, it.redactionID); err != nil {
			return 0, fmt.Errorf("store: mark pruned %s: %w", it.redactionID, err)
		}
	}

	// redaction без примененной цели (цель скрыта другой redaction или неизвестна):
	// вырезать нечего, иначе они выбирались бы при каждом старте.
	if _, err := tx.Exec(ctx, `
		UPDATE redactions r SET pruned = true
		WHERE NOT r.pruned AND NOT EXISTS (
			SELECT 1 FROM events t WHERE t.event_id = r.redacts AND t.redacted_by = r.event_id)`); err != nil {
		return 0, fmt.Errorf("store: mark pruned: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit prune: %w", err)
	}

	return len(items), nil
}

// PurgeRedactedOriginals стирает оригинальный content удаленных событий старше
// retention (redaction.retention_period, как redaction_retention_period в Synapse).
func (db *DB) PurgeRedactedOriginals(ctx context.Context, retention time.Duration) (int64, error) {
	if db == nil || db.pool == nil {
		return 0, ErrNotConfigured
	}

	retention = max(retention, 0)

	tag, err := db.pool.Exec(ctx, `
		UPDATE redactions SET original_content = NULL
		WHERE original_content IS NOT NULL AND received_at < now() - make_interval(secs => $1)`,
		retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: purge redacted originals: %w", err)
	}

	return tag.RowsAffected(), nil
}
