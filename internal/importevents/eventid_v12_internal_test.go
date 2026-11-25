package importevents

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// v12Fixture - живой Synapse 1.165 с комнатами v12 (gen.sh, SCENARIO=v12).
const v12Fixture = "../../cmd/syngo-import/testdata/synapse-1.165-v12.sql"

// TestCheckEventIDSynapseV12 - reference hash всех событий комнат v12
// живого Synapse (create без room_id, auth_events без create, redaction, power_levels
// без создателей) совпадает с их event_id по нашим правилам redaction v12, а create
// комнаты дает ее room_id.
func TestCheckEventIDSynapseV12(t *testing.T) {
	t.Parallel()

	versions := map[string]string{}
	checked := 0

	readCopy(t, v12Fixture, "COPY synapse.rooms ", func(cols []string) {
		versions[cols[0]] = cols[3]
	})

	readCopy(t, v12Fixture, "COPY synapse.event_json ", func(cols []string) {
		eventID, roomID, pdu := cols[0], cols[1], cols[3]
		if versions[roomID] != "12" {
			return
		}

		if msg := checkEventID(Record{EventID: eventID, JSON: pdu}, "12"); msg != "" {
			t.Errorf("%s", msg)
		}

		var head struct {
			Type   string  `json:"type"`
			RoomID *string `json:"room_id"`
		}

		if err := json.Unmarshal([]byte(pdu), &head); err != nil {
			t.Fatalf("%s: %v", eventID, err)
		}

		if head.Type == "m.room.create" && (head.RoomID != nil || "!"+strings.TrimPrefix(eventID, "$") != roomID) {
			t.Errorf("create %s комнаты %s: room_id в PDU %v или room_id не хеш create", eventID, roomID, head.RoomID)
		}

		checked++
	})

	if checked < 30 {
		t.Fatalf("событий v12 в фикстуре %d, ожидали не меньше 30", checked)
	}
}

// readCopy читает строки блока COPY ... FROM stdin дампа pg_dump.
func readCopy(t *testing.T, path, prefix string, row func(cols []string)) {
	t.Helper()

	file, err := os.Open(path) //nolint:gosec // файл фикстуры
	if err != nil {
		t.Fatal(err)
	}

	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)

	inside := false

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case strings.HasPrefix(line, prefix):
			inside = true
		case inside && line == `\.`:
			return
		case inside:
			cols := strings.Split(line, "\t")
			for i := range cols {
				cols[i] = unescapeCopy(cols[i])
			}

			row(cols)
		}
	}
}

// unescapeCopy снимает экранирование текстового формата COPY.
func unescapeCopy(value string) string {
	var b strings.Builder

	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 == len(value) {
			b.WriteByte(value[i])

			continue
		}

		i++

		switch value[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte(value[i])
		}
	}

	return b.String()
}
