package api

import (
	"context"

	"github.com/aisa-it/syngo/internal/core"
)

// Видимость истории по history_visibility: границы и фильтр - в
// core.VisibilityService и core.Visibility. Без БД (тестовые
// хранилища в памяти) фильтр не применяется.

func (s *Server) visibility(ctx context.Context, userID string, roomIDs []string) map[string]core.Visibility {
	if s.historyVisibility == nil {
		return nil
	}

	return s.historyVisibility.Cutoffs(ctx, userID, roomIDs)
}

// roomVisibility — границы видимости одной комнаты; ok=false - фильтр не применяется
// (нет БД).
func (s *Server) roomVisibility(ctx context.Context, userID, roomID string) (core.Visibility, bool) {
	vis, ok := s.visibility(ctx, userID, []string{roomID})[roomID]

	return vis, ok
}

// visibleTo — видно ли событие пользователю: собственные события членства видны всегда.
func visibleTo(vis core.Visibility, userID string, event *core.Event) bool {
	return vis.VisibleTo(userID, event)
}

// filterVisible оставляет видимые события; второй результат - было ли что-то скрыто.
func filterVisible(vis core.Visibility, userID string, events []core.Event) ([]core.Event, bool) {
	return vis.FilterVisible(userID, events)
}

// applySyncVisibility скрывает из timeline комнат с членством join события, которые
// пользователь не должен видеть. Границы видимости возвращаются: по ним же
// считаются агрегации связей, без повторного запроса.
func (s *Server) applySyncVisibility(ctx context.Context, userID string, data *core.SyncData) map[string]core.Visibility {
	joined := make([]string, 0, len(data.Memberships))

	for _, m := range data.Memberships {
		if m.Membership == "join" && len(data.Timeline[m.RoomID]) > 0 {
			joined = append(joined, m.RoomID)
		}
	}

	vis := s.visibility(ctx, userID, joined)

	for roomID, v := range vis {
		data.Timeline[roomID], _ = filterVisible(v, userID, data.Timeline[roomID])
	}

	return vis
}
