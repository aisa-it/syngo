package api

import (
	"errors"
	"net/http"

	"github.com/aisa-it/syngo/internal/core"
)

// accountAuth — UIA-шаг для /account/password и /account/deactivate.
type accountAuth struct {
	Type     string `json:"type"`
	Password string `json:"password"`
	Session  string `json:"session"`
	// Identifier и устаревший User - чей пароль проверяется; должен совпадать с
	// владельцем сессии.
	Identifier *struct {
		Type string `json:"type"`
		User string `json:"user"`
	} `json:"identifier"`
	User string `json:"user"`
}

// identifiedUser - пользователь из identifier/user стадии UIA ("" - не указан).
func (a *accountAuth) identifiedUser() string {
	if a.Identifier != nil && a.Identifier.User != "" {
		return a.Identifier.User
	}

	return a.User
}

// changePasswordRequest — тело POST /account/password.
type changePasswordRequest struct {
	NewPassword   string       `json:"new_password"`
	LogoutDevices *bool        `json:"logout_devices"`
	Auth          *accountAuth `json:"auth"`
}

// deactivateRequest — тело POST /account/deactivate.
type deactivateRequest struct {
	Auth *accountAuth `json:"auth"`
}

// handleChangePassword — POST /_matrix/client/v3/account/password.
//
// UIA: m.login.password с текущим паролем. logout_devices по умолчанию true -
// остальные устройства удаляются вместе с токенами и ключами, текущая сессия
// остается (спецификация; раньше отзывался и текущий токен).
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.auth == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	var req changePasswordRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	if req.NewPassword == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "new_password is required")

		return
	}

	if !s.accountUIA(w, r, session.UserID, req.Auth) {
		return
	}

	logout := req.LogoutDevices == nil || *req.LogoutDevices

	// Без хранилища устройств (тестовая сборка) - прежнее поведение core.
	if err := s.auth.ChangePassword(r.Context(), session.UserID, req.NewPassword, logout && s.profiles == nil); err != nil {
		s.writeAuthError(w, err)

		return
	}

	if logout && s.profiles != nil {
		if err := s.deleteOtherDevices(r, session.UserID, session.DeviceID); err != nil {
			s.log.ErrorContext(r.Context(), "api: logout devices after password change", "error", err)
			writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// deleteOtherDevices удаляет все устройства пользователя, кроме keep (пустой -
// все), вместе с токенами и ключами E2EE.
func (s *Server) deleteOtherDevices(r *http.Request, userID, keep string) error {
	devices, err := s.profiles.Devices(r.Context(), userID)
	if err != nil {
		return err
	}

	for _, device := range devices {
		if device.DeviceID == keep {
			continue
		}

		if err := s.profiles.DeleteDevice(r.Context(), userID, device.DeviceID); err != nil && !errors.Is(err, core.ErrNotFound) {
			return err
		}
	}

	// Служебное устройство admin-токена в списке скрыто, но удаляется вместе с
	// остальными: иначе admin-токен пережил бы «выйти везде».
	if keep != core.AdminDeviceID {
		if err := s.profiles.DeleteDevice(r.Context(), userID, core.AdminDeviceID); err != nil && !errors.Is(err, core.ErrNotFound) {
			return err
		}
	}

	return nil
}

// handleDeactivate — POST /_matrix/client/v3/account/deactivate.
func (s *Server) handleDeactivate(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.auth == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	var req deactivateRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	if !s.accountUIA(w, r, session.UserID, req.Auth) {
		return
	}

	if err := s.auth.DeactivateAccount(r.Context(), session.UserID); err != nil {
		s.writeAuthError(w, err)

		return
	}

	// Деактивация - не только флаг: выход из комнат, удаление устройств с ключами
	// и pushers (как Synapse). Раньше членства, устройства и pushers оставались,
	// и деактивированный продолжал числиться в комнатах.
	s.cleanupDeactivated(r, session.UserID)

	writeJSON(w, http.StatusOK, map[string]any{"id_server_unbind_result": "success"})
}

// accountUIA проверяет UIA-шаг m.login.password; при отсутствии шага отдаёт 401 с flow.
func (s *Server) accountUIA(w http.ResponseWriter, r *http.Request, userID string, auth *accountAuth) bool {
	return s.passwordUIA(w, r, userID, auth)
}

// cleanupDeactivated выводит деактивированного пользователя из комнат и удаляет
// его устройства и pushers. Ошибки логируются: деактивация уже состоялась.
func (s *Server) cleanupDeactivated(r *http.Request, userID string) {
	ctx := r.Context()

	if s.memberships != nil && s.rooms != nil {
		rooms, err := s.memberships.ActiveRooms(ctx, userID)
		if err != nil {
			s.log.WarnContext(ctx, "api: deactivate: memberships", "error", err)
		}

		for _, roomID := range rooms {
			if _, err := s.rooms.Leave(ctx, userID, roomID); err != nil {
				s.log.WarnContext(ctx, "api: deactivate: leave room", "room_id", roomID, "error", err)
			}
		}

		s.eventsWritten(ctx)
	}

	if s.pushersService != nil {
		list, err := s.pushersService.List(ctx, userID)
		if err != nil {
			s.log.WarnContext(ctx, "api: deactivate: pushers", "error", err)
		}

		for i := range list {
			if err := s.pushersService.Delete(ctx, userID, list[i].AppID, list[i].Pushkey); err != nil {
				s.log.WarnContext(ctx, "api: deactivate: delete pusher", "error", err)
			}
		}
	}

	if s.profiles != nil {
		if err := s.deleteOtherDevices(r, userID, ""); err != nil {
			s.log.WarnContext(ctx, "api: deactivate: devices", "error", err)
		}
	}
}
