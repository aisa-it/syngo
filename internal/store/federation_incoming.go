package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// Прием событий федерации по одному: событие, состояние на него,
// forward extremities, room_state и пометки отклоненных и soft-failed - в одной
// транзакции под блокировкой комнаты. Раньше события транзакции /send писались
// одной транзакцией вместе с inbox, а состояние разрешалось после коммита: один
// плохой PDU откатывал всю транзакцию, а отклоненное событие успевало попасть в
// /sync до разрешения.

// RejectedAuthError - rejected_reason события, не прошедшего auth rules
// (rejections.reason Synapse).
const RejectedAuthError = "auth_error"

// FederationTxnSeen - транзакция (origin, txn_id) уже принята.
func (db *DB) FederationTxnSeen(ctx context.Context, origin, txnID string) (bool, error) {
	var seen bool

	if err := db.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM federation_inbox WHERE origin = $1 AND txn_id = $2)`,
		origin, txnID).Scan(&seen); err != nil {
		return false, fmt.Errorf("store: federation inbox %s/%s: %w", origin, txnID, err)
	}

	return seen, nil
}

// ApplyFederationEvents пишет события комнаты roomID и применяет к ним apply (state
// resolution, core.ApplyRoomEvents) в одной транзакции под блокировкой состояния
// комнаты. Отклоненные события (result.Rejected) и soft-failed (result.SoftFailed)
// остаются в events вне таймлайна (outlier, rejected_reason / soft_failed); redaction
// применяется только принятыми. Событие, известное как outlier без состояния
// (auth-событие, догруженное раньше), становится событием таймлайна с новым
// stream_ordering, если принято.
func (db *DB) ApplyFederationEvents(
	ctx context.Context, origin, roomID string, pdus []FedPDU,
	apply func(core.RoomStateTx) (core.RoomStateResult, error),
) (core.RoomStateResult, error) {
	var result core.RoomStateResult

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("store: federation events begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockRoomState(ctx, tx, roomID); err != nil {
		return result, err
	}

	var positions []int64

	committed := false

	defer func() {
		if !committed {
			for _, position := range positions {
				db.forgetPendingPosition(position)
			}
		}
	}()

	written := make([]*FedPDU, 0, len(pdus))

	var revived []string

	for i := range pdus {
		pdu := &pdus[i]
		if pdu.RoomID != roomID {
			return result, fmt.Errorf("store: event %s of room %s in batch of %s", pdu.EventID, pdu.RoomID, roomID)
		}

		position, inserted, err := insertFedEventTx(ctx, tx, origin, pdu, false, "")
		if err != nil {
			return result, err
		}

		if inserted {
			// Номер выдан до коммита: /sync не должен отдать next_batch за него.
			db.registerPendingPosition(position)
			positions = append(positions, position)
			written = append(written, pdu)

			continue
		}

		var outlier, known bool

		if err := tx.QueryRow(ctx, `
			SELECT e.outlier, e.rejected_reason IS NOT NULL
			       OR EXISTS (SELECT 1 FROM event_state_groups g WHERE g.event_id = e.event_id)
			FROM events e WHERE e.event_id = $1`, pdu.EventID).Scan(&outlier, &known); err != nil {
			return result, fmt.Errorf("store: existing event %s: %w", pdu.EventID, err)
		}

		if outlier && !known {
			revived = append(revived, pdu.EventID)
			written = append(written, pdu)
		}
	}

	result, err = apply(newRoomStateTx(tx, roomID))
	if err != nil {
		return result, err
	}

	excluded := make(map[string]struct{}, len(result.Rejected)+len(result.SoftFailed))

	if len(result.Rejected) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE events SET outlier = true, rejected_reason = coalesce(rejected_reason, $2)
			WHERE event_id = ANY($1)`, result.Rejected, RejectedAuthError); err != nil {
			return result, fmt.Errorf("store: mark rejected: %w", err)
		}

		for _, id := range result.Rejected {
			excluded[id] = struct{}{}
		}
	}

	if len(result.SoftFailed) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE events SET outlier = true, soft_failed = true WHERE event_id = ANY($1)`, result.SoftFailed); err != nil {
			return result, fmt.Errorf("store: mark soft-failed: %w", err)
		}

		for _, id := range result.SoftFailed {
			excluded[id] = struct{}{}
		}
	}

	if err := db.reviveOutliers(ctx, tx, revived, excluded, &positions); err != nil {
		return result, err
	}

	for _, pdu := range written {
		if _, skip := excluded[pdu.EventID]; skip {
			continue
		}

		if err := db.linkRedactions(ctx, tx, redactionRefOfPDU(pdu)); err != nil {
			return result, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("store: federation events commit: %w", err)
	}

	committed = true

	return result, nil
}

// reviveOutliers переводит принятые outlier-события в таймлайн с новым
// stream_ordering (иначе /sync с токеном позже их записи их не увидит).
func (db *DB) reviveOutliers(
	ctx context.Context, tx pgx.Tx, ids []string, excluded map[string]struct{}, positions *[]int64,
) error {
	accepted := make([]string, 0, len(ids))

	for _, id := range ids {
		if _, skip := excluded[id]; !skip {
			accepted = append(accepted, id)
		}
	}

	if len(accepted) == 0 {
		return nil
	}

	rows, err := tx.Query(ctx, `
		UPDATE events SET outlier = false, stream_ordering = nextval('events_stream_ordering_seq')
		WHERE event_id = ANY($1) RETURNING stream_ordering`, accepted)
	if err != nil {
		return fmt.Errorf("store: revive outliers: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var position int64
		if err := rows.Scan(&position); err != nil {
			return fmt.Errorf("store: revive outliers: %w", err)
		}

		db.registerPendingPosition(position)
		*positions = append(*positions, position)
	}

	return rows.Err()
}

// InsertFederationOutliers пишет события вне таймлайна (state и auth-события,
// догруженные по /event, /state при догоне дыры): без состояния на событие и
// forward extremities. rejected - событие не прошло auth по своим auth_events.
func (db *DB) InsertFederationOutliers(ctx context.Context, origin string, pdus []FedPDU, rejected map[string]bool) error {
	if len(pdus) == 0 {
		return nil
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: outliers begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	for i := range pdus {
		reason := ""
		if rejected[pdus[i].EventID] {
			reason = RejectedAuthError
		}

		if _, _, err := insertFedEventTx(ctx, tx, origin, &pdus[i], true, reason); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: outliers commit: %w", err)
	}

	return nil
}

// insertFedEventTx пишет событие федерации (повтор - ничего) и его auth-связи.
func insertFedEventTx(
	ctx context.Context, tx pgx.Tx, origin string, pdu *FedPDU, outlier bool, rejectedReason string,
) (position int64, inserted bool, err error) {
	prev := pdu.Prev
	if prev == nil {
		prev = []string{}
	}

	auth := pdu.Auth
	if auth == nil {
		auth = []string{}
	}

	eventOrigin := domainOfMatrixID(pdu.Sender)
	if eventOrigin == "" {
		eventOrigin = origin
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
		                    depth, origin_server_ts, origin, signatures, hashes, pdu_json, outlier, rejected_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (event_id) DO NOTHING
		RETURNING stream_ordering`,
		pdu.EventID, pdu.RoomID, pdu.Type, pdu.Sender, pdu.StateKey, jsonbSafe(pdu.Content), prev, auth,
		pdu.Depth, pdu.OriginTS, eventOrigin, nullJSON(jsonbSafe(pdu.Signatures)), nullJSON(jsonbSafe(pdu.Hashes)),
		nullText(string(pdu.PDU)), outlier, nullText(rejectedReason)).Scan(&position)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}

	if err != nil {
		return 0, false, fmt.Errorf("store: insert federation event %s: %w", pdu.EventID, err)
	}

	for _, authID := range auth {
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_auth (event_id, auth_event_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			pdu.EventID, authID); err != nil {
			return 0, false, fmt.Errorf("store: event auth %s: %w", pdu.EventID, err)
		}
	}

	return position, true, nil
}

// jsonbSafe - JSON без \u0000 в строках: jsonb PostgreSQL его не принимает
// (SQLSTATE 22P05), и одно такое событие откатывало всю транзакцию /send, а
// локальное - отвечало 500. NUL в строках и ключах заменяется на
// U+FFFD только в колонке jsonb; исходный PDU (pdu_json, текст) хранится как
// пришел, поэтому подпись, hashes и event_id не меняются.
func jsonbSafe(raw []byte) []byte {
	if !bytes.Contains(raw, []byte(`\u0000`)) {
		return raw
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return raw
	}

	var out bytes.Buffer

	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(withoutNUL(value)); err != nil {
		return raw
	}

	return bytes.TrimRight(out.Bytes(), "\n")
}

// withoutNUL заменяет NUL в строках и ключах значения JSON на U+FFFD.
func withoutNUL(value any) any {
	switch typed := value.(type) {
	case string:
		return strings.ReplaceAll(typed, "\x00", "�")
	case []any:
		for i := range typed {
			typed[i] = withoutNUL(typed[i])
		}

		return typed
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[strings.ReplaceAll(key, "\x00", "�")] = withoutNUL(item)
		}

		return out
	default:
		return value
	}
}
