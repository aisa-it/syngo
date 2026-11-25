//go:build integration

package exportdata_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/aisa-it/syngo/internal/exportdata"
	"github.com/aisa-it/syngo/internal/importevents"
	"github.com/aisa-it/syngo/internal/media"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
)

func freshDB(t *testing.T) *store.DB {
	t.Helper()

	dsn := testdb.DSN(t)

	m, err := migrate.New("file://../../migrations", "pgx5://"+strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres://"), "postgresql://"))
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	db, err := store.Open(context.Background(), dsn, 8, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(db.Close)

	return db
}

// seedProfile создаёт профиль приёмки: 2 пользователя, 2 комнаты, 50 событий, 2 медиа.
func seedProfile(t *testing.T, db *store.DB, mediaDir string) {
	t.Helper()

	ctx := context.Background()

	for _, u := range [][3]string{
		{"@alice:test.local", "alice", "$argon2id$v=19$m=65536,t=2,p=1$c2FsdA$a2V5"},
		{"@bob:test.local", "bob", "$argon2id$v=19$m=65536,t=2,p=1$c2FsdA$a2V5"},
	} {
		if _, err := db.Pool().Exec(ctx,
			"INSERT INTO users (user_id, localpart, password_hash) VALUES ($1,$2,$3)", u[0], u[1], u[2]); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}

	rooms := []string{"!one:test.local", "!two:test.local"}

	for _, r := range rooms {
		if _, err := db.Pool().Exec(ctx,
			"INSERT INTO rooms (room_id, creator) VALUES ($1,'@alice:test.local')", r); err != nil {
			t.Fatalf("insert room: %v", err)
		}
	}

	// 50 событий: по 25 на комнату, stream_ordering 1..50.
	for i := int64(1); i <= 50; i++ {
		roomID := rooms[0]
		if i%2 == 0 {
			roomID = rooms[1]
		}

		content := []byte(fmt.Sprintf(`{"body":"msg %d"}`, i))

		_, err := db.Pool().Exec(ctx, `
			INSERT INTO events (event_id, room_id, type, sender, content, depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,'m.room.message','@alice:test.local',$3,$4,1700000000000,'test.local',$5)`,
			fmt.Sprintf("$e%d", i), roomID, content, i, i)
		if err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
	}

	// membership: alice join в обе комнаты, bob join во вторую. Каждому membership нужен
	// реальный m.room.member event (FK room_memberships.event_id → events.event_id).
	stream := int64(51)

	for _, row := range [][2]string{{rooms[0], "@alice:test.local"}, {rooms[1], "@alice:test.local"}, {rooms[1], "@bob:test.local"}} {
		eventID := fmt.Sprintf("$member%d", stream)

		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,'m.room.member','@alice:test.local',$3,'{"membership":"join"}',$4,1700000000000,'test.local',$5)`,
			eventID, row[0], row[1], stream, stream); err != nil {
			t.Fatalf("insert member event: %v", err)
		}

		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
			VALUES ($1,$2,$3,'@alice:test.local','join')`,
			eventID, row[0], row[1]); err != nil {
			t.Fatalf("insert membership: %v", err)
		}

		stream++
	}

	// 2 медиа в нашем объектном хранилище.
	objects, err := media.NewLocalStore(mediaDir)
	if err != nil {
		t.Fatalf("object store: %v", err)
	}

	for i, payload := range [][]byte{[]byte("first media"), []byte("second media!")} {
		mediaID := fmt.Sprintf("media%d", i+1)

		key := "test.local/" + mediaID

		sum := sha256.Sum256(payload)
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO media (content_uri, media_id, server_name, content_type, size_bytes, sha256, storage_key)
			VALUES ($1,$2,'test.local','text/plain',$3,$4,$5)`,
			"mxc://test.local/"+mediaID, mediaID, len(payload), sum[:], key); err != nil {
			t.Fatalf("insert media: %v", err)
		}

		// файл кладём через Put (как Upload), хеш не важен для экспорта.
		if _, err := objects.Put(ctx, key, bytes.NewReader(payload)); err != nil {
			t.Fatalf("put object: %v", err)
		}
	}
}

// TestExportRoundTrip — наш дамп читается нашими импортёрами
// и даёт идентичные event_id/порядок/состояние/медиа.
func TestExportRoundTrip(t *testing.T) {
	src := freshDB(t)
	mediaDir := t.TempDir()

	seedProfile(t, src, mediaDir)

	var dump bytes.Buffer

	mediaOut := t.TempDir()

	exporter := exportdata.New(src.Pool(), exportdata.Options{
		BatchSize: 7, MediaSourceDir: mediaDir, MediaOutDir: mediaOut,
	})

	report, err := exporter.Run(context.Background(), &dump)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	// 50 сообщений + 3 membership-события
	if report.Users != 2 || report.Rooms != 2 || report.Events != 53 || report.Media != 2 || report.MediaCopied != 2 {
		t.Fatalf("отчёт выгрузки: %+v", report)
	}

	ctx := context.Background()

	// Снимок ожиданий до очистки БД (дальше используем ту же базу как «новую»).
	expectedEvents := map[string][]string{}

	for _, roomID := range []string{"!one:test.local", "!two:test.local"} {
		events, err := src.RoomEventsSince(ctx, roomID, 0, 1000)
		if err != nil {
			t.Fatalf("capture src events: %v", err)
		}

		for i := range events {
			expectedEvents[roomID] = append(expectedEvents[roomID], events[i].EventID)
		}
	}

	// «Новая» БД: та же база после откат/наката схемы.
	dst := freshDB(t)

	// users/rooms/memberships создаём из дампа (импортёр этих kind'ов — cmd/syngo-import).
	// membership пишем ПОСЛЕ событий: у room_memberships есть FK на events.event_id.
	lines := strings.Split(strings.TrimSpace(dump.String()), "\n")

	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("dump line: %v", err)
		}

		var err error

		switch rec["kind"] {
		case "user":
			_, err = dst.Pool().Exec(ctx, "INSERT INTO users (user_id, localpart, password_hash) VALUES ($1,$2,$3)",
				rec["user_id"], rec["localpart"], rec["password_hash"])
		case "room":
			_, err = dst.Pool().Exec(ctx, "INSERT INTO rooms (room_id, creator) VALUES ($1,$2)",
				rec["room_id"], rec["creator"])
		}

		if err != nil {
			t.Fatalf("insert from dump: %v", err)
		}
	}

	// События — нашим импортёром (тот же формат).
	importer := importevents.New(storeWriter{db: dst}, importevents.Options{BatchSize: 10, ProgressEvery: -1})

	importReport, err := importer.Import(ctx, strings.NewReader(dump.String()))
	if err != nil {
		t.Fatalf("import events: %v", err)
	}

	if importReport.Inserted != 53 || importReport.Bad != 0 {
		t.Fatalf("импорт событий: %+v", importReport)
	}

	// Членство — теперь, когда события уже есть.
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("dump line: %v", err)
		}

		if rec["kind"] != "membership" {
			continue
		}

		if _, err := dst.Pool().Exec(ctx, `
			INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
			SELECT event_id, room_id, $1, '@alice:test.local', $2
			FROM events WHERE room_id = $3 AND type = 'm.room.member' AND state_key = $1
			LIMIT 1`, rec["user_id"], rec["membership"], rec["room_id"]); err != nil {
			t.Fatalf("insert membership from dump: %v", err)
		}
	}

	// Сверяем события и порядок с сохранённым снимком.
	dstEvents, err := dst.RoomEventsSince(ctx, "!one:test.local", 0, 100)
	if err != nil {
		t.Fatalf("dst events: %v", err)
	}

	want := expectedEvents["!one:test.local"]
	if len(want) != len(dstEvents) {
		t.Fatalf("событий в комнате: dump=%d dst=%d", len(want), len(dstEvents))
	}

	for i := range want {
		if want[i] != dstEvents[i].EventID {
			t.Fatalf("event_id на позиции %d: %s != %s", i, want[i], dstEvents[i].EventID)
		}

		if dstEvents[i].StreamOrdering <= 0 {
			t.Fatalf("stream_ordering не восстановлен на позиции %d", i)
		}
	}

	// Медиа: файлы скопированы первым экспортом в раскладку Synapse.
	for _, id := range []string{"media1", "media2"} {
		path := filepath.Join(mediaOut, "local_content", id[0:2], id[2:4], id[4:])

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("файл медиа %s: %v", id, err)
		}

		if len(data) == 0 {
			t.Fatalf("файл медиа %s пуст", id)
		}
	}
}

// storeWriter адаптирует store.DB к importevents.BatchWriter.
type storeWriter struct{ db *store.DB }

func (w storeWriter) InsertEvents(ctx context.Context, events []importevents.Event) (int64, error) {
	rows := make([]store.EventRow, len(events))

	for i := range events {
		ev := events[i]
		rows[i] = store.EventRow{
			EventID: ev.EventID, RoomID: ev.RoomID, Type: ev.Type, Sender: ev.Sender,
			StateKey: ev.StateKey, Content: ev.Content, PrevEvents: ev.PrevEvents,
			AuthEvents: ev.AuthEvents, Depth: ev.Depth, OriginServerTS: ev.OriginServerTS,
			Origin: ev.Origin, StreamOrdering: ev.StreamOrdering,
		}
	}

	return w.db.InsertEvents(ctx, rows)
}

func (w storeWriter) ExistingRooms(ctx context.Context) (map[string]struct{}, error) {
	rows, err := w.db.Pool().Query(ctx, "SELECT room_id FROM rooms")
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	out := map[string]struct{}{}

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}

		out[id] = struct{}{}
	}

	return out, rows.Err()
}

func (w storeWriter) SyncStreamOrdering(ctx context.Context) error {
	return w.db.SyncStreamOrdering(ctx)
}

// TestExportAfterStream - выгрузка для отката после cutover - только
// события, созданные после переключения (stream_ordering больше импортированных),
// с манифестом по ним, без строк состояния; медиа - созданные после времени cutover.
func TestExportAfterStream(t *testing.T) {
	db := freshDB(t)
	mediaDir := t.TempDir()

	seedProfile(t, db, mediaDir)

	ctx := context.Background()

	cutover := time.Now().UTC()

	if _, err := db.Pool().Exec(ctx, `UPDATE media SET created_at = $1 WHERE media_id = 'media2'`, cutover.Add(time.Minute)); err != nil {
		t.Fatalf("media: %v", err)
	}

	if _, err := db.Pool().Exec(ctx, `UPDATE media SET created_at = $1 WHERE media_id = 'media1'`, cutover.Add(-time.Hour)); err != nil {
		t.Fatalf("media: %v", err)
	}

	var out bytes.Buffer

	report, err := exportdata.New(db.Pool(), exportdata.Options{
		BatchSize: 2, AfterStream: 50, HasAfterStream: true, MediaSince: cutover,
	}).Run(ctx, &out)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	// После позиции 50 - три membership-события профиля.
	if report.Events != 3 || report.StateRows != 0 || report.Media != 1 {
		t.Fatalf("отчет: %s", report.Text())
	}

	var manifest struct {
		Events    int64 `json:"events"`
		MinStream int64 `json:"min_stream"`
		MaxStream int64 `json:"max_stream"`
	}

	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(line, `"kind":"state"`) {
			t.Fatalf("в выгрузке после cutover строка состояния: %s", line)
		}

		if strings.Contains(line, `"kind":"manifest"`) {
			if err := json.Unmarshal([]byte(line), &manifest); err != nil {
				t.Fatalf("манифест: %v", err)
			}
		}
	}

	if manifest.Events != 3 || manifest.MinStream != 51 || manifest.MaxStream != 53 {
		t.Fatalf("манифест: %+v", manifest)
	}

	if !strings.Contains(out.String(), `"media_id":"media2"`) || strings.Contains(out.String(), `"media_id":"media1"`) {
		t.Fatalf("медиа после cutover: %s", out.String())
	}
}
