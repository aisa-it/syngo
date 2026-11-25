package core

import "errors"

// Виды ошибок входных данных сервисов. Транспорт отображает их в
// коды Matrix: сервис решает, что запрос плохой, а HTTP-слой - как об этом сказать.
var (
	// ErrMissingParam - не передан обязательный параметр (M_MISSING_PARAM).
	ErrMissingParam = errors.New("core: missing parameter")
	// ErrInvalidParam - параметр передан, но недопустим (M_INVALID_PARAM).
	ErrInvalidParam = errors.New("core: invalid parameter")
	// ErrMalformed - данные не разбираются по схеме (M_BAD_JSON).
	ErrMalformed = errors.New("core: malformed data")
	// ErrUnrecognized - неизвестное значение перечисления (M_UNRECOGNIZED).
	ErrUnrecognized = errors.New("core: unrecognized value")
	// ErrInvalidSignature - подпись не проверяется (M_INVALID_SIGNATURE).
	ErrInvalidSignature = errors.New("core: invalid signature")
	// ErrResourceLimit - превышен лимит ресурса (M_RESOURCE_LIMIT_EXCEEDED).
	ErrResourceLimit = errors.New("core: resource limit exceeded")
	// ErrServerManaged - данными управляет сервер, клиент их напрямую не меняет.
	ErrServerManaged = errors.New("core: managed by the server")
)

// RequestError - отказ сервиса с текстом для клиента. Kind - один из видов выше
// или ErrNotFound; Message уходит в поле error ответа без изменений.
type RequestError struct {
	Kind    error
	Message string
}

// Error реализует error.
func (e *RequestError) Error() string {
	return e.Kind.Error() + ": " + e.Message
}

// Unwrap дает errors.Is(err, ErrMissingParam) и т.п.
func (e *RequestError) Unwrap() error {
	return e.Kind
}

// requestError - короткая запись отказа сервиса.
func requestError(kind error, message string) error {
	return &RequestError{Kind: kind, Message: message}
}
