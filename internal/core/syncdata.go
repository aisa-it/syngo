package core

import (
	"context"
	"fmt"
)

// RoomMembership — членство пользователя в комнате (для /sync).
type RoomMembership struct {
	RoomID     string
	Membership string
	// LeaveStream — stream_ordering события, которым членство стало leave/ban.
	// Для leave/ban события комнаты отдаются только до этой позиции и один раз:
	// иначе вышедший/забаненный продолжает получать переписку.
	LeaveStream int64
	// Forgotten - комната забыта (POST /forget): initial sync ее не отдает, как
	// Synapse.
	Forgotten bool
	// Sender - отправитель события членства: sliding sync показывает выход по
	// кику (leave от другого), а свой выход - только в окне.
	Sender string
}

// SyncDataStore — батч-доступ к данным /sync. Контракт из ADR-0005 §5: фиксированное
// число запросов независимо от числа комнат, никаких запросов «на комнату».
type SyncDataStore interface {
	// MembershipRooms отдаёт комнаты пользователя одним запросом.
	MembershipRooms(ctx context.Context, userID string) ([]RoomMembership, error)
	// EventsSince отдаёт события по списку комнат после позиции since, не больше
	// perRoom событий на комнату (одним запросом). upper ограничивает выборку по
	// комнате сверху: для leave/ban это позиция события выхода, чтобы переписка
	// после него не попадала в ответ даже при ошибке в хендлере.
	EventsSince(ctx context.Context, roomIDs []string, since int64, perRoom int,
		upper map[string]int64) (map[string][]Event, error)
	// StateForRooms отдаёт текущее состояние комнат из room_state (одним запросом).
	StateForRooms(ctx context.Context, roomIDs []string) (map[string][]Event, error)
	// StateDeltaForRooms отдает из текущего состояния комнат только события с
	// позицией после since и m.room.member перечисленных пользователей (одним
	// запросом): для incremental sync, где снапшот у клиента уже есть.
	StateDeltaForRooms(ctx context.Context, roomIDs []string, since int64, members []string) (map[string][]Event, error)
	// StateMembersForRooms - текущее состояние комнат без m.room.member, кроме
	// членства перечисленных пользователей (одним запросом): initial sync с
	// lazy_load_members не читает тысячи событий членства.
	StateMembersForRooms(ctx context.Context, roomIDs, members []string) (map[string][]Event, error)
}

// SyncLoadOptions - параметры загрузки из фильтра клиента.
type SyncLoadOptions struct {
	// LazyMembers - фильтр просит lazy_load_members: состояние join-комнат при
	// полной загрузке берется только с членством отправителей порции и самого
	// пользователя, фильтром в SQL.
	LazyMembers bool
}

// SyncData — загруженный срез данных для будущего ответа /sync.
type SyncData struct {
	Memberships []RoomMembership
	Timeline    map[string][]Event
	State       map[string][]Event
	// NewlyJoined — комнаты, куда пользователь вошел после since: для них timeline
	// загружен как при первом sync (последние события, а не только после since).
	NewlyJoined map[string]bool
}

// SyncDataLoader собирает данные /sync фиксированным числом запросов (3-5).
//
// Это слой данных: он не формирует HTTP-ответ и не реализует long-poll. Вынесен
// отдельно, чтобы стенд (cmd/syncload) измерял ровно то, что пойдёт в обработчик,
// и чтобы число запросов проверялось тестом.
type SyncDataLoader struct {
	store SyncDataStore
}

// NewSyncDataLoader создаёт загрузчик поверх хранилища.
func NewSyncDataLoader(store SyncDataStore) *SyncDataLoader {
	return &SyncDataLoader{store: store}
}

// Load собирает данные для пользователя: членства, события после since и состояние
// комнат, где есть новые события.
func (l *SyncDataLoader) Load(ctx context.Context, userID string, since int64, perRoomLimit int) (SyncData, error) {
	return l.LoadWith(ctx, userID, since, perRoomLimit, SyncLoadOptions{})
}

// LoadWith - Load с параметрами фильтра.
func (l *SyncDataLoader) LoadWith(
	ctx context.Context, userID string, since int64, perRoomLimit int, opts SyncLoadOptions,
) (SyncData, error) {
	if perRoomLimit <= 0 {
		perRoomLimit = 20
	}

	memberships, err := l.store.MembershipRooms(ctx, userID)
	if err != nil {
		return SyncData{}, fmt.Errorf("sync: membership rooms: %w", err)
	}

	// Забытые комнаты в initial sync не попадают; incremental отдает выход, если
	// он в окне, - другие устройства узнают о нем (Complement TestRoomForget).
	if since == 0 {
		kept := memberships[:0]

		for i := range memberships {
			if !memberships[i].Forgotten || memberships[i].Membership == "join" {
				kept = append(kept, memberships[i])
			}
		}

		memberships = kept
	}

	if len(memberships) == 0 {
		return SyncData{Memberships: memberships, Timeline: map[string][]Event{}, State: map[string][]Event{}}, nil
	}

	roomIDs := make([]string, 0, len(memberships))
	for i := range memberships {
		roomIDs = append(roomIDs, memberships[i].RoomID)
	}

	// Верхняя граница по комнате: leave/ban отдают события только до выхода.
	upper := make(map[string]int64, len(memberships))

	for i := range memberships {
		if memberships[i].LeaveStream > 0 && memberships[i].Membership != "join" {
			upper[memberships[i].RoomID] = memberships[i].LeaveStream
		}
	}

	timeline, err := l.store.EventsSince(ctx, roomIDs, since, perRoomLimit, upper)
	if err != nil {
		return SyncData{}, fmt.Errorf("sync: events since %d: %w", since, err)
	}

	// Комната, в которую пользователь вошел после since, отдается как при первом
	// sync: последние perRoom событий с limited и prev_batch. Иначе клиент получал
	// одно событие входа и считал, что истории нет: терялись сообщения между
	// приглашением и входом - так Element не видел запрос верификации в новом
	// личном чате. Как newly_joined_rooms в Synapse.
	newlyJoined := map[string]bool{}

	if since > 0 {
		fresh := make([]string, 0)

		for i := range memberships {
			if memberships[i].Membership == "join" && memberships[i].LeaveStream > since {
				fresh = append(fresh, memberships[i].RoomID)
				newlyJoined[memberships[i].RoomID] = true
			}
		}

		if len(fresh) > 0 {
			full, err := l.store.EventsSince(ctx, fresh, 0, perRoomLimit, nil)
			if err != nil {
				return SyncData{}, fmt.Errorf("sync: newly joined rooms: %w", err)
			}

			for roomID, events := range full {
				timeline[roomID] = events
			}
		}
	}

	// Состояние запрашиваем только для комнат, где есть изменения: для остальных
	// клиент уже имеет актуальный снапшот из предыдущего /sync.
	//
	// Полный снапшот нужен в initial sync, в комнатах, куда пользователь только
	// что вошел, и в приглашениях/выходах (invite_state, state на момент выхода).
	// Для остальных join-комнат incremental sync отдает только изменения после
	// since и члены отправителей порции (для lazy_load_members): раньше на каждый
	// sync уходил полный room_state изменившейся комнаты - 2000 событий членства
	// на одно сообщение в большой комнате.
	kinds := make(map[string]string, len(memberships))
	for i := range memberships {
		kinds[memberships[i].RoomID] = memberships[i].Membership
	}

	full := make([]string, 0, len(timeline))
	lazy := make([]string, 0, len(timeline))
	delta := make([]string, 0, len(timeline))
	senders := map[string]struct{}{userID: {}}
	lazySenders := map[string]struct{}{userID: {}}

	for roomID, events := range timeline {
		if since > 0 && kinds[roomID] == "join" && !newlyJoined[roomID] {
			delta = append(delta, roomID)

			for i := range events {
				senders[events[i].Sender] = struct{}{}
			}

			continue
		}

		// Приглашениям и выходам нужно членство пригласившего и свое - для них
		// полное состояние, как раньше.
		if opts.LazyMembers && kinds[roomID] == "join" {
			lazy = append(lazy, roomID)

			for i := range events {
				lazySenders[events[i].Sender] = struct{}{}
			}

			continue
		}

		full = append(full, roomID)
	}

	state := map[string][]Event{}

	if len(full) > 0 {
		state, err = l.store.StateForRooms(ctx, full)
		if err != nil {
			return SyncData{}, fmt.Errorf("sync: state for rooms: %w", err)
		}
	}

	if len(lazy) > 0 {
		lazyState, err := l.store.StateMembersForRooms(ctx, lazy, setKeys(lazySenders))
		if err != nil {
			return SyncData{}, fmt.Errorf("sync: lazy state for rooms: %w", err)
		}

		for roomID, events := range lazyState {
			state[roomID] = events
		}
	}

	if len(delta) > 0 {
		changes, err := l.store.StateDeltaForRooms(ctx, delta, since, setKeys(senders))
		if err != nil {
			return SyncData{}, fmt.Errorf("sync: state delta for rooms: %w", err)
		}

		for roomID, events := range changes {
			state[roomID] = events
		}
	}

	return SyncData{Memberships: memberships, Timeline: timeline, State: state, NewlyJoined: newlyJoined}, nil
}

// setKeys - ключи множества строк.
func setKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}

	return out
}
