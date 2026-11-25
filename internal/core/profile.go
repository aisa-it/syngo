package core

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// Profile — публичный профиль пользователя.
type Profile struct {
	UserID      string
	DisplayName string
	AvatarURL   string
}

// DeviceInfo — устройство пользователя для /devices.
type DeviceInfo struct {
	DeviceID    string
	DisplayName string
	LastSeenIP  string
	LastSeenTS  *int64
}

// ProfileStore — доступ к профилям и устройствам.
type ProfileStore interface {
	Profile(ctx context.Context, userID string) (Profile, error)
	UpdateDisplayName(ctx context.Context, userID, displayName string) error
	UpdateAvatarURL(ctx context.Context, userID, avatarURL string) error
	Devices(ctx context.Context, userID string) ([]DeviceInfo, error)
	Device(ctx context.Context, userID, deviceID string) (DeviceInfo, error)
	UpdateDeviceDisplayName(ctx context.Context, userID, deviceID, displayName string) error
	// DeleteDevice удаляет устройство вместе с его токенами и ключами.
	DeleteDevice(ctx context.Context, userID, deviceID string) error
	// SharesRoom сообщает, есть ли у двух пользователей общая комната (для приватности профиля).
	SharesRoom(ctx context.Context, first, second string) (bool, error)
}

// ProfilePropagationOptions - зависимости рассылки профиля.
type ProfilePropagationOptions struct {
	// Lock сериализует рассылку по пользователю; nil - без сериализации.
	Lock func(userID string) (unlock func())
	// OnEvent получает отправленные m.room.member для рассылки пирам.
	OnEvent func(ctx context.Context, roomID, eventID string)
	// Warn - сбой чтения комнат пользователя; Skip - комната пропущена (нет прав и т.п.).
	Warn func(ctx context.Context, msg string, err error, args ...any)
	Skip func(ctx context.Context, msg string, err error, args ...any)
}

// ProfilePropagator рассылает m.room.member(join) с обновленным displayname и
// avatar_url во все комнаты пользователя (MSC4429): иначе другие
// участники не увидят новое имя до следующего membership-события. Перенесено из
// HTTP-слоя.
type ProfilePropagator struct {
	profiles    ProfileStore
	memberships MembershipStore
	rooms       *RoomService
	opts        ProfilePropagationOptions
	now         func() time.Time
}

// NewProfilePropagator создает рассылку профиля.
func NewProfilePropagator(
	profiles ProfileStore, memberships MembershipStore, rooms *RoomService, opts ProfilePropagationOptions,
) *ProfilePropagator {
	return &ProfilePropagator{profiles: profiles, memberships: memberships, rooms: rooms, opts: opts, now: time.Now}
}

// Propagate отправляет текущий профиль userID во все комнаты с membership=join.
func (p *ProfilePropagator) Propagate(ctx context.Context, userID string) {
	memberships, err := p.memberships.MembershipRooms(ctx, userID)
	if err != nil {
		if p.opts.Warn != nil {
			p.opts.Warn(ctx, "profile propagation rooms failed", err)
		}

		return
	}

	// Сериализуем рассылку по пользователю и читаем профиль уже под замком:
	// две быстрые смены имени не должны уехать в комнаты в обратном порядке.
	if p.opts.Lock != nil {
		unlock := p.opts.Lock(userID)
		defer unlock()
	}

	profile, err := p.profiles.Profile(ctx, userID)
	if err != nil {
		return
	}

	content, err := json.Marshal(map[string]any{
		"membership":  "join",
		"displayname": profile.DisplayName,
		"avatar_url":  profile.AvatarURL,
	})
	if err != nil {
		return
	}

	for _, roomID := range joinedFromMemberships(memberships) {
		eventID, duplicate, err := p.rooms.SendState(ctx, userID, "PROFILE", roomID,
			"m.room.member", userID, "profile:"+userID+":"+roomID+":"+strconv.FormatInt(p.now().UnixNano(), 10),
			content)
		if err != nil {
			if p.opts.Skip != nil {
				p.opts.Skip(ctx, "profile propagation skipped", err, "room_id", roomID)
			}

			continue
		}

		// Профиль в комнате уже такой - нового события нет, рассылать нечего.
		if p.opts.OnEvent != nil && !duplicate {
			p.opts.OnEvent(ctx, roomID, eventID)
		}
	}
}
