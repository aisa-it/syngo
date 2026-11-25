package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/pushers"
	"github.com/aisa-it/syngo/internal/pushrules"
)

// pushPayload - тело уведомления на push-gateway (Push Gateway API, /_matrix/push/v1/notify).
type pushPayload struct {
	Notification notification `json:"notification"`
}

type notification struct {
	EventID string          `json:"event_id"`
	RoomID  string          `json:"room_id"`
	Type    string          `json:"type,omitempty"`
	Sender  string          `json:"sender,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	Prio    string          `json:"prio"`
	Counts  counts          `json:"counts"`
	Devices []device        `json:"devices"`
}

type counts struct {
	Unread int `json:"unread"`
}

type device struct {
	AppID   string                     `json:"app_id"`
	Pushkey string                     `json:"pushkey"`
	Data    map[string]json.RawMessage `json:"data,omitempty"`
	Tweaks  map[string]json.RawMessage `json:"tweaks"`
}

// dispatchPushes асинхронно отправляет уведомление о событии на http-pushers
// участников комнаты (TestPushSync). Ошибки доставки не влияют на API.
// Получателю push уходит, только если его push-правила дали notify: раньше
// push шел всем на любое событие, в том числе в заглушенных комнатах, с
// unread=1.
//
// Сетевая доставка - через очереди pushers (pushers.Dispatcher):
// медленный шлюз одного пользователя не задерживает push другим, есть повторы,
// rejected удаляет pusher. Приглашенный получает push о приглашении, хотя еще
// не участник.
func (s *Server) dispatchPushes(ctx context.Context, ev core.Event) {
	if s.pushActions == nil {
		return
	}

	// Получатели и действия правил - на момент события (core.PushTargets).
	targets, err := s.pushActions.PushTargets(ctx, ev)
	if err != nil {
		s.log.WarnContext(ctx, "api: push rules evaluation failed", "error", err)

		return
	}

	if len(targets) == 0 {
		return
	}

	dispatcher := s.pushDispatcher()
	actions := s.pushActions

	// Фон не должен умирать вместе с HTTP-запросом клиента, поэтому контекст свой.
	// Здесь только счетчики из БД и постановка в очереди: сеть - в очередях pushers.
	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)

	go func() {
		defer cancel()
		// Паника в фоне не должна ронять homeserver.
		defer func() {
			if rec := recover(); rec != nil {
				s.log.ErrorContext(bgCtx, "api: push dispatch panic", "panic", fmt.Sprint(rec))
			}
		}()

		for _, target := range targets {
			// Бейдж - число комнат с непрочитанными уведомлениями (как Synapse по
			// умолчанию).
			unread := 1

			if rooms, err := actions.UnreadRooms(bgCtx, target.UserID); err == nil {
				unread = rooms
			}

			for i := range target.Pushers {
				s.enqueuePush(bgCtx, dispatcher, target.Pushers[i], ev, target.Result, unread)
			}
		}
	}()
}

// dispatchStoredPush - push по уже сохраненному событию (state, приглашение):
// содержимое и state_key берутся из БД.
func (s *Server) dispatchStoredPush(ctx context.Context, roomID, eventID string) {
	if s.pushActions == nil || s.rooms == nil || eventID == "" {
		return
	}

	ev, err := s.rooms.StoredEvent(ctx, roomID, eventID)
	if err != nil {
		s.log.WarnContext(ctx, "api: push event load failed", "room_id", roomID, "error", err)

		return
	}

	s.dispatchPushes(ctx, ev)
}

// pushDispatcher - общий диспетчер доставки и HTTP-клиент (раньше
// на каждое событие создавался новый http.Transport). Создается при первом
// push: конфиг push читается в этот момент.
func (s *Server) pushDispatcher() *pushers.Dispatcher {
	s.pushOnce.Do(func() {
		opts := s.pushOptions
		opts.Client = s.newPushClient()
		opts.Logger = s.log
		opts.OnRejected = s.removeRejectedPushers

		s.pushQueue.Store(pushers.NewDispatcher(opts))
	})

	return s.pushQueue.Load()
}

// removeRejectedPushers удаляет pushers, которые шлюз вернул в rejected
// (Push Gateway API): pushkey недействителен, слать на него бессмысленно.
func (s *Server) removeRejectedPushers(ctx context.Context, userID, appID string, pushkeys []string) {
	if s.pushersService == nil {
		return
	}

	for _, key := range pushkeys {
		if err := s.pushersService.Delete(ctx, userID, appID, key); err != nil {
			s.log.WarnContext(ctx, "api: rejected pusher delete failed", "error", err)

			continue
		}

		s.log.InfoContext(ctx, "api: pusher removed: rejected by gateway", "user_id", userID, "app_id", appID)
	}
}

// ClosePush останавливает доставку push и фоновый разбор push-действий
// (graceful shutdown).
func (s *Server) ClosePush(ctx context.Context) error {
	// Фоновый разбор push-действий - до закрытия пула БД.
	if s.pushActions != nil {
		if err := s.pushActions.Close(ctx); err != nil {
			return err
		}
	}

	return s.pushDispatcher().Close(ctx)
}

// enqueuePush собирает уведомление для pusher и ставит его в очередь.
func (s *Server) enqueuePush(
	ctx context.Context, dispatcher *pushers.Dispatcher, pusher pushers.Pusher, ev core.Event,
	res pushrules.Result, unread int,
) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(pusher.Data, &data); err != nil {
		return
	}

	var pushURL string
	if json.Unmarshal(data["url"], &pushURL) != nil || pushURL == "" {
		return
	}

	if err := s.checkPushURL(pushURL); err != nil {
		s.log.WarnContext(ctx, "api: push url rejected", "gateway", pushers.RedactURL(pushURL), "error", err)

		return
	}

	// data без url уходит шлюзу в devices[].data, как в Synapse.
	delete(data, "url")

	tweaks := res.Tweaks
	if tweaks == nil {
		tweaks = map[string]json.RawMessage{}
	}

	prio := "low"
	if ev.Type == "m.room.encrypted" || res.Highlight || res.Sound != "" {
		prio = "high"
	}

	notif := notification{
		EventID: ev.EventID, RoomID: ev.RoomID, Prio: prio,
		Counts:  counts{Unread: unread},
		Devices: []device{{AppID: pusher.AppID, Pushkey: pusher.Pushkey, Data: data, Tweaks: tweaks}},
	}

	// format=event_id_only: содержимое не уходит третьей стороне (Sygnal у Element).
	var format string
	_ = json.Unmarshal(data["format"], &format)

	if format != "event_id_only" {
		notif.Type, notif.Sender, notif.Content = ev.Type, ev.Sender, ev.Content
	}

	body, err := json.Marshal(pushPayload{Notification: notif})
	if err != nil {
		return
	}

	if !dispatcher.Enqueue(pushers.Notification{
		UserID: pusher.UserID, AppID: pusher.AppID, Pushkey: pusher.Pushkey, URL: pushURL, Body: body,
	}) {
		s.log.WarnContext(ctx, "api: push queue is full, notification dropped",
			"gateway", pushers.RedactURL(pushURL))
	}
}
