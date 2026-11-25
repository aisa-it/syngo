package core

import "encoding/json"

// Event — событие Matrix в комнате. Одно и то же представление используется и для
// обычных сообщений (StateKey == nil), и для state-событий (StateKey != nil).
//
// StreamOrdering назначает сервер при записи (сквозной порядок поступления),
// Depth вычисляется из prev_events: max(prev.depth) + 1.
type Event struct {
	EventID        string
	RoomID         string
	Type           string
	Sender         string
	StateKey       *string
	Content        json.RawMessage
	PrevEvents     []string
	AuthEvents     []string
	Depth          int64
	OriginServerTS int64
	Origin         string
	StreamOrdering int64
	// RoomVersion — версия комнаты, по которой считаются правила redaction
	// (reference hash зависит от версии: v11 create сохраняет content целиком).
	// 0 — неизвестна, используется DefaultRoomVersion.
	RoomVersion int
	// RedactedBy — событие m.room.redaction, скрывшее content. Content уже вырезан
	// при записи redaction, оригинал - в redactions.original_content.
	RedactedBy string
	// PDU - исходный PDU события: подписанный JSON ровно в том виде, в каком он
	// уходит в федерацию (свое событие - как мы его подписали при создании, чужое -
	// как пришло). Пишется в events.pdu_json; при чтении событий клиентского API не
	// заполняется. Пусто - PDU не сохранен (федерация выключена), отдающая сторона
	// собирает его из полей.
	PDU json.RawMessage
}

// IsState сообщает, является ли событие state-событием.
func (e Event) IsState() bool {
	return e.StateKey != nil
}

// StateRef — ссылка на materialized-состояние комнаты.
type StateRef struct {
	RoomID   string
	Type     string
	StateKey string
	EventID  string
}
