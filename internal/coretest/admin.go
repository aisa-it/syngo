package coretest

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

// MemAdminStore - in-memory core.AdminStore: пользователи, комнаты,
// блокировки и аудит. Изменения пишут аудит, как транзакции store.
type MemAdminStore struct {
	mu     sync.Mutex
	users  map[string]*core.User // localpart -> пользователь
	rooms  map[string]core.AdminRoomRow
	blocks map[string]core.RoomBlock
	audit  []core.AdminAuditRow
	tokens map[string]string // hash токена -> user_id
	purges int
	nextID int64
	purged []string
	// Err - ошибка всех методов, кроме записи аудита (сбой хранилища).
	Err error
	// CountErr - ошибка подсчета total.
	CountErr error
}

// NewMemAdminStore создает пустое хранилище.
func NewMemAdminStore() *MemAdminStore {
	return &MemAdminStore{
		users: map[string]*core.User{}, rooms: map[string]core.AdminRoomRow{},
		blocks: map[string]core.RoomBlock{}, tokens: map[string]string{},
	}
}

// AddUser добавляет пользователя.
func (m *MemAdminStore) AddUser(user core.User) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.users[user.Localpart] = &user
}

// User - копия пользователя по localpart.
func (m *MemAdminStore) User(localpart string) core.User {
	m.mu.Lock()
	defer m.mu.Unlock()

	return *m.users[localpart]
}

// AddRoom добавляет комнату в список admin API.
func (m *MemAdminStore) AddRoom(row core.AdminRoomRow) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.rooms[row.RoomID] = row
}

// AddToken регистрирует активный admin-токен.
func (m *MemAdminStore) AddToken(token, userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.tokens[string(core.HashToken(token))] = userID
}

// Purged - удаленные комнаты.
func (m *MemAdminStore) Purged() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.purged...)
}

// AuditLog - записи аудита: action, target и details.result/reason.
func (m *MemAdminStore) AuditLog() []core.AdminAuditRow {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]core.AdminAuditRow(nil), m.audit...)
}

// Purges - число вызовов чистки аудита.
func (m *MemAdminStore) Purges() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.purges
}

func (m *MemAdminStore) writeAudit(actor, action, target string, details map[string]any) {
	out := map[string]any{"result": core.AuditResultOK}
	for k, v := range details {
		out[k] = v
	}

	raw, _ := json.Marshal(out)
	m.nextID++
	m.audit = append(m.audit, core.AdminAuditRow{
		ID: m.nextID, Actor: actor, Action: action, Target: target, Details: raw, CreatedAt: time.Now(),
	})
}

func (m *MemAdminStore) userByID(userID string) *core.User {
	for _, u := range m.users {
		if u.UserID == userID {
			return u
		}
	}

	return nil
}

func (m *MemAdminStore) admins() int {
	n := 0

	for _, u := range m.users {
		if u.IsAdmin && !u.Deactivated {
			n++
		}
	}

	return n
}

// AdminUsers - пользователи с localpart > from по возрастанию.
func (m *MemAdminStore) AdminUsers(_ context.Context, from string, limit int) ([]core.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	var out []core.User

	for _, u := range m.users {
		if u.Localpart > from {
			out = append(out, *u)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Localpart < out[j].Localpart })

	return out[:min(limit, len(out))], nil
}

// CountUsers - число пользователей.
func (m *MemAdminStore) CountUsers(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.users), m.CountErr
}

// WriteAdminAudit пишет запись аудита.
func (m *MemAdminStore) WriteAdminAudit(_ context.Context, actor, action, target string, details map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.writeAudit(actor, action, target, details)

	return nil
}

// PurgeAdminAudit считает вызовы (записи не стареют).
func (m *MemAdminStore) PurgeAdminAudit(context.Context, time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.purges++

	return nil
}

// AdminAuditPage - записи после afterID.
func (m *MemAdminStore) AdminAuditPage(_ context.Context, afterID int64, limit int) ([]core.AdminAuditRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	var out []core.AdminAuditRow

	for _, row := range m.audit {
		if row.ID > afterID && len(out) < limit {
			out = append(out, row)
		}
	}

	return out, nil
}

// CountAdminAudit - число записей аудита.
func (m *MemAdminStore) CountAdminAudit(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.audit), m.CountErr
}

// AdminRooms - комнаты с room_id > from по возрастанию.
func (m *MemAdminStore) AdminRooms(_ context.Context, from string, limit int) ([]core.AdminRoomRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	var out []core.AdminRoomRow

	for id, row := range m.rooms {
		if id > from {
			out = append(out, row)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].RoomID < out[j].RoomID })

	return out[:min(limit, len(out))], nil
}

// CountRooms - число комнат.
func (m *MemAdminStore) CountRooms(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.rooms), m.CountErr
}

// AdminSetRoomBlock ставит или снимает блокировку с аудитом.
func (m *MemAdminStore) AdminSetRoomBlock(_ context.Context, actor, roomID string, block bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return false, m.Err
	}

	_, was := m.blocks[roomID]

	if block {
		m.blocks[roomID] = core.RoomBlock{Blocked: true, BlockedBy: actor, BlockedAt: time.Now()}
	} else {
		delete(m.blocks, roomID)
	}

	action := "room_block"
	if !block {
		action = "room_unblock"
	}

	m.writeAudit(actor, action, roomID, nil)

	return was != block, nil
}

// RoomBlockInfo - блокировка комнаты.
func (m *MemAdminStore) RoomBlockInfo(_ context.Context, roomID string) (core.RoomBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return core.RoomBlock{}, m.Err
	}

	return m.blocks[roomID], nil
}

// AdminPurgeRoom удаляет комнату с аудитом; нет комнаты - ErrNotFound.
func (m *MemAdminStore) AdminPurgeRoom(_ context.Context, actor, roomID string, block bool, details map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	delete(m.rooms, roomID)
	m.purged = append(m.purged, roomID)

	if block {
		m.blocks[roomID] = core.RoomBlock{Blocked: true, BlockedBy: actor}
	}

	m.writeAudit(actor, "room_purge", roomID, details)

	return nil
}

// AdminSetUserDeactivated отключает или включает пользователя с guard последнего админа.
func (m *MemAdminStore) AdminSetUserDeactivated(_ context.Context, actor, userID string, deactivated bool) error {
	return m.updateUser(actor, userID, map[bool]string{true: "deactivate", false: "activate"}[deactivated],
		func(u *core.User) { u.Deactivated = deactivated }, deactivated)
}

// AdminSetUserAdmin выдает или снимает роль с guard последнего админа.
func (m *MemAdminStore) AdminSetUserAdmin(_ context.Context, actor, userID string, admin bool) error {
	return m.updateUser(actor, userID, map[bool]string{true: "admin", false: "unadmin"}[admin],
		func(u *core.User) { u.IsAdmin = admin }, !admin)
}

// AdminSetPasswordHash меняет хеш пароля.
func (m *MemAdminStore) AdminSetPasswordHash(_ context.Context, actor, userID, hash string) error {
	return m.updateUser(actor, userID, "password", func(u *core.User) { u.PasswordHash = hash }, false)
}

func (m *MemAdminStore) updateUser(actor, userID, action string, apply func(*core.User), demotes bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	user := m.userByID(userID)
	if user == nil {
		return core.ErrNotFound
	}

	if demotes && user.IsAdmin && !user.Deactivated && m.admins() <= 1 {
		return core.ErrLastAdmin
	}

	apply(user)
	m.writeAudit(actor, action, userID, nil)

	return nil
}

// AdminRevokeToken отзывает admin-токен с аудитом.
func (m *MemAdminStore) AdminRevokeToken(_ context.Context, actor string, tokenHash []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	userID, ok := m.tokens[string(tokenHash)]
	if !ok {
		return core.ErrInvalidToken
	}

	delete(m.tokens, string(tokenHash))
	m.writeAudit(actor, "logout", userID, nil)

	return nil
}
