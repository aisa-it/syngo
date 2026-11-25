package auth

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

// signedInvite собирает m.room.member с third_party_invite, подписанный ключом
// identity-сервера, и возвращает content.
func signedInvite(t *testing.T, key signing.Key, mxid, token string) string {
	t.Helper()

	signed := map[string]any{"mxid": mxid, "token": token}

	canonical, err := signing.CanonicalJSON(signed)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}

	signature := key.Sign(canonical)
	content := map[string]any{
		"membership": "invite",
		"third_party_invite": map[string]any{
			"display_name": "bob",
			"signed": map[string]any{
				"mxid":       mxid,
				"token":      token,
				"signatures": map[string]any{"identity.test": map[string]string{key.KeyID: signature}},
			},
		},
	}

	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return string(raw)
}

// TestThirdPartyInvite — третьесторонний invite: mxid совпадает с целью, token и
// подписи непустые, приглашающий в комнате, а подпись `signed` проверяется
// публичным ключом из m.room.third_party_invite.
func TestThirdPartyInvite(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("1")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	stateFor := func(token, publicKey string) State {
		return State{
			Create:  &Event{Type: "m.room.create", Sender: "@admin:test", Content: []byte(`{"creator":"@admin:test","m.federate":true}`)},
			Members: map[string]string{"@admin:test": "join"},
			ThirdPartyInvites: map[string]json.RawMessage{
				token: json.RawMessage(fmt.Sprintf(`{"public_key":%q}`, publicKey)),
			},
		}
	}

	member := func(content string) Event {
		return Event{
			Type: "m.room.member", RoomID: "!r:test", Sender: "@admin:test",
			StateKey: strPtr("@bob:test"), Content: []byte(content),
		}
	}

	// Корректно подписанное приглашение принимается.
	if err := Check(member(signedInvite(t, key, "@bob:test", "tok")), stateFor("tok", key.Public())); err != nil {
		t.Fatalf("корректный 3pid invite отклонён: %v", err)
	}

	// Подпись чужим ключом отклоняется.
	other, err := signing.Generate("1")
	if err != nil {
		t.Fatalf("generate other: %v", err)
	}

	if err := Check(member(signedInvite(t, other, "@bob:test", "tok")), stateFor("tok", key.Public())); err == nil {
		t.Fatal("подпись чужим ключом принята")
	}

	// Нет m.room.third_party_invite с этим токеном — отказ.
	if err := Check(member(signedInvite(t, key, "@bob:test", "tok")), stateFor("other", key.Public())); err == nil {
		t.Fatal("неизвестный токен принят")
	}

	bad := []string{
		`{"membership":"invite","third_party_invite":{"signed":{"mxid":"@eve:test","token":"tok","signatures":{"t":{"k":"v"}}}}}`,
		`{"membership":"invite","third_party_invite":{"signed":{"mxid":"@bob:test","token":"","signatures":{"t":{"k":"v"}}}}}`,
		`{"membership":"invite","third_party_invite":{"signed":{"mxid":"@bob:test","token":"tok","signatures":{}}}}`,
	}

	for _, content := range bad {
		if err := Check(member(content), stateFor("tok", key.Public())); err == nil {
			t.Fatalf("некорректный 3pid invite принят: %s", content)
		}
	}
}
