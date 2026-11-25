package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Федеративный /hierarchy: Synapse запрашивает его при открытии
// нашей комнаты по алиасу и для пространств с нашими комнатами. Раньше - 404
// M_UNRECOGNIZED, предпросмотр комнаты и пространства на чужом сервере не
// работал. Ориентир - get_federation_hierarchy в synapse/handlers/room_summary.py.

// federationHierarchyResponse - тело GET /_matrix/federation/v1/hierarchy/{roomId}.
type federationHierarchyResponse struct {
	Room                 spaceRoom      `json:"room"`
	Children             []spaceSummary `json:"children"`
	InaccessibleChildren []string       `json:"inaccessible_children"`
}

func (s *Server) registerFederationHierarchyRoutes(r chi.Router) {
	r.Get("/_matrix/federation/v1/hierarchy/{roomId}", s.handleFederationHierarchy)
	// MSC2946 до стабилизации: старые Synapse и fallback при M_UNRECOGNIZED на v1.
	r.Get("/_matrix/federation/unstable/org.matrix.msc2946/hierarchy/{roomId}", s.handleFederationHierarchy)
}

// handleFederationHierarchy отдает сводку нашей комнаты, ее m.space.child и
// сводки известных нам детей (без их детей).
//
// Комната, которой у нас нет или которая недоступна запросившему серверу, -
// 404 M_NOT_FOUND, как в Synapse (существование закрытой комнаты не
// раскрывается); запрещенный m.room.server_acl - 403. Ребенок, в котором нас
// нет, пропускается; недоступный серверу - в inaccessible_children, чтобы тот
// не пытался в него войти.
func (s *Server) handleFederationHierarchy(w http.ResponseWriter, r *http.Request) {
	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	origin := federationOrigin(r)
	if origin == "" {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Unauthorized federation request")

		return
	}

	roomID := roomIDParam(r)
	if roomID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	suggestedOnly, ok := parseHierarchyBool(w, r, "suggested_only")
	if !ok {
		return
	}

	ctx := r.Context()
	walker := s.newHierarchyWalker()

	root, err := walker.state(ctx, roomID)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	if !walker.hostJoined(root) {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Unknown room")

		return
	}

	if !root.serverAllowed(origin) {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Server is banned from room")

		return
	}

	accessible, err := walker.localAccessible(ctx, root, "", origin)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	if !accessible {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Unknown room")

		return
	}

	resp := federationHierarchyResponse{
		Room:                 walker.localEntry(root, suggestedOnly, true, true),
		Children:             []spaceSummary{},
		InaccessibleChildren: []string{},
	}

	children := resp.Room.ChildrenState
	if len(children) > hierarchyMaxChildrenPerSpace {
		children = children[:hierarchyMaxChildrenPerSpace]
	}

	for _, child := range children {
		st, err := walker.state(ctx, child.StateKey)
		if err != nil {
			s.writeRoomError(w, err)

			return
		}

		// Комнаты, в которой нас нет, мы не знаем - сервер спросит ее у via.
		if !walker.hostJoined(st) {
			continue
		}

		accessible, err := walker.localAccessible(ctx, st, "", origin)
		if err != nil {
			s.writeRoomError(w, err)

			return
		}

		if !accessible || !st.serverAllowed(origin) {
			resp.InaccessibleChildren = append(resp.InaccessibleChildren, child.StateKey)

			continue
		}

		resp.Children = append(resp.Children, walker.localEntry(st, suggestedOnly, false, true).spaceSummary)
	}

	writeFederationJSON(w, resp)
}

// parseHierarchyBool - булев параметр запроса: "true"/"false", нет - false,
// иное - 400 M_INVALID_PARAM (parse_boolean в Synapse).
func parseHierarchyBool(w http.ResponseWriter, r *http.Request, name string) (value, ok bool) {
	switch r.URL.Query().Get(name) {
	case "", "false":
		return false, true
	case "true":
		return true, true
	}

	writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid "+name)

	return false, false
}
