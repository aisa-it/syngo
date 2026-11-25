package core

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aisa-it/syngo/internal/pushers"
)

// Pushers (/pushers). Логика регистрации перенесена из
// HTTP-обработчиков.

// ErrTooManyPushers - у пользователя уже pushers.MaxPerUser pushers.
var ErrTooManyPushers = errors.New("core: too many pushers")

// PushersStore - хранение pushers пользователя.
type PushersStore interface {
	// UpsertPusher добавляет или обновляет pusher; лимит - ErrTooManyPushers.
	UpsertPusher(ctx context.Context, pusher pushers.Pusher, maxPerUser int) error
	DeletePusher(ctx context.Context, userID, appID, pushkey string) error
	Pushers(ctx context.Context, userID string) ([]pushers.Pusher, error)
}

// PushersService - доменный сервис pushers.
type PushersService struct {
	store PushersStore
	// checkURL проверяет адрес шлюза http-pusher'а (SSRF).
	checkURL func(rawURL string) error
}

// NewPushersService создает сервис; checkURL проверяет URL шлюза до сохранения.
func NewPushersService(store PushersStore, checkURL func(rawURL string) error) *PushersService {
	return &PushersService{store: store, checkURL: checkURL}
}

// List - pushers пользователя (пустой список, а не nil).
func (s *PushersService) List(ctx context.Context, userID string) ([]pushers.Pusher, error) {
	list, err := s.store.Pushers(ctx, userID)
	if err != nil {
		return nil, err
	}

	if list == nil {
		list = []pushers.Pusher{}
	}

	return list, nil
}

// Delete удаляет pusher пользователя (деактивация, rejected от шлюза).
func (s *PushersService) Delete(ctx context.Context, userID, appID, pushkey string) error {
	return s.store.DeletePusher(ctx, userID, appID, pushkey)
}

// Set обрабатывает тело POST /pushers/set: kind=null удаляет pusher, иначе
// запись проверяется и сохраняется с привязкой к устройству (logout и удаление
// устройства удаляют pusher).
func (s *PushersService) Set(ctx context.Context, userID, deviceID string, body []byte) error {
	pusher, remove, err := pushers.ParseSetRequest(body)
	if err != nil {
		return requestError(ErrInvalidParam, "Invalid pusher: "+err.Error())
	}

	if remove {
		return s.store.DeletePusher(ctx, userID, pusher.AppID, pusher.Pushkey)
	}

	pusher.UserID = userID
	pusher.DeviceID = deviceID

	// SSRF: URL pusher'а проверяется до сохранения; путь
	// /_matrix/push/v1/notify и схема - всегда, адрес - кроме режима тестов.
	if pusher.Kind == "http" {
		var data struct {
			URL string `json:"url"`
		}

		if err := json.Unmarshal(pusher.Data, &data); err != nil {
			return requestError(ErrInvalidParam, "Invalid pusher data")
		}

		if err := s.checkURL(data.URL); err != nil {
			return requestError(ErrInvalidParam, "Unsafe push URL: "+err.Error())
		}
	}

	if err := s.store.UpsertPusher(ctx, pusher, pushers.MaxPerUser); err != nil {
		if errors.Is(err, ErrTooManyPushers) {
			return requestError(ErrResourceLimit, "Too many pushers")
		}

		return err
	}

	return nil
}
