package core

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

// Каталог публичных комнат (GET/POST /publicRooms). Логика перенесена из
// HTTP-обработчика; поля, поиск и пагинация как в Synapse.

// DirectoryStore - публичные комнаты со сводкой состояния.
type DirectoryStore interface {
	// PublicRoomsDirectory - все опубликованные и не заблокированные комнаты.
	PublicRoomsDirectory(ctx context.Context) ([]PublicRoom, error)
}

// PublicRoom - комната в каталоге (PublishedRoomsChunk спеки).
type PublicRoom struct {
	RoomID         string
	JoinedMembers  int
	Name           string
	Topic          string
	CanonicalAlias string
	AvatarURL      string
	JoinRule       string
	RoomType       string
	WorldReadable  bool
	GuestCanJoin   bool
}

// PublicRoomsQuery - параметры выборки каталога.
type PublicRoomsQuery struct {
	// Limit - размер страницы; 0 и меньше - без ограничения (как Synapse без limit).
	Limit int
	// Since - next_batch/prev_batch прошлой страницы.
	Since string
	// Search - filter.generic_search_term: подстрока name, topic или canonical_alias
	// без учета регистра.
	Search string
	// RoomTypes - filter.room_types; пустая строка - комнаты без типа (null в
	// фильтре). nil - без фильтра по типу.
	RoomTypes []string
}

// PublicRoomsPage - страница каталога.
type PublicRoomsPage struct {
	Rooms []PublicRoom
	Next  string
	Prev  string
	// Total - число комнат по фильтру (total_room_count_estimate).
	Total int
}

// DirectoryService - доменный сервис каталога комнат.
type DirectoryService struct {
	store DirectoryStore
}

// NewDirectoryService создает сервис поверх хранилища.
func NewDirectoryService(store DirectoryStore) *DirectoryService {
	return &DirectoryService{store: store}
}

// PublicRooms - страница каталога: фильтр по generic_search_term, порядок как в
// Synapse (больше участников - выше, при равенстве - по room_id), курсор - смещение.
func (s *DirectoryService) PublicRooms(ctx context.Context, q PublicRoomsQuery) (PublicRoomsPage, error) {
	rooms, err := s.store.PublicRoomsDirectory(ctx)
	if err != nil {
		return PublicRoomsPage{}, err
	}

	term := strings.ToLower(strings.TrimSpace(q.Search))
	matched := make([]PublicRoom, 0, len(rooms))

	for i := range rooms {
		if (term == "" || publicRoomMatches(rooms[i], term)) && roomTypeMatches(rooms[i], q.RoomTypes) {
			matched = append(matched, rooms[i])
		}
	}

	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].JoinedMembers != matched[j].JoinedMembers {
			return matched[i].JoinedMembers > matched[j].JoinedMembers
		}

		return matched[i].RoomID < matched[j].RoomID
	})

	offset := parseDirectoryToken(q.Since)
	if offset > len(matched) {
		offset = len(matched)
	}

	end := len(matched)
	if q.Limit > 0 && offset+q.Limit < end {
		end = offset + q.Limit
	}

	page := PublicRoomsPage{Rooms: matched[offset:end], Total: len(matched)}

	if end < len(matched) {
		page.Next = directoryToken(end)
	}

	if offset > 0 {
		prev := 0
		if q.Limit > 0 && offset-q.Limit > 0 {
			prev = offset - q.Limit
		}

		page.Prev = directoryToken(prev)
	}

	return page, nil
}

func publicRoomMatches(room PublicRoom, term string) bool {
	for _, field := range []string{room.Name, room.Topic, room.CanonicalAlias} {
		if strings.Contains(strings.ToLower(field), term) {
			return true
		}
	}

	return false
}

func roomTypeMatches(room PublicRoom, types []string) bool {
	if types == nil {
		return true
	}

	for _, t := range types {
		if t == room.RoomType {
			return true
		}
	}

	return false
}

// directoryToken - курсор каталога: "o" + смещение. Непрозрачен для клиента.
func directoryToken(offset int) string {
	return "o" + strconv.Itoa(offset)
}

// parseDirectoryToken разбирает курсор; чужой или битый курсор (например, room_id
// из старого формата) - начало списка.
func parseDirectoryToken(token string) int {
	raw, ok := strings.CutPrefix(token, "o")
	if !ok {
		return 0
	}

	offset, err := strconv.Atoi(raw)
	if err != nil || offset < 0 {
		return 0
	}

	return offset
}
