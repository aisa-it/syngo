package api

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// presenceAudience - кто из кандидатов делит с пользователем комнату (store).
type presenceAudience interface {
	UsersSharingRoom(ctx context.Context, userID string, candidates []string) ([]string, error)
	RoomMates(ctx context.Context, userID string) ([]string, error)
}

// notifyPresence будит /sync самого пользователя и участников его комнат: смена
// presence касается только их, общий Notify досрочно отвечал бы пустым
// ответом всем long-poll сервера. Участники читаются в фоне: запрос
// не задерживает /sync и не входит в его бюджет SQL (ADR-0005).
func (s *Server) notifyPresence(ctx context.Context, userID string) { //nolint:contextcheck // фон без значений запроса
	s.notifySyncUser(userID)

	if s.presenceAudience == nil {
		return
	}

	// Фоновый контекст без отмены и без значений запроса: отмена клиентом и
	// счетчик SQL /sync (гейт ADR-0005) его не касаются.
	bg, cancel := context.WithTimeout(valuelessContext{context.WithoutCancel(ctx)}, 10*time.Second)

	go func() {
		defer cancel()

		// Свой статус уходит и серверам общих комнат.
		s.sendPresenceEDU(bg, userID)

		mates, err := s.presenceAudience.RoomMates(bg, userID)
		if err != nil {
			s.log.WarnContext(bg, "api: presence room mates failed", "error", err)

			return
		}

		for _, mate := range mates {
			s.notifySyncUser(mate)
		}
	}()
}

// valuelessContext - контекст без значений родителя (для фоновой работы после
// ответа клиенту).
type valuelessContext struct{ context.Context }

// Value не отдает значения родительского запроса.
func (valuelessContext) Value(any) any { return nil }

// presenceChangedFor - есть ли после версии since изменения presence, которые
// попадут в /sync пользователя: свои или участников общих комнат. Нужен
// long-poll: presence не поднимает ActivityVersion.
func (s *Server) presenceChangedFor(ctx context.Context, userID string, since int64) bool {
	if s.ephemeral == nil || since < 0 {
		return false
	}

	candidates := make([]string, 0)

	for _, event := range s.ephemeral.PresenceChangedSince(since) {
		if event.UserID == userID {
			return true
		}

		candidates = append(candidates, event.UserID)
	}

	if len(candidates) == 0 || s.presenceAudience == nil {
		return false
	}

	sharing, err := s.presenceAudience.UsersSharingRoom(ctx, userID, candidates)

	return err == nil && len(sharing) > 0
}

// presenceSection собирает presence для /sync (как в Synapse):
//   - статусы, изменившиеся после версии из токена (initial - все известные), -
//     свой и тех, с кем есть общая комната (один запрос на всех кандидатов);
//   - incremental: текущий статус участников, вошедших в комнату за окно (их join
//     в timeline), и всех участников комнат, куда вошел сам пользователь.
//
// Статусы в памяти процесса (core.EphemeralStore): запросов к БД нет, если
// чужих изменений нет.
func (s *Server) presenceSection(
	ctx context.Context, userID string, since, sinceEphemeral int64, resp *syncResponse, newlyJoined map[string]bool,
) ephemeralEvents {
	out := ephemeralEvents{Events: []ephemeralEvent{}}
	if s.ephemeral == nil {
		return out
	}

	picked := map[string]core.PresenceEvent{}

	// Участники, с которыми общая комната появилась за окно: их статус нужен, даже
	// если он не менялся.
	if since > 0 {
		for roomID := range resp.Rooms.Join {
			room := resp.Rooms.Join[roomID]
			newly := newlyJoined[roomID]

			collect := func(events []eventView) {
				for i := range events {
					member := joinedMemberOf(events[i])
					if member != "" && member != userID {
						picked[member] = s.ephemeral.Presence(member)
					}
				}
			}

			collect(room.Timeline.Events)

			if newly {
				collect(room.State.Events)
			}
		}
	}

	candidates := make([]string, 0)
	changed := map[string]core.PresenceEvent{}

	for _, event := range s.ephemeral.PresenceChangedSince(sinceEphemeral) {
		switch {
		case event.UserID == userID:
			picked[userID] = event
		case picked[event.UserID].UserID != "":
			picked[event.UserID] = event
		default:
			changed[event.UserID] = event
			candidates = append(candidates, event.UserID)
		}
	}

	if len(candidates) > 0 && s.presenceAudience != nil {
		sort.Strings(candidates)

		sharing, err := s.presenceAudience.UsersSharingRoom(ctx, userID, candidates)
		if err != nil {
			s.log.WarnContext(ctx, "api: presence audience failed", "error", err)
		}

		for _, member := range sharing {
			picked[member] = changed[member]
		}
	}

	users := make([]string, 0, len(picked))
	for member := range picked {
		users = append(users, member)
	}

	sort.Strings(users)

	now := s.ephemeral.Now().UnixMilli()

	for _, member := range users {
		out.Events = append(out.Events, presenceView(picked[member], now))
	}

	return out
}

// presenceView - событие m.presence в формате спеки.
func presenceView(event core.PresenceEvent, nowMS int64) ephemeralEvent {
	content := map[string]any{
		"presence":         event.Presence,
		"currently_active": event.Presence == core.PresenceOnline,
	}

	if event.StatusMsg != "" {
		content["status_msg"] = event.StatusMsg
	}

	if event.LastActiveMS > 0 {
		ago := nowMS - event.LastActiveMS
		if ago < 0 {
			ago = 0
		}

		content["last_active_ago"] = ago
	}

	return ephemeralEvent{Type: "m.presence", Sender: event.UserID, Content: content}
}

// joinedMemberOf - state_key события m.room.member с membership join, иначе "".
func joinedMemberOf(event eventView) string {
	if event.Type != "m.room.member" || event.StateKey == nil {
		return ""
	}

	var content struct {
		Membership string `json:"membership"`
	}

	if json.Unmarshal(event.Content, &content) != nil || content.Membership != "join" {
		return ""
	}

	return *event.StateKey
}

// deviceListLeftLookup - кто перестал делить комнаты с пользователем (store).
type deviceListLeftLookup interface {
	DeviceListLeft(ctx context.Context, userID string, leftRooms, leavers []string) ([]string, error)
}

// deviceListsLeft - device_lists.left в incremental /sync (спека «Tracking the
// device list for a user»): участники комнат, из которых пользователь
// вышел за окно, и вышедшие за окно из его комнат, если общих комнат с join
// больше нет. Клиент перестает отслеживать их ключи. Запрос - только если в
// ответе есть выходы.
func (s *Server) deviceListsLeft(ctx context.Context, userID string, resp *syncResponse) []string {
	if s.deviceListLeft == nil {
		return []string{}
	}

	leftRooms := make([]string, 0)

	for roomID := range resp.Rooms.Leave {
		events := resp.Rooms.Leave[roomID].Timeline.Events
		for i := range events {
			if m := ownMembership(userID, events[i]); m == "leave" || m == "ban" {
				leftRooms = append(leftRooms, roomID)

				break
			}
		}
	}

	leavers := make([]string, 0)

	for roomID := range resp.Rooms.Join {
		events := resp.Rooms.Join[roomID].Timeline.Events
		for i := range events {
			ev := &events[i]
			if ev.Type != "m.room.member" || ev.StateKey == nil || *ev.StateKey == userID {
				continue
			}

			if m := ownMembership(*ev.StateKey, *ev); m == "leave" || m == "ban" {
				leavers = append(leavers, *ev.StateKey)
			}
		}
	}

	return s.deviceListsLeftFor(ctx, userID, leftRooms, leavers)
}

// deviceListsLeftFor - кто из участников покинутых комнат и вышедших из комнат
// пользователя больше не делит с ним ни одной комнаты (общий для /sync и
// расширения e2ee sliding sync).
func (s *Server) deviceListsLeftFor(ctx context.Context, userID string, leftRooms, leavers []string) []string {
	if s.deviceListLeft == nil || (len(leftRooms) == 0 && len(leavers) == 0) {
		return []string{}
	}

	sort.Strings(leftRooms)
	sort.Strings(leavers)

	left, err := s.deviceListLeft.DeviceListLeft(ctx, userID, leftRooms, leavers)
	if err != nil {
		s.log.WarnContext(ctx, "api: device list left failed", "error", err)

		return []string{}
	}

	if left == nil {
		return []string{}
	}

	return left
}

// roomSummaryLookup - сводки комнат для summary в /sync (store).
type roomSummaryLookup interface {
	RoomSummaries(ctx context.Context, userID string, roomIDs []string) (map[string]store.RoomSummary, error)
}

// fillRoomSummaries кладет summary (m.heroes, m.joined_member_count,
// m.invited_member_count) в join-комнаты при lazy_load_members, как Synapse: без
// полного списка участников клиент называет комнату и считает участников по
// summary. Initial - все комнаты; incremental - вошедшие и с
// изменением членства в ответе. Один запрос.
//
// Изменение членства - m.room.member в timeline или, если timeline обрезан, в
// state (как _generate_room_entry в Synapse). Члены отправителей порции, которые
// lazy_load_members кладет в state каждого incremental, изменением не считаются:
// раньше из-за них сводка (count и array_agg по всем участникам) считалась на
// каждое сообщение - 9,5 мс на комнату в 450 участников.
func (s *Server) fillRoomSummaries(
	ctx context.Context, userID string, since int64, resp *syncResponse, newlyJoined map[string]bool,
) {
	if s.roomSummaries == nil {
		return
	}

	rooms := make([]string, 0, len(resp.Rooms.Join))

	for roomID := range resp.Rooms.Join {
		room := resp.Rooms.Join[roomID]
		if since == 0 || newlyJoined[roomID] || hasMemberEvent(room.Timeline.Events) ||
			(room.Timeline.Limited && hasMemberEvent(room.State.Events)) {
			rooms = append(rooms, roomID)
		}
	}

	if len(rooms) == 0 {
		return
	}

	sort.Strings(rooms)

	summaries, err := s.roomSummaries.RoomSummaries(ctx, userID, rooms)
	if err != nil {
		s.log.WarnContext(ctx, "api: room summaries failed", "error", err)

		return
	}

	for _, roomID := range rooms {
		summary := summaries[roomID]
		room := resp.Rooms.Join[roomID]

		heroes := summary.Heroes
		if heroes == nil {
			heroes = []string{}
		}

		room.Summary = &roomSummaryView{Heroes: heroes, Joined: summary.Joined, Invited: summary.Invited}
		resp.Rooms.Join[roomID] = room
	}
}

// hasMemberEvent - среди событий есть m.room.member.
func hasMemberEvent(events []eventView) bool {
	for i := range events {
		if events[i].Type == "m.room.member" {
			return true
		}
	}

	return false
}
