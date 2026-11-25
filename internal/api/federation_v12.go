package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/auth"
	"github.com/aisa-it/syngo/internal/store"
)

// Комнаты v12 по федерации (MSC4291).
//
// room_id комнаты v12 - «!» и хеш ее m.room.create, домена в нем нет:
//   - у самого create в PDU нет room_id - он выводится из event_id create;
//   - create не входит в auth_events остальных событий, но авторизует их неявно
//     (его id - room_id с «$» вместо «!»);
//   - сервер комнаты - сервер создателя: к нему (и к пригласившему) идут
//     make_join/make_leave нашего пользователя.

// createIDOfRoom - event_id m.room.create комнаты v12 по ее room_id ("" для
// комнат ранних версий).
func createIDOfRoom(roomID string) string {
	if !auth.IsCreateHashRoomID(roomID) {
		return ""
	}

	return "$" + roomID[1:]
}

// roomIDOfCreate - room_id комнаты v12 по event_id ее m.room.create.
func roomIDOfCreate(eventID string) string {
	if len(eventID) < 2 || eventID[0] != '$' {
		return ""
	}

	return "!" + eventID[1:]
}

// errV12PDUShape - PDU не по формату комнаты v12.
var errV12PDUShape = errors.New("api: pdu is not formatted for room v12")

// checkV12PDU проверяет формат PDU комнаты v12 до записи: у create нет room_id,
// у остальных событий create не в auth_events (Synapse отвечает 400 на
// event_from_pdu_json и 403 в _check_create). eventID - посчитанный id события;
// для create возвращается выведенный из него room_id, для остальных - room_id
// PDU.
func checkV12PDU(object map[string]any, eventID string, version int) (string, error) {
	roomID, _ := object["room_id"].(string)
	if version < core.HashRoomIDVersion {
		return roomID, nil
	}

	eventType, _ := object["type"].(string)
	stateKey, isState := object["state_key"].(string)

	if eventType == "m.room.create" && isState && stateKey == "" {
		if _, has := object["room_id"]; has {
			return "", fmt.Errorf("%w: m.room.create with room_id", errV12PDUShape)
		}

		return roomIDOfCreate(eventID), nil
	}

	if !auth.IsCreateHashRoomID(roomID) {
		return "", fmt.Errorf("%w: room_id %q is not a create event hash", errV12PDUShape, roomID)
	}

	if slices.Contains(stringSlice(object["auth_events"]), createIDOfRoom(roomID)) {
		return "", fmt.Errorf("%w: auth_events contain m.room.create", errV12PDUShape)
	}

	return roomID, nil
}

// v12RoomIDOf - room_id PDU комнаты v12 после проверки формата (checkV12PDU);
// для ранних версий - "" (room_id берется из PDU как есть).
func v12RoomIDOf(raw json.RawMessage, eventID string, version int) (string, error) {
	if version < core.HashRoomIDVersion {
		return "", nil
	}

	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", err
	}

	return checkV12PDU(object, eventID, version)
}

// withImplicitCreate - auth-события для проверки auth rules: в комнате v12
// к auth_events события добавляется create комнаты (если его там нет).
func withImplicitCreate(roomID, eventType string, authIDs []string) []string {
	createID := createIDOfRoom(roomID)
	if createID == "" || eventType == "m.room.create" || slices.Contains(authIDs, createID) {
		return authIDs
	}

	return append(append([]string{}, authIDs...), createID)
}

// roomServer - сервер комнаты: домен room_id, у комнаты v12 - сервер
// создателя по известному m.room.create. known=false - комнату v12 мы не знаем.
func (s *Server) roomServer(ctx context.Context, roomID string) (server string, known bool) {
	if server := domainOfIdentifier(roomID); server != "" {
		return server, true
	}

	createID := createIDOfRoom(roomID)
	if createID == "" || s.db == nil {
		return "", false
	}

	create, err := s.db.EventPDU(ctx, createID)
	if err != nil {
		return "", false
	}

	return domainOfID(create.Sender), true
}

// remoteRoomTarget - комната другого сервера для client join/leave и
// серверы, через которые с ней работать. Для комнат ранних версий - домен
// room_id; для v12 - сервер создателя, пригласивший наш пользователь, серверы
// с участниками и via из запроса (как remote_room_hosts в Synapse). Неизвестная
// комната v12 без via и приглашения - не чужая: локальный путь ответит 404.
func (s *Server) remoteRoomTarget(ctx context.Context, roomID, userID string, via []string) (remote bool, servers []string) {
	ours := s.cfg.Matrix.ServerName

	server, known := s.roomServer(ctx, roomID)
	if known && server == ours {
		return false, nil
	}

	if !auth.IsCreateHashRoomID(roomID) {
		return server != "", via
	}

	servers = append(servers, via...)

	if s.db != nil {
		if member, err := store.NewRoomStore(s.db).CurrentStateEvent(ctx, roomID, "m.room.member", userID); err == nil &&
			membershipOfContent(member.Content) == "invite" {
			servers = append(servers, domainOfID(member.Sender))
		}

		if hosts, err := s.db.JoinedHosts(ctx, roomID); err == nil {
			servers = append(servers, hosts...)
		}
	}

	if server != "" {
		servers = append(servers, server)
	}

	out := servers[:0]

	for _, candidate := range servers {
		if candidate != "" && candidate != ours && !slices.Contains(out, candidate) {
			out = append(out, candidate)
		}
	}

	return len(out) > 0, out
}

// joinVia - серверы из параметров client join: via (v1.12) и server_name
// (устаревший, его шлет Element).
func joinVia(r *http.Request) []string {
	query := r.URL.Query()

	return append(append([]string{}, query["via"]...), query["server_name"]...)
}
