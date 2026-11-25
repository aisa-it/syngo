//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
)

func TestImportSynapseDeviceInbox(t *testing.T) {
	const user, device = "@alice:c4.test", "PHONE"

	ctx := context.Background()
	dsn := testdb.DSN(t)
	migrateSchema(t, dsn)
	db, err := store.Open(ctx, dsn, 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)

	src := loadSynapseFixture(t, db, dsn)
	_, err = src.Exec(ctx, `INSERT INTO device_inbox (user_id, device_id, stream_id, message_json)
		SELECT $1, $2, stream_id, jsonb_build_object('sender', $1::text, 'type', 'example.ping',
		    'content', jsonb_build_object('n', n))::text FROM (VALUES (32, 1), (30, 1), (31, 2)) AS events(stream_id, n)`, user, device)
	if err != nil {
		t.Fatal(err)
	}

	lines := byKind(runExportSQL(t, src), "to_device")
	if len(lines) != 3 {
		t.Fatalf("строк очереди в источнике: %d", len(lines))
	}
	dump := writeDump(t, lines...)
	importDump := func(path string) error {
		im := newImporter(db, "c4.test", 10)
		if err := im.importFile(ctx, path, 0); err != nil {
			return err
		}
		if len(im.errors) != 0 {
			return fmt.Errorf("ошибки импорта: %+v", im.errors)
		}

		return nil
	}
	check := func(want ...int) []int64 {
		t.Helper()
		pending, err := db.PendingToDevice(ctx, user, device, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		var values []int
		var ids []int64
		for _, event := range pending {
			var data struct{ N int }
			if err := json.Unmarshal(event.Content, &data); err != nil {
				t.Fatal(err)
			}
			values = append(values, data.N)
			ids = append(ids, event.ID)
		}
		if !slices.Equal(values, want) {
			t.Fatalf("очередь назначения %v, ожидали %v", values, want)
		}

		return ids
	}

	// В старом дампе идентификаторов не было. Предыдущий импортер уже записал
	// первую доставку без метки происхождения, затем работу прервали.
	legacy := make([]string, 0, len(lines))
	for _, line := range lines {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, "stream_id")
		b, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		legacy = append(legacy, string(b))
	}
	_, err = db.Pool().Exec(ctx, `INSERT INTO device_inbox (user_id, device_id, event_type, sender, content)
		VALUES ($1, $2, 'example.ping', $1, '{"n":1}')`, user, device)
	if err != nil {
		t.Fatal(err)
	}
	oldDump := writeDump(t, legacy...)
	for range 2 {
		if err := importDump(oldDump); err != nil {
			t.Fatal(err)
		}
		check(1, 2, 1)
	}

	// Новый экспорт связывает исходные ID с тремя прежними доставками. Даже
	// конкурентные повторы, а затем повтор после подтверждения, сохраняют очередь.
	var imports errgroup.Group
	for range 2 {
		imports.Go(func() error { return importDump(dump) })
	}
	if err := imports.Wait(); err != nil {
		t.Fatal(err)
	}
	ids := check(1, 2, 1)
	t.Logf("из Synapse экспортировано %d сообщений, в очереди SYNGO %d", len(lines), len(ids))
	if err := db.DeleteToDevice(ctx, ids); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dump, oldDump} {
		if err := importDump(path); err != nil {
			t.Fatal(err)
		}
		check()
	}

	// Два новых исходных ID с тем же содержимым остаются двумя доставками,
	// даже если в целевой очереди уже есть независимое локальное сообщение.
	_, err = src.Exec(ctx, `INSERT INTO device_inbox (user_id, device_id, stream_id, message_json)
		SELECT user_id, device_id, id, message_json FROM device_inbox CROSS JOIN (VALUES (41), (42)) AS new(id)
		WHERE stream_id = 30`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Pool().Exec(ctx, `INSERT INTO device_inbox (user_id, device_id, event_type, sender, content)
		VALUES ($1, $2, 'example.ping', $1, '{"n":1}')`, user, device)
	if err != nil {
		t.Fatal(err)
	}
	newDump := writeDump(t, byKind(runExportSQL(t, src), "to_device")...)
	for range 2 {
		if err := importDump(newDump); err != nil {
			t.Fatal(err)
		}
		check(1, 1, 1)
	}
}
