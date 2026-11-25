package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/aisa-it/syngo/internal/core"
)

// adminLoginRequest — POST /_syngo/admin/v1/login.
type adminLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleAdminLogin выдаёт токен со scope admin и реальным TTL (ADR-0008 п.2).
// Роль проверяется ДО создания токена, чтобы не плодить сессии
// непривилегированных пользователей.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Admin.Enabled {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Admin API is disabled")

		return
	}

	if s.auth == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	var req adminLoginRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	// Тот же лимит неудач по учетке, что у CS-логина, и то же ведро: иначе пароль
	// перебирается попеременно через оба входа.
	accountKey := s.auth.LoginLimitKey(req.Username)
	if s.loginAccountLimited(w, r, accountKey) {
		return
	}

	// 1) Аутентификация без выдачи токена: проверяем пароль через Login в режиме
	// «только проверка» — для этого сначала убеждаемся, что пользователь админ.
	user, err := s.auth.UserByLogin(r.Context(), req.Username)
	if err != nil {
		if !s.recordLoginFailure(w, r, accountKey) {
			s.writeAuthError(w, core.ErrInvalidCredentials)
		}

		return
	}

	// 2) Проверяем пароль без выдачи пользовательского токена (ADR-0008 п.2):
	// VerifyCredentials не создаёт сессию, поэтому окна Login→Logout больше нет.
	if _, err := s.auth.VerifyCredentials(r.Context(), req.Username, req.Password); err != nil {
		if !errors.Is(err, core.ErrInvalidCredentials) || !s.recordLoginFailure(w, r, accountKey) {
			s.writeAuthError(w, err)
		}

		return
	}

	// Роль проверяется после пароля и с тем же ответом: раньше «Not an admin» без
	// пароля позволял перечислять пользователей и роли.
	if !user.IsAdmin {
		if !s.recordLoginFailure(w, r, accountKey) {
			s.writeAuthError(w, core.ErrInvalidCredentials)
		}

		return
	}

	// 3) Выдаём admin-токен с scope и TTL из конфига.
	result, err := s.auth.IssueToken(r.Context(), user, core.AdminDeviceID, "admin", "admin", s.cfg.Admin.TokenTTL.Std())
	if err != nil {
		s.writeAuthError(w, err)

		return
	}

	// Вход без записи в аудит не проходит: токен отзываем.
	if err := s.recordAdminAudit(r.Context(), result.UserID, "login", result.UserID,
		map[string]any{"device_id": result.DeviceID}); err != nil {
		s.log.ErrorContext(r.Context(), "api: admin login audit failed", "user_id", result.UserID, "error", err)
		_ = s.auth.Logout(r.Context(), result.AccessToken)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"admin_token": result.AccessToken,
		"user_id":     result.UserID,
		"expires_in":  s.cfg.Admin.TokenTTL.Std().Seconds(),
	})
}

// requireAdminWrite пускает на изменяющие admin-маршруты только токен со scope
// admin (read-only admin:read не должен писать).
func (s *Server) requireAdminWrite(next http.Handler) http.Handler {
	return s.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := sessionFrom(r.Context())
		if !ok || session.Scope != "admin" {
			writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Admin write scope required")

			return
		}

		next.ServeHTTP(w, r)
	}))
}

// requireAdmin — аутентификация admin-контура (ADR-0008 п.2): сам проверяет токен,
// потому что requireAuth намеренно не пускает admin-токены в CS-API.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Admin.Enabled {
			writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Admin API is disabled")

			return
		}

		if s.auth == nil || s.admin == nil {
			writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

			return
		}

		token := bearerToken(r)
		if token == "" {
			writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

			return
		}

		session, err := s.auth.WhoAmI(r.Context(), token)
		if err != nil {
			s.writeAuthError(w, err)

			return
		}

		if !adminScopeRejected(session.Scope) && session.Scope != "admin" && session.Scope != "admin:read" {
			writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Admin token required")

			return
		}

		user, err := s.auth.UserByID(r.Context(), session.UserID)
		if err != nil || !user.IsAdmin {
			writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Not an admin")

			return
		}

		ctx := context.WithValue(r.Context(), sessionCtxKey{}, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// adminUser — карточка пользователя в admin API.
type adminUser struct {
	UserID      string `json:"user_id"`
	Localpart   string `json:"localpart"`
	DisplayName string `json:"display_name"`
	Deactivated bool   `json:"deactivated"`
	IsAdmin     bool   `json:"is_admin"`
}

// handleAdminListUsers — GET /_syngo/admin/v1/users.
func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	limit := 50

	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid limit")

			return
		}

		limit = parsed
	}

	// from и next_token — непрозрачный курсор (base64 localpart), ADR-0008.
	page, err := s.admin.Users(r.Context(), r.URL.Query().Get("from"), limit)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: admin users failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	items := make([]adminUser, 0, len(page.Items))

	for _, u := range page.Items {
		items = append(items, adminUser{
			UserID: u.UserID, Localpart: u.Localpart, DisplayName: u.DisplayName,
			Deactivated: u.Deactivated, IsAdmin: u.IsAdmin,
		})
	}

	// Формат по ADR-0008: {items,total,next_token}.
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"total":      page.Total,
		"next_token": page.Next,
	})
}

// handleAdminLogout - POST /_syngo/admin/v1/logout: отзыв текущего admin-токена.
// Доступен и токену admin:read: выйти может любой admin-токен.
func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if err := s.admin.Logout(r.Context(), session.UserID, bearerToken(r)); err != nil {
		s.writeAuthError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// recordAdminAudit пишет действие в аудит (см. core.AdminService.RecordAudit).
func (s *Server) recordAdminAudit(ctx context.Context, actor, action, target string, details map[string]any) error {
	if s.admin == nil {
		return nil
	}

	return s.admin.RecordAudit(ctx, actor, action, target, details)
}

// handleAdminUserAction — POST /_syngo/admin/v1/users/{userId}/{action}.
// action: deactivate | activate | password | admin | unadmin.
func (s *Server) handleAdminUserAction(w http.ResponseWriter, r *http.Request) {
	actor, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	target := normalizeUserID(pathParamValue(r, "userId"))
	action := pathParamValue(r, "action")

	// Защита от самоблокировки: оператор не выключает себя и не снимает с себя роль.
	if err := s.admin.CheckUserAction(r.Context(), actor.UserID, target, action); err != nil {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Cannot apply this action to yourself")

		return
	}

	switch action {
	case "deactivate", "activate":
		if err := s.admin.SetUserDeactivated(r.Context(), actor.UserID, target, action == "deactivate"); err != nil {
			s.writeAdminUserError(w, err)

			return
		}

		writeJSON(w, http.StatusOK, map[string]any{})
	case "admin", "unadmin":
		if err := s.admin.SetUserAdmin(r.Context(), actor.UserID, target, action == "admin"); err != nil {
			s.writeAdminUserError(w, err)

			return
		}

		writeJSON(w, http.StatusOK, map[string]any{})
	case "password":
		var req struct {
			NewPassword string `json:"new_password"`
		}

		if !decodeRoomBody(w, r, &req) {
			return
		}

		hash, err := s.auth.HashPassword(req.NewPassword)
		if err != nil {
			s.admin.AuditFailure(r.Context(), actor.UserID, action, target, err)
			s.writeAuthError(w, err)

			return
		}

		if err := s.admin.SetUserPasswordHash(r.Context(), actor.UserID, target, hash); err != nil {
			s.writeRoomError(w, err)

			return
		}

		writeJSON(w, http.StatusOK, map[string]any{})
	default:
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Unknown action")
	}
}

// writeAdminUserError - ответ на ошибку смены роли или деактивации.
func (s *Server) writeAdminUserError(w http.ResponseWriter, err error) {
	if errors.Is(err, core.ErrLastAdmin) {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Cannot remove the last admin")

		return
	}

	s.writeRoomError(w, err)
}

// handleAdminAudit — GET /_syngo/admin/v1/audit?after_id=&limit=.
func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	afterID := int64(0)

	if raw := query.Get("after_id"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			afterID = parsed
		}
	}

	limit := 50

	if raw := query.Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}

	rows, total, next, err := s.admin.Audit(r.Context(), afterID, limit)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: admin audit failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items":      rows,
		"total":      total,
		"next_token": strconv.FormatInt(next, 10),
	})
}

// handleAdminListRooms - GET /_syngo/admin/v1/rooms?from=&limit=.
func (s *Server) handleAdminListRooms(w http.ResponseWriter, r *http.Request) {
	limit := 50

	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid limit")

			return
		}

		limit = parsed
	}

	page, err := s.admin.Rooms(r.Context(), r.URL.Query().Get("from"), limit)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: admin rooms failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	// Формат по ADR-0008: {items,total,next_token}.
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      page.Items,
		"total":      page.Total,
		"next_token": page.Next,
	})
}

// handleAdminDeleteRoom - DELETE /_syngo/admin/v1/rooms/{roomId}[?purge=true[&block=true]].
func (s *Server) handleAdminDeleteRoom(w http.ResponseWriter, r *http.Request) {
	actor, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	// По умолчанию — блокировка (как в Synapse): join запрещён, история остаётся.
	// Физическое удаление — только явным ?purge=true.
	if r.URL.Query().Get("purge") != "true" {
		s.setRoomBlock(w, r, actor.UserID, roomID, true)

		return
	}

	block := r.URL.Query().Get("block") == "true"

	notified, err := s.admin.PurgeRoom(r.Context(), actor.UserID, roomID, block)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"purged": true, "notified": notified, "blocked": block})
}

// handleAdminRoomBlockStatus - GET /_syngo/admin/v1/rooms/{roomId}/block (как в Synapse:
// {"block": bool, "user_id": кто заблокировал}).
func (s *Server) handleAdminRoomBlockStatus(w http.ResponseWriter, r *http.Request) {
	roomID, ok := pathParam(r, "roomId")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	info, err := s.admin.RoomBlockInfo(r.Context(), roomID)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	body := map[string]any{"block": info.Blocked}
	if info.Blocked {
		body["user_id"] = info.BlockedBy
	}

	writeJSON(w, http.StatusOK, body)
}

// handleAdminRoomBlock - PUT /_syngo/admin/v1/rooms/{roomId}/block {"block": bool}:
// блокировка и разблокировка комнаты, как в Synapse admin API.
func (s *Server) handleAdminRoomBlock(w http.ResponseWriter, r *http.Request) {
	actor, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	var req struct {
		Block *bool `json:"block"`
	}

	if !decodeRoomBody(w, r, &req) {
		return
	}

	if req.Block == nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "block is required")

		return
	}

	s.setRoomBlock(w, r, actor.UserID, roomID, *req.Block)
}

// setRoomBlock ставит или снимает блокировку с аудитом в той же транзакции.
func (s *Server) setRoomBlock(w http.ResponseWriter, r *http.Request, actor, roomID string, block bool) {
	if err := s.admin.SetRoomBlock(r.Context(), actor, roomID, block); err != nil {
		s.writeRoomError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"block": block, "blocked": block})
}
