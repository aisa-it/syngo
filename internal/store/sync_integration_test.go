//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
)

// queryCounter - трейсер, который считает запросы одного вызова через контекст
// (counterKey) и записывает их текст с аргументами (recordKey) - для проверки
// планов. Подключен к пулу openDB: раньше пул открывался без
// трейсера, и счетчик TestSyncLoaderQueryCountIsConstant всегда был 0.
type queryCounter struct{}

type counterKey struct{}

type recordKey struct{}

// recordedQuery - запрос, выполненный под recordKey.
type recordedQuery struct {
	SQL  string
	Args []any
}

func (queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if counter, ok := ctx.Value(counterKey{}).(*int); ok {
		*counter++
	}

	if recorded, ok := ctx.Value(recordKey{}).(*[]recordedQuery); ok {
		*recorded = append(*recorded, recordedQuery{SQL: data.SQL, Args: data.Args})
	}

	return ctx
}

func (queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// openCountingDB открывает пул с трейсером queryCounter: запросы, выполненные с
// контекстом, где лежит counterKey, увеличивают счетчик вызова.
func openCountingDB(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.OpenWithOptions(context.Background(), store.Options{
		DSN: testdb.DSN(t), MaxConns: 4, Tracer: queryCounter{},
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	return db
}

// freshSyncStore поднимает схему с нуля и возвращает хранилище данных /sync.
func freshSyncStore(t *testing.T) *store.SyncStore {
	t.Helper()

	migrator := newMigrator(t)

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean before up: %v", err)
	}

	if err := migrator.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = migrator.Down()
		_, _ = migrator.Close()
	})

	return store.NewSyncStore(openCountingDB(t))
}

// seedRooms создаёт user комнат по eventsPerRoom событий и членство пользователя.
func seedRooms(t *testing.T, syncStore *store.SyncStore, rooms, eventsPerRoom int) string {
	t.Helper()

	ctx := context.Background()
	db := openDB(t)

	const userID = benchUserID

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ($1, 'bench', 'x')
		 ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO rooms (room_id, room_version, creator, is_public)
		 SELECT '!bench' || i || ':test.local', '10', $1, false FROM generate_series(1, $2) i
		 ON CONFLICT (room_id) DO NOTHING`, userID, rooms); err != nil {
		t.Fatalf("insert rooms: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO events (event_id, room_id, type, sender, content, depth, origin_server_ts, origin)
		 SELECT '$e' || r || '_' || k || ':test.local', '!bench' || r || ':test.local',
		        'm.room.message', $1, '{"body":"x"}'::jsonb, k, 1700000000000 + k, 'test.local'
		 FROM generate_series(1, $2) r, generate_series(1, $3) k
		 ON CONFLICT (event_id) DO NOTHING`, userID, rooms, eventsPerRoom); err != nil {
		t.Fatalf("insert events: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
		 SELECT (SELECT e.event_id FROM events e WHERE e.room_id = '!bench' || r || ':test.local'
		         ORDER BY e.stream_ordering DESC LIMIT 1),
		        '!bench' || r || ':test.local', $1, $1, 'join'
		 FROM generate_series(1, $2) r
		 ON CONFLICT (room_id, user_id) DO UPDATE SET membership = 'join'`, userID, rooms); err != nil {
		t.Fatalf("insert memberships: %v", err)
	}

	return userID
}

// TestSyncLoaderQueryCountIsConstant — число SQL-запросов на сборку
// данных /sync не растет с числом комнат. Замер и initial, и incremental.
func TestSyncLoaderQueryCountIsConstant(t *testing.T) {
	syncStore := freshSyncStore(t)
	loader := core.NewSyncDataLoader(syncStore)
	ctx := context.Background()

	measure := func(rooms int) int {
		userID := seedRooms(t, syncStore, rooms, 3)

		var queries int

		callCtx := context.WithValue(ctx, counterKey{}, &queries)

		data, err := loader.Load(callCtx, userID, 0, 5)
		if err != nil {
			t.Fatalf("Load(%d комнат) error: %v", rooms, err)
		}

		if len(data.Memberships) != rooms {
			t.Fatalf("членств %d, ожидали %d", len(data.Memberships), rooms)
		}

		if len(data.Timeline) != rooms {
			t.Fatalf("комнат с событиями %d, ожидали %d", len(data.Timeline), rooms)
		}

		return queries
	}

	small := measure(10)
	large := measure(100)

	// Пустой счетчик значит, что трейсер не подключен: тогда сравнение 0 == 0
	// ничего не доказывает.
	if small == 0 || large == 0 {
		t.Fatalf("трейсер не посчитал запросы: 10 комнат → %d, 100 комнат → %d", small, large)
	}

	if small != large {
		t.Fatalf("запросов на sync зависит от числа комнат: 10 комнат → %d, 100 комнат → %d", small, large)
	}

	if small > syncLoadQueryBudget {
		t.Fatalf("запросов на sync %d, SLO - не больше %d", small, syncLoadQueryBudget)
	}

	// Incremental: новые события в каждой комнате и рост числа комнат не
	// добавляют запросов.
	position, err := syncStore.StreamPosition(ctx)
	if err != nil {
		t.Fatalf("StreamPosition() error: %v", err)
	}

	incremental := func(rooms int) int {
		seedRooms(t, syncStore, rooms, 4) // по новому событию в каждой из rooms комнат

		var queries int

		data, err := loader.Load(context.WithValue(ctx, counterKey{}, &queries), benchUserID, position, 5)
		if err != nil {
			t.Fatalf("Load(delta, %d комнат) error: %v", rooms, err)
		}

		if len(data.Timeline) == 0 {
			t.Fatalf("дельта по %d комнатам пуста", rooms)
		}

		return queries
	}

	deltaSmall := incremental(10)
	deltaLarge := incremental(100)

	if deltaSmall == 0 || deltaSmall != deltaLarge || deltaSmall > syncLoadQueryBudget {
		t.Fatalf("incremental: запросов 10 комнат → %d, 100 комнат → %d (бюджет %d, не должно зависеть от комнат)",
			deltaSmall, deltaLarge, syncLoadQueryBudget)
	}
}

// syncLoadQueryBudget - SLO ADR-0005 §2: SQL-запросов на сборку данных /sync.
const syncLoadQueryBudget = 8

// benchUserID - пользователь, которого создает seedRooms.
const benchUserID = "@bench:test.local"

// TestSyncLoaderDelta — incremental-сборка отдаёт только новые события и состояние
// затронутых комнат.
func TestSyncLoaderDelta(t *testing.T) {
	syncStore := freshSyncStore(t)
	loader := core.NewSyncDataLoader(syncStore)
	ctx := context.Background()

	userID := seedRooms(t, syncStore, 5, 3)

	position, err := syncStore.StreamPosition(ctx)
	if err != nil {
		t.Fatalf("StreamPosition() error: %v", err)
	}

	// Ничего нового: timeline пуст, состояние не запрашивается.
	empty, err := loader.Load(ctx, userID, position, 5)
	if err != nil {
		t.Fatalf("Load(empty) error: %v", err)
	}

	if len(empty.Timeline) != 0 || len(empty.State) != 0 {
		t.Fatalf("пустая дельта не пуста: timeline=%d state=%d", len(empty.Timeline), len(empty.State))
	}

	db := openDB(t)

	for i := 1; i <= 5; i++ {
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO events (event_id, room_id, type, sender, content, depth, origin_server_ts, origin)
			 VALUES ($1, $2, 'm.room.message', $3, '{"body":"new"}'::jsonb, 100, 1700000001000, 'test.local')`,
			fmt.Sprintf("$delta%d:test.local", i), fmt.Sprintf("!bench%d:test.local", i), userID); err != nil {
			t.Fatalf("insert delta: %v", err)
		}
	}

	delta, err := loader.Load(ctx, userID, position, 5)
	if err != nil {
		t.Fatalf("Load(delta) error: %v", err)
	}

	if len(delta.Timeline) != 5 {
		t.Fatalf("комнат с новыми событиями %d, ожидали 5", len(delta.Timeline))
	}

	for roomID, events := range delta.Timeline {
		if len(events) != 1 {
			t.Fatalf("в комнате %s событий %d, ожидали 1", roomID, len(events))
		}

		if events[0].Content == nil {
			t.Fatalf("событие без content: %+v", events[0])
		}
	}

	// Состояние запрошено для затронутых комнат (в комнатах есть membership-события нет,
	// поэтому state пуст — проверяем только что карта создана без ошибок).
	if delta.State == nil {
		t.Fatal("State должен быть не nil")
	}
}

// TestStateAtStreamPicksLastBeforePosition — состояние на позицию берёт последнее
// state-событие каждого ключа не позже pos (state на начало обрезанного
// timeline в initial sync).
func TestStateAtStreamPicksLastBeforePosition(t *testing.T) {
	syncStore := freshSyncStore(t)
	ctx := context.Background()
	db := openDB(t)

	const roomID = "!stateat:test.local"

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO rooms (room_id, room_version, creator) VALUES ($1,'10','@a:test.local')`, roomID); err != nil {
		t.Fatalf("room: %v", err)
	}

	seed := []struct {
		id, typ, key, content string
		pos                   int64
	}{
		{"$name-old:test.local", "m.room.name", "", `{"name":"old"}`, 5},
		{"$name-new:test.local", "m.room.name", "", `{"name":"new"}`, 9},
		{"$topic:test.local", "m.room.topic", "", `{"topic":"t"}`, 6},
	}

	for _, ev := range seed {
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, prev_events,
			                    auth_events, depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,$3,'@a:test.local',$4,$5,'{}','{}',1,$6,'test',$6)`,
			ev.id, roomID, ev.typ, ev.key, ev.content, ev.pos); err != nil {
			t.Fatalf("seed %s: %v", ev.id, err)
		}
	}

	states, err := syncStore.StateAtRoomsStream(ctx, map[string]int64{roomID: 7})
	if err != nil {
		t.Fatalf("StateAtRoomsStream: %v", err)
	}

	got := map[string]string{}
	for _, ev := range states[roomID] {
		got[ev.Type] = ev.EventID
	}

	if got["m.room.name"] != "$name-old:test.local" {
		t.Fatalf("name на позиции 7 = %s, ожидали старое", got["m.room.name"])
	}

	if got["m.room.topic"] != "$topic:test.local" {
		t.Fatalf("topic отсутствует: %v", got)
	}
}

// TestSafePositionSkipsUncommitted — выданный, но не закоммиченный номер не
// попадает в безопасную позицию: /sync не перескочит его.
func TestSafePositionSkipsUncommitted(t *testing.T) {
	_ = freshSyncStore(t) // поднимает схему
	db := openDB(t)
	ctx := context.Background()

	base, err := db.SyncPosition(ctx)
	if err != nil {
		t.Fatalf("SyncPosition: %v", err)
	}

	// Имитируем выданный, но не записанный в БД номер.
	db.RegisterPendingForTest(base + 100)

	safe, err := db.SyncPosition(ctx)
	if err != nil {
		t.Fatalf("SyncPosition: %v", err)
	}

	if safe >= base+100 {
		t.Fatalf("safe position %d перескочила незакоммиченный номер %d", safe, base+100)
	}

	if safe > base {
		t.Fatalf("safe position %d выше базовой %d без видимых событий", safe, base)
	}

	// Залипший номер не должен держать позицию вечно: по истечении TTL
	// ограничение снимается, даже если событие так и не появилось.
	db.ExpirePendingForTest()

	if safeAfterTTL, err := db.SyncPosition(ctx); err != nil {
		t.Fatalf("SyncPosition после TTL: %v", err)
	} else if safeAfterTTL != base {
		t.Fatalf("после TTL safe position %d, ожидали %d", safeAfterTTL, base)
	}
}

// TestEventsSinceHonoursLeaveBound — верхняя граница из `upper`
// отсекает события после выхода прямо в SQL, поэтому даже при ошибке в хендлере
// вышедший не получит переписку комнаты.
func TestEventsSinceHonoursLeaveBound(t *testing.T) {
	syncStore := freshSyncStore(t)

	ctx := context.Background()
	userID := seedRooms(t, syncStore, 1, 5)
	db := openDB(t)

	const roomID = "!bench1:test.local"

	// Позиция выхода — третье по счёту событие комнаты.
	var leaveStream int64

	if err := db.Pool().QueryRow(ctx,
		`SELECT stream_ordering FROM events WHERE room_id = $1 ORDER BY stream_ordering LIMIT 1 OFFSET 2`,
		roomID).Scan(&leaveStream); err != nil {
		t.Fatalf("read leave stream: %v", err)
	}

	events, err := syncStore.EventsSince(ctx, []string{roomID}, 0, 100, map[string]int64{roomID: leaveStream})
	if err != nil {
		t.Fatalf("EventsSince() error: %v", err)
	}

	if got := len(events[roomID]); got != 3 {
		t.Fatalf("после границы выхода пришло %d событий, ожидали 3", got)
	}

	for i := range events[roomID] {
		if events[roomID][i].StreamOrdering > leaveStream {
			t.Fatalf("событие %d за границей выхода (граница %d)",
				events[roomID][i].StreamOrdering, leaveStream)
		}

		if events[roomID][i].Sender == "" {
			t.Fatal("событие без sender")
		}
	}

	// Без границы — все пять событий (join-комната читает историю целиком).
	all, err := syncStore.EventsSince(ctx, []string{roomID}, 0, 100, nil)
	if err != nil {
		t.Fatalf("EventsSince() without bound: %v", err)
	}

	if got := len(all[roomID]); got != 5 {
		t.Fatalf("без границы пришло %d событий, ожидали 5", got)
	}

	_ = userID
}

// TestLazyMembersStateInSQL - lazy_load_members в SQL: текущее
// состояние и состояние на позиции отдают из m.room.member только перечисленных
// пользователей, остальное состояние - целиком.
func TestLazyMembersStateInSQL(t *testing.T) {
	syncStore := freshSyncStore(t)
	ctx := context.Background()
	db := openDB(t)

	const roomID = "!lazy:test.local"

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO rooms (room_id, room_version, creator) VALUES ($1,'10','@a:test.local')`, roomID); err != nil {
		t.Fatalf("room: %v", err)
	}

	seed := []struct {
		id, typ, key string
		pos          int64
	}{
		{"$create:test.local", "m.room.create", "", 1},
		{"$ma:test.local", "m.room.member", "@a:test.local", 2},
		{"$mb:test.local", "m.room.member", "@b:test.local", 3},
		{"$mc:test.local", "m.room.member", "@c:test.local", 4},
	}

	for _, ev := range seed {
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, prev_events,
			                    auth_events, depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,$3,'@a:test.local',$4,'{"membership":"join"}','{}','{}',1,$5,'test',$5)`,
			ev.id, roomID, ev.typ, ev.key, ev.pos); err != nil {
			t.Fatalf("seed %s: %v", ev.id, err)
		}

		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO room_state (room_id, type, state_key, event_id) VALUES ($1,$2,$3,$4)`,
			roomID, ev.typ, ev.key, ev.id); err != nil {
			t.Fatalf("state %s: %v", ev.id, err)
		}
	}

	ids := func(events []core.Event) []string {
		out := make([]string, 0, len(events))
		for i := range events {
			out = append(out, events[i].EventID)
		}

		sort.Strings(out)

		return out
	}

	want := []string{"$create:test.local", "$mb:test.local"}

	current, err := syncStore.StateMembersForRooms(ctx, []string{roomID}, []string{"@b:test.local"})
	if err != nil {
		t.Fatalf("StateMembersForRooms: %v", err)
	}

	if got := ids(current[roomID]); !slices.Equal(got, want) {
		t.Fatalf("текущее состояние: %v, ожидали %v", got, want)
	}

	atStream, err := syncStore.StateAtRoomsStreamMembers(ctx, map[string]int64{roomID: 10}, []string{"@b:test.local"})
	if err != nil {
		t.Fatalf("StateAtRoomsStreamMembers: %v", err)
	}

	if got := ids(atStream[roomID]); !slices.Equal(got, want) {
		t.Fatalf("состояние на позиции: %v, ожидали %v", got, want)
	}

	all, err := syncStore.StateAtRoomsStream(ctx, map[string]int64{roomID: 10})
	if err != nil || len(all[roomID]) != 4 {
		t.Fatalf("без фильтра: %v %v", ids(all[roomID]), err)
	}
}
