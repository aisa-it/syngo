package core

import (
	"context"
	"encoding/json"
)

// To-device сообщения и изменения ключей устройств (/sendToDevice, /keys/changes).
// Логика перенесена из HTTP-обработчиков.

// DeviceTarget - адрес доставки to-device сообщения: устройство получателя.
type DeviceTarget struct {
	UserID   string
	DeviceID string
}

// ToDeviceStore - очередь to-device сообщений и журнал изменений ключей устройств.
type ToDeviceStore interface {
	// SendToDevice пишет сообщения в очереди устройств; повтор txnID -
	// alreadyProcessed=true без записи.
	SendToDevice(ctx context.Context, userID, deviceID, eventType, txnID string,
		messages map[DeviceTarget]json.RawMessage) (alreadyProcessed bool, err error)
	// DeviceListChangesFor - пользователи с общими комнатами, у которых менялись
	// ключи в окне (since, upper].
	DeviceListChangesFor(ctx context.Context, userID string, since, upper int64) ([]string, error)
}

// FederatedToDeviceStore - запись to-device сообщений вместе с EDU
// m.direct_to_device для серверов получателей одной транзакцией.
type FederatedToDeviceStore interface {
	// SendToDeviceFederated - как SendToDevice, плюс edus: destination -> EDU.
	SendToDeviceFederated(ctx context.Context, userID, deviceID, eventType, txnID string,
		messages map[DeviceTarget]json.RawMessage, edus map[string][]byte) (alreadyProcessed bool, err error)
}

// ToDeviceService - доменный сервис to-device сообщений.
type ToDeviceService struct {
	store ToDeviceStore
	// serverName - свой сервер; получатели других серверов уходят им EDU, если
	// хранилище это умеет (federated), иначе отбрасываются.
	serverName string
	federated  FederatedToDeviceStore
	newID      func() string
}

// WithFederation включает доставку to-device пользователям других серверов
// (m.direct_to_device через outbox). newID - message_id EDU.
func (s *ToDeviceService) WithFederation(serverName string, store FederatedToDeviceStore, newID func() string) *ToDeviceService {
	s.serverName, s.federated, s.newID = serverName, store, newID

	return s
}

// NewToDeviceService создает сервис поверх хранилища.
func NewToDeviceService(store ToDeviceStore) *ToDeviceService {
	return &ToDeviceService{store: store}
}

// Send ставит сообщения messages (получатель -> устройство -> content) в очереди
// устройств. queued=false - адресатов нет и ничего не записано: long-poll будить
// не нужно.
func (s *ToDeviceService) Send(
	ctx context.Context,
	userID, deviceID, eventType, txnID string,
	messages map[string]map[string]json.RawMessage,
) (queued bool, err error) {
	targets := map[DeviceTarget]json.RawMessage{}
	remote := map[string]map[string]map[string]json.RawMessage{}

	for target, byDevice := range messages {
		// Получатель другого сервера раньше писался в локальную очередь и не
		// доставлялся никому: теперь - EDU его серверу.
		if s.serverName != "" && IsRemoteUser(target, s.serverName) {
			if s.federated == nil || len(byDevice) == 0 {
				continue
			}

			server := userDomain(target)
			if remote[server] == nil {
				remote[server] = map[string]map[string]json.RawMessage{}
			}

			remote[server][target] = byDevice

			continue
		}

		for device, content := range byDevice {
			targets[DeviceTarget{UserID: target, DeviceID: device}] = content
		}
	}

	if len(targets) == 0 && len(remote) == 0 {
		return false, nil
	}

	if len(remote) == 0 {
		if _, err := s.store.SendToDevice(ctx, userID, deviceID, eventType, txnID, targets); err != nil {
			return false, err
		}

		return true, nil
	}

	edus := make(map[string][]byte, len(remote))

	for server, byUser := range remote {
		edu, err := json.Marshal(map[string]any{
			"edu_type": "m.direct_to_device",
			"content": map[string]any{
				"sender": userID, "type": eventType, "message_id": s.newID(), "messages": byUser,
			},
		})
		if err != nil {
			return false, err
		}

		edus[server] = edu
	}

	if _, err := s.federated.SendToDeviceFederated(ctx, userID, deviceID, eventType, txnID, targets, edus); err != nil {
		return false, err
	}

	return len(targets) > 0, nil
}

// KeyChanges - у кого из соседей userID по комнатам менялись ключи устройств в
// окне (since, upper] (пустой список, а не nil). Только пользователи с общими
// комнатами, как device_lists в /sync.
func (s *ToDeviceService) KeyChanges(ctx context.Context, userID string, since, upper int64) ([]string, error) {
	changed, err := s.store.DeviceListChangesFor(ctx, userID, since, upper)
	if err != nil {
		return nil, err
	}

	if changed == nil {
		changed = []string{}
	}

	return changed, nil
}
