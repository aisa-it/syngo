package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

// Фильтры /sync (POST/GET /user/{userId}/filter). Логика перенесена из
// HTTP-обработчиков.

// FilterStore - хранение фильтров пользователя.
type FilterStore interface {
	SaveUserFilter(ctx context.Context, userID, filterID string, filter json.RawMessage) error
	// UserFilter - сохраненный фильтр; нет фильтра - ошибка.
	UserFilter(ctx context.Context, userID, filterID string) (json.RawMessage, error)
}

// FilterService - доменный сервис фильтров. Без хранилища (сервер без БД)
// фильтр получает id, но не сохраняется: Element без него не стартует.
type FilterService struct {
	store FilterStore
}

// NewFilterService создает сервис; store может быть nil.
func NewFilterService(store FilterStore) *FilterService {
	return &FilterService{store: store}
}

// Create сохраняет фильтр и возвращает его id.
func (s *FilterService) Create(ctx context.Context, userID string, filter json.RawMessage) (string, error) {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)

	filterID := hex.EncodeToString(buf)

	if s.store != nil {
		if err := s.store.SaveUserFilter(ctx, userID, filterID, filter); err != nil {
			return "", err
		}
	}

	return filterID, nil
}

// Get - сохраненный фильтр; нет фильтра или хранилища - ErrNotFound.
func (s *FilterService) Get(ctx context.Context, userID, filterID string) (json.RawMessage, error) {
	if s.store == nil {
		return nil, ErrNotFound
	}

	raw, err := s.store.UserFilter(ctx, userID, filterID)
	if err != nil {
		return nil, err
	}

	return raw, nil
}
