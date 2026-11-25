// Package pushers — реестр push-уведомлений.
//
// Спека Client-Server: GET /pushers и POST /pushers/set. Ключ — (user_id, app_id,
// pushkey); kind=null в теле удаляет pusher. Для kind=http обязателен data.url.
package pushers

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// ErrInvalidPusher — нарушение формата или обязательных полей.
var ErrInvalidPusher = errors.New("pushers: invalid pusher")

// Лимиты полей: как в спецификации и Synapse - app_id до 64
// символов, pushkey до 512 байт; остальное - чтобы запись не раздувалась.
const (
	MaxAppIDLength   = 64
	MaxPushkeyLength = 512
	MaxTextLength    = 256
	MaxDataBytes     = 4096
	// MaxPerUser - pushers на пользователя: одно событие дает не больше
	// MaxPerUser запросов на шлюзы от его имени.
	MaxPerUser = 50
)

// Pusher — запись реестра.
type Pusher struct {
	UserID            string          `json:"-"`
	AppID             string          `json:"app_id"`
	Pushkey           string          `json:"pushkey"`
	Kind              string          `json:"kind"`
	AppDisplayName    string          `json:"app_display_name"`
	DeviceDisplayName string          `json:"device_display_name"`
	ProfileTag        string          `json:"profile_tag,omitempty"`
	Lang              string          `json:"lang"`
	Data              json.RawMessage `json:"data"`
	// DeviceID - устройство, с которого pusher зарегистрирован:
	// logout и удаление устройства удаляют его pushers.
	DeviceID string `json:"-"`
	// Append - поле запроса /pushers/set: false (по умолчанию) удаляет тот же
	// app_id+pushkey у других пользователей.
	Append bool `json:"-"`
}

// Validate проверяет обязательные поля и поддерживаемые kind.
func (p *Pusher) Validate() error {
	if p.AppID == "" || p.Pushkey == "" {
		return fmt.Errorf("%w: app_id и pushkey обязательны", ErrInvalidPusher)
	}

	if err := checkLimits(p.AppID, p.Pushkey); err != nil {
		return err
	}

	for _, text := range []string{p.AppDisplayName, p.DeviceDisplayName, p.ProfileTag, p.Lang} {
		if len(text) > MaxTextLength {
			return fmt.Errorf("%w: слишком длинное поле", ErrInvalidPusher)
		}
	}

	if len(p.Data) > MaxDataBytes {
		return fmt.Errorf("%w: data больше %d байт", ErrInvalidPusher, MaxDataBytes)
	}

	switch p.Kind {
	case "http":
		var data struct {
			URL string `json:"url"`
		}

		if err := json.Unmarshal(p.Data, &data); err != nil || data.URL == "" {
			return fmt.Errorf("%w: для kind=http нужен data.url", ErrInvalidPusher)
		}
	case "email":
		// email-пуши не поддерживаем, но запись принимаем как Synapse (kind валиден).
	default:
		return fmt.Errorf("%w: kind должен быть http или email", ErrInvalidPusher)
	}

	return nil
}

// checkLimits - длина ключа pusher.
func checkLimits(appID, pushkey string) error {
	if utf8.RuneCountInString(appID) > MaxAppIDLength {
		return fmt.Errorf("%w: app_id длиннее %d символов", ErrInvalidPusher, MaxAppIDLength)
	}

	if len(pushkey) > MaxPushkeyLength {
		return fmt.Errorf("%w: pushkey длиннее %d байт", ErrInvalidPusher, MaxPushkeyLength)
	}

	return nil
}

// ParseSetRequest разбирает тело POST /pushers/set: kind=null — удаление.
func ParseSetRequest(body []byte) (Pusher, bool, error) {
	var raw struct {
		AppID             string          `json:"app_id"`
		Pushkey           string          `json:"pushkey"`
		Kind              json.RawMessage `json:"kind"`
		AppDisplayName    string          `json:"app_display_name"`
		DeviceDisplayName string          `json:"device_display_name"`
		ProfileTag        string          `json:"profile_tag"`
		Lang              string          `json:"lang"`
		Data              json.RawMessage `json:"data"`
		Append            bool            `json:"append"`
	}

	if err := json.Unmarshal(body, &raw); err != nil {
		return Pusher{}, false, fmt.Errorf("%w: bad json", ErrInvalidPusher)
	}

	if raw.AppID == "" || raw.Pushkey == "" {
		return Pusher{}, false, fmt.Errorf("%w: app_id и pushkey обязательны", ErrInvalidPusher)
	}

	// Удаляет только явный "kind": null (спецификация); без kind - ошибка, а не
	// молчаливое удаление.
	if len(raw.Kind) == 0 {
		return Pusher{}, false, fmt.Errorf("%w: kind обязателен", ErrInvalidPusher)
	}

	if string(raw.Kind) == "null" {
		if err := checkLimits(raw.AppID, raw.Pushkey); err != nil {
			return Pusher{}, false, err
		}

		return Pusher{AppID: raw.AppID, Pushkey: raw.Pushkey}, true, nil
	}

	var kind string
	if err := json.Unmarshal(raw.Kind, &kind); err != nil {
		return Pusher{}, false, fmt.Errorf("%w: kind должен быть строкой", ErrInvalidPusher)
	}

	pusher := Pusher{
		AppID: raw.AppID, Pushkey: raw.Pushkey, Kind: kind,
		AppDisplayName: raw.AppDisplayName, DeviceDisplayName: raw.DeviceDisplayName,
		ProfileTag: raw.ProfileTag, Lang: raw.Lang, Data: raw.Data, Append: raw.Append,
	}

	if err := pusher.Validate(); err != nil {
		return Pusher{}, false, err
	}

	return pusher, false, nil
}
