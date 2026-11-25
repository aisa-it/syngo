package api

import "context"

// unreadCounts - unread_notifications комнаты.
type unreadCounts struct {
	Notification int `json:"notification_count"`
	Highlight    int `json:"highlight_count"`
}

// fillUnreadNotifications выставляет unread_notifications во все join-комнаты
// ответа /sync из хранимых действий: фоновый разбор событий до upper
// ждется ограниченное время - счетчик соответствует отданному
// timeline, - и счетчики всех комнат читаются одним запросом. threads - фильтр клиента просит
// unread_thread_notifications: тогда unread_notifications - только основная
// лента, а треды - отдельно (как Synapse). Ошибка не роняет /sync: счетчики
// просто не отдаются.
func (s *Server) fillUnreadNotifications(
	ctx context.Context, resp *syncResponse, userID string, upper int64, threads bool,
) {
	if s.pushActions == nil || len(resp.Rooms.Join) == 0 {
		return
	}

	roomIDs := make([]string, 0, len(resp.Rooms.Join))
	for roomID := range resp.Rooms.Join {
		roomIDs = append(roomIDs, roomID)
	}

	stored, ok := s.pushActions.UnreadCounts(ctx, userID, upper, roomIDs)
	if !ok {
		return
	}

	for roomID := range resp.Rooms.Join {
		room := resp.Rooms.Join[roomID]

		var total unreadCounts

		for thread, c := range stored[roomID] {
			if threads && thread != "" {
				if room.UnreadThreadNotifications == nil {
					room.UnreadThreadNotifications = map[string]unreadCounts{}
				}

				room.UnreadThreadNotifications[thread] = unreadCounts{Notification: c.Notification, Highlight: c.Highlight}

				continue
			}

			total.Notification += c.Notification
			total.Highlight += c.Highlight
		}

		room.UnreadNotifications = &total
		resp.Rooms.Join[roomID] = room
	}
}
