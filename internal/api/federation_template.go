package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/auth"
	"github.com/aisa-it/syngo/internal/store"
)

// Шаблоны make_join/make_leave.
//
// Раньше make_join был вне X-Matrix, не проверял право войти и параметр ver,
// make_leave всегда отвечал room_version "10". Теперь шаблон строится по
// текущему состоянию комнаты, как в Synapse:
//   - пользователь с сервера-отправителя (иначе 403), сервер не запрещен ACL;
//   - комната наша (есть наш участник), иначе 404;
//   - make_join: версия комнаты среди ver (по умолчанию ["1"], спека), иначе
//     400 M_INCOMPATIBLE_ROOM_VERSION;
//   - restricted join: мы выбираем авторизующего участника и кладем
//     join_authorised_via_users_server;
//   - событие обязано проходить auth rules по текущему состоянию (ban,
//     join_rules), иначе 403.

// writeMembershipTemplate отвечает на make_join/make_leave.
func (s *Server) writeMembershipTemplate(w http.ResponseWriter, r *http.Request, membership, logLabel string) {
	if s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	roomID, okRoom := pathParam(r, "roomId")
	userID, okUser := pathParam(r, "userId")

	if !okRoom || !okUser || roomID == "" || userID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room or user id")

		return
	}

	origin := federationOrigin(r)
	if origin == "" || domainOf(userID) != origin {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "User not from origin")

		return
	}

	if !s.requireServerACL(w, r, roomID, origin) {
		return
	}

	if rejected := s.requireResidentRoom(r.Context(), roomID); rejected != nil {
		rejected.write(w)

		return
	}

	version := s.roomVersionOf(r.Context(), roomID)
	if version == 0 {
		version = core.DefaultRoomVersion
	}

	if membership == "join" && !joinVersionSupported(r, version) {
		// Спека: сервер обязан явно назвать версию комнаты, которую вступающий не умеет.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"errcode": "M_INCOMPATIBLE_ROOM_VERSION", "error": "Your homeserver does not support the features required to join this room",
			"room_version": strconv.Itoa(version),
		})

		return
	}

	template, rejected := s.membershipTemplate(r.Context(), roomID, userID, membership)
	if rejected != nil {
		s.log.InfoContext(r.Context(), "api: "+logLabel+" rejected", "room_id", roomID, "user_id", userID, "reason", rejected.message)
		rejected.write(w)

		return
	}

	s.writeJoinResponse(r.Context(), logLabel, w, map[string]any{
		"room_version": strconv.Itoa(version), "event": template,
	})
}

// joinVersionSupported - версия комнаты среди ver запроса (по умолчанию ["1"])
// и не выше FederationMaxRoomVersion.
func joinVersionSupported(r *http.Request, version int) bool {
	if version > core.FederationMaxRoomVersion {
		return false
	}

	supported := r.URL.Query()["ver"]
	if len(supported) == 0 {
		supported = []string{"1"}
	}

	return slices.Contains(supported, strconv.Itoa(version))
}

// membershipTemplate собирает шаблон member-события userID по текущему
// состоянию и проверяет его auth rules.
func (s *Server) membershipTemplate(ctx context.Context, roomID, userID, membership string) (map[string]any, *membershipError) {
	internal := func() *membershipError {
		return &membershipError{status: http.StatusInternalServerError, errcode: ErrCodeUnknown, message: "Failed to read room"}
	}

	prev, depth, err := s.latestEvents(ctx, roomID)
	if err != nil {
		return nil, internal()
	}

	if len(prev) == 0 {
		return nil, &membershipError{status: http.StatusNotFound, errcode: ErrCodeNotFound, message: "Unknown room"}
	}

	content := map[string]any{"membership": membership}
	stateKey := userID

	draft := store.FedPDU{RoomID: roomID, Type: "m.room.member", Sender: userID, StateKey: &stateKey}

	draft.Content, _ = json.Marshal(content)

	state, err := s.currentAuthState(ctx, draft)
	if err != nil {
		return nil, internal()
	}

	if membership == "join" && isRestrictedJoinRule(state.JoinRules) {
		switch state.Members[userID] {
		case "join", "invite":
		default:
			authoriser, err := s.restrictedJoinAuthoriser(ctx, roomID, userID, state)

			switch {
			case errors.Is(err, errNoAllowedMembership):
				return nil, &membershipError{
					status: http.StatusForbidden, errcode: "M_UNABLE_TO_AUTHORISE_JOIN",
					message: "You do not belong to any of the required rooms to join this room",
				}
			case errors.Is(err, errNoAuthoriser):
				return nil, &membershipError{
					status: http.StatusBadRequest, errcode: "M_UNABLE_TO_GRANT_JOIN",
					message: "No local user can authorise the join",
				}
			case err != nil:
				return nil, internal()
			}

			content["join_authorised_via_users_server"] = authoriser
			draft.Content, _ = json.Marshal(content)

			if state, err = s.currentAuthState(ctx, draft); err != nil {
				return nil, internal()
			}
		}
	}

	if err := auth.Check(auth.Event{
		RoomID: roomID, Type: "m.room.member", Sender: userID, StateKey: &stateKey,
		Content: draft.Content, Prev: prev, Depth: depth + 1,
	}, state); err != nil {
		return nil, forbiddenMembership("You are not allowed to " + membership + " this room")
	}

	authEvents, err := s.templateAuthEvents(ctx, roomID, userID, membership, joinAuthoriser(draft.Content))
	if err != nil {
		return nil, internal()
	}

	// Инвариант шаблона: каждый auth_event обязан быть уже сохранённым событием
	// комнаты. Иначе вступающий сервер подписывает событие с висячей ссылкой,
	// Synapse не может её разрешить и join отвергается.
	if missing := s.missingEventIDs(ctx, authEvents); len(missing) > 0 {
		s.log.ErrorContext(ctx, "api: membership template has unknown auth events",
			"room_id", roomID, "user_id", userID, "missing", missing)

		return nil, &membershipError{status: http.StatusInternalServerError, errcode: ErrCodeUnknown, message: "Room state is inconsistent"}
	}

	// origin_server_ts обязателен в шаблоне: без него Synapse отвечает
	// «Malformed join event» при сборке send_join.
	return map[string]any{
		"type":             "m.room.member",
		"state_key":        userID,
		"sender":           userID,
		"room_id":          roomID,
		"content":          content,
		"depth":            depth + 1,
		"prev_events":      prev,
		"auth_events":      authEvents,
		"origin_server_ts": time.Now().UnixMilli(),
	}, nil
}

// templateAuthEvents - auth_events шаблона по правилам выбора спеки: create,
// power_levels, членство пользователя; join_rules - для join; членство
// авторизующего restricted join. В комнате v12 create в auth_events не входит
// (Synapse отвергает такой PDU).
func (s *Server) templateAuthEvents(ctx context.Context, roomID, userID, membership, authoriser string) ([]string, error) {
	keys := []stateKeyRef{{"m.room.power_levels", ""}}
	if createIDOfRoom(roomID) == "" {
		keys = append([]stateKeyRef{{"m.room.create", ""}}, keys...)
	}

	if membership == "join" {
		keys = append(keys, stateKeyRef{"m.room.join_rules", ""})
	}

	keys = append(keys, stateKeyRef{"m.room.member", userID})

	if authoriser != "" {
		keys = append(keys, stateKeyRef{"m.room.member", authoriser})
	}

	events, err := s.currentStateEvents(ctx, roomID, keys)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(events))
	for i := range events {
		ids = append(ids, events[i].EventID)
	}

	return ids, nil
}
