package pushrules_test

import (
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/pushrules"
)

const alice = "@alice:example.org"

func strPtr(s string) *string { return &s }

func message(body string, extra map[string]any) pushrules.Event {
	content := map[string]any{"msgtype": "m.text", "body": body}
	for k, v := range extra {
		content[k] = v
	}

	raw, _ := json.Marshal(content)

	return pushrules.Event{Type: "m.room.message", Sender: "@bob:example.org", RoomID: "!r:example.org", Content: raw}
}

func groupRoom() pushrules.Context {
	return pushrules.RoomInfo{MemberCount: 5, DisplayName: "Алиса"}.Context("@bob:example.org")
}

// TestDefaultsSpecSet - полный набор серверных правил из
// спецификации, все с default=true, master выключен.
func TestDefaultsSpecSet(t *testing.T) {
	t.Parallel()

	rs := pushrules.Build(alice, nil)

	want := map[string][]string{
		"override": {
			".m.rule.master", ".m.rule.suppress_notices", ".m.rule.invite_for_me", ".m.rule.member_event",
			".m.rule.is_user_mention", ".m.rule.contains_display_name", ".m.rule.is_room_mention",
			".m.rule.roomnotif", ".m.rule.tombstone", ".m.rule.reaction", ".m.rule.room.server_acl",
			".m.rule.suppress_edits",
		},
		"content": {".m.rule.contains_user_name"},
		"underride": {
			".m.rule.call", ".m.rule.encrypted_room_one_to_one", ".m.rule.room_one_to_one",
			".m.rule.message", ".m.rule.encrypted",
		},
	}

	for kind, ids := range want {
		list := *rs.List(kind)
		if len(list) != len(ids) {
			t.Fatalf("%s: %d правил, ожидали %d", kind, len(list), len(ids))
		}

		for i, id := range ids {
			if list[i].RuleID != id || !list[i].Default {
				t.Fatalf("%s[%d] = %s default=%v, ожидали %s", kind, i, list[i].RuleID, list[i].Default, id)
			}

			if enabled := id != ".m.rule.master"; list[i].Enabled != enabled {
				t.Fatalf("%s enabled=%v", id, list[i].Enabled)
			}
		}
	}

	if rs.Content[0].Pattern != "alice" {
		t.Fatalf("contains_user_name pattern = %q", rs.Content[0].Pattern)
	}

	raw, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string][]map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}

	if _, ok := decoded["override"][0]["conditions"]; !ok {
		t.Fatalf("у override нет conditions: %s", raw)
	}

	if _, ok := decoded["content"][0]["conditions"]; ok {
		t.Fatalf("у content не должно быть conditions: %s", raw)
	}
}

func TestEvaluateDefaults(t *testing.T) {
	t.Parallel()

	rs := pushrules.Build(alice, nil)
	group := groupRoom()
	dm := pushrules.RoomInfo{MemberCount: 2, DisplayName: "Алиса"}.Context("@bob:example.org")

	cases := []struct {
		name      string
		ev        pushrules.Event
		ctx       pushrules.Context
		rule      string
		notify    bool
		highlight bool
	}{
		{"обычное сообщение", message("привет всем", nil), group, ".m.rule.message", true, false},
		{"личка", message("привет", nil), dm, ".m.rule.room_one_to_one", true, false},
		{"notice", pushrules.Event{Type: "m.room.message", Content: json.RawMessage(`{"msgtype":"m.notice","body":"x"}`)}, group, ".m.rule.suppress_notices", false, false},
		{"имя в тексте", message("Алиса, глянь", nil), group, ".m.rule.contains_display_name", true, true},
		{"имя внутри слова", message("Алисакот", nil), group, ".m.rule.message", true, false},
		{"логин в тексте", message("ping alice!", nil), group, ".m.rule.contains_user_name", true, true},
		{"m.mentions", message("hi", map[string]any{"m.mentions": map[string]any{"user_ids": []string{alice}}}), group, ".m.rule.is_user_mention", true, true},
		{"m.mentions без нас глушит legacy", message("Алиса", map[string]any{"m.mentions": map[string]any{}}), group, ".m.rule.message", true, false},
		{"правка", message("* x", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": "$e"}}), group, ".m.rule.suppress_edits", false, false},
		{"реакция", pushrules.Event{Type: "m.reaction", Content: json.RawMessage(`{}`)}, group, ".m.rule.reaction", false, false},
		{"шифрованное в группе", pushrules.Event{Type: "m.room.encrypted", Content: json.RawMessage(`{}`)}, group, ".m.rule.encrypted", true, false},
		{"шифрованное в личке", pushrules.Event{Type: "m.room.encrypted", Content: json.RawMessage(`{}`)}, dm, ".m.rule.encrypted_room_one_to_one", true, false},
		{"звонок", pushrules.Event{Type: "m.call.invite", Content: json.RawMessage(`{}`)}, group, ".m.rule.call", true, false},
		{"приглашение мне", pushrules.Event{Type: "m.room.member", StateKey: strPtr(alice), Content: json.RawMessage(`{"membership":"invite"}`)}, group, ".m.rule.invite_for_me", true, false},
		{"чужое членство", pushrules.Event{Type: "m.room.member", StateKey: strPtr("@carol:x"), Content: json.RawMessage(`{"membership":"join"}`)}, group, ".m.rule.member_event", false, false},
		{"tombstone", pushrules.Event{Type: "m.room.tombstone", StateKey: strPtr(""), Content: json.RawMessage(`{}`)}, group, ".m.rule.tombstone", true, true},
		{"@room без прав", message("@room обед", nil), group, ".m.rule.message", true, false},
		{"@room с правами", message("@room обед", nil), pushrules.RoomInfo{MemberCount: 5, PowerLevels: json.RawMessage(`{"users":{"@bob:example.org":50}}`)}.Context("@bob:example.org"), ".m.rule.roomnotif", true, true},
		{"m.mentions.room с правами", message("x", map[string]any{"m.mentions": map[string]any{"room": true}}), pushrules.RoomInfo{MemberCount: 5, PowerLevels: json.RawMessage(`{"users_default":"60"}`)}.Context("@bob:example.org"), ".m.rule.is_room_mention", true, true},
		{"произвольный тип", pushrules.Event{Type: "m.room.topic", StateKey: strPtr(""), Content: json.RawMessage(`{}`)}, group, "", false, false},
	}

	for _, tc := range cases {
		res := rs.Evaluate(tc.ev, tc.ctx)
		if res.RuleID != tc.rule || res.Notify != tc.notify || res.Highlight != tc.highlight {
			t.Errorf("%s: rule=%q notify=%v highlight=%v, ожидали %q %v %v",
				tc.name, res.RuleID, res.Notify, res.Highlight, tc.rule, tc.notify, tc.highlight)
		}
	}
}

// TestEvaluateUserRules - заглушенная комната (override с room_id, как делает
// Element), правило комнаты, ключевое слово и переопределения серверных правил.
func TestEvaluateUserRules(t *testing.T) {
	t.Parallel()

	stored := []pushrules.StoredRule{
		{
			Kind: "override", RuleID: "!muted:example.org", Enabled: true,
			Conditions: json.RawMessage(`[{"kind":"event_match","key":"room_id","pattern":"!muted:example.org"}]`),
			Actions:    json.RawMessage(`[]`),
		},
		{Kind: "room", RuleID: "!quiet:example.org", Enabled: true, Actions: json.RawMessage(`["dont_notify"]`)},
		{Kind: "content", RuleID: "kw", Enabled: true, Pattern: "релиз*", Actions: json.RawMessage(`["notify",{"set_tweak":"highlight","value":true}]`)},
		{Kind: "sender", RuleID: "@spam:example.org", Enabled: true, Actions: json.RawMessage(`[]`)},
		// Переопределение серверного: .m.rule.message без уведомления.
		{Kind: "underride", RuleID: ".m.rule.message", Enabled: true, Conditions: json.RawMessage(`null`), Actions: json.RawMessage(`["dont_notify"]`)},
		// Строка старого кода (conditions не null) игнорируется.
		{Kind: "underride", RuleID: ".m.rule.encrypted", Enabled: false, Conditions: json.RawMessage(`[]`), Actions: json.RawMessage(`["notify"]`)},
	}

	rs := pushrules.Build(alice, stored)
	group := groupRoom()

	if rs.Override[0].RuleID != ".m.rule.master" || rs.Override[1].RuleID != "!muted:example.org" {
		t.Fatalf("порядок override: %s, %s", rs.Override[0].RuleID, rs.Override[1].RuleID)
	}

	muted := message("Алиса!", nil)
	muted.RoomID = "!muted:example.org"

	if res := rs.Evaluate(muted, group); res.Notify {
		t.Fatalf("в заглушенной комнате уведомление: %+v", res)
	}

	quiet := message("привет", nil)
	quiet.RoomID = "!quiet:example.org"

	if res := rs.Evaluate(quiet, group); res.Notify || res.RuleID != "!quiet:example.org" {
		t.Fatalf("room-правило: %+v", res)
	}

	if res := rs.Evaluate(message("завтра Релизы", nil), group); !res.Highlight || res.RuleID != "kw" {
		t.Fatalf("ключевое слово: %+v", res)
	}

	spam := message("hello", nil)
	spam.Sender = "@spam:example.org"

	if res := rs.Evaluate(spam, group); res.Notify {
		t.Fatalf("sender-правило: %+v", res)
	}

	if res := rs.Evaluate(message("просто так", nil), group); res.Notify || res.RuleID != ".m.rule.message" {
		t.Fatalf("переопределенный .m.rule.message: %+v", res)
	}

	enc := pushrules.Event{Type: "m.room.encrypted", Content: json.RawMessage(`{}`)}
	if res := rs.Evaluate(enc, group); !res.Notify {
		t.Fatalf("старая строка .m.rule.encrypted не должна выключать правило: %+v", res)
	}

	// master включен - тишина везде.
	master := pushrules.StoredRule{
		Kind: "override", RuleID: ".m.rule.master", Enabled: true, Conditions: json.RawMessage(`null`), Actions: json.RawMessage(`null`),
	}
	rs = pushrules.Build(alice, append([]pushrules.StoredRule{master}, stored...))

	if res := rs.Evaluate(message("Алиса", nil), group); res.Notify || res.RuleID != ".m.rule.master" {
		t.Fatalf("master: %+v", res)
	}
}

func TestConditions(t *testing.T) {
	t.Parallel()

	ev := pushrules.Event{
		Type: "m.room.message", Sender: "@bob:x", RoomID: "!r:x",
		Content: json.RawMessage(`{"body":"x","a.b":{"c":"Value"},"n":5,"flag":false,"nil":null,"list":["q",3,true]}`),
	}

	cases := []struct {
		cond string
		ctx  pushrules.Context
		want bool
	}{
		{`{"kind":"event_match","key":"content.a\\.b.c","pattern":"val*"}`, pushrules.Context{}, true},
		{`{"kind":"event_match","key":"content.a\\.b.c","pattern":"val"}`, pushrules.Context{}, false},
		{`{"kind":"event_match","key":"content.a.b.c","pattern":"*"}`, pushrules.Context{}, false},
		{`{"kind":"event_match","key":"content.n","pattern":"5"}`, pushrules.Context{}, false},
		{`{"kind":"event_match","key":"sender","pattern":"@bob:?"}`, pushrules.Context{}, true},
		{`{"kind":"event_property_is","key":"content.n","value":5}`, pushrules.Context{}, true},
		{`{"kind":"event_property_is","key":"content.n","value":"5"}`, pushrules.Context{}, false},
		{`{"kind":"event_property_is","key":"content.flag","value":false}`, pushrules.Context{}, true},
		{`{"kind":"event_property_is","key":"content.nil","value":null}`, pushrules.Context{}, true},
		{`{"kind":"event_property_is","key":"content.absent","value":null}`, pushrules.Context{}, false},
		{`{"kind":"event_property_contains","key":"content.list","value":3}`, pushrules.Context{}, true},
		{`{"kind":"event_property_contains","key":"content.list","value":"z"}`, pushrules.Context{}, false},
		{`{"kind":"room_member_count","is":">=3"}`, pushrules.Context{MemberCount: 3}, true},
		{`{"kind":"room_member_count","is":"<3"}`, pushrules.Context{MemberCount: 3}, false},
		{`{"kind":"room_member_count"}`, pushrules.Context{MemberCount: 3}, false},
		{`{"kind":"sender_notification_permission","key":"room"}`, pushrules.Context{SenderPowerLevel: 49}, false},
		{`{"kind":"sender_notification_permission","key":"room"}`, pushrules.Context{SenderPowerLevel: 10, NotificationLevels: map[string]int{"room": 10}}, true},
		{`{"kind":"unknown_kind"}`, pushrules.Context{}, false},
	}

	for _, tc := range cases {
		stored := []pushrules.StoredRule{{
			Kind: "override", RuleID: "r", Enabled: true,
			Conditions: json.RawMessage(`[` + tc.cond + `]`), Actions: json.RawMessage(`["notify"]`),
		}}

		rs := pushrules.Build(alice, stored)
		res := rs.Evaluate(ev, tc.ctx)

		if got := res.RuleID == "r"; got != tc.want {
			t.Errorf("%s: совпало=%v, ожидали %v", tc.cond, got, tc.want)
		}
	}
}

func TestValidateActions(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]bool{
		`[]`: true,
		`["notify",{"set_tweak":"sound","value":"default"}]`: true,
		`["dont_notify","coalesce"]`:                         true,
		`["bogus"]`:                                          false,
		`[{"value":1}]`:                                      false,
		`{"notify":1}`:                                       false,
		`null`:                                               false,
	} {
		if got := pushrules.ValidateActions(json.RawMessage(raw)); got != want {
			t.Errorf("%s: %v, ожидали %v", raw, got, want)
		}
	}
}
