package core

import (
	"context"
	"encoding/json"
)

// Данные /sync сверх SyncDataLoader: позиция потока, state на момент выхода и на
// начало отданного timeline, очередь to-device, изменения ключей устройств и
// счетчики ключей. Перенесено из HTTP-обработчика; запросы те же и
// в том же порядке - число запросов /sync не зависит от числа комнат (ADR-0005).

// toDeviceBatch - сколько to-device сообщений отдается за один /sync.
const toDeviceBatch = 100

// ToDeviceEvent — одно to-device сообщение из очереди.
type ToDeviceEvent struct {
	ID        int64
	Type      string
	Sender    string
	Content   json.RawMessage
	StreamPos int64
}

// SyncStateStore - состояние комнат на позицию потока (одним запросом).
type SyncStateStore interface {
	// StateAtRoomsStream - состояние комнат на позиции positions.
	StateAtRoomsStream(ctx context.Context, positions map[string]int64) (map[string][]Event, error)
	// StateAtRoomsStreamMembers - то же; members != nil - m.room.member только
	// перечисленных пользователей (lazy_load_members).
	StateAtRoomsStreamMembers(ctx context.Context, positions map[string]int64, members []string) (map[string][]Event, error)
}

// SyncQueueStore - позиция потока, очередь to-device и журнал изменений ключей.
type SyncQueueStore interface {
	// SyncPosition - максимальная позиция по событиям, to-device и изменениям ключей.
	SyncPosition(ctx context.Context) (int64, error)
	PendingToDevice(ctx context.Context, userID, deviceID string, since int64, limit int) ([]ToDeviceEvent, error)
	DeleteToDevice(ctx context.Context, ids []int64) error
	DeviceListChangesFor(ctx context.Context, userID string, since, upper int64) ([]string, error)
}

// SyncKeysStore - счетчики ключей устройства для /sync.
type SyncKeysStore interface {
	OneTimeKeyCounts(ctx context.Context, userID, deviceID string) (map[string]int, error)
	UnusedFallbackKeyTypes(ctx context.Context, userID, deviceID string) ([]string, error)
}

// SyncService - доменный сервис данных /sync поверх хранилища.
type SyncService struct {
	state SyncStateStore
	queue SyncQueueStore
	keys  SyncKeysStore
}

// NewSyncService создает сервис поверх хранилищ.
func NewSyncService(state SyncStateStore, queue SyncQueueStore, keys SyncKeysStore) *SyncService {
	return &SyncService{state: state, queue: queue, keys: keys}
}

// Position - позиция синхронизации: максимум по событиям, to-device и изменениям ключей.
func (s *SyncService) Position(ctx context.Context) (int64, error) {
	return s.queue.SyncPosition(ctx)
}

// FillLeaveState - state покинутых и забаненных комнат на момент выхода, иначе
// клиент с переустановкой не отрисует комнату. Один батч-запрос;
// ошибка - state остается как есть.
func (s *SyncService) FillLeaveState(ctx context.Context, data *SyncData, userID string) {
	leavePositions := map[string]int64{}

	for _, membership := range data.Memberships {
		if (membership.Membership == "leave" || membership.Membership == "ban") && membership.LeaveStream > 0 {
			leavePositions[membership.RoomID] = membership.LeaveStream
		}
	}

	if len(leavePositions) == 0 {
		return
	}

	states, err := s.state.StateAtRoomsStream(ctx, leavePositions)
	if err != nil {
		return
	}

	for roomID, state := range states {
		data.State[roomID] = filterLeaveState(state, userID)
	}
}

// filterLeaveState оставляет в состоянии покинутой комнаты то, что нужно для
// отрисовки: name, avatar, canonical_alias, topic и членство самого пользователя.
func filterLeaveState(state []Event, userID string) []Event {
	kept := make([]Event, 0, len(state))

	for i := range state {
		ev := state[i]

		switch ev.Type {
		case "m.room.name", "m.room.avatar", "m.room.canonical_alias", "m.room.topic":
			kept = append(kept, ev)
		case "m.room.member":
			if ev.StateKey != nil && *ev.StateKey == userID {
				kept = append(kept, ev)
			}
		}
	}

	return kept
}

// FillStateAtStart - state комнат с обрезанным timeline на начало отданных
// событий, а не текущий (как в Synapse): в initial sync и для комнат, куда
// пользователь только что вошел; для остальных incremental state -
// текущий. data.Timeline загружен с perRoom+1 событием. Ошибка - state
// остается как есть.
func (s *SyncService) FillStateAtStart(
	ctx context.Context, data *SyncData, userID string, since int64, perRoom int, lazyMembers bool,
) {
	if since != 0 && len(data.NewlyJoined) == 0 {
		return
	}

	positions := make(map[string]int64, len(data.Timeline))
	// lazyPositions - join-комнаты при lazy_load_members: членство только
	// отправителей отданной порции и свое, фильтром в SQL.
	// Приглашениям и выходам нужно членство пригласившего - им полное.
	lazyPositions := map[string]int64{}
	members := []string{userID}
	seen := map[string]bool{userID: true}

	joined := make(map[string]bool, len(data.Memberships))
	for _, m := range data.Memberships {
		joined[m.RoomID] = m.Membership == "join"
	}

	for roomID, events := range data.Timeline {
		if since != 0 && !data.NewlyJoined[roomID] {
			continue
		}

		if perRoom <= 0 || len(events) <= perRoom {
			continue
		}

		pos := events[len(events)-perRoom].StreamOrdering - 1

		if !lazyMembers || !joined[roomID] {
			positions[roomID] = pos

			continue
		}

		lazyPositions[roomID] = pos

		for i := len(events) - perRoom; i < len(events); i++ {
			if !seen[events[i].Sender] {
				seen[events[i].Sender] = true
				members = append(members, events[i].Sender)
			}
		}
	}

	for _, batch := range []struct {
		positions map[string]int64
		members   []string
	}{{positions, nil}, {lazyPositions, members}} {
		if len(batch.positions) == 0 {
			continue
		}

		states, err := s.state.StateAtRoomsStreamMembers(ctx, batch.positions, batch.members)
		if err != nil {
			continue
		}

		// data.State может быть nil (лоадер не нашёл состояния) — запись в
		// nil-map паникует, поэтому создаём её при необходимости.
		if data.State == nil && len(states) > 0 {
			data.State = make(map[string][]Event, len(states))
		}

		for roomID, state := range states {
			data.State[roomID] = state
		}
	}
}

// PendingToDevice - недоставленные to-device сообщения устройства после закладки
// since (не больше 100), их id и максимальная позиция. Удаляются отдельно, после
// записи ответа клиенту: упавший ответ не теряет m.room_key.
func (s *SyncService) PendingToDevice(
	ctx context.Context, userID, deviceID string, since int64,
) (pending []ToDeviceEvent, ids []int64, maxPos int64, err error) {
	pending, err = s.queue.PendingToDevice(ctx, userID, deviceID, since, toDeviceBatch)
	if err != nil {
		return nil, nil, 0, err
	}

	ids = make([]int64, 0, len(pending))

	for i := range pending {
		ids = append(ids, pending[i].ID)

		if pending[i].StreamPos > maxPos {
			maxPos = pending[i].StreamPos
		}
	}

	return pending, ids, maxPos, nil
}

// DeleteToDevice удаляет доставленные to-device сообщения.
func (s *SyncService) DeleteToDevice(ctx context.Context, ids []int64) error {
	return s.queue.DeleteToDevice(ctx, ids)
}

// DeviceListChanges - пользователи с общими комнатами (и сам пользователь), у
// которых менялись ключи в окне (since, upper].
func (s *SyncService) DeviceListChanges(ctx context.Context, userID string, since, upper int64) ([]string, error) {
	return s.queue.DeviceListChangesFor(ctx, userID, since, upper)
}

// OneTimeKeyCounts - счетчики one-time keys устройства.
func (s *SyncService) OneTimeKeyCounts(ctx context.Context, userID, deviceID string) (map[string]int, error) {
	return s.keys.OneTimeKeyCounts(ctx, userID, deviceID)
}

// UnusedFallbackKeyTypes - алгоритмы невыданных fallback keys устройства,
// пустой список, а не nil.
func (s *SyncService) UnusedFallbackKeyTypes(ctx context.Context, userID, deviceID string) ([]string, error) {
	types, err := s.keys.UnusedFallbackKeyTypes(ctx, userID, deviceID)
	if err != nil {
		return nil, err
	}

	if types == nil {
		types = []string{}
	}

	return types, nil
}
