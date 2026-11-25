package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Admin-контур (ADR-0008): операции над пользователями и
// комнатами, аудит. Логика перенесена из HTTP-обработчиков; вход и
// проверка admin-токена остаются за Auth.

// Результаты admin-действий в аудите: кто, что, над чем и чем кончилось.
const (
	AuditResultOK       = "ok"
	AuditResultDenied   = "denied"
	AuditResultNotFound = "not_found"
	AuditResultError    = "error"
)

// ErrAdminSelf - действие оператора над самим собой (отключить себя или снять с
// себя роль): защита от самоблокировки.
var ErrAdminSelf = errors.New("core: admin action on self")

// AdminAuditRow - запись admin_audit.
type AdminAuditRow struct {
	ID        int64           `json:"id"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Target    string          `json:"target"`
	Details   json.RawMessage `json:"details"`
	CreatedAt time.Time       `json:"created_at"`
}

// AdminRoomRow - комната в списке admin API.
type AdminRoomRow struct {
	RoomID      string `json:"room_id"`
	Creator     string `json:"creator"`
	RoomVersion string `json:"room_version"`
	IsPublic    bool   `json:"is_public"`
	Joined      int    `json:"joined_members"`
}

// RoomBlock - блокировка комнаты (admin API).
type RoomBlock struct {
	Blocked   bool
	BlockedBy string
	BlockedAt time.Time
}

// AdminStore - хранение для admin-контура. Изменяющие операции пишут аудит в
// своей транзакции: действие без записи в аудит не проходит.
type AdminStore interface {
	// AdminUsers - страница пользователей после localpart from.
	AdminUsers(ctx context.Context, from string, limit int) ([]User, error)
	CountUsers(ctx context.Context) (int, error)
	// WriteAdminAudit пишет запись аудита отдельным запросом.
	WriteAdminAudit(ctx context.Context, actor, action, target string, details map[string]any) error
	// PurgeAdminAudit удаляет записи старше retention (0 - не чистить).
	PurgeAdminAudit(ctx context.Context, retention time.Duration) error
	AdminAuditPage(ctx context.Context, afterID int64, limit int) ([]AdminAuditRow, error)
	CountAdminAudit(ctx context.Context) (int, error)
	// AdminRooms - страница комнат после room_id from.
	AdminRooms(ctx context.Context, from string, limit int) ([]AdminRoomRow, error)
	CountRooms(ctx context.Context) (int, error)
	AdminSetRoomBlock(ctx context.Context, actor, roomID string, block bool) (bool, error)
	RoomBlockInfo(ctx context.Context, roomID string) (RoomBlock, error)
	// AdminPurgeRoom удаляет комнату с историей; нет комнаты - ErrNotFound.
	AdminPurgeRoom(ctx context.Context, actor, roomID string, block bool, details map[string]any) error
	// AdminSetUserDeactivated, AdminSetUserAdmin: guard последнего админа - ErrLastAdmin.
	AdminSetUserDeactivated(ctx context.Context, actor, userID string, deactivated bool) error
	AdminSetUserAdmin(ctx context.Context, actor, userID string, admin bool) error
	AdminSetPasswordHash(ctx context.Context, actor, userID, hash string) error
	// AdminRevokeToken отзывает admin-токен; нет активного - ErrInvalidToken.
	AdminRevokeToken(ctx context.Context, actor string, tokenHash []byte, adminDeviceID string) error
}

// AdminOptions - настройки admin-сервиса.
type AdminOptions struct {
	ServerName string
	// AuditRetention - срок хранения аудита (0 - бессрочно).
	AuditRetention time.Duration
	// OnEvent получает служебные события purge (tombstone, leave) для рассылки пирам.
	OnEvent func(ctx context.Context, roomID, eventID string)
	// Warn получает некритичный сбой (чистка аудита, total, запись отказа в аудит).
	Warn func(ctx context.Context, msg string, err error, args ...any)
}

// AdminService - доменный сервис admin-контура.
type AdminService struct {
	store     AdminStore
	roomStore RoomStore
	rooms     *RoomService
	opts      AdminOptions
}

// NewAdminService создает сервис. rooms и roomStore нужны для purge (tombstone и
// leave участникам); rooms может быть nil - тогда purge никого не уведомляет.
func NewAdminService(store AdminStore, roomStore RoomStore, rooms *RoomService, opts AdminOptions) *AdminService {
	return &AdminService{store: store, roomStore: roomStore, rooms: rooms, opts: opts}
}

// Page - страница списка admin API (ADR-0008): {items,total,next_token}.
type Page[T any] struct {
	Items []T
	// Total - всего записей (0, если подсчет не удался: это не ошибка страницы).
	Total int
	Next  string
}

// Users - страница пользователей. from и Next - непрозрачный курсор
// base64(localpart), ADR-0008; limit ограничивается сотней.
func (s *AdminService) Users(ctx context.Context, from string, limit int) (Page[User], error) {
	if from != "" {
		if decoded, err := base64.RawURLEncoding.DecodeString(from); err == nil {
			from = string(decoded)
		}
	}

	users, err := s.store.AdminUsers(ctx, from, min(limit, 100))
	if err != nil {
		return Page[User]{}, err
	}

	page := Page[User]{Items: users}
	if len(users) > 0 {
		page.Next = base64.RawURLEncoding.EncodeToString([]byte(users[len(users)-1].Localpart))
	}

	if page.Total, err = s.store.CountUsers(ctx); err != nil {
		s.warn(ctx, "admin users total failed", err)
	}

	return page, nil
}

// Audit - страница аудита после afterID; Next - id последней записи.
func (s *AdminService) Audit(ctx context.Context, afterID int64, limit int) (rows []AdminAuditRow, total int, next int64, err error) {
	rows, err = s.store.AdminAuditPage(ctx, afterID, limit)
	if err != nil {
		return nil, 0, 0, err
	}

	next = afterID
	if len(rows) > 0 {
		next = rows[len(rows)-1].ID
	}

	if total, err = s.store.CountAdminAudit(ctx); err != nil {
		s.warn(ctx, "admin audit total failed", err)
	}

	return rows, total, next, nil
}

// Rooms - страница комнат; total - число всех комнат, а не размер страницы.
func (s *AdminService) Rooms(ctx context.Context, from string, limit int) (Page[AdminRoomRow], error) {
	rooms, err := s.store.AdminRooms(ctx, from, min(limit, 100))
	if err != nil {
		return Page[AdminRoomRow]{}, err
	}

	page := Page[AdminRoomRow]{Items: rooms}
	if len(rooms) > 0 {
		page.Next = rooms[len(rooms)-1].RoomID
	}

	if page.Total, err = s.store.CountRooms(ctx); err != nil {
		s.warn(ctx, "admin rooms total failed", err)
	}

	return page, nil
}

// Logout отзывает текущий admin-токен.
func (s *AdminService) Logout(ctx context.Context, actor, token string) error {
	return s.store.AdminRevokeToken(ctx, actor, HashToken(token), AdminDeviceID)
}

// RecordAudit пишет действие в аудит отдельным запросом и попутно чистит записи
// старше retention.
func (s *AdminService) RecordAudit(ctx context.Context, actor, action, target string, details map[string]any) error {
	if err := s.store.PurgeAdminAudit(ctx, s.opts.AuditRetention); err != nil {
		s.warn(ctx, "purge admin audit failed", err)
	}

	return s.store.WriteAdminAudit(ctx, actor, action, target, details)
}

// AuditFailure пишет в аудит отказ или ошибку admin-действия: успешные действия
// пишутся в транзакции самого действия, а неуспешные ничего не меняют в БД, и их
// запись - отдельный запрос. Сбой записи - в Warn.
func (s *AdminService) AuditFailure(ctx context.Context, actor, action, target string, err error) {
	result, reason := AuditResultError, "internal"

	switch {
	case errors.Is(err, ErrLastAdmin):
		result, reason = AuditResultDenied, "last_admin"
	case errors.Is(err, ErrNotFound):
		result, reason = AuditResultNotFound, "not_found"
	case errors.Is(err, ErrAdminSelf):
		result, reason = AuditResultDenied, "self"
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrNotAllowed):
		result, reason = AuditResultDenied, "forbidden"
	case errors.Is(err, ErrBadJSON), errors.Is(err, ErrWeakPassword):
		result, reason = AuditResultDenied, "invalid_request"
	}

	if auditErr := s.RecordAudit(ctx, actor, action, target,
		map[string]any{"result": result, "reason": reason}); auditErr != nil {
		s.warn(ctx, "admin audit failed", auditErr, "action", action, "target", target)
	}
}

// CheckUserAction - защита от самоблокировки: оператор не выключает себя и не
// снимает с себя роль. Отказ пишется в аудит.
func (s *AdminService) CheckUserAction(ctx context.Context, actor, target, action string) error {
	if target == actor && (action == "deactivate" || action == "unadmin") {
		s.AuditFailure(ctx, actor, action, target, ErrAdminSelf)

		return ErrAdminSelf
	}

	return nil
}

// SetUserDeactivated отключает или включает пользователя. Мутация, отзыв сессий,
// guard последнего админа и аудит - одна транзакция.
func (s *AdminService) SetUserDeactivated(ctx context.Context, actor, target string, deactivated bool) error {
	action := "activate"
	if deactivated {
		action = "deactivate"
	}

	return s.audited(ctx, actor, action, target,
		s.store.AdminSetUserDeactivated(ctx, actor, target, deactivated))
}

// SetUserAdmin выдает или снимает роль администратора (атомарно с guard
// последнего админа и аудитом).
func (s *AdminService) SetUserAdmin(ctx context.Context, actor, target string, admin bool) error {
	action := "unadmin"
	if admin {
		action = "admin"
	}

	return s.audited(ctx, actor, action, target, s.store.AdminSetUserAdmin(ctx, actor, target, admin))
}

// SetUserPasswordHash меняет хеш пароля; хеш, отзыв токенов и аудит - одна
// транзакция. Хеширует пароль Auth.
func (s *AdminService) SetUserPasswordHash(ctx context.Context, actor, target, hash string) error {
	return s.audited(ctx, actor, "password", target, s.store.AdminSetPasswordHash(ctx, actor, target, hash))
}

// RoomBlockInfo - блокировка комнаты.
func (s *AdminService) RoomBlockInfo(ctx context.Context, roomID string) (RoomBlock, error) {
	return s.store.RoomBlockInfo(ctx, roomID)
}

// SetRoomBlock ставит или снимает блокировку с аудитом в той же транзакции.
func (s *AdminService) SetRoomBlock(ctx context.Context, actor, roomID string, block bool) error {
	action := "room_block"
	if !block {
		action = "room_unblock"
	}

	_, err := s.store.AdminSetRoomBlock(ctx, actor, roomID, block)

	return s.audited(ctx, actor, action, roomID, err)
}

// PurgeRoom удаляет комнату физически: сначала tombstone и leave локальным
// участникам, чтобы участники и пиры увидели закрытие, затем удаление с аудитом
// в одной транзакции; блокировка переживает purge.
// Возвращает число отправленных служебных событий.
func (s *AdminService) PurgeRoom(ctx context.Context, actor, roomID string, block bool) (int, error) {
	if _, err := s.roomStore.RoomByID(ctx, roomID); err != nil {
		return 0, s.audited(ctx, actor, "room_purge", roomID, err)
	}

	notified := s.notifyRoomGone(ctx, roomID)

	err := s.store.AdminPurgeRoom(ctx, actor, roomID, block, map[string]any{"notified": notified})

	return notified, s.audited(ctx, actor, "room_purge", roomID, err)
}

// notifyRoomGone отправляет tombstone и leave локальным участникам перед purge.
func (s *AdminService) notifyRoomGone(ctx context.Context, roomID string) int {
	if s.rooms == nil {
		return 0
	}

	notified := 0

	// Служебные события purge пишутся и в заблокированную комнату.
	ctx = WithRoomBlockBypass(ctx)

	// Tombstone пишет создатель комнаты: у него достаточно power level.
	room, err := s.roomStore.RoomByID(ctx, roomID)
	if err == nil && room.Creator != "" {
		if eventID, _, err := s.rooms.SendState(ctx, room.Creator, AdminDeviceID, roomID,
			"m.room.tombstone", "", "purge-tombstone:"+roomID,
			json.RawMessage(`{"body":"This room has been replaced and is no longer active.","replacement_room":""}`)); err == nil {
			s.onEvent(ctx, roomID, eventID)
			notified++
		}
	}

	members, err := s.roomStore.JoinedMembers(ctx, roomID)
	if err != nil {
		return notified
	}

	for userID := range members {
		// Пропускаем удаленных участников: local leave им не нужен.
		if serverOf(userID) != s.opts.ServerName {
			continue
		}

		eventID, _, err := s.rooms.SendState(ctx, userID, AdminDeviceID, roomID,
			"m.room.member", userID, "purge-leave:"+roomID+":"+userID,
			json.RawMessage(`{"membership":"leave","reason":"room purged by admin"}`))
		if err != nil {
			continue
		}

		s.onEvent(ctx, roomID, eventID)
		notified++
	}

	return notified
}

// audited пишет отказ в аудит, если err != nil, и возвращает err.
func (s *AdminService) audited(ctx context.Context, actor, action, target string, err error) error {
	if err != nil {
		s.AuditFailure(ctx, actor, action, target, err)
	}

	return err
}

func (s *AdminService) onEvent(ctx context.Context, roomID, eventID string) {
	if s.opts.OnEvent != nil {
		s.opts.OnEvent(ctx, roomID, eventID)
	}
}

func (s *AdminService) warn(ctx context.Context, msg string, err error, args ...any) {
	if s.opts.Warn != nil {
		s.opts.Warn(ctx, msg, err, args...)
	}
}

// serverOf - домен идентификатора Matrix (часть после последнего двоеточия).
func serverOf(id string) string {
	if i := strings.LastIndexByte(id, ':'); i >= 0 {
		return id[i+1:]
	}

	return ""
}
