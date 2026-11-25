package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

// TestParseSyncTimeout — таймаут long-poll: мусор и отрицательные → 0, лимит из конфига.
func TestParseSyncTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		max  int
		want time.Duration
	}{
		{"", 30000, 0},
		{"0", 30000, 0},
		{"-5", 30000, 0},
		{"abc", 30000, 0},
		{"1000", 30000, time.Second},
		{"60000", 30000, 30 * time.Second},
		{"1000", 0, 0},
	}

	for _, tc := range cases {
		if got := parseSyncTimeout(tc.raw, tc.max); got != tc.want {
			t.Fatalf("parseSyncTimeout(%q, %d) = %v, ожидали %v", tc.raw, tc.max, got, tc.want)
		}
	}
}

// TestLazyLoadMembers — в state остаются только member'ы отправителей порции и сам пользователь.
func TestLazyLoadMembers(t *testing.T) {
	t.Parallel()

	memberEvent := func(stateKey string) core.Event {
		key := stateKey
		content, _ := json.Marshal(map[string]string{"membership": "join"})

		return core.Event{Type: "m.room.member", StateKey: &key, Sender: stateKey, Content: content}
	}

	topic := core.Event{Type: "m.room.topic", Content: json.RawMessage(`{"topic":"x"}`)}
	timeline := []core.Event{{Type: "m.room.message", Sender: "@alice:test.local"}}

	state := []core.Event{memberEvent("@alice:test.local"), memberEvent("@bob:test.local"), memberEvent("@carol:test.local"), topic}

	filtered := lazyLoadMembers(state, timeline, "@bob:test.local")

	if len(filtered) != 3 {
		t.Fatalf("lazy-load вернул %d событий, ожидали 3 (alice, bob, topic)", len(filtered))
	}

	for _, event := range filtered {
		if event.Type == "m.room.member" && event.StateKey != nil && *event.StateKey == "@carol:test.local" {
			t.Fatal("в lazy-load попал лишний участник carol")
		}
	}
}

// TestFilterInviteState — для invite остаются create/name/topic/avatar и свой member.
func TestFilterInviteState(t *testing.T) {
	t.Parallel()

	key := ""
	create := core.Event{Type: "m.room.create", StateKey: &key, Content: json.RawMessage(`{}`)}
	power := core.Event{Type: "m.room.power_levels", StateKey: &key, Content: json.RawMessage(`{}`)}

	bob := "@bob:test.local"
	ownMember := core.Event{Type: "m.room.member", StateKey: &bob, Content: json.RawMessage(`{"membership":"invite"}`)}

	alice := "@alice:test.local"
	otherMember := core.Event{Type: "m.room.member", StateKey: &alice, Content: json.RawMessage(`{"membership":"join"}`)}

	views := filterInviteState([]core.Event{create, power, ownMember, otherMember}, bob)

	if len(views) != 2 {
		t.Fatalf("invite_state = %d событий, ожидали 2 (create и свой member)", len(views))
	}
}

// TestFilterInviteStateIncludesInviter — stripped state по спецификации: членство
// пригласившего, join_rules и encryption (иначе Element называл личный чат
// «Empty room»), посторонние участники и power_levels - нет.
func TestFilterInviteStateIncludesInviter(t *testing.T) {
	t.Parallel()

	key := ""
	bob, alice, carol := "@bob:test.local", "@alice:test.local", "@carol:test.local"

	events := []core.Event{
		{Type: "m.room.create", StateKey: &key, Content: json.RawMessage(`{}`)},
		{Type: "m.room.join_rules", StateKey: &key, Content: json.RawMessage(`{"join_rule":"invite"}`)},
		{Type: "m.room.encryption", StateKey: &key, Content: json.RawMessage(`{"algorithm":"m.megolm.v1.aes-sha2"}`)},
		{Type: "m.room.power_levels", StateKey: &key, Content: json.RawMessage(`{}`)},
		{Type: "m.room.member", StateKey: &alice, Sender: alice, Content: json.RawMessage(`{"membership":"join"}`)},
		{Type: "m.room.member", StateKey: &carol, Sender: carol, Content: json.RawMessage(`{"membership":"join"}`)},
		{Type: "m.room.member", StateKey: &bob, Sender: alice, Content: json.RawMessage(`{"membership":"invite","is_direct":true}`)},
	}

	got := map[string]bool{}
	for _, v := range filterInviteState(events, bob) {
		sk := ""
		if v.StateKey != nil {
			sk = *v.StateKey
		}

		got[v.Type+"|"+sk] = true
	}

	for _, want := range []string{"m.room.create|", "m.room.join_rules|", "m.room.encryption|", "m.room.member|" + alice, "m.room.member|" + bob} {
		if !got[want] {
			t.Fatalf("нет %s в invite_state: %v", want, got)
		}
	}

	if got["m.room.member|"+carol] || got["m.room.power_levels|"] {
		t.Fatalf("лишнее в invite_state: %v", got)
	}
}
