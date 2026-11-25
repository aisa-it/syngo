package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aisa-it/syngo/internal/core"
)

// Кросс-подписи (CS API «Cross-signing»). Проверки ключей и подписей -
// в core.CrossSigningService, здесь разбор запроса, UIA и ответ.

// crossSigningUploadRequest — тело POST /keys/device_signing/upload.
type crossSigningUploadRequest struct {
	MasterKey      json.RawMessage `json:"master_key"`
	SelfSigningKey json.RawMessage `json:"self_signing_key"`
	UserSigningKey json.RawMessage `json:"user_signing_key"`
	Auth           *accountAuth    `json:"auth"`
}

// handleDeviceSigningUpload — POST /keys/device_signing/upload.
func (s *Server) handleDeviceSigningUpload(w http.ResponseWriter, r *http.Request) {
	if s.crossSigning == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	var req crossSigningUploadRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	// Замена мастер-ключа подтверждается паролем (UIA); ответ на отказ пишет accountUIA.
	confirm := func() bool { return s.accountUIA(w, r, session.UserID, req.Auth) }

	err := s.crossSigning.UploadKeys(r.Context(), session.UserID, session.DeviceID, core.CrossSigningUpload{
		MasterKey: req.MasterKey, SelfSigningKey: req.SelfSigningKey, UserSigningKey: req.UserSigningKey,
	}, confirm)

	switch {
	case err == nil:
		s.sendSigningKeyUpdate(r.Context(), session.UserID)
		writeJSON(w, http.StatusOK, map[string]any{})
	case errors.Is(err, core.ErrNotConfirmed):
	case writeRequestError(w, err):
	case errors.Is(err, core.ErrKeysRead):
		s.log.ErrorContext(r.Context(), "api: read cross-signing keys failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read keys")
	default:
		s.log.ErrorContext(r.Context(), "api: device signing upload failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to store keys")
	}
}

// handleSignaturesUpload — POST /keys/signatures/upload.
func (s *Server) handleSignaturesUpload(w http.ResponseWriter, r *http.Request) {
	if s.crossSigning == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	var body map[string]map[string]json.RawMessage
	if !decodeRoomBody(w, r, &body) {
		return
	}

	failures, err := s.crossSigning.UploadSignatures(r.Context(), session.UserID, session.DeviceID, body)

	switch {
	case err == nil:
	case errors.Is(err, core.ErrKeysRead):
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read keys")

		return
	case errors.Is(err, core.ErrDeviceKeysRead):
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read device keys")

		return
	default:
		s.log.ErrorContext(r.Context(), "api: signatures upload failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to store signatures")

		return
	}

	s.sendSignaturesUpdate(r.Context(), session.UserID, body)

	out := make(map[string]map[string]any, len(failures))

	for userID, byKey := range failures {
		out[userID] = make(map[string]any, len(byKey))
		for keyID, ferr := range byKey {
			out[userID][keyID] = requestErrorBody(ferr)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"failures": out})
}

// crossSigningQuery дополняет ответ /keys/query ключами кросс-подписи и
// подписями (подписи подмешиваются в deviceKeys). Без хранилища - пустые разделы.
func (s *Server) crossSigningQuery(
	ctx context.Context, requester string, deviceKeys map[string]map[string]json.RawMessage, users []string,
) (core.CrossSigningKeysView, error) {
	if s.crossSigning == nil {
		return core.CrossSigningKeysView{
			Master: map[string]json.RawMessage{}, SelfSigning: map[string]json.RawMessage{},
			UserSigning: map[string]json.RawMessage{},
		}, nil
	}

	return s.crossSigning.Query(ctx, requester, deviceKeys, users)
}

// logRecordKeysChange - сбой учета изменения ключей устройства: ответ не меняется,
// ключи уже сохранены.
func (s *Server) logRecordKeysChange(ctx context.Context, err error) {
	s.log.WarnContext(ctx, "api: record keys change failed", "error", err)
}
