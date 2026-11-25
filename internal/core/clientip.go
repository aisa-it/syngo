package core

import "context"

// clientIPKey - ключ IP клиента в контексте запроса.
type clientIPKey struct{}

// WithClientIP кладет в контекст IP клиента, определенный HTTP-слоем с учетом
// доверенных прокси. Его читают аудит admin и логи: в них должен попадать адрес
// клиента, а не TLS-прокси перед сервисом.
func WithClientIP(ctx context.Context, ip string) context.Context {
	if ip == "" {
		return ctx
	}

	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFrom возвращает IP клиента из контекста; false - запрос пришел не
// через HTTP-слой (фоновые задачи, тесты core).
func ClientIPFrom(ctx context.Context) (string, bool) {
	ip, ok := ctx.Value(clientIPKey{}).(string)

	return ip, ok && ip != ""
}
