package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/aisa-it/syngo/internal/core"
)

// publicRoomsChunk — элемент ответа /publicRooms (PublishedRoomsChunk спеки).
type publicRoomsChunk struct {
	RoomID         string `json:"room_id"`
	Name           string `json:"name,omitempty"`
	Topic          string `json:"topic,omitempty"`
	CanonicalAlias string `json:"canonical_alias,omitempty"`
	AvatarURL      string `json:"avatar_url,omitempty"`
	RoomType       string `json:"room_type,omitempty"`
	NumJoined      int    `json:"num_joined_members"`
	WorldRead      bool   `json:"world_readable"`
	GuestCanJoin   bool   `json:"guest_can_join"`
	JoinRule       string `json:"join_rule"`
}

// publicRoomsResponse — тело ответа /publicRooms.
type publicRoomsResponse struct {
	Chunk     []publicRoomsChunk `json:"chunk"`
	NextBatch string             `json:"next_batch,omitempty"`
	PrevBatch string             `json:"prev_batch,omitempty"`
	Total     int                `json:"total_room_count_estimate"`
}

// publicRoomsRequest — тело POST /publicRooms.
type publicRoomsRequest struct {
	Limit  *int   `json:"limit"`
	Since  string `json:"since"`
	Filter *struct {
		GenericSearchTerm string    `json:"generic_search_term"`
		RoomTypes         []*string `json:"room_types"`
	} `json:"filter"`
}

// handlePublicRooms — GET и POST /_matrix/client/v3/publicRooms; выборка -
// в core.DirectoryService. Без limit отдается весь каталог, как в
// Synapse; POST поддерживает filter.generic_search_term и filter.room_types
// (Complement TestPublicRooms). Параметр server (каталог другого сервера)
// не поддерживается: федерация вне скоупа, отдается свой каталог.
func (s *Server) handlePublicRooms(w http.ResponseWriter, r *http.Request) {
	if s.directory == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	query := r.URL.Query()
	q := core.PublicRoomsQuery{Since: query.Get("since")}

	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "limit must be an integer")

			return
		}

		q.Limit = parsed
	}

	if r.Method == http.MethodPost {
		var req publicRoomsRequest
		if !decodeBody(w, r, &req) {
			return
		}

		if req.Limit != nil {
			q.Limit = *req.Limit
		}

		if req.Since != "" {
			q.Since = req.Since
		}

		if req.Filter != nil {
			q.Search = req.Filter.GenericSearchTerm

			if req.Filter.RoomTypes != nil {
				q.RoomTypes = make([]string, 0, len(req.Filter.RoomTypes))

				for _, t := range req.Filter.RoomTypes {
					if t == nil {
						q.RoomTypes = append(q.RoomTypes, "")
					} else {
						q.RoomTypes = append(q.RoomTypes, *t)
					}
				}
			}
		}
	}

	page, err := s.directory.PublicRooms(r.Context(), q)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: public rooms failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	resp := publicRoomsResponse{
		Chunk:     make([]publicRoomsChunk, 0, len(page.Rooms)),
		NextBatch: page.Next,
		PrevBatch: page.Prev,
		Total:     page.Total,
	}

	for i := range page.Rooms {
		room := page.Rooms[i]
		resp.Chunk = append(resp.Chunk, publicRoomsChunk{
			RoomID: room.RoomID, Name: room.Name, Topic: room.Topic,
			CanonicalAlias: room.CanonicalAlias, AvatarURL: room.AvatarURL, RoomType: room.RoomType,
			NumJoined: room.JoinedMembers, WorldRead: room.WorldReadable,
			GuestCanJoin: room.GuestCanJoin, JoinRule: room.JoinRule,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// roomsReady проверяет, что сервис комнат сконфигурирован.
func (s *Server) roomsReady(w http.ResponseWriter) bool {
	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Room storage is not configured")

		return false
	}

	return true
}

// roomVisibilityBody — тело GET/PUT /directory/list/room/{roomId}.
type roomVisibilityBody struct {
	Visibility string `json:"visibility"`
}

// handleRoomVisibilityGet — GET /directory/list/room/{roomId}: опубликована ли
// комната в каталоге (без авторизации, как в Synapse).
func (s *Server) handleRoomVisibilityGet(w http.ResponseWriter, r *http.Request) {
	if !s.roomsReady(w) {
		return
	}

	public, err := s.rooms.RoomPublished(r.Context(), roomIDParam(r))
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Room not found")

			return
		}

		s.writeRoomError(w, err)

		return
	}

	visibility := "private"
	if public {
		visibility = "public"
	}

	writeJSON(w, http.StatusOK, roomVisibilityBody{Visibility: visibility})
}

// handleRoomVisibilityPut — PUT /directory/list/room/{roomId}: публикация в каталоге.
// Права - как у m.room.canonical_alias (Synapse check_can_change_room_list).
func (s *Server) handleRoomVisibilityPut(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if !s.roomsReady(w) {
		return
	}

	var req roomVisibilityBody
	if !decodeBody(w, r, &req) {
		return
	}

	if req.Visibility != "public" && req.Visibility != "private" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "visibility must be public or private")

		return
	}

	if err := s.rooms.SetRoomPublished(r.Context(), session.UserID, roomIDParam(r), req.Visibility == "public"); err != nil {
		s.writeRoomError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}
