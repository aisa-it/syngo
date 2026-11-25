package core

import (
	"context"
	"encoding/json"
)

// Отметки прочтения: receipts и m.fully_read (POST /receipt, /read_markers).
// Запись перенесена из HTTP-обработчиков.

// ReadMarkerStore - сохранность отметок прочтения.
type ReadMarkerStore interface {
	UpsertReceipt(ctx context.Context, receipt ReceiptEvent) error
	AllReceipts(ctx context.Context) ([]ReceiptEvent, error)
	// SetAccountData - m.fully_read хранится в room account data.
	SetAccountData(ctx context.Context, userID, roomID, eventType string, content []byte) error
}

// ReadMarkersService - доменный сервис отметок прочтения поверх эфемерного
// хранилища в памяти. Без ReadMarkerStore (сервер без БД) receipts живут только в
// памяти, m.fully_read не сохраняется.
type ReadMarkersService struct {
	ephemeral *EphemeralStore
	store     ReadMarkerStore
	// warn - receipt не сохранился в хранилище (в памяти он есть).
	warn func(ctx context.Context, err error)
	// beforePublish - receipt сохранен, но еще не виден в эфемерном потоке /sync:
	// здесь сбрасываются счетчики непрочитанного.
	beforePublish func(ctx context.Context, receipt ReceiptEvent)
}

// OnReceiptStored задает действие после сохранения receipt и до его появления в
// эфемерном потоке. /sync, увидевший новую версию потока, должен читать уже
// сброшенные счетчики: иначе комната, затем тихая, осталась бы со старыми.
func (s *ReadMarkersService) OnReceiptStored(fn func(ctx context.Context, receipt ReceiptEvent)) {
	s.beforePublish = fn
}

// NewReadMarkersService создает сервис; store и warn могут быть nil.
func NewReadMarkersService(
	ephemeral *EphemeralStore, store ReadMarkerStore, warn func(ctx context.Context, err error),
) *ReadMarkersService {
	return &ReadMarkersService{ephemeral: ephemeral, store: store, warn: warn}
}

// Load поднимает сохраненные отметки в память: после рестарта клиенты видят
// их как прежде.
func (s *ReadMarkersService) Load(ctx context.Context) error {
	if s.store == nil {
		return nil
	}

	receipts, err := s.store.AllReceipts(ctx)
	if err != nil {
		return err
	}

	s.ephemeral.LoadReceipts(receipts)

	return nil
}

// Set сохраняет отметку: m.fully_read - в room account data (не receipt, по
// спецификации), остальные - receipt в памяти и в хранилище. Ошибка - только
// сбой записи m.fully_read: сбой сохранения receipt не ломает отметку в памяти,
// клиент ее уже получит, теряется только сохранность.
func (s *ReadMarkersService) Set(ctx context.Context, userID, roomID, markerType, threadID, eventID string) error {
	if markerType == ReceiptFullyRead {
		if s.store == nil {
			return nil
		}

		content, _ := json.Marshal(map[string]string{"event_id": eventID})

		return s.store.SetAccountData(ctx, userID, roomID, ReceiptFullyRead, content)
	}

	receipt := ReceiptEvent{
		RoomID: roomID, EventID: eventID, UserID: userID, Type: markerType, ThreadID: threadID,
		TS: s.ephemeral.Now().UnixMilli(),
	}

	if s.store != nil {
		if err := s.store.UpsertReceipt(ctx, receipt); err != nil && s.warn != nil {
			s.warn(ctx, err)
		}
	}

	if s.beforePublish != nil {
		s.beforePublish(ctx, receipt)
	}

	s.ephemeral.StoreReceipt(receipt)

	return nil
}

// ApplyRemote сохраняет receipt пользователя другого сервера из m.receipt:
// время - из EDU, счетчики непрочитанного наших пользователей он
// не трогает.
func (s *ReadMarkersService) ApplyRemote(ctx context.Context, receipt ReceiptEvent) {
	if s.store != nil {
		if err := s.store.UpsertReceipt(ctx, receipt); err != nil && s.warn != nil {
			s.warn(ctx, err)
		}
	}

	s.ephemeral.StoreReceipt(receipt)
}
