package api

import (
	"log/slog"
	"regexp"
	"strings"
)

// Маскирование секретов в логах - вторая линия защиты поверх
// правила «не логировать заголовки и тела»: slog.HandlerOptions.ReplaceAttr
// процесса. Атрибуты с именами секретов заменяются целиком, в строках и
// ошибках маскируются access_token=..., Bearer ..., подписи X-Matrix.

// redacted - замена значения секрета.
const redacted = "[REDACTED]"

// secretLogKeys - имена атрибутов, значения которых в лог не попадают.
var secretLogKeys = map[string]struct{}{
	"authorization": {}, "access_token": {}, "refresh_token": {}, "token": {},
	"password": {}, "new_password": {}, "old_password": {}, "secret": {},
	"shared_secret": {}, "registration_shared_secret": {}, "mac": {}, "cookie": {},
	"set-cookie": {}, "body": {}, "dsn": {},
}

var (
	secretParamPattern = regexp.MustCompile(
		`(?i)((?:access_token|refresh_token|login_token|token|password|secret)=)[^&\s"']+`)
	bearerPattern  = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',]+`)
	xMatrixPattern = regexp.MustCompile(`(?i)(sig=)("[^"]*"|[^\s,]+)`)
	dsnPattern     = regexp.MustCompile(`(postgres(?:ql)?://[^:/@\s]+:)[^@\s]+@`)
)

// RedactLogString маскирует секреты в строке.
func RedactLogString(value string) string {
	value = secretParamPattern.ReplaceAllString(value, "${1}"+redacted)
	value = bearerPattern.ReplaceAllString(value, "${1}"+redacted)
	value = xMatrixPattern.ReplaceAllString(value, "${1}"+redacted)

	return dsnPattern.ReplaceAllString(value, "${1}"+redacted+"@")
}

// RedactLogAttr - ReplaceAttr для slog-хендлера процесса.
func RedactLogAttr(_ []string, attr slog.Attr) slog.Attr {
	if _, ok := secretLogKeys[strings.ToLower(attr.Key)]; ok {
		return slog.String(attr.Key, redacted)
	}

	switch attr.Value.Kind() {
	case slog.KindString:
		if value := attr.Value.String(); value != "" {
			if masked := RedactLogString(value); masked != value {
				return slog.String(attr.Key, masked)
			}
		}
	case slog.KindAny:
		if err, ok := attr.Value.Any().(error); ok && err != nil {
			text := err.Error()
			if masked := RedactLogString(text); masked != text {
				return slog.String(attr.Key, masked)
			}
		}
	default:
	}

	return attr
}
