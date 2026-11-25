package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDump создаёт временный JSONL-файл с переданными строками.
func writeDump(t *testing.T, lines ...string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "dump.jsonl")

	body := ""

	for _, line := range lines {
		body += line + "\n"
	}

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}

	return path
}

// readDump собирает записи scanDump в срез (для проверок).
func readDump(path string, limit int) ([]dumpRecord, []importError, error) {
	var records []dumpRecord

	_, lineErrors, err := scanDump(path, limit, func(record *dumpRecord, _ []byte) error {
		records = append(records, *record)

		return nil
	})

	return records, lineErrors, err
}

func TestReadDump(t *testing.T) {
	t.Parallel()

	path := writeDump(t,
		`{"kind":"user","user_id":"@alice:test.local","password_hash":"$2b$12$x"}`,
		"# комментарий",
		"",
		`{"kind":"room","room_id":"!r:test.local","room_version":"10"}`,
		`{"kind":"event","event_id":"$e1","room_id":"!r:test.local","type":"m.room.message","sender":"@alice:test.local","content":{"body":"привет"},"origin_server_ts":1700000000000,"depth":1,"stream_ordering":7}`,
	)

	records, lineErrors, err := readDump(path, 0)
	if err != nil || len(lineErrors) != 0 {
		t.Fatalf("readDump() error: %v %v", err, lineErrors)
	}

	if len(records) != 3 {
		t.Fatalf("записей %d, ожидали 3", len(records))
	}

	if records[0].Kind != "user" || records[0].Localpart != "" {
		t.Fatalf("user: %+v", records[0])
	}

	if records[2].Stream == nil || *records[2].Stream != 7 {
		t.Fatalf("stream_ordering не разобран: %+v", records[2].Stream)
	}

	if string(records[2].Content) != `{"body":"привет"}` {
		t.Fatalf("content = %s", records[2].Content)
	}
}

func TestReadDumpLimit(t *testing.T) {
	t.Parallel()

	path := writeDump(t,
		`{"kind":"user","user_id":"@a:test.local"}`,
		`{"kind":"user","user_id":"@b:test.local"}`,
		`{"kind":"user","user_id":"@c:test.local"}`,
	)

	records, _, err := readDump(path, 2)
	if err != nil {
		t.Fatalf("readDump() error: %v", err)
	}

	if len(records) != 2 {
		t.Fatalf("записей %d, ожидали 2 (лимит)", len(records))
	}
}

// TestReadDumpErrors - битая строка и строка без kind попадают в
// ошибки строк, а чтение идет дальше (раньше первая битая строка прерывала
// импорт до начала записи, хотя README обещает продолжение).
func TestReadDumpErrors(t *testing.T) {
	t.Parallel()

	path := writeDump(t,
		`{"kind":"user","user_id":"@a:test.local"}`,
		`{"kind":"user",`,
		`{"user_id":"@b:test.local"}`,
		`{"kind":"user","user_id":"@c:test.local"}`,
	)

	records, lineErrors, err := readDump(path, 0)
	if err != nil {
		t.Fatalf("битая строка не должна прерывать чтение: %v", err)
	}

	if len(records) != 2 || records[1].UserID != "@c:test.local" {
		t.Fatalf("записи после битой строки потеряны: %+v", records)
	}

	if len(lineErrors) != 2 || lineErrors[0].Line != 2 || lineErrors[1].Line != 3 {
		t.Fatalf("ошибки строк: %+v", lineErrors)
	}
}

func TestBelongsToServer(t *testing.T) {
	t.Parallel()

	local := dumpRecord{UserID: "@alice:test.local", RoomID: "!r:test.local", Sender: "@alice:test.local"}
	if !belongsToServer(&local, "test.local") {
		t.Fatal("локальные сущности должны проходить фильтр")
	}

	foreign := dumpRecord{UserID: "@bob:other.local", RoomID: "!r:test.local"}
	if belongsToServer(&foreign, "test.local") {
		t.Fatal("чужие пользователи должны отбрасываться")
	}

	empty := dumpRecord{}
	if !belongsToServer(&empty, "test.local") {
		t.Fatal("запись без идентификаторов не должна отбрасываться фильтром")
	}

	// Комната v12: room_id без домена, своя по создателю.
	v12 := "!BGO0qy-BCFl9JY0_IBxh2iYSjWGT3yVx5xdDtzn5PLg"
	if !belongsToServer(&dumpRecord{RoomID: v12, Creator: "@alice:test.local"}, "test.local") {
		t.Fatal("комната v12 своего создателя должна проходить фильтр")
	}

	if belongsToServer(&dumpRecord{RoomID: v12, Creator: "@bob:other.local"}, "test.local") {
		t.Fatal("комната v12 чужого создателя должна отбрасываться")
	}
}

// TestRoomVersionsReport - dry-run считает комнаты по версиям, v12
// поддерживается, нестабильные и будущие версии перечисляются как неподдерживаемые.
func TestRoomVersionsReport(t *testing.T) {
	t.Parallel()

	var counts counters

	for _, version := range []string{"12", "10", "", "12", "org.matrix.hydra.11", "13", "1"} {
		countRecord(&counts, &dumpRecord{Kind: "room", RoomVersion: version, Creator: "@a:test.local"}, "test.local")
	}

	if got := counts.roomVersionsText(); got != "1=1 10=2 12=2 13=1 org.matrix.hydra.11=1" {
		t.Fatalf("версии: %s", got)
	}

	if got := strings.Join(counts.unsupportedRoomVersions(), ", "); got != `"13" (1), "org.matrix.hydra.11" (1)` {
		t.Fatalf("неподдерживаемые: %s", got)
	}

	if (&counters{}).roomVersionsText() != "-" {
		t.Fatal("пустой список версий")
	}
}

func TestCountRecord(t *testing.T) {
	t.Parallel()

	records := []dumpRecord{
		{Kind: "user", UserID: "@a:test.local"},
		{Kind: "room", RoomID: "!r:test.local"},
		{Kind: "event", RoomID: "!r:test.local"},
		{Kind: "membership", RoomID: "!r:test.local", UserID: "@a:test.local"},
		{Kind: "media", MediaID: "abcdef"},
		{Kind: "unknown"},
		{Kind: "user", UserID: "@b:other.local"},
	}

	var counts counters

	for i := range records {
		countRecord(&counts, &records[i], "test.local")
	}

	if counts.Users != 1 || counts.Rooms != 1 || counts.Events != 1 ||
		counts.Memberships != 1 || counts.Media != 1 {
		t.Fatalf("счётчики: %+v", counts)
	}

	if counts.Skipped != 2 {
		t.Fatalf("пропущено %d, ожидали 2 (unknown + чужой сервер)", counts.Skipped)
	}
}

// TestCountRecordMedia - dry-run не считает превью ссылок медиа к
// переносу, карантин передается в INSERT, отсутствие карантина - NULL.
func TestCountRecordMedia(t *testing.T) {
	t.Parallel()

	preview, admin, empty := "http://example.org/", "@admin:test.local", ""
	records := []dumpRecord{
		{Kind: "media", MediaID: "plainmedia"},
		{Kind: "media", MediaID: "urlpreview", URLCache: &preview},
		{Kind: "media", MediaID: "quarantine", Quarantined: &admin},
	}

	var counts counters

	for i := range records {
		countRecord(&counts, &records[i], "")
	}

	if counts.Media != 2 || counts.Skipped != 1 {
		t.Fatalf("счётчики: %+v", counts)
	}

	if quarantinedBy(&records[2]) != admin || quarantinedBy(&records[0]) != nil ||
		quarantinedBy(&dumpRecord{Quarantined: &empty}) != nil {
		t.Fatal("карантин для INSERT")
	}
}

// TestImportReportAndContinueOnError — ошибка на строке не останавливает
// импорт, отчёт содержит счётчики и ошибку с номером строки.
func TestImportReportAndContinueOnError(t *testing.T) {
	t.Parallel()

	path := writeDump(t,
		`{"kind":"user","user_id":"@a:test.local"}`,
		`{"kind":"room","room_id":"!r:test.local"}`,
		`{"kind":"membership","user_id":"@a:test.local"}`, // без room_id — ошибка строки
		`{"kind":"room_alias","alias":"#r:test.local","room_id":"!r:test.local"}`,
	)

	records, _, err := readDump(path, 0)
	if err != nil {
		t.Fatalf("readDump() error: %v", err)
	}

	if len(records) != 4 {
		t.Fatalf("записей %d", len(records))
	}

	// Номера строк сохранены — по ним отчёт укажет место ошибки.
	if records[0].line != 1 || records[3].line != 4 {
		t.Fatalf("line = %d/%d, ожидали 1/4", records[0].line, records[3].line)
	}

	report := importReport{Counts: counters{Users: 1, Rooms: 1, Aliases: 1}, Errors: []importError{
		{Line: 3, Kind: "membership", Error: "room_alias без alias/room_id"},
	}}

	reportPath := t.TempDir() + "/report.json"

	if err := writeReport(reportPath, report); err != nil {
		t.Fatalf("writeReport() error: %v", err)
	}

	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}

	for _, want := range []string{`"users": 1`, `"aliases": 1`, `"line": 3`, "room_alias"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("в отчёте нет %q: %s", want, data)
		}
	}
}
