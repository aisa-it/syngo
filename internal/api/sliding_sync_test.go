package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestParseSlidingBody - разбор тела sliding sync: списки, фильтры (nil - нет
// условия, пустой - есть), подписки, расширения с умолчаниями lists/rooms ["*"].
func TestParseSlidingBody(t *testing.T) {
	t.Parallel()

	space := "m.space"
	since := "42"
	limit := 5

	body := &slidingSyncBody{
		ConnID: "room-list",
		Lists: map[string]slidingListBody{"all": {
			slidingRoomBody: slidingRoomBody{RequiredState: [][]string{{"m.room.name", ""}, {"m.room.member", "$LAZY"}}, TimelineLimit: 5000},
			Ranges:          [][]int{{0, 19}},
			Filters:         &slidingFiltersBody{NotRoomTypes: []*string{&space, nil}, Tags: []string{}},
		}},
		RoomSubscriptions: map[string]slidingRoomBody{"!r:s": {TimelineLimit: -3}},
		Extensions: slidingExtensionsBody{
			ToDevice:    &slidingToDeviceBody{Enabled: true, Since: &since, Limit: &limit},
			E2EE:        &slidingToggleBody{Enabled: true},
			AccountData: &slidingRoomsExtBody{Enabled: true},
			Receipts:    &slidingRoomsExtBody{Enabled: false},
			Typing:      &slidingRoomsExtBody{Enabled: true, Lists: []string{}, Rooms: []string{"!r:s"}},
		},
	}

	req, ext, msg := parseSlidingBody(body)
	if msg != "" {
		t.Fatalf("ошибка разбора: %s", msg)
	}

	list := req.Lists["all"]
	if req.ConnID != "room-list" || list.Config.TimelineLimit != core.SlidingMaxTimelineLimit ||
		!list.Config.RequiredState.Lazy() || list.Ranges[0] != [2]int{0, 19} {
		t.Fatalf("список: %+v", list)
	}

	f := list.Filters
	if !f.HasNotRoomTypes || f.HasRoomTypes || !f.HasTags || f.HasNotTags || len(f.NotRoomTypes) != 2 {
		t.Fatalf("фильтры: %+v", f)
	}

	if req.Subscriptions["!r:s"].TimelineLimit != 0 {
		t.Fatalf("подписка: %+v", req.Subscriptions)
	}

	if !ext.toDevice || ext.toDeviceSince != 42 || ext.toDeviceLimit != 5 || !ext.e2ee || ext.receipts != nil {
		t.Fatalf("расширения: %+v", ext)
	}

	if !slices.Equal(ext.accountData.Lists, []string{"*"}) || !slices.Equal(ext.accountData.Rooms, []string{"*"}) ||
		len(ext.typing.Lists) != 0 || !slices.Equal(ext.typing.Rooms, []string{"!r:s"}) {
		t.Fatalf("lists/rooms расширений: %+v %+v", ext.accountData, ext.typing)
	}

	bad := "x"

	for name, b := range map[string]*slidingSyncBody{
		"range":          {Lists: map[string]slidingListBody{"a": {Ranges: [][]int{{0}}}}},
		"negative range": {Lists: map[string]slidingListBody{"a": {Ranges: [][]int{{-1, 3}}}}},
		"required_state": {Lists: map[string]slidingListBody{"a": {slidingRoomBody: slidingRoomBody{RequiredState: [][]string{{"x"}}}}}},
		"subscription":   {RoomSubscriptions: map[string]slidingRoomBody{"!r": {RequiredState: [][]string{{"a", "b", "c"}}}}},
		"since":          {Extensions: slidingExtensionsBody{ToDevice: &slidingToDeviceBody{Enabled: true, Since: &bad}}},
	} {
		if _, _, msg := parseSlidingBody(b); msg == "" {
			t.Fatalf("%s: ожидали ошибку", name)
		}
	}
}

// TestExtensionRooms - lists/rooms расширения сужают комнаты ответа.
func TestExtensionRooms(t *testing.T) {
	t.Parallel()

	out := &core.SlidingSyncOutput{
		Relevant: map[string]bool{"!a": true, "!b": true, "!sub": true},
		Lists: map[string]core.SlidingListResult{
			"one": {Ops: []core.SlidingListOp{{RoomIDs: []string{"!a"}}}},
			"two": {Ops: []core.SlidingListOp{{RoomIDs: []string{"!b"}}}},
		},
	}

	for name, tc := range map[string]struct {
		lists, rooms []string
		want         []string
	}{
		"все":        {[]string{"*"}, []string{"*"}, []string{"!a", "!b", "!sub"}},
		"список":     {[]string{"two"}, []string{}, []string{"!b"}},
		"все списки": {[]string{"*"}, []string{}, []string{"!a", "!b"}},
		"комната":    {[]string{}, []string{"!sub", "!unknown"}, []string{"!sub"}},
		"ничего":     {[]string{}, []string{}, []string{}},
		"нет списка": {[]string{"missing"}, nil, []string{}},
		"и то, и то": {[]string{"one"}, []string{"!sub"}, []string{"!a", "!sub"}},
	} {
		got := extensionRooms(&slidingRoomsExtBody{Lists: tc.lists, Rooms: tc.rooms}, out)
		if !slices.Equal(got, tc.want) {
			t.Fatalf("%s: %v, ожидали %v", name, got, tc.want)
		}
	}
}

// TestTimelineReceipts - при первой отдаче комнаты - отметки событий ее timeline
// и свои.
func TestTimelineReceipts(t *testing.T) {
	t.Parallel()

	all := []core.ReceiptEvent{
		{EventID: "$in", UserID: "@b:s"}, {EventID: "$old", UserID: "@b:s"}, {EventID: "$old", UserID: "@me:s"},
	}
	room := &slidingRoomView{Timeline: []eventView{{EventID: "$in"}}}

	got := timelineReceipts(all, "@me:s", room)
	if len(got) != 2 || got[0].EventID != "$in" || got[1].UserID != "@me:s" {
		t.Fatalf("отметки: %+v", got)
	}

	if got := timelineReceipts(all, "@me:s", nil); len(got) != 1 {
		t.Fatalf("без комнаты в ответе - только свои: %+v", got)
	}
}

// TestSlidingSyncRoute - поддержка объявлена в /versions (по ней Element X
// включает протокол); маршрут закрыт токеном; без БД - 503, а не 404.
func TestSlidingSyncRoute(t *testing.T) {
	t.Parallel()

	srv := newRouteAuthServer(t)
	path := "/_matrix/client/unstable/org.matrix.simplified_msc3575/sync"

	versions := do(t, srv, http.MethodGet, "/_matrix/client/versions", "", "")
	if !strings.Contains(versions.Body.String(), `"org.matrix.simplified_msc3575":true`) {
		t.Fatalf("versions: %s", versions.Body.String())
	}

	assertTokenRequired(t, srv, http.MethodPost, path, `{}`)

	token := registerUser(t, srv, "ssroute", "password-123456").AccessToken
	if rec := do(t, srv, http.MethodPost, path+"?timeout=0", `{"lists":{}}`, token); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("без БД: %d %s", rec.Code, rec.Body.String())
	}
}
