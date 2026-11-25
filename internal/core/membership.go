package core

import "context"

// Комнаты пользователя по членству (/joined_rooms, поиск, рассылка профиля).
// Перенесено из HTTP-обработчиков.

// MembershipStore - комнаты пользователя по членству.
type MembershipStore interface {
	// MembershipRooms - комнаты пользователя с типом членства (join, invite, leave, ban).
	MembershipRooms(ctx context.Context, userID string) ([]RoomMembership, error)
	// JoinedRoomIDs - комнаты с membership=join по возрастанию room_id.
	JoinedRoomIDs(ctx context.Context, userID string) ([]string, error)
}

// MembershipService - доменный сервис членства пользователя в комнатах.
type MembershipService struct {
	store MembershipStore
}

// NewMembershipService создает сервис поверх хранилища.
func NewMembershipService(store MembershipStore) *MembershipService {
	return &MembershipService{store: store}
}

// JoinedRooms - комнаты, где пользователь состоит (пустой список, а не nil).
func (s *MembershipService) JoinedRooms(ctx context.Context, userID string) ([]string, error) {
	rooms, err := s.store.JoinedRoomIDs(ctx, userID)
	if err != nil {
		return nil, err
	}

	if rooms == nil {
		rooms = []string{}
	}

	return rooms, nil
}

// ActiveRooms - комнаты, где пользователь состоит или приглашен (join, invite):
// из них выводится деактивированная учетная запись.
func (s *MembershipService) ActiveRooms(ctx context.Context, userID string) ([]string, error) {
	rooms, err := s.store.MembershipRooms(ctx, userID)
	if err != nil {
		return nil, err
	}

	active := make([]string, 0, len(rooms))

	for i := range rooms {
		if rooms[i].Membership == "join" || rooms[i].Membership == "invite" {
			active = append(active, rooms[i].RoomID)
		}
	}

	return active, nil
}

// joinedFromMemberships - комнаты с membership=join из полного списка членства.
func joinedFromMemberships(rooms []RoomMembership) []string {
	joined := make([]string, 0, len(rooms))

	for i := range rooms {
		if rooms[i].Membership == "join" {
			joined = append(joined, rooms[i].RoomID)
		}
	}

	return joined
}
