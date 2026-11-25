package store

import (
	"context"
	"encoding/json"
	"testing"
)

// TestWithDisplayName - имя удаленного устройства уходит в
// unsigned.device_display_name, остальной объект ключей не меняется.
func TestWithDisplayName(t *testing.T) {
	t.Parallel()

	keys := []byte(`{"device_id":"D","unsigned":{"x":1}}`)

	var got map[string]any
	if err := json.Unmarshal(withDisplayName(keys, "Phone"), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	unsigned, _ := got["unsigned"].(map[string]any)
	if unsigned["device_display_name"] != "Phone" || unsigned["x"] != float64(1) || got["device_id"] != "D" {
		t.Fatalf("ключи: %v", got)
	}

	if string(withDisplayName(keys, "")) != string(keys) {
		t.Fatalf("без имени объект изменен")
	}

	if string(withDisplayName([]byte(`not json`), "Phone")) != "not json" {
		t.Fatalf("битый JSON изменен")
	}

	if err := json.Unmarshal(withDisplayName([]byte(`{"device_id":"D"}`), "P"), &got); err != nil {
		t.Fatalf("без unsigned: %v", err)
	}
}

// TestFederationE2EEEmptyInputs - пустые запросы не ходят в БД.
func TestFederationE2EEEmptyInputs(t *testing.T) {
	t.Parallel()

	db := &DB{}
	ctx := context.Background()

	if states, err := db.RemoteDeviceListStates(ctx, nil); err != nil || len(states) != 0 {
		t.Fatalf("states: %v %v", states, err)
	}

	if keys, err := db.RemoteDeviceKeys(ctx, nil); err != nil || len(keys) != 0 {
		t.Fatalf("keys: %v %v", keys, err)
	}

	if users, err := db.RemoteUsersSharingRoom(ctx, nil, "ours"); err != nil || users != nil {
		t.Fatalf("users: %v %v", users, err)
	}

	db.SetOutboxDestinationFilter(func(string) bool { return false })

	if err := db.EnqueueEDU(ctx, []string{"a.test", ""}, []byte(`{}`)); err != nil {
		t.Fatalf("enqueue без разрешенных получателей: %v", err)
	}

	var nilDB *DB
	if _, err := nilDB.SendToDeviceFederated(ctx, "@a:x", "D", "t", "txn", nil, nil); err == nil {
		t.Fatalf("без БД ожидали ошибку")
	}
}
