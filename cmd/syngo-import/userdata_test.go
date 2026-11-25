package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// TestCountUserDataKinds - dry-run считает виды данных пользователей,
// user_stats не считается ни данными, ни пропуском.
func TestCountUserDataKinds(t *testing.T) {
	t.Parallel()

	var counts counters

	for _, kind := range []string{
		"device", "access_token", "device_keys", "one_time_key", "fallback_key", "cross_signing_key",
		"cross_signing_signature", "room_keys_version", "room_key", "account_data", "push_rule",
		"pusher", "filter", "to_device", "user_external_id", "receipt", "user_stats", "unknown",
	} {
		countRecord(&counts, &dumpRecord{Kind: kind, UserID: "@a:test.local"}, "test.local")
	}

	want := counters{
		Devices: 1, AccessTokens: 1, DeviceKeys: 1, OneTimeKeys: 1, FallbackKeys: 1, CrossSigning: 1,
		Signatures: 1, RoomKeyVersion: 1, RoomKeys: 1, AccountData: 1, PushRules: 1, Pushers: 1,
		Filters: 1, ToDevice: 1, ExternalIDs: 1, Receipts: 1, Skipped: 1,
	}

	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts = %+v, ожидали %+v", counts, want)
	}

	(&importer{counts: counters{LockedAsDeactivated: 1}}).printUserData()
}

// TestUserDataRejectsBrokenRecords - некорректные строки отклоняются до записи в БД
// (ошибка строки в отчете, импорт идет дальше).
func TestUserDataRejectsBrokenRecords(t *testing.T) {
	t.Parallel()

	im := &importer{}
	ctx := context.Background()
	prio := 0

	cases := map[string]dumpRecord{
		"device без id":            {Kind: "device", UserID: "@a:t"},
		"токен без устройства":     {Kind: "access_token", UserID: "@a:t", Token: "syt_x"},
		"кросс-ключ не JSON":       {Kind: "cross_signing_key", UserID: "@a:t", KeyJSON: json.RawMessage(`[`)},
		"кросс-ключ из двух":       {Kind: "cross_signing_key", UserID: "@a:t", KeyJSON: json.RawMessage(`{"keys":{"ed25519:a":"a","ed25519:b":"b"}}`)},
		"неизвестный вид правила":  {Kind: "push_rule", UserID: "@a:t", RuleKind: "postcontent", RuleName: "x"},
		"enabled без правила":      {Kind: "push_rule", UserID: "@a:t", RuleKind: "override", RuleName: "gone"},
		"content без pattern":      {Kind: "push_rule", UserID: "@a:t", RuleKind: "content", RuleName: "c", Priority: &prio, Conditions: json.RawMessage(`[]`)},
		"to-device без типа":       {Kind: "to_device", UserID: "@a:t", DeviceID: "D", Message: json.RawMessage(`{}`)},
		"to-device не JSON-объект": {Kind: "to_device", UserID: "@a:t", DeviceID: "D", Message: json.RawMessage(`1`)},
		"external id без subject":  {Kind: "user_external_id", UserID: "@a:t", AuthProvider: "oidc"},
		"receipt без события":      {Kind: "receipt", UserID: "@a:t", RoomID: "!r:t", ReceiptType: "m.read"},
	}

	for name, record := range cases {
		handled, err := im.importUserData(ctx, &record)
		if !handled || err == nil {
			t.Errorf("%s: handled=%v err=%v, ожидали ошибку строки", name, handled, err)
		}
	}

	if handled, _ := im.importUserData(ctx, &dumpRecord{Kind: "event"}); handled {
		t.Fatal("event не относится к данным пользователей")
	}

	if !reflect.DeepEqual(im.counts, counters{}) {
		t.Fatalf("отклоненные строки посчитаны: %+v", im.counts)
	}

	if err := im.finishUserData(ctx); err != nil {
		t.Fatalf("finishUserData без версий бэкапа: %v", err)
	}
}

func TestUserDataHelpers(t *testing.T) {
	t.Parallel()

	if rawJSON(nil) != "{}" || rawJSON(json.RawMessage("null")) != "{}" || rawJSON(json.RawMessage(`[1]`)) != "[1]" {
		t.Fatal("rawJSON")
	}

	zero := int64(0)
	ms := int64(1795196412635)

	if msTime(nil) != nil || msTime(&zero) != nil || !msTime(&ms).Equal(time.UnixMilli(ms)) {
		t.Fatal("msTime")
	}

	if nullable("") != nil || *nullable("x") != "x" {
		t.Fatal("nullable")
	}

	report := &verifyReport{Users: 1, Mismatches: []verifyMismatch{{UserID: "@a:t", What: "devices", Source: 2, Target: 1}}}
	report.print()
}
