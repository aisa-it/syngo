package auth_test

import (
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/auth"
)

// TestValidateAuthEvents — набор auth_events проверяется до правил: дубли,
// чужая комната, лишние типы, отсутствие create/membership отправителя.
func TestValidateAuthEvents(t *testing.T) {
	t.Parallel()

	create := ev("m.room.create", "@alice:test.local", ptr(""), `{"creator":"@alice:test.local"}`, nil)
	create.EventID = "$create"

	pl := ev("m.room.power_levels", "@alice:test.local", ptr(""), `{"users":{"@alice:test.local":100}}`, nil)
	pl.EventID = "$pl"

	aliceMember := ev("m.room.member", "@alice:test.local", ptr("@alice:test.local"), `{"membership":"join"}`, nil)
	aliceMember.EventID = "$alice"

	message := ev("m.room.message", "@alice:test.local", nil, `{"body":"x"}`, nil)
	message.EventID = "$msg"

	bobJoin := ev("m.room.member", "@bob:remote.test", ptr("@bob:remote.test"), `{"membership":"join"}`, nil)
	bobJoin.EventID = "$bobjoin"

	bobLeave := ev("m.room.member", "@bob:remote.test", ptr("@bob:remote.test"), `{"membership":"leave"}`, nil)
	bobLeave.EventID = "$bobleave"

	cases := []struct {
		name      string
		event     auth.Event
		auth      []auth.Event
		wantError bool
	}{
		{
			name:  "полный набор принят",
			event: message,
			auth:  []auth.Event{create, pl, aliceMember},
		},
		{
			name:      "дубль отклонён",
			event:     message,
			auth:      []auth.Event{create, pl, aliceMember, aliceMember},
			wantError: true,
		},
		{
			name:  "событие из другой комнаты отклонено",
			event: message,
			auth: func() []auth.Event {
				foreign := aliceMember
				foreign.EventID = "$foreign"
				foreign.RoomID = "!other:test.local"
				return []auth.Event{create, pl, aliceMember, foreign}
			}(),
			wantError: true,
		},
		{
			name:  "лишний тип отклонён",
			event: message,
			auth: func() []auth.Event {
				extra := ev("m.room.message", "@alice:test.local", nil, `{"body":"y"}`, nil)
				extra.EventID = "$extra"
				return []auth.Event{create, pl, aliceMember, extra}
			}(),
			wantError: true,
		},
		{
			name:      "без create отклонён",
			event:     message,
			auth:      []auth.Event{pl, aliceMember},
			wantError: true,
		},
		{
			name:      "без membership отправителя отклонён",
			event:     message,
			auth:      []auth.Event{create, pl},
			wantError: true,
		},
		{
			// Первый вход в публичную комнату - членства еще нет.
			name:  "первый join за себя без membership принят",
			event: bobJoin,
			auth:  []auth.Event{create, pl},
		},
		{
			name:      "leave за себя без membership отклонён",
			event:     bobLeave,
			auth:      []auth.Event{create, pl},
			wantError: true,
		},
		{
			name:      "пустой набор отклонён",
			event:     message,
			auth:      nil,
			wantError: true,
		},
		{
			name:  "create сам не требует auth_events",
			event: create,
			auth:  nil,
		},
	}

	for _, tc := range cases {
		err := auth.ValidateAuthEvents(tc.event, tc.auth)
		if tc.wantError && !errors.Is(err, auth.ErrUnauthorized) {
			t.Errorf("%s: ожидали ErrUnauthorized, получили %v", tc.name, err)
		}

		if !tc.wantError && err != nil {
			t.Errorf("%s: неожиданный отказ: %v", tc.name, err)
		}
	}
}
