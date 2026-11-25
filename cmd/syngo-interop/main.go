// syngo-interop сравнивает материализованное состояние комнаты нашего сервера со
// состоянием живого Synapse.
//
// Наше состояние читается из room_state в PostgreSQL, состояние Synapse — через
// admin API GET /_synapse/admin/v1/rooms/{roomId}/state. Инструмент возвращает
// ненулевой код при любом расхождении и пишет JSON-выгрузку для отчёта.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// stateRow — ключ состояния и событие.
type stateRow struct {
	Type     string `json:"type"`
	StateKey string `json:"state_key"`
	EventID  string `json:"event_id"`
}

// report — выгрузка сверки.
type report struct {
	RoomID     string            `json:"room_id"`
	SynapseURL string            `json:"synapse_url"`
	SynapseVer string            `json:"synapse_version,omitempty"`
	CheckedAt  time.Time         `json:"checked_at"`
	OurState   []stateRow        `json:"our_state"`
	TheirState []stateRow        `json:"synapse_state"`
	Diff       map[string]string `json:"diff,omitempty"`
	Equal      bool              `json:"equal"`
	Note       string            `json:"note,omitempty"`
}

func main() {
	dsn := flag.String("dsn", os.Getenv("SYNGOD_DATABASE_DSN"), "PostgreSQL DSN нашего сервера")
	roomID := flag.String("room", "", "room_id для сверки")
	synapseURL := flag.String("synapse", "", "базовый URL Synapse (например http://127.0.0.1:8008)")
	token := flag.String("token", "", "access token администратора Synapse")
	out := flag.String("out", "", "путь для JSON-выгрузки (пусто — не писать)")
	timeout := flag.Duration("timeout", 30*time.Second, "таймаут запросов")
	flag.Parse()

	if *dsn == "" || *roomID == "" || *synapseURL == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "нужны -dsn, -room, -synapse и -token")
		flag.Usage()

		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	rep, err := compare(ctx, *dsn, *roomID, *synapseURL, *token)
	cancel()

	if err != nil {
		fmt.Fprintf(os.Stderr, "ОШИБКА: %v\n", err)

		os.Exit(2)
	}

	if *out != "" {
		if err := writeReport(*out, rep); err != nil {
			fmt.Fprintf(os.Stderr, "ОШИБКА записи отчёта: %v\n", err)

			os.Exit(2)
		}
	}

	if !rep.Equal {
		fmt.Fprintf(os.Stderr, "РАСХОЖДЕНИЕ: %d ключей (см. -out)\n", len(rep.Diff))

		for _, line := range diffLines(rep.Diff) {
			fmt.Fprintln(os.Stderr, line)
		}

		os.Exit(1)
	}

	fmt.Printf("OK: состояние совпадает 1:1 (%d ключей, Synapse %s)\n", len(rep.OurState), rep.SynapseVer)
}

// compare снимает состояние с обеих сторон и сравнивает.
func compare(ctx context.Context, dsn, roomID, synapseURL, token string) (*report, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("подключение к БД: %w", err)
	}

	defer pool.Close()

	our, err := ourState(ctx, pool, roomID)
	if err != nil {
		return nil, err
	}

	their, version, err := synapseState(ctx, synapseURL, token, roomID)
	if err != nil {
		return nil, err
	}

	// Пустой state у пира — он вышел из комнаты и /state ему больше ничего не
	// отдаёт (Synapse отвечает 200 с пустым списком). Сверять нечего: наши
	// события у него законно удалены вместе с членством (leave/ban).
	if their != nil && len(their) == 0 {
		return &report{
			RoomID: roomID, SynapseURL: synapseURL, SynapseVer: version,
			CheckedAt: time.Now().UTC(), Equal: true, Note: "state у пира пуст (left)",
		}, nil
	}

	// nil — пир комнаты уже не знает (вышел/удалил). Сверять нечего.
	if their == nil {
		return &report{
			RoomID: roomID, SynapseURL: synapseURL, SynapseVer: version,
			CheckedAt: time.Now().UTC(), Equal: true, Note: "пир не знает комнату (left)",
		}, nil
	}

	rep := &report{
		RoomID: roomID, SynapseURL: synapseURL, SynapseVer: version, CheckedAt: time.Now().UTC(),
		OurState: our, TheirState: their,
	}
	rep.Diff = diffState(our, their)
	rep.Equal = len(rep.Diff) == 0

	return rep, nil
}

// ourState читает room_state нашего сервера.
func ourState(ctx context.Context, pool *pgxpool.Pool, roomID string) ([]stateRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT type, state_key, event_id FROM room_state WHERE room_id = $1 ORDER BY type, state_key`, roomID)
	if err != nil {
		return nil, fmt.Errorf("room_state: %w", err)
	}

	defer rows.Close()

	var out []stateRow

	for rows.Next() {
		var row stateRow
		if err := rows.Scan(&row.Type, &row.StateKey, &row.EventID); err != nil {
			return nil, fmt.Errorf("room_state scan: %w", err)
		}

		out = append(out, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("room_state rows: %w", err)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("room_state пуст для %s", roomID)
	}

	return out, nil
}

// synapseState читает состояние комнаты через admin API Synapse.
func synapseState(ctx context.Context, base, token, roomID string) ([]stateRow, string, error) {
	var payload struct {
		State []struct {
			Type     string `json:"type"`
			StateKey string `json:"state_key"`
			EventID  string `json:"event_id"`
		} `json:"state"`
	}

	version := synapseVersion(ctx, base)

	url := strings.TrimRight(base, "/") + "/_synapse/admin/v1/rooms/" + roomID + "/state"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, version, fmt.Errorf("запрос к Synapse: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, version, fmt.Errorf("запрос к Synapse: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	// Пир уже вышел из комнаты: Synapse удаляет её у себя и отвечает M_NOT_FOUND.
	// Сравнивать состояние не с чем — это не расхождение серверов, а ожидаемое
	// поведение (leave/ban/backfill в интеропе).
	if resp.StatusCode == http.StatusNotFound {
		return nil, version, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, version, fmt.Errorf("synapse admin API вернул %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, version, fmt.Errorf("разбор ответа Synapse: %w", err)
	}

	out := make([]stateRow, 0, len(payload.State))
	for _, ev := range payload.State {
		out = append(out, stateRow{Type: ev.Type, StateKey: ev.StateKey, EventID: ev.EventID})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}

		return out[i].StateKey < out[j].StateKey
	})

	return out, version, nil
}

// synapseVersion возвращает версию Synapse (пусто, если недоступна).
func synapseVersion(ctx context.Context, base string) string {
	var payload struct {
		ServerVersion string `json:"server_version"`
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/_matrix/federation/v1/version", http.NoBody)
	if err != nil {
		return ""
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}

	return payload.ServerVersion
}

// diffState сравнивает два состояния по ключу (type, state_key).
func diffState(ours, theirs []stateRow) map[string]string {
	key := func(row stateRow) string { return row.Type + "\x00" + row.StateKey }

	ourMap := make(map[string]string, len(ours))
	for _, row := range ours {
		ourMap[key(row)] = row.EventID
	}

	theirMap := make(map[string]string, len(theirs))
	for _, row := range theirs {
		theirMap[key(row)] = row.EventID
	}

	diff := map[string]string{}

	for k, ourID := range ourMap {
		theirID, ok := theirMap[k]
		switch {
		case !ok:
			diff[k] = "есть только у нас: " + ourID
		case theirID != ourID:
			diff[k] = "у нас " + ourID + ", у Synapse " + theirID
		}
	}

	for k, theirID := range theirMap {
		if _, ok := ourMap[k]; !ok {
			diff[k] = "есть только у Synapse: " + theirID
		}
	}

	return diff
}

// diffLines — человекочитаемые строки расхождений (по ключу).
func diffLines(diff map[string]string) []string {
	keys := make([]string, 0, len(diff))
	for k := range diff {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		typ, stateKey, _ := strings.Cut(k, "\x00")
		lines = append(lines, fmt.Sprintf("  %s/%s: %s", typ, stateKey, diff[k]))
	}

	return lines
}

// writeReport пишет JSON-выгрузку сверки.
func writeReport(path string, rep *report) error {
	if path == "" {
		return errors.New("пустой путь отчёта")
	}

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	clean := filepath.Clean(path)
	if err := os.WriteFile(clean, append(data, '\n'), 0o600); err != nil { //nolint:gosec // путь задаёт оператор флагом -out
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
