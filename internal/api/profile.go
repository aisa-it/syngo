package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// profileResponse — тело GET /profile/{userId}.
type profileResponse struct {
	DisplayName string `json:"displayname,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// displayNameRequest — тело PUT /profile/{userId}/displayname.
type displayNameRequest struct {
	DisplayName string `json:"displayname"`
}

// avatarURLRequest — тело PUT /profile/{userId}/avatar_url.
type avatarURLRequest struct {
	AvatarURL string `json:"avatar_url"`
}

// devicesResponse — тело GET /devices.
type devicesResponse struct {
	Devices []deviceView `json:"devices"`
}

// deviceView — устройство в ответе /devices.
type deviceView struct {
	DeviceID    string `json:"device_id"`
	DisplayName string `json:"display_name,omitempty"`
	LastSeenIP  string `json:"last_seen_ip,omitempty"`
	LastSeenTS  *int64 `json:"last_seen_ts,omitempty"`
}

// newDeviceView конвертирует доменное устройство в ответ.
func newDeviceView(device core.DeviceInfo) deviceView {
	return deviceView{
		DeviceID:    device.DeviceID,
		DisplayName: device.DisplayName,
		LastSeenIP:  device.LastSeenIP,
		LastSeenTS:  device.LastSeenTS,
	}
}

// profileReady проверяет, что хранилище профилей сконфигурировано.
func (s *Server) profileReady(w http.ResponseWriter) bool {
	if s.profiles == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Profile storage is not configured")

		return false
	}

	return true
}

// profileVisible проверяет, можно ли показать чужой профиль. Как в Synapse:
// по умолчанию профиль любого существующего пользователя виден и без токена
// (require_auth_for_profile_requests: false; 404 только если пользователя нет);
// с auth.limit_profile_to_shared_rooms - только авторизованному при общей комнате,
// иначе 404 (аноним - 401: без сессии общую комнату не проверить). Пишет ошибку и
// возвращает false, если показывать нельзя.
func (s *Server) profileVisible(w http.ResponseWriter, r *http.Request, target string) bool {
	if !s.cfg.Auth.LimitProfileToSharedRooms {
		return true
	}

	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return false
	}

	viewer := session.UserID
	if viewer == target {
		return true
	}

	shares, err := s.profiles.SharesRoom(r.Context(), viewer, target)
	if err != nil {
		s.writeRoomError(w, err)

		return false
	}

	if !shares {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Profile not found")

		return false
	}

	return true
}

// handleProfile обрабатывает GET /profile/{userId}.
func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	if !s.profileReady(w) {
		return
	}

	userID := normalizeUserID(pathParamValue(r, "userId"))
	if userID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid user id")

		return
	}

	if !s.profileVisible(w, r, userID) {
		return
	}

	// Чужой пользователь - через /query/profile его сервера; сбой
	// федерации - 404, как отсутствующий профиль.
	profile, err := s.readProfile(r.Context(), userID)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, profile)
}

// handleDisplayName обрабатывает GET и PUT /profile/{userId}/displayname.
func (s *Server) handleDisplayName(w http.ResponseWriter, r *http.Request) {
	s.handleProfileField(w, r, "displayname")
}

// handleAvatarURL обрабатывает GET и PUT /profile/{userId}/avatar_url.
func (s *Server) handleAvatarURL(w http.ResponseWriter, r *http.Request) {
	s.handleProfileField(w, r, "avatar_url")
}

// handleProfileField — общая логика GET/PUT одного поля профиля.
func (s *Server) handleProfileField(w http.ResponseWriter, r *http.Request, field string) {
	if !s.profileReady(w) {
		return
	}

	userID := normalizeUserID(pathParamValue(r, "userId"))
	if userID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid user id")

		return
	}

	if r.Method == http.MethodGet {
		if !s.profileVisible(w, r, userID) {
			return
		}

		profile, err := s.readProfile(r.Context(), userID)
		if err != nil {
			s.writeRoomError(w, err)

			return
		}

		writeProfileField(w, field, profile)

		return
	}

	// PUT идет под requireAuth, сессия есть всегда; проверка - на случай ошибки в
	// маршрутах. Менять можно только свой профиль.
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if userID != session.UserID {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Cannot change another user's profile")

		return
	}

	var value string

	if field == "displayname" {
		var req displayNameRequest
		if !decodeBody(w, r, &req) {
			return
		}

		value = req.DisplayName
	} else {
		var req avatarURLRequest
		if !decodeBody(w, r, &req) {
			return
		}

		value = req.AvatarURL
	}

	// No-op PUT не должен рассылать membership-события.
	old, oldErr := s.profiles.Profile(r.Context(), userID)
	changed := oldErr != nil || (field == "displayname" && old.DisplayName != value) ||
		(field == "avatar_url" && old.AvatarURL != value)

	var err error
	if field == "displayname" {
		err = s.profiles.UpdateDisplayName(r.Context(), userID, value)
	} else {
		err = s.profiles.UpdateAvatarURL(r.Context(), userID, value)
	}

	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	// MSC4429: рассылка в фон, ответ клиенту не ждёт N×SendState.
	if changed {
		s.propagateProfileAsync(r.Context(), userID)
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// writeProfileField отвечает одним полем профиля (пустое - пустой строкой, как раньше).
func writeProfileField(w http.ResponseWriter, field string, profile profileResponse) {
	value := profile.AvatarURL
	if field == "displayname" {
		value = profile.DisplayName
	}

	writeJSON(w, http.StatusOK, map[string]string{field: value})
}

// handleDevices обрабатывает GET /devices.
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if !s.profileReady(w) {
		return
	}

	devices, err := s.profiles.Devices(r.Context(), session.UserID)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	resp := devicesResponse{Devices: make([]deviceView, 0, len(devices))}
	for i := range devices {
		resp.Devices = append(resp.Devices, newDeviceView(devices[i]))
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleDevice обрабатывает GET/PUT/DELETE /devices/{deviceId}.
func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if !s.profileReady(w) {
		return
	}

	deviceID := pathParamValue(r, "deviceId")

	switch r.Method {
	case http.MethodGet:
		device, err := s.profiles.Device(r.Context(), session.UserID, deviceID)
		if err != nil {
			s.writeRoomError(w, err)

			return
		}

		writeJSON(w, http.StatusOK, newDeviceView(device))
	case http.MethodPut:
		var req struct {
			DisplayName string `json:"display_name"`
		}

		if !decodeBody(w, r, &req) {
			return
		}

		if err := s.profiles.UpdateDeviceDisplayName(r.Context(), session.UserID, deviceID, req.DisplayName); err != nil {
			s.writeRoomError(w, err)

			return
		}

		writeJSON(w, http.StatusOK, map[string]any{})
	case http.MethodDelete:
		// UIA паролем, как /delete_devices: иначе украденный токен выбивал остальные
		// сессии и стирал их ключи.
		var req struct {
			Auth *accountAuth `json:"auth"`
		}

		if r.ContentLength != 0 && !decodeBody(w, r, &req) {
			return
		}

		if s.auth != nil && !s.passwordUIA(w, r, session.UserID, req.Auth) {
			return
		}

		// Удаление устройства отзывает его токены и стирает ключи E2EE.
		if err := s.profiles.DeleteDevice(r.Context(), session.UserID, deviceID); err != nil {
			s.writeRoomError(w, err)

			return
		}

		s.sendDeviceListUpdate(r.Context(), session.UserID, []string{deviceID}, true)

		writeJSON(w, http.StatusOK, map[string]any{})
	default:
		writeMatrixError(w, http.StatusMethodNotAllowed, ErrCodeUnrecognized, "Unrecognized request")
	}
}

// normalizeUserID принимает user_id как есть либо percent-encoded.
func normalizeUserID(raw string) string {
	userID := strings.TrimSpace(raw)

	// Element шлёт id в URL-кодированном виде (%40user%3Aserver) — раскодируем,
	// иначе сравнение с сессией даёт 403.
	if decoded, err := url.PathUnescape(userID); err == nil {
		userID = decoded
	}
	if !strings.HasPrefix(userID, "@") || !strings.Contains(userID, ":") {
		return ""
	}

	return userID
}

// registerProfileRoutes подключает профиль и устройства. Чтение профиля публичное
// (Synapse require_auth_for_profile_requests: false, Complement TestProfile*),
// изменение и устройства - по access token.
func (s *Server) registerProfileRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.optionalAuth)
		r.Get("/profile/{userId}", s.handleProfile)
		r.Get("/profile/{userId}/displayname", s.handleDisplayName)
		r.Get("/profile/{userId}/avatar_url", s.handleAvatarURL)
	})
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Put("/profile/{userId}/displayname", s.handleDisplayName)
		r.Put("/profile/{userId}/avatar_url", s.handleAvatarURL)
		r.Get("/devices", s.handleDevices)
		r.Get("/devices/{deviceId}", s.handleDevice)
		r.Put("/devices/{deviceId}", s.handleDevice)
		r.Delete("/devices/{deviceId}", s.handleDevice)
	})
}

// propagateProfileAsync запускает рассылку профиля в фоне со своим контекстом.
func (s *Server) propagateProfileAsync(ctx context.Context, userID string) {
	pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)

	go func() {
		defer cancel()

		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("api: profile propagation panic", "panic", rec)
			}
		}()

		s.propagateProfile(pushCtx, userID)
	}()
}

// propagateProfile отправляет m.room.member(join) с обновлённым displayname/avatar
// во все комнаты пользователя (MSC4429); логика - в
// core.ProfilePropagator.
func (s *Server) propagateProfile(ctx context.Context, userID string) {
	if s.profilePropagator == nil {
		return
	}

	s.profilePropagator.Propagate(ctx, userID)

	// Новые m.room.member записаны в фоне: будим long-poll, иначе участники
	// видели новое имя только со следующим событием.
	s.eventsWritten(ctx)
}
