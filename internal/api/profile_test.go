package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// memProfileStore — in-memory реализация core.ProfileStore для HTTP-тестов.
type memProfileStore struct {
	profiles map[string]core.Profile
	devices  map[string][]core.DeviceInfo
	tokens   map[string]bool // "user/device" -> есть активный токен
	shares   map[string]bool // "a/b"
}

func newMemProfileStore() *memProfileStore {
	return &memProfileStore{
		profiles: map[string]core.Profile{},
		devices:  map[string][]core.DeviceInfo{},
		tokens:   map[string]bool{},
		shares:   map[string]bool{},
	}
}

func (m *memProfileStore) Profile(_ context.Context, userID string) (core.Profile, error) {
	profile, ok := m.profiles[userID]
	if !ok {
		return core.Profile{}, fmt.Errorf("profile: %w", core.ErrNotFound)
	}

	return profile, nil
}

func (m *memProfileStore) UpdateDisplayName(_ context.Context, userID, name string) error {
	profile, ok := m.profiles[userID]
	if !ok {
		return fmt.Errorf("profile: %w", core.ErrNotFound)
	}

	profile.DisplayName = name
	m.profiles[userID] = profile

	return nil
}

func (m *memProfileStore) UpdateAvatarURL(_ context.Context, userID, url string) error {
	profile, ok := m.profiles[userID]
	if !ok {
		return fmt.Errorf("profile: %w", core.ErrNotFound)
	}

	profile.AvatarURL = url
	m.profiles[userID] = profile

	return nil
}

func (m *memProfileStore) Devices(_ context.Context, userID string) ([]core.DeviceInfo, error) {
	return m.devices[userID], nil
}

func (m *memProfileStore) Device(_ context.Context, userID, deviceID string) (core.DeviceInfo, error) {
	for _, device := range m.devices[userID] {
		if device.DeviceID == deviceID {
			return device, nil
		}
	}

	return core.DeviceInfo{}, fmt.Errorf("device: %w", core.ErrNotFound)
}

func (m *memProfileStore) UpdateDeviceDisplayName(_ context.Context, userID, deviceID, name string) error {
	devices := m.devices[userID]

	for i := range devices {
		if devices[i].DeviceID == deviceID {
			devices[i].DisplayName = name
			m.devices[userID] = devices

			return nil
		}
	}

	return fmt.Errorf("device: %w", core.ErrNotFound)
}

func (m *memProfileStore) DeleteDevice(_ context.Context, userID, deviceID string) error {
	devices := m.devices[userID]

	for i := range devices {
		if devices[i].DeviceID != deviceID {
			continue
		}

		m.devices[userID] = append(devices[:i], devices[i+1:]...)
		delete(m.tokens, userID+"/"+deviceID)

		return nil
	}

	return fmt.Errorf("device: %w", core.ErrNotFound)
}

func (m *memProfileStore) SharesRoom(_ context.Context, first, second string) (bool, error) {
	return m.shares[first+"/"+second] || m.shares[second+"/"+first], nil
}

// newProfileServer собирает сервер с профилями на in-memory хранилище.
func newProfileServer(t *testing.T) (*Server, *memProfileStore) {
	t.Helper()

	server := newAuthServer(t, core.AuthOptions{})
	store := newMemProfileStore()
	server.profiles = store

	return server, store
}

// TestProfileUpdate — смена displayname и avatar_url своего профиля.
func TestProfileUpdate(t *testing.T) {
	t.Parallel()

	server, store := newProfileServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	store.profiles[alice.UserID] = core.Profile{UserID: alice.UserID, DisplayName: "Alice"}

	rec := do(t, server, http.MethodPut,
		"/_matrix/client/v3/profile/"+alice.UserID+"/displayname",
		`{"displayname":"Alice New"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT displayname: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/profile/"+alice.UserID+"/displayname", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET displayname: status %d", rec.Code)
	}

	if body := rec.Body.String(); body != "{\"displayname\":\"Alice New\"}\n" {
		t.Fatalf("displayname = %s", body)
	}

	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/profile/"+alice.UserID+"/avatar_url",
		`{"avatar_url":"mxc://test.local/ava"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT avatar_url: status %d", rec.Code)
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/profile/"+alice.UserID, "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET profile: status %d", rec.Code)
	}

	if body := rec.Body.String(); body != "{\"displayname\":\"Alice New\",\"avatar_url\":\"mxc://test.local/ava\"}\n" {
		t.Fatalf("profile = %s", body)
	}
}

// TestProfileVisibleWithoutSharedRoom — по умолчанию (как в Synapse) чужой профиль
// виден без общей комнаты: Element проверяет адресата приглашения через GET /profile
// и на 404 считает его несуществующим. Несуществующий пользователь - 404.
func TestProfileVisibleWithoutSharedRoom(t *testing.T) {
	t.Parallel()

	server, store := newProfileServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")
	store.profiles[bob.UserID] = core.Profile{UserID: bob.UserID, DisplayName: "Bob"}

	for _, path := range []string{"", "/displayname", "/avatar_url"} {
		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/profile/"+bob.UserID+path, "", alice.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET profile%s без общей комнаты: status %d, ожидали 200", path, rec.Code)
		}
	}

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/profile/@nobody:test.local", "", alice.AccessToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("профиль несуществующего: status %d, ожидали 404", rec.Code)
	}
}

// TestProfilePublicRead - GET профиля доступен без токена (Synapse
// require_auth_for_profile_requests: false, Complement TestProfileDisplayName и
// TestProfileAvatarURL); PUT без токена - 401, невалидный токен на GET - 401.
// С limit_profile_to_shared_rooms аноним получает 401.
func TestProfilePublicRead(t *testing.T) {
	t.Parallel()

	server, store := newProfileServer(t)
	bob := registerUser(t, server, "bob", "correct horse battery")
	store.profiles[bob.UserID] = core.Profile{UserID: bob.UserID, DisplayName: "Bob", AvatarURL: "mxc://test.local/b"}

	for _, path := range []string{"", "/displayname", "/avatar_url"} {
		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/profile/"+bob.UserID+path, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("анонимный GET profile%s: status %d, body %s", path, rec.Code, rec.Body.String())
		}
	}

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/profile/"+bob.UserID, "", "bogus-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET с невалидным токеном: status %d, ожидали 401", rec.Code)
	}

	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/profile/"+bob.UserID+"/displayname",
		`{"displayname":"anon"}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("анонимный PUT: status %d, ожидали 401", rec.Code)
	}

	limited, limitedStore := newProfileServer(t)
	limited.cfg.Auth.LimitProfileToSharedRooms = true
	carol := registerUser(t, limited, "carol", "correct horse battery")
	limitedStore.profiles[carol.UserID] = core.Profile{UserID: carol.UserID, DisplayName: "Carol"}

	rec = do(t, limited, http.MethodGet, "/_matrix/client/v3/profile/"+carol.UserID, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("анонимный GET при limit_profile_to_shared_rooms: status %d, ожидали 401", rec.Code)
	}
}

// TestProfilePrivacy — с auth.limit_profile_to_shared_rooms чужой профиль без общей
// комнаты недоступен на всех ручках профиля.
func TestProfilePrivacy(t *testing.T) {
	t.Parallel()

	server, store := newProfileServer(t)
	server.cfg.Auth.LimitProfileToSharedRooms = true
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	store.profiles[alice.UserID] = core.Profile{UserID: alice.UserID, DisplayName: "Alice"}
	store.profiles[bob.UserID] = core.Profile{UserID: bob.UserID, DisplayName: "Bob"}

	for _, path := range []string{"", "/displayname", "/avatar_url"} {
		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/profile/"+bob.UserID+path, "", alice.AccessToken)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("чужой профиль%s без комнаты: status %d, ожидали 404", path, rec.Code)
		}
	}

	store.shares[alice.UserID+"/"+bob.UserID] = true

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/profile/"+bob.UserID, "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("профиль при общей комнате: status %d", rec.Code)
	}

	// Чужой профиль нельзя менять даже при общей комнате.
	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/profile/"+bob.UserID+"/displayname",
		`{"displayname":"hacked"}`, alice.AccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT чужого профиля: status %d, ожидали 403", rec.Code)
	}
}

// TestDevices — список, переименование и удаление устройства с инвалидацией токена.
func TestDevices(t *testing.T) {
	t.Parallel()

	server, store := newProfileServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	store.devices[alice.UserID] = []core.DeviceInfo{
		{DeviceID: alice.DeviceID, DisplayName: "phone"},
		{DeviceID: "SECOND", DisplayName: "laptop"},
	}
	store.tokens[alice.UserID+"/SECOND"] = true

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/devices", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET devices: status %d", rec.Code)
	}

	if body := rec.Body.String(); !contains(body, "SECOND") || !contains(body, "laptop") {
		t.Fatalf("devices = %s", body)
	}

	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/devices/SECOND",
		`{"display_name":"workstation"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT device: status %d", rec.Code)
	}

	// Без UIA - 401 с потоком m.login.password.
	rec = do(t, server, http.MethodDelete, "/_matrix/client/v3/devices/SECOND", "", alice.AccessToken)
	if rec.Code != http.StatusUnauthorized || !contains(rec.Body.String(), `"stages":["m.login.password"]`) {
		t.Fatalf("DELETE device без UIA: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodDelete, "/_matrix/client/v3/devices/SECOND",
		`{"auth":{"type":"m.login.password","password":"correct horse battery"}}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE device: status %d %s", rec.Code, rec.Body.String())
	}

	if store.tokens[alice.UserID+"/SECOND"] {
		t.Fatal("токен удалённого устройства должен быть отозван")
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/devices/SECOND", "", alice.AccessToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET удалённого устройства: status %d, ожидали 404", rec.Code)
	}
}

// contains — простая проверка подстроки (без импорта strings ради одной строки).
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}

// TestDeleteDeviceUIAOtherUser - UIA паролем другого пользователя не удаляет
// устройство: 403 (Complement TestDeviceManagement).
func TestDeleteDeviceUIAOtherUser(t *testing.T) {
	t.Parallel()

	server, _ := newProfileServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	registerUser(t, server, "bob", "bobspassword1")

	body := `{"auth":{"type":"m.login.password","identifier":{"type":"m.id.user","user":"@bob:test.local"},"password":"bobspassword1"}}`

	rec := do(t, server, http.MethodDelete, "/_matrix/client/v3/devices/"+alice.DeviceID, body, alice.AccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DELETE с паролем bob: %d %s, ожидали 403", rec.Code, rec.Body.String())
	}

	// Свой identifier (localpart в другом регистре) проходит дальше проверки.
	body = `{"auth":{"type":"m.login.password","identifier":{"type":"m.id.user","user":"ALICE"},"password":"wrong"}}`

	rec = do(t, server, http.MethodDelete, "/_matrix/client/v3/devices/"+alice.DeviceID, body, alice.AccessToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("DELETE со своим identifier и неверным паролем: %d %s, ожидали 401", rec.Code, rec.Body.String())
	}
}
