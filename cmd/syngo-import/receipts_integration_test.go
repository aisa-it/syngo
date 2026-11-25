//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/api"
	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/synapseexport"
	"github.com/aisa-it/syngo/internal/testdb"
)

// Комнаты фикстуры Synapse (internal/importevents/testdata/synapse-1.165.sql).
const (
	// receiptGeneral - «Общая»: последнее сообщение bob (stream 25), после него
	// сообщение alice (27) - отметка bob стоит на нем.
	receiptGeneral = "!JqfZItlTHtfMcShmLM:c4.test"
	receiptGenLast = "$fb8a1oPwWfwIEHtrWHQn5juLGl6MtyEpne1C2u53j9Q"
	// receiptV1 - комната v1: сообщение alice (35), затем bob (36 удалено, 38) -
	// приватная отметка alice в основной ленте ("main") стоит на 38.
	receiptV1     = "!VtOpRajKObOItZqozt:c4.test"
	receiptV1Last = "$179518974410IoHFS:c4.test"
	// receiptThird - комната без отметок: сообщение bob (50) после alice остается
	// непрочитанным у alice (контроль, что разбор дошел до конца).
	receiptThird = "!gPyHzvtsYISNRQlqqU:c4.test"
	receiptAlice = "@alice:c4.test"
	receiptBob   = "@bob:c4.test"
)

// addReceiptsToSynapse кладет отметки в receipts_linearized фикстуры, как их пишет
// Synapse: m.read без треда, m.read.private в треде main, плюс то, что не
// переносится (отметка удаленного пользователя и тип не m.read*).
func addReceiptsToSynapse(t *testing.T, db *store.DB) {
	t.Helper()

	if _, err := db.Pool().Exec(context.Background(), `
		INSERT INTO synapse_fixture.receipts_linearized
			(stream_id, room_id, receipt_type, user_id, event_id, data, thread_id) VALUES
		(1, $1, 'm.read', '@bob:c4.test', $2, '{"ts":1795189750000}', NULL),
		(2, $3, 'm.read.private', '@alice:c4.test', $4, '{"ts":1795189750001}', 'main'),
		(3, $1, 'm.read', '@zed:remote.test', $2, '{"ts":1795189750002}', NULL),
		(4, $1, 'org.example.receipt', '@alice:c4.test', $2, '{"ts":1795189750003}', NULL)`,
		receiptGeneral, receiptGenLast, receiptV1, receiptV1Last); err != nil {
		t.Fatalf("receipts_linearized: %v", err)
	}
}

// receiptServer - syngod поверх базы (новый процесс: receipts грузятся из БД).
type receiptServer struct {
	client
	srv *api.Server
}

func startReceiptServer(t *testing.T, db *store.DB) receiptServer {
	t.Helper()

	cfg := config.Default()
	cfg.Matrix.ServerName = "c4.test"
	cfg.RateLimit = config.RateLimit{}
	cfg.Sync.TokenSecret = "integration-secret"
	cfg.Media.Directory = t.TempDir()

	srv := api.New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	return receiptServer{client: client{t: t, handler: srv.Handler()}, srv: srv}
}

func (s receiptServer) stop() {
	_ = s.srv.ClosePush(context.Background())
}

// receiptToken выдает пользователю фикстуры токен (в фикстуре событий сессий нет).
func receiptToken(t *testing.T, db *store.DB, userID, deviceID string) string {
	t.Helper()

	token := "tok-" + deviceID

	if _, err := db.Pool().Exec(context.Background(), `
		WITH d AS (INSERT INTO devices (user_id, device_id) VALUES ($1, $2) ON CONFLICT DO NOTHING)
		INSERT INTO access_tokens (token_hash, user_id, device_id) VALUES ($3, $1, $2) ON CONFLICT DO NOTHING`,
		userID, deviceID, core.HashToken(token)); err != nil {
		t.Fatalf("токен %s: %v", userID, err)
	}

	return token
}

// waitPushProcessed ждет, пока фон syngod разберет все события и досчитает
// пользователей users (если история досчитывается): до этого счетчики не готовы.
func waitPushProcessed(t *testing.T, db *store.DB, users ...string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)

	for {
		var done bool

		if err := db.Pool().QueryRow(context.Background(), `
			SELECT s.processed >= (SELECT coalesce(max(stream_ordering), 0) FROM events)
			   AND (s.backfill_upto = 0 OR
			        (SELECT count(*) FROM event_push_backfill WHERE user_id = ANY($1)) = cardinality($1))
			FROM event_push_stream s`, users).Scan(&done); err != nil {
			t.Fatalf("позиция разбора: %v", err)
		}

		if done {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("разбор push-действий не закончился")
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// unreadCounts - notification_count комнат из initial /sync.
func unreadCounts(c client, token string, rooms ...string) (counts map[string]int, sync map[string]any) {
	sync = c.ok(http.MethodGet, "/_matrix/client/v3/sync?timeout=0", token, "")
	counts = map[string]int{}

	for _, roomID := range rooms {
		n, _ := dig(dig(sync, "rooms", "join"), roomID, "unread_notifications", "notification_count").(float64)
		counts[roomID] = int(n)
	}

	return counts, sync
}

// checkUnread сверяет счетчики alice и bob после разбора.
func checkUnread(t *testing.T, c client, stage, aliceToken, bobToken string, alice, bob map[string]int) (aliceSync, bobSync map[string]any) {
	t.Helper()

	got, aliceSync := unreadCounts(c, aliceToken, receiptGeneral, receiptV1, receiptThird)
	if !maps.Equal(got, alice) {
		t.Fatalf("%s: непрочитанное alice %v, ожидалось %v", stage, got, alice)
	}

	got, bobSync = unreadCounts(c, bobToken, receiptGeneral, receiptV1, receiptThird)
	if !maps.Equal(got, bob) {
		t.Fatalf("%s: непрочитанное bob %v, ожидалось %v", stage, got, bob)
	}

	return aliceSync, bobSync
}

// receiptsIn - m.receipt комнаты из /sync строкой (для проверок вхождения).
func receiptsIn(sync map[string]any, roomID string) string {
	raw, _ := json.Marshal(dig(dig(sync, "rooms", "join"), roomID, "ephemeral"))

	return string(raw)
}

// TestImportSynapseReceipts - отметки прочтения m.read и m.read.private
// переносятся из Synapse, и непрочитанное после переезда считается от отметки, а
// не от последнего своего сообщения. Раньше receipts не переносились: bob видел
// непрочитанным сообщение alice, которое прочитал в Synapse, alice - сообщение bob.
// Проверяется три пути: дозагрузка отметок в уже работающую базу (сводка
// посчитана - пересчет затронутых ключей), полный импорт по runbook (отметки
// раньше событий, позиции проставляет импорт событий) с разбором событий по
// одному и он же с досчетом истории при старте (большой импорт).
func TestImportSynapseReceipts(t *testing.T) {
	dsn := testdb.DSN(t)

	migrateSchema(t, dsn)

	ctx := context.Background()

	db, err := store.Open(ctx, dsn, 8, 0)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	src := loadSynapseFixture(t, db, dsn)
	addReceiptsToSynapse(t, db)

	all := runExportSQL(t, src)

	receipts := byKind(all, "receipt")
	if len(receipts) != 2 || !strings.Contains(strings.Join(receipts, "\n"), `"thread_id":"main"`) ||
		strings.Contains(strings.Join(receipts, "\n"), "remote.test") {
		t.Fatalf("export.sql: отметки %v", receipts)
	}

	var events bytes.Buffer
	if _, err := synapseexport.ExportEvents(ctx, src, &events, synapseexport.Options{BatchSize: 7}); err != nil {
		t.Fatalf("выгрузка событий: %v", err)
	}

	eventLines := strings.Split(strings.TrimSpace(events.String()), "\n")
	importLines := func(stage string, lines []string, extra ...string) {
		t.Helper()

		args := make([]string, 0, 8+len(extra))
		args = append(args, "-dsn", dsn, "-file", writeDump(t, lines...), "-server-name", "c4.test",
			"-report", filepath.Join(t.TempDir(), "r.json"))
		if err := runCmd(t, append(args, extra...)...); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
	}

	// 1. Перенос без отметок (как раньше): ложные непрочитанные.
	importLines("импорт без отметок", append(append(byKind(all, "user", "room", "room_alias"), eventLines...),
		byKind(all, "membership")...))

	aliceToken := receiptToken(t, db, receiptAlice, "ALICE")
	bobToken := receiptToken(t, db, receiptBob, "BOB")

	server := startReceiptServer(t, db)
	waitPushProcessed(t, db, receiptAlice, receiptBob)
	checkUnread(t, server.client, "без отметок", aliceToken, bobToken,
		map[string]int{receiptGeneral: 0, receiptV1: 1, receiptThird: 1},
		map[string]int{receiptGeneral: 1, receiptV1: 0, receiptThird: 0})
	server.stop()

	// 2. Отметки дозагружены в работающую базу: сводка уже посчитана и
	// пересчитывается импортом; после рестарта /sync отдает отметки.
	importLines("дозагрузка отметок", receipts)

	server = startReceiptServer(t, db)
	waitPushProcessed(t, db, receiptAlice, receiptBob)
	aliceSync, bobSync := checkUnread(t, server.client, "после дозагрузки", aliceToken, bobToken,
		map[string]int{receiptGeneral: 0, receiptV1: 0, receiptThird: 1},
		map[string]int{receiptGeneral: 0, receiptV1: 0, receiptThird: 0})
	server.stop()

	if got := receiptsIn(bobSync, receiptGeneral); !strings.Contains(got, receiptGenLast) || !strings.Contains(got, receiptBob) {
		t.Fatalf("/sync bob: нет m.read в ephemeral: %s", got)
	}

	if got := receiptsIn(aliceSync, receiptV1); !strings.Contains(got, `"m.read.private"`) || !strings.Contains(got, receiptV1Last) {
		t.Fatalf("/sync alice: нет m.read.private в ephemeral: %s", got)
	}

	if got := receiptsIn(bobSync, receiptV1); strings.Contains(got, "m.read.private") {
		t.Fatalf("/sync bob: чужая приватная отметка видна: %s", got)
	}

	// 3. Чистая база, порядок runbook: данные пользователей (с отметками и сверкой)
	// раньше событий, членство после.
	resetSchema(t, dsn)

	importLines("пользователи и комнаты", byKind(all, "user", "room", "room_alias"))
	importLines("данные пользователей", byKind(all, "receipt", "user_stats"), "-verify")
	importLines("события", eventLines)
	importLines("членство", byKind(all, "membership"))

	var positioned, total int
	if err := db.Pool().QueryRow(ctx, `SELECT count(event_stream), count(*) FROM receipts`).Scan(&positioned, &total); err != nil ||
		total != 2 || positioned != 2 {
		t.Fatalf("receipts после импорта событий: с позицией %d из %d, %v", positioned, total, err)
	}

	aliceToken = receiptToken(t, db, receiptAlice, "ALICE")
	bobToken = receiptToken(t, db, receiptBob, "BOB")

	want := func(stage string) {
		t.Helper()

		server := startReceiptServer(t, db)
		defer server.stop()

		waitPushProcessed(t, db, receiptAlice, receiptBob)
		checkUnread(t, server.client, stage, aliceToken, bobToken,
			map[string]int{receiptGeneral: 0, receiptV1: 0, receiptThird: 1},
			map[string]int{receiptGeneral: 0, receiptV1: 0, receiptThird: 0})
	}

	// Разбор событий по одному (небольшой импорт): отметки из receipts.event_stream.
	want("импорт по runbook")

	// Большой импорт считается историей и досчитывается по пользователю при
	// старте: отметки берутся из памяти процесса, загруженной из БД. Отметок на
	// большом сервере много (здесь - посторонние комнаты), их загрузка идет
	// дольше начала досчета: досчет должен ждать ее, а не считать без отметок.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO receipts (room_id, user_id, receipt_type, thread_id, event_id, ts)
		SELECT '!noise' || (n / 50) || ':c4.test', '@u' || (n % 50) || ':c4.test', 'm.read', '', '$e' || n, n
		FROM generate_series(1, 200000) n;
		DELETE FROM event_push_actions;
		DELETE FROM event_push_summary;
		DELETE FROM event_push_backfill;
		UPDATE event_push_stream SET processed = (SELECT max(stream_ordering) FROM events),
		                             backfill_upto = (SELECT max(stream_ordering) FROM events)`); err != nil {
		t.Fatalf("сброс разбора: %v", err)
	}

	want("досчет истории при старте")
}
