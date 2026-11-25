package core

import (
	"errors"
	"testing"
)

// TestParsePageToken - from пагинации комнаты: наш "s<pos>" и токены Synapse,
// оставшиеся у клиентов после переезда.
func TestParsePageToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		token string
		want  pageToken
	}{
		{"s42", pageToken{kind: pageTokenStream, stream: 42}},
		{"s-7", pageToken{kind: pageTokenStream, stream: -7}},
		// RoomStreamToken Synapse: topological - depth события, затем stream.
		{"t10165-123456", pageToken{kind: pageTokenTopological, depth: 10165, stream: 123456}},
		// Отрицательный stream - событие backfill Synapse.
		{"t12--5", pageToken{kind: pageTokenTopological, depth: 12, stream: -5}},
		// StreamToken Synapse целиком: первая часть - позиция комнаты.
		{"t10165-123456_789_0_0_0_0_0_0_0_0_0_0_0_0", pageToken{kind: pageTokenTopological, depth: 10165, stream: 123456}},
		{"s123456_789_12_m5~1.3_0_0_0_0_0_0_0_0_0_0", pageToken{kind: pageTokenLive, stream: 123456}},
		{"m123456~1.123450~2.123456", pageToken{kind: pageTokenLive, stream: 123456}},
		{"m5~", pageToken{kind: pageTokenLive, stream: 5}},
		{"m123456~1.123450_7_0", pageToken{kind: pageTokenLive, stream: 123456}},
	}

	for _, tc := range cases {
		got, err := parsePageToken(tc.token)
		if err != nil {
			t.Fatalf("parsePageToken(%q): %v", tc.token, err)
		}

		if got != tc.want {
			t.Fatalf("parsePageToken(%q) = %+v, ожидали %+v", tc.token, got, tc.want)
		}
	}

	for _, bad := range []string{
		"", "xx", "s", "s5abc", "t10", "t10-", "t-5", "tx-1", "t1-2-3", "m", "m~1.2", "mx~1",
		"s1_abc", "t1-2_x", "v1.e5.mac", "12", "s1 2",
	} {
		_, err := parsePageToken(bad)
		if err == nil {
			t.Fatalf("parsePageToken(%q) без ошибки", bad)
		}

		if !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("parsePageToken(%q) = %v, ожидали ErrInvalidParam (M_INVALID_PARAM)", bad, err)
		}
	}
}
