package core_test

import (
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

// TestTypingExpires — «печатает» истекает через TypingTimeout.
func TestTypingExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	store := core.NewEphemeralStore().WithClock(func() time.Time { return now })

	store.SetTyping("!r:test", "@a:test", true)

	if users := store.Typing("!r:test"); len(users) != 1 {
		t.Fatalf("typing = %v, ожидали одного", users)
	}

	now = now.Add(core.TypingTimeout + time.Second)

	if users := store.Typing("!r:test"); len(users) != 0 {
		t.Fatalf("typing после таймаута = %v, ожидали пусто", users)
	}

	// Явное выключение убирает сразу.
	store.SetTyping("!r:test", "@a:test", true)
	store.SetTyping("!r:test", "@a:test", false)

	if users := store.Typing("!r:test"); len(users) != 0 {
		t.Fatalf("typing после false = %v", users)
	}
}

// TestPresenceExpires — статус истекает в offline.
func TestPresenceExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	store := core.NewEphemeralStore().WithClock(func() time.Time { return now })

	store.SetPresence("@a:test", "online", "тут")

	presence := store.Presence("@a:test")
	if presence.Presence != "online" || presence.StatusMsg != "тут" {
		t.Fatalf("presence = %+v", presence)
	}

	now = now.Add(core.PresenceTimeout + time.Second)

	if presence := store.Presence("@a:test"); presence.Presence != "offline" {
		t.Fatalf("presence после таймаута = %+v", presence)
	}
}

// TestReceiptsLast wins — последний receipt пользователя перекрывает предыдущий.
func TestReceiptsLatest(t *testing.T) {
	t.Parallel()

	store := core.NewEphemeralStore()

	store.SetReceipt("!r:test", "@a:test", "m.read", "", "$e1")
	store.SetReceipt("!r:test", "@a:test", "m.read", "", "$e2")

	receipts := store.Receipts("!r:test", "@b:test")
	if len(receipts) != 1 {
		t.Fatalf("receipts = %+v, ожидали один на пользователя", receipts)
	}

	if receipts[0].EventID != "$e2" {
		t.Fatalf("receipt event = %q, ожидали $e2", receipts[0].EventID)
	}
}

// TestReceiptsByTypeAndPrivacy — receipts хранятся по типу и треду, m.read.private
// виден только владельцу и не затирает публичный.
func TestReceiptsByTypeAndPrivacy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	store := core.NewEphemeralStore().WithClock(func() time.Time { return now })

	store.SetReceipt("!r:test", "@a:test", core.ReceiptRead, "", "$pub")
	store.SetReceipt("!r:test", "@a:test", core.ReceiptReadPrivate, "", "$priv")
	store.SetReceipt("!r:test", "@a:test", core.ReceiptRead, "$thread", "$inthread")

	types := func(viewer string) map[string]string {
		out := map[string]string{}
		for _, r := range store.Receipts("!r:test", viewer) {
			out[r.Type+"|"+r.ThreadID] = r.EventID

			if r.TS != now.UnixMilli() {
				t.Fatalf("ts = %d, ожидали %d", r.TS, now.UnixMilli())
			}
		}

		return out
	}

	if got := types("@b:test"); len(got) != 2 || got["m.read|"] != "$pub" || got["m.read|$thread"] != "$inthread" {
		t.Fatalf("чужой видит: %v", got)
	}

	if got := types("@a:test"); got["m.read.private|"] != "$priv" || got["m.read|"] != "$pub" {
		t.Fatalf("владелец видит: %v", got)
	}
}

// TestTypingStopIsReported — после typing=false и после таймаута комната какое-то
// время отдает пустой m.typing, чтобы индикатор погас.
func TestTypingStopIsReported(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	store := core.NewEphemeralStore().WithClock(func() time.Time { return now })

	if store.TypingChanged("!r:test") {
		t.Fatal("без набора комната не должна отдавать m.typing")
	}

	store.SetTyping("!r:test", "@a:test", true)
	store.SetTyping("!r:test", "@a:test", false)

	if len(store.Typing("!r:test")) != 0 || !store.TypingChanged("!r:test") {
		t.Fatal("после typing=false нужен пустой m.typing")
	}

	now = now.Add(core.TypingTimeout + time.Second)
	if !store.TypingChanged("!r:test") {
		t.Fatal("в окне после остановки m.typing еще отдается")
	}

	now = now.Add(2 * core.TypingTimeout)
	if store.TypingChanged("!r:test") {
		t.Fatal("старое изменение не должно отдаваться бесконечно")
	}
}

// TestEphemeralVersionsByRoom - версии изменений по комнатам:
// typing и receipts после версии since отдаются, до нее - нет; приватный
// receipt меняет комнату только для владельца; загруженные из БД receipts
// новее любого токена прошлого процесса.
func TestEphemeralVersionsByRoom(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	store := core.NewEphemeralStore().WithClock(func() time.Time { return now })

	// Токен прошлого процесса старше версии старта.
	previous := store.Version() - 1

	store.LoadReceipts([]core.ReceiptEvent{{RoomID: "!a:test", UserID: "@b:test", Type: core.ReceiptRead, EventID: "$1", TS: 1}})

	if got := store.ReceiptsSince("!a:test", "@x:test", previous); len(got) != 1 {
		t.Fatalf("загруженный receipt для старого токена: %v", got)
	}

	since := store.Version()

	if got := store.ReceiptsSince("!a:test", "@x:test", since); len(got) != 0 {
		t.Fatalf("загруженный receipt для свежего токена: %v", got)
	}

	if _, changed := store.TypingSince("!a:test", since); changed {
		t.Fatal("typing не менялся")
	}

	store.SetTyping("!a:test", "@b:test", true)

	if users, changed := store.TypingSince("!a:test", since); !changed || len(users) != 1 {
		t.Fatalf("typing после since: %v %v", users, changed)
	}

	since = store.Version()
	store.SetReceipt("!a:test", "@c:test", core.ReceiptRead, "", "$2")
	store.SetReceipt("!a:test", "@c:test", core.ReceiptReadPrivate, "", "$3")

	// Видны только новые: публичный c всем, приватный - только c.
	if got := store.ReceiptsSince("!a:test", "@x:test", since); len(got) != 1 || got[0].EventID != "$2" {
		t.Fatalf("новые receipts для x: %v", got)
	}

	if got := store.ReceiptsSince("!a:test", "@c:test", since); len(got) != 2 {
		t.Fatalf("новые receipts для c: %v", got)
	}

	// Все receipts - как раньше.
	if got := store.Receipts("!a:test", "@x:test"); len(got) != 2 {
		t.Fatalf("все receipts для x: %v", got)
	}

	if store.Now() != now {
		t.Fatal("Now() не берет часы хранилища")
	}
}

// TestPresenceVersions - версия presence растет только при реальном изменении;
// синк без set_presence не снимает явный unavailable.
func TestPresenceVersions(t *testing.T) {
	t.Parallel()

	store := core.NewEphemeralStore()

	start := store.Version()
	if !store.SyncPresence("@a:test", "") || store.Version() == start {
		t.Fatal("первый синк должен перевести в online и поднять версию")
	}

	version := store.Version()
	if store.SyncPresence("@a:test", "") || store.Version() != version {
		t.Fatal("повторный синк без изменений не должен поднимать версию")
	}

	if !store.SetPresence("@a:test", core.PresenceUnavailable, "") {
		t.Fatal("PUT unavailable - изменение")
	}

	if store.SyncPresence("@a:test", "") || store.Presence("@a:test").Presence != core.PresenceUnavailable {
		t.Fatalf("синк снял явный unavailable: %+v", store.Presence("@a:test"))
	}

	changed := store.PresenceChangedSince(version)
	if len(changed) != 1 || changed[0].UserID != "@a:test" {
		t.Fatalf("изменения после версии: %+v", changed)
	}

	if store.SyncPresence("@a:test", core.PresenceOffline) {
		t.Fatal("set_presence=offline статус не меняет")
	}
}
