package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

// Account data и теги комнат. Логика перенесена
// из HTTP-обработчиков.

// AccountDataTag - тип комнатной account data с тегами комнаты.
const AccountDataTag = "m.tag"

// serverAccountDataTypes - типы account data, которыми управляет сервер: клиент
// меняет их через свои API (m.fully_read - /read_markers, m.push_rules -
// /pushrules), а не через account data. Запись и удаление - 405 M_BAD_JSON, как в
// Synapse (_check_can_set_account_data_type).
var serverAccountDataTypes = map[string]string{
	ReceiptFullyRead:     "Cannot set m.fully_read through this API. Use /rooms/{roomId}/read_markers instead.",
	AccountDataPushRules: "Cannot set m.push_rules through this API. Use /pushrules instead.",
}

// AccountDataStore - хранение account data. Нет записи - ErrNotFound.
type AccountDataStore interface {
	// AccountData - content записи (roomID == "" - глобальная).
	AccountData(ctx context.Context, userID, roomID, eventType string) ([]byte, error)
	SetAccountData(ctx context.Context, userID, roomID, eventType string, content []byte) error
	// DeleteAccountData удаляет запись (остается с content {} для /sync).
	DeleteAccountData(ctx context.Context, userID, roomID, eventType string) error
	// AccountDataChanges - записи для /sync из (since, upper]; при since == 0 и
	// для комнат fullRooms - все записи до upper, кроме удаленных.
	AccountDataChanges(
		ctx context.Context, userID string, since, upper int64, fullRooms []string,
	) (global map[string][]byte, perRoom map[string]map[string][]byte, err error)
	// SetRoomTag и DeleteRoomTag меняют один тег одним запросом.
	SetRoomTag(ctx context.Context, userID, roomID, tag string, content []byte) error
	DeleteRoomTag(ctx context.Context, userID, roomID, tag string) error
}

// AccountDataService - доменный сервис account data и тегов.
type AccountDataService struct {
	store     AccountDataStore
	pushRules *PushRulesService
}

// NewAccountDataService создает сервис; pushRules дает актуальный m.push_rules.
func NewAccountDataService(store AccountDataStore, pushRules *PushRulesService) *AccountDataService {
	return &AccountDataService{store: store, pushRules: pushRules}
}

// AccountDataKey - адрес записи: roomID == "" - глобальная.
type AccountDataKey struct {
	UserID, RoomID, EventType string
}

// CheckWritable - клиент вправе писать и удалять этот тип (иначе ErrServerManaged).
func CheckWritable(eventType string) error {
	if message, ok := serverAccountDataTypes[eventType]; ok {
		return requestError(ErrServerManaged, message)
	}

	return nil
}

// Get - content записи. m.push_rules - производное от push-правил: отдаются
// актуальные, даже если правила не менялись (как Synapse).
func (s *AccountDataService) Get(ctx context.Context, key AccountDataKey) (json.RawMessage, error) {
	if key.RoomID == "" && key.EventType == AccountDataPushRules {
		return s.pushRules.Snapshot(ctx, key.UserID)
	}

	return s.store.AccountData(ctx, key.UserID, key.RoomID, key.EventType)
}

// Set сохраняет content (любой JSON, числа - без потери точности).
func (s *AccountDataService) Set(ctx context.Context, key AccountDataKey, raw json.RawMessage) error {
	if err := CheckWritable(key.EventType); err != nil {
		return err
	}

	content, err := normalizeJSON(raw)
	if err != nil {
		return requestError(ErrMalformed, "Malformed JSON")
	}

	return s.store.SetAccountData(ctx, key.UserID, key.RoomID, key.EventType, content)
}

// Delete удаляет запись; нет записи - ErrNotFound.
func (s *AccountDataService) Delete(ctx context.Context, key AccountDataKey) error {
	if err := CheckWritable(key.EventType); err != nil {
		return err
	}

	return s.store.DeleteAccountData(ctx, key.UserID, key.RoomID, key.EventType)
}

// Tags - теги комнаты; нет записи или она не разбирается - пустой список, как Synapse.
func (s *AccountDataService) Tags(ctx context.Context, userID, roomID string) (map[string]json.RawMessage, error) {
	content, err := s.store.AccountData(ctx, userID, roomID, AccountDataTag)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	var parsed struct {
		Tags map[string]json.RawMessage `json:"tags"`
	}

	// Нет записи (content == nil) или она не разбирается - Tags остается nil.
	if json.Unmarshal(content, &parsed) != nil {
		parsed.Tags = nil
	}

	if parsed.Tags == nil {
		parsed.Tags = map[string]json.RawMessage{}
	}

	return parsed.Tags, nil
}

// SetTag добавляет или заменяет тег; content - JSON-объект (order и т.п.).
// Тег меняется одним запросом в хранилище: раньше было чтение-изменение-запись,
// и параллельные PUT теряли теги.
func (s *AccountDataService) SetTag(ctx context.Context, userID, roomID, tag string, raw json.RawMessage) error {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return requestError(ErrMalformed, "Malformed JSON")
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return requestError(ErrMalformed, "Malformed JSON")
	}

	return s.store.SetRoomTag(ctx, userID, roomID, tag, encoded)
}

// DeleteTag удаляет тег; отсутствующий тег - не ошибка (как Synapse).
func (s *AccountDataService) DeleteTag(ctx context.Context, userID, roomID, tag string) error {
	return s.store.DeleteRoomTag(ctx, userID, roomID, tag)
}

// SyncChanges - account data для /sync: изменения из (since, upper],
// при initial sync и для комнат, куда пользователь вошел после since, - все
// (как Synapse). m.push_rules при initial sync отдается всегда, дальше -
// когда правила менялись, и всегда в актуальном виде.
func (s *AccountDataService) SyncChanges(
	ctx context.Context, userID string, since, upper int64, newlyJoined map[string]bool,
) (global map[string][]byte, perRoom map[string]map[string][]byte, err error) {
	fullRooms := make([]string, 0, len(newlyJoined))
	for roomID := range newlyJoined {
		fullRooms = append(fullRooms, roomID)
	}

	global, perRoom, err = s.store.AccountDataChanges(ctx, userID, since, upper, fullRooms)
	if err != nil {
		return nil, nil, err
	}

	if global == nil {
		global = map[string][]byte{}
	}

	s.pushRules.RefreshSnapshot(ctx, userID, global, since == 0)

	return global, perRoom, nil
}

// normalizeJSON приводит JSON к компактному виду; числа сохраняются как есть.
func normalizeJSON(raw []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}

	return json.Marshal(payload)
}
