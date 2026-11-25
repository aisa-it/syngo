package api

import (
	"errors"
	"net/http"

	"github.com/aisa-it/syngo/internal/core"
)

// requestErrorStatus - HTTP-статус и код Matrix для вида отказа сервиса
// (core.RequestError). Неизвестный вид - 400 M_UNKNOWN.
func requestErrorStatus(kind error) (status int, code string) {
	switch {
	case errors.Is(kind, core.ErrNotFound):
		return http.StatusNotFound, ErrCodeNotFound
	case errors.Is(kind, core.ErrMissingParam):
		return http.StatusBadRequest, ErrCodeMissingParam
	case errors.Is(kind, core.ErrInvalidParam):
		return http.StatusBadRequest, ErrCodeInvalidParam
	case errors.Is(kind, core.ErrMalformed):
		return http.StatusBadRequest, ErrCodeBadJSON
	case errors.Is(kind, core.ErrUnrecognized):
		return http.StatusBadRequest, ErrCodeUnrecognized
	case errors.Is(kind, core.ErrInvalidSignature):
		return http.StatusBadRequest, ErrCodeInvalidSignature
	case errors.Is(kind, core.ErrResourceLimit):
		return http.StatusBadRequest, ErrCodeResourceLimitExceeded
	case errors.Is(kind, core.ErrServerManaged):
		// Как Synapse: запись серверного типа account data - 405 M_BAD_JSON.
		return http.StatusMethodNotAllowed, ErrCodeBadJSON
	default:
		return http.StatusBadRequest, ErrCodeUnknown
	}
}

// writeRequestError отвечает на отказ сервиса с текстом для клиента; false -
// err не core.RequestError и ответ не записан.
func writeRequestError(w http.ResponseWriter, err error) bool {
	var reqErr *core.RequestError
	if !errors.As(err, &reqErr) {
		return false
	}

	status, code := requestErrorStatus(reqErr.Kind)
	writeMatrixError(w, status, code, reqErr.Message)

	return true
}

// requestErrorBody - тело ошибки Matrix для отказа внутри ответа (например,
// failures в /keys/signatures/upload).
func requestErrorBody(err error) map[string]string {
	var reqErr *core.RequestError
	if !errors.As(err, &reqErr) {
		return map[string]string{"errcode": ErrCodeUnknown, "error": err.Error()}
	}

	_, code := requestErrorStatus(reqErr.Kind)

	return map[string]string{"errcode": code, "error": reqErr.Message}
}
