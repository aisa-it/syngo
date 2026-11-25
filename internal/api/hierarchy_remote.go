package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/aisa-it/syngo/internal/federation/transport"
)

// Чужие комнаты пространства для клиентского /hierarchy: комнату,
// в которой нас нет, спрашиваем федеративным /hierarchy у серверов из via ее
// m.space.child (_summarize_remote_room_hierarchy и get_room_hierarchy
// федеративного клиента Synapse), с таймаутом и кэшем remoteQueries.

// hierarchyMaxServers - сколько серверов из via пробовать (MAX_SERVERS_PER_SPACE).
const hierarchyMaxServers = 3

// errNoHierarchyServers - в via нет сервера, которого можно спросить.
var errNoHierarchyServers = errors.New("api: no servers to ask for hierarchy")

// remoteSpace - разобранный ответ федеративного /hierarchy.
type remoteSpace struct {
	room spaceRoom
	// children - сводки детей по room_id: ребенка не-пространство повторно не спрашиваем.
	children map[string]spaceSummary
	// inaccessible - дети, недоступные нашему серверу: их не спрашиваем.
	inaccessible map[string]bool
}

// remoteHierarchy - сводка чужой комнаты roomID от серверов via; false - ни
// один не ответил годным ответом (или федерация выключена).
func (s *Server) remoteHierarchy(ctx context.Context, roomID string, via []string, suggestedOnly bool) (remoteSpace, bool) {
	if s.federationTransport == nil {
		return remoteSpace{}, false
	}

	key := "hierarchy " + roomID + " " + strconv.FormatBool(suggestedOnly)

	raw, ok := s.cachedRemoteHierarchy(ctx, key, roomID, via, suggestedOnly)
	if !ok {
		return remoteSpace{}, false
	}

	space, err := parseRemoteHierarchy(raw, roomID)
	if err != nil {
		return remoteSpace{}, false
	}

	return space, true
}

// cachedRemoteHierarchy - тело ответа из кэша или от сервера; одновременные
// запросы одной комнаты идут в сеть один раз.
func (s *Server) cachedRemoteHierarchy(ctx context.Context, key, roomID string, via []string, suggestedOnly bool) (json.RawMessage, bool) {
	if entry, ok := s.remoteQueries.get(key); ok {
		return entry.value, entry.value != nil
	}

	result, err, _ := s.remoteQueries.flight.Do(key, func() (any, error) {
		raw, err := s.fetchRemoteHierarchy(context.WithoutCancel(ctx), roomID, via, suggestedOnly)
		if err != nil {
			// Пустой via - не отказ сервера: другой родитель может дать рабочий.
			if !errors.Is(err, errNoHierarchyServers) {
				s.remoteQueries.put(key, nil, remoteQueryNegativeTTL)
			}

			return nil, err
		}

		s.remoteQueries.put(key, raw, remoteQueryTTL)

		return raw, nil
	})
	if err != nil {
		return nil, false
	}

	raw, _ := result.(json.RawMessage)

	return raw, raw != nil
}

// fetchRemoteHierarchy спрашивает серверы via по очереди (не больше трех, свой
// пропускаем): v1, при M_UNRECOGNIZED - unstable MSC2946. Ответ сервера должен
// пройти проверку parseRemoteHierarchy, иначе спрашиваем следующий.
func (s *Server) fetchRemoteHierarchy(ctx context.Context, roomID string, via []string, suggestedOnly bool) (json.RawMessage, error) {
	query := ""
	if suggestedOnly {
		query = "?suggested_only=true"
	}

	escaped := url.PathEscape(roomID)
	stable := "/_matrix/federation/v1/hierarchy/" + escaped + query
	unstable := "/_matrix/federation/unstable/org.matrix.msc2946/hierarchy/" + escaped + query

	lastErr := errNoHierarchyServers
	tried := 0

	for _, destination := range via {
		if tried == hierarchyMaxServers {
			break
		}

		if destination == s.cfg.Matrix.ServerName {
			continue
		}

		if _, err := transport.ParseServerName(destination); err != nil {
			continue
		}

		tried++

		raw, err := s.requestRemoteHierarchy(ctx, destination, stable, unstable)
		if err == nil {
			_, err = parseRemoteHierarchy(raw, roomID)
		}

		if err == nil {
			return raw, nil
		}

		s.log.WarnContext(ctx, "api: remote hierarchy failed", "room_id", roomID, "destination", destination, "error", err)
		lastErr = err
	}

	return nil, lastErr
}

// requestRemoteHierarchy - запрос к одному серверу с таймаутом.
func (s *Server) requestRemoteHierarchy(ctx context.Context, destination, stable, unstable string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteQueryTimeout)
	defer cancel()

	raw, status, err := s.federationTransport.Send(ctx, destination, http.MethodGet, stable, nil)
	if unrecognizedEndpoint(status, err) {
		raw, status, err = s.federationTransport.Send(ctx, destination, http.MethodGet, unstable, nil)
	}

	if err == nil && status/100 != 2 {
		err = fmt.Errorf("HTTP %d", status)
	}

	return raw, err
}

// unrecognizedEndpoint - сервер не знает эндпоинта (_is_unknown_endpoint в
// Synapse): 404/405 без errcode или с M_UNRECOGNIZED, 400 с M_UNRECOGNIZED.
// Тело отказа транспорт отдает в тексте ошибки.
func unrecognizedEndpoint(status int, err error) bool {
	text := ""
	if err != nil {
		text = err.Error()
	}

	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return !strings.Contains(text, `"errcode"`) || strings.Contains(text, "M_UNRECOGNIZED")
	case http.StatusBadRequest:
		return strings.Contains(text, "M_UNRECOGNIZED")
	}

	return false
}

// parseRemoteHierarchy проверяет ответ как Synapse: room - объект с запрошенным
// room_id, children_state - список событий с type, state_key и content.via из
// строк, children - список объектов, inaccessible_children - список строк.
// Поля сводки разбираются мягко: поле не того типа пропускается.
func parseRemoteHierarchy(raw json.RawMessage, roomID string) (remoteSpace, error) {
	var body struct {
		Room         map[string]json.RawMessage   `json:"room"`
		Children     []map[string]json.RawMessage `json:"children"`
		Inaccessible []string                     `json:"inaccessible_children"`
	}

	if err := json.Unmarshal(raw, &body); err != nil {
		return remoteSpace{}, fmt.Errorf("invalid hierarchy response: %w", err)
	}

	if body.Room == nil {
		return remoteSpace{}, errors.New("invalid hierarchy response: no room")
	}

	space := remoteSpace{
		room:         spaceRoom{spaceSummary: parseSpaceSummary(body.Room), ChildrenState: []spaceChildEvent{}},
		children:     make(map[string]spaceSummary, len(body.Children)),
		inaccessible: make(map[string]bool, len(body.Inaccessible)),
	}

	if space.room.RoomID != roomID {
		return remoteSpace{}, errors.New("invalid hierarchy response: wrong room returned")
	}

	if state, ok := body.Room["children_state"]; ok {
		var events []json.RawMessage
		if err := json.Unmarshal(state, &events); err != nil {
			return remoteSpace{}, fmt.Errorf("invalid hierarchy response: children_state: %w", err)
		}

		for _, item := range events {
			event, err := parseHierarchyEvent(item)
			if err != nil {
				return remoteSpace{}, fmt.Errorf("invalid hierarchy response: %w", err)
			}

			space.room.ChildrenState = append(space.room.ChildrenState, event)
		}
	}

	for _, fields := range body.Children {
		if child := parseSpaceSummary(fields); child.RoomID != "" {
			space.children[child.RoomID] = child
		}
	}

	for _, id := range body.Inaccessible {
		space.inaccessible[id] = true
	}

	return space, nil
}

// parseHierarchyEvent - stripped-событие children_state (_validate_hierarchy_event).
func parseHierarchyEvent(raw json.RawMessage) (spaceChildEvent, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return spaceChildEvent{}, errors.New("event in children_state is not an object")
	}

	var event spaceChildEvent

	if json.Unmarshal(fields["type"], &event.Type) != nil || json.Unmarshal(fields["state_key"], &event.StateKey) != nil {
		return spaceChildEvent{}, errors.New("event in children_state without type or state_key")
	}

	var content struct {
		Via []string `json:"via"`
	}

	if err := json.Unmarshal(fields["content"], &content); err != nil || content.Via == nil {
		return spaceChildEvent{}, errors.New("event in children_state without content.via")
	}

	event.Content = fields["content"]
	_ = json.Unmarshal(fields["sender"], &event.Sender)
	_ = json.Unmarshal(fields["origin_server_ts"], &event.OriginServerTS)

	return event, nil
}

// parseSpaceSummary - поля сводки комнаты из ответа чужого сервера.
func parseSpaceSummary(fields map[string]json.RawMessage) spaceSummary {
	var summary spaceSummary

	targets := map[string]any{
		"room_id": &summary.RoomID, "name": &summary.Name, "topic": &summary.Topic,
		"avatar_url": &summary.AvatarURL, "canonical_alias": &summary.CanonicalAlias,
		"num_joined_members": &summary.NumJoinedMembers, "world_readable": &summary.WorldReadable,
		"guest_can_join": &summary.GuestCanJoin, "join_rule": &summary.JoinRule,
		"room_type": &summary.RoomType, "room_version": &summary.RoomVersion,
		"encryption": &summary.Encryption, "allowed_room_ids": &summary.AllowedRoomIDs,
	}

	for name, target := range targets {
		if raw, ok := fields[name]; ok {
			_ = json.Unmarshal(raw, target)
		}
	}

	return summary
}
