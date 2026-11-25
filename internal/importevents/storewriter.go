package importevents

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// StoreWriter - BatchWriter поверх store.DB. Раньше адаптер был скопирован в
// syngo-import-events и в тесты; общий нужен и syngo-import.
type StoreWriter struct{ DB *store.DB }

// NewStoreWriter создает писателя в PostgreSQL.
func NewStoreWriter(db *store.DB) StoreWriter { return StoreWriter{DB: db} }

// InsertEvents пишет пачку событий: обычные - в events, отклоненные,
// soft-failed и outlier - в excluded_events.
func (w StoreWriter) InsertEvents(ctx context.Context, events []Event) (int64, error) {
	rows := make([]store.EventRow, 0, len(events))

	var excluded []store.ExcludedEventRow

	for i := range events {
		ev := events[i]

		if ev.Excluded != "" {
			excluded = append(excluded, store.ExcludedEventRow{
				EventID: ev.EventID, RoomID: ev.RoomID, Type: ev.Type, Sender: ev.Sender,
				StateKey: ev.StateKey, Reason: ev.Excluded, RejectedReason: ev.RejectedReason,
				Depth: ev.Depth, OriginServerTS: ev.OriginServerTS, StreamOrdering: ev.StreamOrdering,
				PDUJSON: ev.PDUJSON,
			})

			continue
		}

		rows = append(rows, store.EventRow{
			EventID:        ev.EventID,
			RoomID:         ev.RoomID,
			Type:           ev.Type,
			Sender:         ev.Sender,
			StateKey:       ev.StateKey,
			Content:        ev.Content,
			PrevEvents:     ev.PrevEvents,
			AuthEvents:     ev.AuthEvents,
			Depth:          ev.Depth,
			OriginServerTS: ev.OriginServerTS,
			Origin:         ev.Origin,
			StreamOrdering: ev.StreamOrdering,
			PDUJSON:        ev.PDUJSON,
			Signatures:     ev.Signatures,
			Hashes:         ev.Hashes,
			Redacts:        ev.Redacts,
		})
	}

	inserted, err := w.DB.InsertEvents(ctx, rows)
	if err != nil {
		return inserted, err
	}

	if len(excluded) == 0 {
		return inserted, nil
	}

	n, err := w.DB.InsertExcludedEvents(ctx, excluded)

	return inserted + n, err
}

// ReplaceRoomState заменяет состояние комнаты состоянием источника.
func (w StoreWriter) ReplaceRoomState(ctx context.Context, roomID string, rows []StateRow) ([]StateRow, error) {
	refs := make([]core.StateRef, len(rows))
	for i := range rows {
		refs[i] = core.StateRef{RoomID: roomID, Type: rows[i].Type, StateKey: rows[i].StateKey, EventID: rows[i].EventID}
	}

	missing, err := w.DB.ReplaceRoomState(ctx, roomID, refs)
	if err != nil {
		return nil, err
	}

	out := make([]StateRow, len(missing))
	for i := range missing {
		out[i] = StateRow{RoomID: roomID, Type: missing[i].Type, StateKey: missing[i].StateKey, EventID: missing[i].EventID}
	}

	return out, nil
}

// ApplyPendingRedactions применяет импортированные redaction.
func (w StoreWriter) ApplyPendingRedactions(ctx context.Context) (int64, error) {
	return w.DB.ApplyPendingRedactions(ctx)
}

// RoomVersions - версии комнат для проверки event_id.
func (w StoreWriter) RoomVersions(ctx context.Context) (map[string]string, error) {
	return w.DB.RoomVersions(ctx)
}

// SyncStreamOrdering подтягивает sequence событий до максимума из БД.
func (w StoreWriter) SyncStreamOrdering(ctx context.Context) error {
	return w.DB.SyncStreamOrdering(ctx)
}

// ApplyImportedReceipts проставляет позиции отметкам прочтения, импортированным
// раньше событий, и поднимает до них сводку непрочитанного.
func (w StoreWriter) ApplyImportedReceipts(ctx context.Context) (int64, error) {
	return w.DB.ApplyImportedReceipts(ctx)
}

// IsRowError - ошибка данных строки (классы SQLSTATE 22 и 23), а не отказ БД.
func (w StoreWriter) IsRowError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}

	return pgErr.Code[:2] == "22" || pgErr.Code[:2] == "23"
}

// ExistingRooms возвращает множество комнат, уже импортированных в БД.
func (w StoreWriter) ExistingRooms(ctx context.Context) (map[string]struct{}, error) {
	rows, err := w.DB.Pool().Query(ctx, "SELECT room_id FROM rooms")
	if err != nil {
		return nil, fmt.Errorf("select rooms: %w", err)
	}

	defer rows.Close()

	rooms := make(map[string]struct{}, 1024)

	for rows.Next() {
		var roomID string
		if err := rows.Scan(&roomID); err != nil {
			return nil, fmt.Errorf("scan room: %w", err)
		}

		rooms[roomID] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rooms: %w", err)
	}

	return rooms, nil
}
