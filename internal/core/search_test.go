package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

// recordingSearch запоминает комнаты и границы, переданные в поиск.
type recordingSearch struct {
	rooms map[string]core.Visibility
	limit int
	err   error
}

func (r *recordingSearch) SearchMessagesPage(
	_ context.Context, rooms map[string]core.Visibility, term string, page core.SearchPage,
) ([]core.Event, int, error) {
	r.rooms, r.limit = rooms, page.Limit
	if r.err != nil {
		return nil, 0, r.err
	}

	return []core.Event{{EventID: "$hit", Type: "m.room.message", Content: []byte(`{"body":"` + term + `"}`)}}, 1, nil
}

// Поиск: только комнаты с join, filter.rooms в их пределах, границы видимости
// уходят в хранилище.
func TestSearchService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rooms, roomStore := newRoomService(t)
	first := createRoom(t, rooms)
	second := createRoom(t, rooms)
	foreign := createRoom(t, rooms)

	if _, err := rooms.Leave(ctx, alice, foreign); err != nil {
		t.Fatalf("Leave: %v", err)
	}

	visStore := coretest.NewMemVisibilityStore()
	visStore.Cutoffs[second] = core.Visibility{From: 42}

	search := &recordingSearch{}
	svc := core.NewSearchService(search, roomStore, core.NewVisibilityService(visStore, nil))

	if _, err := svc.SearchRoomEvents(ctx, alice, "", nil, 0); err == nil {
		t.Fatal("пустой search_term принят")
	} else {
		requireKind(t, err, core.ErrInvalidParam, "search_term is required")
	}

	events, err := svc.SearchRoomEvents(ctx, alice, "привет", nil, 5)
	if err != nil || len(events) != 1 {
		t.Fatalf("поиск: %v %v", events, err)
	}

	if len(search.rooms) != 2 || search.rooms[second].From != 42 || search.limit != 5 {
		t.Fatalf("комнаты поиска: %v limit=%d", search.rooms, search.limit)
	}

	if _, ok := search.rooms[foreign]; ok {
		t.Fatal("поиск в комнате, из которой пользователь вышел")
	}

	// filter.rooms: чужая и покинутая комнаты отбрасываются.
	if _, err := svc.SearchRoomEvents(ctx, alice, "x", []string{first, foreign, "!other:test.local"}, 0); err != nil {
		t.Fatalf("filter.rooms: %v", err)
	}

	if len(search.rooms) != 1 || search.rooms[first] != (core.Visibility{}) {
		t.Fatalf("filter.rooms: %v", search.rooms)
	}

	// Без сервиса видимости история не ограничивается.
	if _, err := core.NewSearchService(search, roomStore, nil).SearchRoomEvents(ctx, alice, "x", nil, 0); err != nil ||
		search.rooms[second] != (core.Visibility{}) {
		t.Fatalf("без видимости: %v %v", search.rooms, err)
	}

	search.err = errors.New("db down")

	if _, err := svc.SearchRoomEvents(ctx, alice, "x", nil, 0); !errors.Is(err, search.err) {
		t.Fatalf("сбой поиска: %v", err)
	}

	if _, err := core.NewSearchService(search, failingMemberships{}, nil).SearchRoomEvents(ctx, alice, "x", nil, 0); err == nil {
		t.Fatal("сбой чтения комнат не вернул ошибку")
	}
}

// fakeDirectory - каталог из заданных комнат.
type fakeDirectory struct {
	rooms []core.PublicRoom
	err   error
}

func (f *fakeDirectory) PublicRoomsDirectory(context.Context) ([]core.PublicRoom, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.rooms, nil
}

// Каталог: порядок по числу участников, поиск по name/topic/alias без учета
// регистра, фильтр room_types, пагинация курсором.
func TestDirectoryService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := &fakeDirectory{rooms: []core.PublicRoom{
		{RoomID: "!c:test.local", JoinedMembers: 1, Topic: "Test Topic Wombles"},
		{RoomID: "!a:test.local", JoinedMembers: 3, Name: "Alpha"},
		{RoomID: "!b:test.local", JoinedMembers: 1, CanonicalAlias: "#beta:test.local", RoomType: "m.space"},
	}}
	svc := core.NewDirectoryService(store)

	ids := func(page core.PublicRoomsPage) string {
		out := ""
		for _, room := range page.Rooms {
			out += room.RoomID
		}

		return out
	}

	page, err := svc.PublicRooms(ctx, core.PublicRoomsQuery{})
	if err != nil || ids(page) != "!a:test.local!b:test.local!c:test.local" || page.Next != "" || page.Total != 3 {
		t.Fatalf("весь каталог: %v %+v", err, page)
	}

	page, _ = svc.PublicRooms(ctx, core.PublicRoomsQuery{Search: "wombles"})
	if ids(page) != "!c:test.local" || page.Total != 1 {
		t.Fatalf("поиск по topic: %+v", page)
	}

	page, _ = svc.PublicRooms(ctx, core.PublicRoomsQuery{Search: "BETA"})
	if ids(page) != "!b:test.local" {
		t.Fatalf("поиск по алиасу: %+v", page)
	}

	page, _ = svc.PublicRooms(ctx, core.PublicRoomsQuery{RoomTypes: []string{""}})
	if ids(page) != "!a:test.local!c:test.local" {
		t.Fatalf("room_types [null]: %+v", page)
	}

	page, _ = svc.PublicRooms(ctx, core.PublicRoomsQuery{Limit: 2})
	if ids(page) != "!a:test.local!b:test.local" || page.Next == "" || page.Prev != "" {
		t.Fatalf("первая страница: %+v", page)
	}

	page, _ = svc.PublicRooms(ctx, core.PublicRoomsQuery{Limit: 2, Since: page.Next})
	if ids(page) != "!c:test.local" || page.Next != "" || page.Prev == "" {
		t.Fatalf("вторая страница: %+v", page)
	}

	store.rooms = nil

	if page, err := svc.PublicRooms(ctx, core.PublicRoomsQuery{}); err != nil || page.Rooms == nil || len(page.Rooms) != 0 {
		t.Fatalf("пустой каталог: %v %v", page, err)
	}

	store.err = errors.New("db down")

	if _, err := svc.PublicRooms(ctx, core.PublicRoomsQuery{}); !errors.Is(err, store.err) {
		t.Fatalf("сбой: %v", err)
	}
}
