// Command synapse-fixture генерирует синтетический дамп Synapse для приёмки миграции.
//
//	# профиль приёмки: 100 пользователей, 200 комнат, 100k событий, 500 медиа
//	synapse-fixture -users 100 -rooms 200 -events 100000 -media 500 \
//	    -out /tmp/fixture.jsonl -media-dir /tmp/fixture-media -server-name matrix.example.org
//
// Формат строк — тот же JSONL, что у tools/synapse-export/export.sql (kind: user|room|event|media),
// медиа-файлы раскладываются по Synapse-раскладке local_content/<2>/<2>/<rest>.
//
// Данные детерминированы: одинаковые флаги дают одинаковый дамп (кроме паролей — они хешируются
// bcrypt со случайной солью, как в Synapse).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if err := run(); err != nil {
		log.Fatal("synapse-fixture: ", err)
	}
}

func run() error {
	var (
		users      = flag.Int("users", 100, "число пользователей")
		rooms      = flag.Int("rooms", 200, "число комнат")
		events     = flag.Int("events", 100_000, "число событий")
		mediaCount = flag.Int("media", 500, "число медиа")
		outPath    = flag.String("out", "-", "куда писать JSONL ('-' — stdout)")
		mediaDir   = flag.String("media-dir", "", "каталог media_store для файлов (обязателен при -media > 0)")
		serverName = flag.String("server-name", "example.org", "имя сервера в дампе")
		password   = flag.String("password", "fixture-pass", "пароль всех пользователей (bcrypt, как в Synapse)")
	)

	flag.Parse()

	if *users < 1 || *rooms < 1 {
		return fmt.Errorf("users и rooms должны быть >= 1")
	}

	if *mediaCount > 0 && *mediaDir == "" {
		return fmt.Errorf("для -media > 0 нужен -media-dir")
	}

	writer, closeFn, err := openOutput(*outPath)
	if err != nil {
		return err
	}

	defer closeFn()

	out := bufio.NewWriterSize(writer, 1<<20)

	defer func() { _ = out.Flush() }()

	enc := json.NewEncoder(out)

	// Пароль хешируем один раз: bcrypt с одним хешем на всех — как в реальном дампе (у каждого свой,
	// но для приёмки важна проверка формата, а не уникальность).
	hash, err := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.MinCost)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}

	userIDs := make([]string, *users)

	for i := range *users {
		userIDs[i] = fmt.Sprintf("@user%03d:%s", i, *serverName)

		if err := enc.Encode(map[string]any{
			"kind": "user", "user_id": userIDs[i], "localpart": fmt.Sprintf("user%03d", i),
			"password_hash": string(hash), "display_name": fmt.Sprintf("User %03d", i), "deactivated": false,
		}); err != nil {
			return fmt.Errorf("encode user: %w", err)
		}
	}

	roomIDs := make([]string, *rooms)

	for i := range *rooms {
		roomIDs[i] = fmt.Sprintf("!room%03d:%s", i, *serverName)

		if err := enc.Encode(map[string]any{
			"kind": "room", "room_id": roomIDs[i], "room_version": "10",
			"creator": userIDs[0], "is_public": i%10 == 0,
		}); err != nil {
			return fmt.Errorf("encode room: %w", err)
		}
	}

	// События: детерминированно распределены по комнатам, stream_ordering — сквозной.
	rnd := rand.New(rand.NewSource(42))

	for i := 1; i <= *events; i++ {
		roomID := roomIDs[i%*rooms]
		sender := userIDs[rnd.Intn(*users)]

		rec := map[string]any{
			"kind": "event", "room_id": roomID,
			"event_id":         fmt.Sprintf("$event%07d:%s", i, *serverName),
			"type":             "m.room.message",
			"sender":           sender,
			"content":          map[string]any{"body": fmt.Sprintf("message %d", i), "msgtype": "m.text"},
			"origin_server_ts": 1_700_000_000_000 + int64(i),
			"depth":            i,
			"prev_events":      []string{fmt.Sprintf("$event%07d:%s", i-1, *serverName)},
			"auth_events":      []string{},
			"stream_ordering":  i,
			"outlier":          false,
		}

		if i%50 == 0 {
			// разбавляем state-событиями, чтобы проверить room_state/current state
			rec["type"] = "m.room.topic"
			rec["state_key"] = ""
			rec["content"] = map[string]any{"topic": fmt.Sprintf("topic %d", i)}
		}

		if err := enc.Encode(rec); err != nil {
			return fmt.Errorf("encode event %d: %w", i, err)
		}
	}

	// Членство: для каждой комнаты — отдельные m.room.member события (у каждого свой event_id,
	// как в Synapse) и строки membership, ссылающиеся на них. События идут после истории,
	// поэтому stream_ordering продолжает её.
	stream := int64(*events)

	for i := range *rooms {
		stream++

		memberEventID := fmt.Sprintf("$member%07d:%s", stream, *serverName)

		if err := enc.Encode(map[string]any{
			"kind": "event", "room_id": roomIDs[i], "event_id": memberEventID,
			"type": "m.room.member", "sender": userIDs[0], "state_key": userIDs[0],
			"content":          map[string]any{"membership": "join"},
			"origin_server_ts": 1_700_000_000_000 + stream, "depth": stream,
			"prev_events": []string{}, "auth_events": []string{},
			"stream_ordering": stream, "outlier": false,
		}); err != nil {
			return fmt.Errorf("encode member event: %w", err)
		}

		if err := enc.Encode(map[string]any{
			"kind": "membership", "room_id": roomIDs[i], "user_id": userIDs[0],
			"membership": "join", "forgotten": false, "event_id": memberEventID,
		}); err != nil {
			return fmt.Errorf("encode membership: %w", err)
		}

		if i%10 == 0 && *users > 1 {
			stream++

			memberEventID2 := fmt.Sprintf("$member%07d:%s", stream, *serverName)

			if err := enc.Encode(map[string]any{
				"kind": "event", "room_id": roomIDs[i], "event_id": memberEventID2,
				"type": "m.room.member", "sender": userIDs[1], "state_key": userIDs[1],
				"content":          map[string]any{"membership": "join"},
				"origin_server_ts": 1_700_000_000_000 + stream, "depth": stream,
				"prev_events": []string{}, "auth_events": []string{},
				"stream_ordering": stream, "outlier": false,
			}); err != nil {
				return fmt.Errorf("encode member event 2: %w", err)
			}

			if err := enc.Encode(map[string]any{
				"kind": "membership", "room_id": roomIDs[i], "user_id": userIDs[1],
				"membership": "join", "forgotten": false, "event_id": memberEventID2,
			}); err != nil {
				return fmt.Errorf("encode membership 2: %w", err)
			}
		}
	}

	// Медиа: метаданные + файлы.
	for i := range *mediaCount {
		mediaID := fmt.Sprintf("media%04d", i)
		payload := []byte(fmt.Sprintf("media payload %d — синтетика для приёмки миграции", i))

		if err := writeMediaFile(*mediaDir, mediaID, payload); err != nil {
			return err
		}

		if err := enc.Encode(map[string]any{
			"kind": "media", "media_id": mediaID, "media_type": "text/plain",
			"media_length": len(payload), "upload_name": fmt.Sprintf("file%03d.txt", i),
			"user_id": userIDs[i%*users], "created_at_ms": 1_700_000_000_000 + int64(i),
		}); err != nil {
			return fmt.Errorf("encode media: %w", err)
		}
	}

	if err := out.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}

	if *outPath == "-" {
		fmt.Fprintf(os.Stderr, "fixture: users=%d rooms=%d events=%d media=%d\n", *users, *rooms, *events, *mediaCount)
	}

	return nil
}

// writeMediaFile кладёт файл в Synapse-раскладку local_content/<2>/<2>/<rest>.
func writeMediaFile(root, mediaID string, payload []byte) error {
	key := filepath.ToSlash(filepath.Join("local_content", mediaID[0:2], mediaID[2:4], mediaID[4:]))
	path := filepath.Join(root, filepath.FromSlash(key))

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir media: %w", err)
	}

	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("write media: %w", err)
	}

	return nil
}

// openOutput открывает приёмник дампа.
func openOutput(path string) (*os.File, func(), error) {
	if path == "-" {
		return os.Stdout, func() {}, nil
	}

	f, err := os.Create(strings.TrimSpace(path)) //nolint:gosec // путь задаёт оператор приёмки
	if err != nil {
		return nil, nil, fmt.Errorf("создать дамп: %w", err)
	}

	return f, func() { _ = f.Close() }, nil
}
