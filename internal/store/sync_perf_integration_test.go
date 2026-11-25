//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// Регрессионные тесты производительности /sync и записи событий:
// планы запросов проверяются через EXPLAIN на той же базе, а не замером времени -
// на маленькой тестовой базе время ничего не показывает, а план показывает.

// perfFresh поднимает схему с нуля.
func perfFresh(t *testing.T) *store.DB {
	t.Helper()

	m := newMigrator(t)
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("down: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() { _ = m.Down(); _, _ = m.Close() })

	return openDB(t)
}

func perfExec(t *testing.T, db *store.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.Pool().Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// perfSeed - rooms комнат, users пользователей (каждый в нескольких комнатах),
// events сообщений вперемешку по комнатам, история изменений ключей и очередь
// to-device. Возвращает комнаты пользователя @p1.
func perfSeed(t *testing.T, db *store.DB, rooms, users, events int) []string {
	t.Helper()

	perfExec(t, db, `INSERT INTO users (user_id, localpart, password_hash)
		SELECT '@p'||i||':t', 'p'||i, 'x' FROM generate_series(1, $1) i`, users)
	perfExec(t, db, `INSERT INTO devices (user_id, device_id) SELECT user_id, 'D' FROM users`)
	perfExec(t, db, `INSERT INTO rooms (room_id, creator) SELECT '!q'||i||':t', '@p1:t' FROM generate_series(1, $1) i`, rooms)
	perfExec(t, db, `INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
		SELECT '$c'||i, '!q'||i||':t', 'm.room.create', '@p1:t', '', '{}', 1, 1, 't' FROM generate_series(1, $1) i`, rooms)
	perfExec(t, db, `INSERT INTO room_state (room_id, type, state_key, event_id)
		SELECT '!q'||i||':t', 'm.room.create', '', '$c'||i FROM generate_series(1, $1) i`, rooms)
	perfExec(t, db, `CREATE TEMP TABLE perf_mem AS
		SELECT DISTINCT 1 + ((u * 7 + k * 3) % $1) AS r, '@p'||u||':t' AS u
		FROM generate_series(1, $2) u, generate_series(0, 9) k`, rooms, users)
	perfExec(t, db, `INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
		SELECT '$j'||r||u, '!q'||r||':t', 'm.room.member', u, u, '{"membership":"join"}', 2, 2, 't' FROM perf_mem`)
	perfExec(t, db, `INSERT INTO room_state (room_id, type, state_key, event_id)
		SELECT '!q'||r||':t', 'm.room.member', u, '$j'||r||u FROM perf_mem`)
	perfExec(t, db, `INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
		SELECT '$j'||r||u, '!q'||r||':t', u, u, 'join' FROM perf_mem`)
	perfExec(t, db, `DROP TABLE perf_mem`)
	perfExec(t, db, `INSERT INTO events (event_id, room_id, type, sender, content, depth, origin_server_ts, origin)
		SELECT '$m'||g, '!q'||(1 + (g * 13) % $1)||':t', 'm.room.message', '@p'||(1 + g % $2)||':t',
		       '{"body":"x"}', 3 + g, 1700000000000 + g, 't'
		FROM generate_series(1, $3) g`, rooms, users, events)
	perfExec(t, db, `INSERT INTO device_lists_changes (user_id, device_id)
		SELECT '@p'||(1 + g % $1)||':t', '' FROM generate_series(1, 2000) g`, users)
	perfExec(t, db, `INSERT INTO device_inbox (user_id, device_id, event_type, sender, content)
		SELECT '@p'||(1 + g % $1)||':t', 'D', 'm.room.encrypted', '@p2:t', '{}' FROM generate_series(1, 500) g`, users)
	perfExec(t, db, `ANALYZE`)

	var roomIDs []string
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT array_agg(room_id ORDER BY room_id) FROM room_memberships WHERE user_id = '@p1:t'`).Scan(&roomIDs); err != nil {
		t.Fatalf("rooms of p1: %v", err)
	}

	return roomIDs
}

// planNode - узел плана EXPLAIN (FORMAT JSON).
type planNode struct {
	NodeType       string     `json:"Node Type"`
	RelationName   string     `json:"Relation Name"`
	ActualRows     float64    `json:"Actual Rows"`
	ActualLoops    float64    `json:"Actual Loops"`
	RemovedFilter  float64    `json:"Rows Removed by Filter"`
	RemovedRecheck float64    `json:"Rows Removed by Index Recheck"`
	Alias          string     `json:"Alias"`
	IndexCond      string     `json:"Index Cond"`
	Plans          []planNode `json:"Plans"`
}

// touched - сколько строк таблицы узел прочитал, включая отброшенные фильтром.
func (n planNode) touched() float64 {
	return (n.ActualRows + n.RemovedFilter + n.RemovedRecheck) * n.ActualLoops
}

func (n planNode) walk(visit func(planNode)) {
	visit(n)

	for _, child := range n.Plans {
		child.walk(visit)
	}
}

// explain строит план запроса с теми же аргументами. enable_seqscan=off: если
// подходящего индекса нет, планировщик все равно выберет Seq Scan - так тест
// ловит пропавший или неприменимый индекс и на маленькой базе.
func explain(t *testing.T, db *store.DB, q recordedQuery, analyze bool) planNode {
	t.Helper()

	ctx := context.Background()

	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	defer conn.Release()

	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("set: %v", err)
	}

	defer func() { _, _ = conn.Exec(ctx, `RESET enable_seqscan`) }()

	options := "FORMAT JSON"
	if analyze {
		options = "ANALYZE, FORMAT JSON"
	}

	var raw string
	if err := conn.QueryRow(ctx, "EXPLAIN ("+options+") "+q.SQL, q.Args...).Scan(&raw); err != nil {
		t.Fatalf("explain %s: %v", q.SQL, err)
	}

	var plans []struct {
		Plan planNode `json:"Plan"`
	}

	if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) == 0 {
		t.Fatalf("plan json: %v %s", err, raw)
	}

	return plans[0].Plan
}

// bigTables - таблицы, которые растут с историей сервера: полный скан любой из
// них в /sync или при записи - дефект производительности.
var bigTables = []string{"events", "device_lists_changes", "device_inbox", "room_state", "room_memberships", "account_data"}

// record выполняет fn и возвращает выполненные им запросы.
func record(t *testing.T, fn func(ctx context.Context) error) []recordedQuery {
	t.Helper()

	var recorded []recordedQuery

	if err := fn(context.WithValue(context.Background(), recordKey{}, &recorded)); err != nil {
		t.Fatalf("call: %v", err)
	}

	return recorded
}

// TestSyncQueriesUseIndexes - запросы /sync и записи события не сканируют
// большие таблицы целиком. Две проверки каждого запроса:
//   - план при enable_seqscan=off не содержит Seq Scan по большой таблице;
//   - EXPLAIN ANALYZE: узел большой таблицы читает (с отброшенными фильтром) не
//     больше 10% ее строк - так ловится и полный проход по чужому индексу.
//
// До исправления: SyncPosition и safePosition читали events, device_inbox,
// device_lists_changes целиком; изменения ключей - всю device_lists_changes.
func TestSyncQueriesUseIndexes(t *testing.T) {
	db := perfFresh(t)
	ctx := context.Background()
	syncStore := store.NewSyncStore(db)
	rooms := perfSeed(t, db, 60, 300, 6000)

	sizes := map[string]float64{}

	for _, table := range bigTables {
		var n float64
		if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}

		sizes[table] = n
	}

	upper, err := db.SyncPosition(ctx)
	if err != nil {
		t.Fatalf("position: %v", err)
	}

	var since, eventsSince int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT stream_ordering FROM device_lists_changes ORDER BY stream_ordering DESC OFFSET 20 LIMIT 1`).Scan(&since); err != nil {
		t.Fatalf("since: %v", err)
	}

	if err := db.Pool().QueryRow(ctx, `SELECT max(stream_ordering) - 50 FROM events`).Scan(&eventsSince); err != nil {
		t.Fatalf("events since: %v", err)
	}

	// Незакоммиченный номер включает проверку safePosition.
	db.RegisterPendingForTest(upper + 1)

	calls := map[string]func(ctx context.Context) error{
		"SyncPosition": func(ctx context.Context) error { _, err := db.SyncPosition(ctx); return err },
		"DeviceListChangesFor": func(ctx context.Context) error {
			_, err := db.DeviceListChangesFor(ctx, "@p1:t", since, upper)
			return err
		},
		"EventsSince initial": func(ctx context.Context) error {
			_, err := syncStore.EventsSince(ctx, rooms, 0, 6, nil)
			return err
		},
		"EventsSince incremental": func(ctx context.Context) error {
			_, err := syncStore.EventsSince(ctx, rooms, eventsSince, 6, map[string]int64{rooms[0]: upper})
			return err
		},
		"StateDeltaForRooms": func(ctx context.Context) error {
			_, err := syncStore.StateDeltaForRooms(ctx, rooms, eventsSince, []string{"@p1:t", "@p2:t"})
			return err
		},
		"AuthStateEvents": func(ctx context.Context) error {
			_, err := store.NewRoomStore(db).AuthStateEvents(ctx, rooms[0], []string{"@p1:t"})
			return err
		},
		"MembershipRooms": func(ctx context.Context) error { _, err := syncStore.MembershipRooms(ctx, "@p1:t"); return err },
		"PendingToDevice": func(ctx context.Context) error {
			_, err := db.PendingToDevice(ctx, "@p1:t", "D", 0, 100)
			return err
		},
	}

	for name, call := range calls {
		queries := record(t, call)
		if len(queries) == 0 {
			t.Fatalf("%s: запросы не записаны (трейсер не подключен?)", name)
		}

		for _, q := range queries {
			explain(t, db, q, false).walk(func(n planNode) {
				if n.NodeType == "Seq Scan" && slices.Contains(bigTables, n.RelationName) {
					t.Errorf("%s: Seq Scan по %s в запросе:\n%s", name, n.RelationName, strings.TrimSpace(q.SQL))
				}
			})

			explain(t, db, q, true).walk(func(n planNode) {
				if size, ok := sizes[n.RelationName]; ok && n.touched() > size/10 {
					t.Errorf("%s: %s читает %.0f из %.0f строк %s (строк %.0f, отброшено %.0f, циклов %.0f) в запросе:\n%s",
						name, n.NodeType, n.touched(), size, n.RelationName,
						n.ActualRows, n.RemovedFilter+n.RemovedRecheck, n.ActualLoops, strings.TrimSpace(q.SQL))
				}
			})
		}
	}
}

// TestSyncStateDeltaGenericPlan - изменения состояния после since читаются по
// позиции и в generic-плане. pgx с шестого вызова выполняет
// подготовленный запрос generic-планом, где since неизвестен: планировщик брал
// индекс (room_id, type, state_key) и читал все state-события комнат с фильтром
// по позиции - верхний запрос pg_stat_statements профиля M (16 мс на /sync).
// Частичный индекс events_room_state_stream_idx (миграция 0047) дает условие по
// stream_ordering в Index Cond.
func TestSyncStateDeltaGenericPlan(t *testing.T) {
	db := perfFresh(t)
	ctx := context.Background()
	syncStore := store.NewSyncStore(db)
	// Как на живом сервере: сообщений намного больше, чем событий членства, а
	// event_id - хэши. С упорядоченными '$m1', '$j1' из perfSeed планировщик
	// выбирает Merge Join по events_pkey, который на синтетике рано
	// останавливается, а на настоящих id читает всю таблицу.
	perfExec(t, db, `INSERT INTO users (user_id, localpart, password_hash)
		SELECT '@p'||i||':t', 'p'||i, 'x' FROM generate_series(1, 300) i`)
	perfExec(t, db, `INSERT INTO rooms (room_id, creator) SELECT '!q'||i||':t', '@p1:t' FROM generate_series(1, 60) i`)
	perfExec(t, db, `CREATE TEMP TABLE perf_mem AS
		SELECT DISTINCT 1 + ((u * 7 + k * 3) % 60) AS r, '@p'||u||':t' AS u
		FROM generate_series(1, 300) u, generate_series(0, 9) k`)
	perfExec(t, db, `INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
		SELECT '$'||md5('j'||r||u), '!q'||r||':t', 'm.room.member', u, u, '{"membership":"join"}', 2, 2, 't' FROM perf_mem`)
	perfExec(t, db, `INSERT INTO room_state (room_id, type, state_key, event_id)
		SELECT '!q'||r||':t', 'm.room.member', u, '$'||md5('j'||r||u) FROM perf_mem`)
	perfExec(t, db, `INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
		SELECT '$'||md5('j'||r||u), '!q'||r||':t', u, u, 'join' FROM perf_mem`)
	perfExec(t, db, `DROP TABLE perf_mem`)
	perfExec(t, db, `INSERT INTO events (event_id, room_id, type, sender, content, depth, origin_server_ts, origin)
		SELECT '$'||md5('m'||g), '!q'||(1 + (g * 13) % 60)||':t', 'm.room.message', '@p'||(1 + g % 300)||':t',
		       '{"body":"x"}', 3 + g, 1700000000000 + g, 't'
		FROM generate_series(1, 60000) g`)
	perfExec(t, db, `ANALYZE`)

	var rooms []string
	if err := db.Pool().QueryRow(ctx,
		`SELECT array_agg(room_id ORDER BY room_id) FROM room_memberships WHERE user_id = '@p1:t'`).Scan(&rooms); err != nil {
		t.Fatalf("rooms of p1: %v", err)
	}

	queries := record(t, func(ctx context.Context) error {
		_, err := syncStore.StateDeltaForRooms(ctx, rooms, 100, []string{"@p1:t"})
		return err
	})
	if len(queries) != 1 {
		t.Fatalf("StateDeltaForRooms: %d запросов, ожидали 1", len(queries))
	}

	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	defer conn.Release()

	// Простой протокол без аргументов: $1..$3 остаются параметрами generic-плана.
	results, err := conn.Conn().PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "+queries[0].SQL).ReadAll()
	if err != nil || len(results) != 1 || len(results[0].Rows) != 1 {
		t.Fatalf("explain: %v", err)
	}

	raw := string(results[0].Rows[0][0])

	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &plans); err != nil || len(plans) == 0 {
		t.Fatalf("plan json: %v %s", err, raw)
	}

	found := false

	plans[0].Plan.walk(func(n planNode) {
		if n.RelationName != "events" || n.Alias != "changed" {
			return
		}

		found = true

		// Комната и позиция - в одном условии одного индекса: Index Scan несет его
		// сам, Bitmap Heap Scan - в дочернем Bitmap Index Scan. BitmapAnd
		// (room_id) и (stream_ordering) отдельными индексами - это чтение всех
		// событий после since по серверу.
		ok := false

		n.walk(func(child planNode) {
			if strings.Contains(child.IndexCond, "room_id") && strings.Contains(child.IndexCond, "stream_ordering") {
				ok = true
			}
		})

		if !ok {
			t.Errorf("изменения состояния читаются не по (room_id, stream_ordering) (%s):\n%s", n.NodeType, raw)
		}
	})

	if !found {
		t.Fatalf("в generic-плане нет индексного чтения events changed:\n%s", raw)
	}
}

// TestSyncInitialReadsOnlyLimit - initial sync читает из events не больше
// perRoom строк на комнату, а не всю историю. До исправления
// row_number() по всем событиям комнат: на 1 млн событий - 2,5 с.
func TestSyncInitialReadsOnlyLimit(t *testing.T) {
	db := perfFresh(t)
	syncStore := store.NewSyncStore(db)
	rooms := perfSeed(t, db, 40, 60, 4000)

	const perRoom = 6

	queries := record(t, func(ctx context.Context) error {
		_, err := syncStore.EventsSince(ctx, rooms, 0, perRoom, nil)
		return err
	})

	if len(queries) != 1 {
		t.Fatalf("EventsSince: %d запросов, ожидали 1", len(queries))
	}

	var read float64

	explain(t, db, queries[0], true).walk(func(n planNode) {
		if n.RelationName == "events" {
			read += n.ActualRows * n.ActualLoops
		}
	})

	if limit := float64(len(rooms) * perRoom); read > limit {
		t.Fatalf("initial EventsSince прочитал %.0f строк events, предел %d комнат * %d = %.0f",
			read, len(rooms), perRoom, limit)
	}
}

// TestJoinDeviceListFanout - join пишет одну строку device_lists_changes на
// вступившего, а не на каждого участника (раньше 200 входов давали 20300
// строк). Вступивший при этом видит
// device-листы всех участников комнаты, посторонний - ничего.
func TestJoinDeviceListFanout(t *testing.T) {
	db := perfFresh(t)
	ctx := context.Background()
	rs := store.NewRoomStore(db)

	perfExec(t, db, `INSERT INTO rooms (room_id, creator) VALUES ('!fan:t', '@u0:t'), ('!other:t', '@x:t')`)

	const joins = 200

	var lastBefore int64

	for i := 0; i < joins; i++ {
		userID := fmt.Sprintf("@u%d:t", i)
		event := core.Event{
			EventID: fmt.Sprintf("$fan%d", i), RoomID: "!fan:t", Type: "m.room.member", Sender: userID,
			StateKey: &userID, Content: []byte(`{"membership":"join"}`), Depth: int64(i + 1),
			OriginServerTS: int64(i), Origin: "t",
		}

		written, _, err := rs.AppendEvent(ctx, event, nil)
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}

		if i == joins-1 {
			lastBefore = written.StreamOrdering - 1
		}
	}

	stranger := "@x:t"
	if _, _, err := rs.AppendEvent(ctx, core.Event{
		EventID: "$x", RoomID: "!other:t", Type: "m.room.member", Sender: stranger, StateKey: &stranger,
		Content: []byte(`{"membership":"join"}`), Depth: 1, OriginServerTS: 1, Origin: "t",
	}, nil); err != nil {
		t.Fatalf("stranger join: %v", err)
	}

	var rows int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM device_lists_changes`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}

	if rows != joins+1 {
		t.Fatalf("%d join дали %d строк device_lists_changes, ожидали %d (одна на вступившего)", joins+1, rows, joins+1)
	}

	upper, err := db.SyncPosition(ctx)
	if err != nil {
		t.Fatalf("position: %v", err)
	}

	// Последний вступивший видит всех участников комнаты, хотя их строки старше окна.
	last, err := db.DeviceListChangesFor(ctx, fmt.Sprintf("@u%d:t", joins-1), lastBefore, upper)
	if err != nil {
		t.Fatalf("changes for last: %v", err)
	}

	if len(last) != joins || !slices.Contains(last, "@u0:t") {
		t.Fatalf("вступивший видит %d изменений, ожидали %d участников комнаты", len(last), joins)
	}

	if slices.Contains(last, stranger) {
		t.Fatalf("в изменениях участника комнаты посторонний %s", stranger)
	}

	// Первый участник видит вступления остальных после своего join.
	first, err := db.DeviceListChangesFor(ctx, "@u0:t", lastBefore, upper)
	if err != nil {
		t.Fatalf("changes for first: %v", err)
	}

	if !slices.Contains(first, fmt.Sprintf("@u%d:t", joins-1)) || slices.Contains(first, stranger) {
		t.Fatalf("изменения первого участника: %v", first)
	}

	// Посторонний не видит никого из комнаты.
	other, err := db.DeviceListChangesFor(ctx, stranger, 0, upper)
	if err != nil {
		t.Fatalf("changes for stranger: %v", err)
	}

	if len(other) != 1 || other[0] != stranger {
		t.Fatalf("посторонний видит %v, ожидали только себя", other)
	}
}

// TestSessionLastValidatedThrottled - проверка токена не пишет в access_tokens
// на каждый запрос: метка обновляется не чаще раза в минуту.
func TestSessionLastValidatedThrottled(t *testing.T) {
	db := perfFresh(t)
	auth := store.NewAuthStore(db)

	perfExec(t, db, `INSERT INTO users (user_id, localpart, password_hash) VALUES ('@a:t', 'a', 'x')`)
	perfExec(t, db, `INSERT INTO devices (user_id, device_id) VALUES ('@a:t', 'D')`)
	perfExec(t, db, `INSERT INTO access_tokens (token_hash, user_id, device_id) VALUES ($1, '@a:t', 'D')`,
		core.HashToken("secret"))

	count := func() int {
		var queries int

		ctx := context.WithValue(context.Background(), counterKey{}, &queries)
		if _, err := auth.SessionByTokenHash(ctx, core.HashToken("secret")); err != nil {
			t.Fatalf("session: %v", err)
		}

		return queries
	}

	if first := count(); first != 2 {
		t.Fatalf("первая проверка токена: %d запросов, ожидали 2 (чтение и метка)", first)
	}

	for i := 0; i < 3; i++ {
		if again := count(); again != 1 {
			t.Fatalf("повторная проверка токена: %d запросов, ожидали 1 (без UPDATE)", again)
		}
	}
}
