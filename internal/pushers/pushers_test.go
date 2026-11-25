package pushers_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/pushers"
)

func TestParseSetRequest(t *testing.T) {
	t.Parallel()

	// Создание http-pusher.
	p, remove, err := pushers.ParseSetRequest([]byte(`{"app_id":"com.example","pushkey":"k1","kind":"http","app_display_name":"App","lang":"en","data":{"url":"https://push.example/_matrix/push/v1/notify"}}`))
	if err != nil || remove {
		t.Fatalf("parse: %v remove=%v", err, remove)
	}

	if p.AppID != "com.example" || p.Kind != "http" || p.Lang != "en" {
		t.Fatalf("pusher = %+v", p)
	}

	// kind=null — удаление.
	_, remove, err = pushers.ParseSetRequest([]byte(`{"app_id":"com.example","pushkey":"k1","kind":null}`))
	if err != nil || !remove {
		t.Fatalf("удаление: err=%v remove=%v", err, remove)
	}

	// http без data.url — ошибка.
	if _, _, err := pushers.ParseSetRequest([]byte(`{"app_id":"a","pushkey":"k","kind":"http","data":{}}`)); !errors.Is(err, pushers.ErrInvalidPusher) {
		t.Fatalf("без url: %v", err)
	}

	// Неизвестный kind — ошибка.
	if _, _, err := pushers.ParseSetRequest([]byte(`{"app_id":"a","pushkey":"k","kind":"sms"}`)); !errors.Is(err, pushers.ErrInvalidPusher) {
		t.Fatalf("kind sms: %v", err)
	}

	// Без app_id — ошибка.
	if _, _, err := pushers.ParseSetRequest([]byte(`{"pushkey":"k","kind":null}`)); !errors.Is(err, pushers.ErrInvalidPusher) {
		t.Fatalf("без app_id: %v", err)
	}
}

// TestParseSetRequestLifecycle - append, обязательный kind (удаляет
// только явный null), лимиты длины app_id и pushkey.
func TestParseSetRequestLifecycle(t *testing.T) {
	t.Parallel()

	p, _, err := pushers.ParseSetRequest([]byte(`{"app_id":"a","pushkey":"k","kind":"http","append":true,"data":{"url":"https://p.example/_matrix/push/v1/notify"}}`))
	if err != nil || !p.Append {
		t.Fatalf("append: %+v %v", p, err)
	}

	if _, remove, err := pushers.ParseSetRequest([]byte(`{"app_id":"a","pushkey":"k"}`)); err == nil || remove {
		t.Fatalf("без kind: remove=%v err=%v, ожидали ошибку", remove, err)
	}

	long := strings.Repeat("a", pushers.MaxAppIDLength+1)
	if _, _, err := pushers.ParseSetRequest([]byte(`{"app_id":"` + long + `","pushkey":"k","kind":null}`)); !errors.Is(err, pushers.ErrInvalidPusher) {
		t.Fatalf("длинный app_id: %v", err)
	}

	key := strings.Repeat("k", pushers.MaxPushkeyLength+1)
	if _, _, err := pushers.ParseSetRequest([]byte(`{"app_id":"a","pushkey":"` + key + `","kind":"email","data":{}}`)); !errors.Is(err, pushers.ErrInvalidPusher) {
		t.Fatalf("длинный pushkey: %v", err)
	}

	big := strings.Repeat("x", pushers.MaxDataBytes)
	if _, _, err := pushers.ParseSetRequest([]byte(`{"app_id":"a","pushkey":"k","kind":"http","data":{"url":"https://p.example/_matrix/push/v1/notify","x":"` + big + `"}}`)); !errors.Is(err, pushers.ErrInvalidPusher) {
		t.Fatalf("большой data: %v", err)
	}
}
