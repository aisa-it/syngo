package importevents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/aisa-it/syngo/internal/core"
)

// checkEventID сверяет event_id события с reference hash его исходного PDU:
// так видно, что PDU перенесен без искажений и наш расчет
// event_id совпадает с Synapse. Возвращает описание расхождения или "".
//
// Комнаты v1/v2 пропускаются - там event_id не хеш, а выдан сервером; v3
// кодирует хеш обычным base64, v4+ - URL-safe.
func checkEventID(rec Record, roomVersion string) string {
	if rec.JSON == "" {
		return ""
	}

	version, err := strconv.Atoi(roomVersion)
	if err != nil || version < 3 {
		return ""
	}

	dec := json.NewDecoder(bytes.NewReader([]byte(rec.JSON)))
	dec.UseNumber()

	var object map[string]any
	if err := dec.Decode(&object); err != nil {
		return fmt.Sprintf("%s: PDU не разбирается для проверки event_id: %v", rec.EventID, err)
	}

	got, err := core.EventID(object, version)
	if err != nil {
		return fmt.Sprintf("%s: reference hash: %v", rec.EventID, err)
	}

	if version == 3 {
		got = strings.NewReplacer("-", "+", "_", "/").Replace(got)
	}

	if got != rec.EventID {
		return fmt.Sprintf("%s: event_id не совпадает с reference hash PDU (%s)", rec.EventID, got)
	}

	return ""
}
