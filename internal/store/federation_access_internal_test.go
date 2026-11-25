package store

import "testing"

// TestServerMaySee - видимость события серверу по history_visibility и
// членству его участников на момент события.
func TestServerMaySee(t *testing.T) {
	t.Parallel()

	cases := []struct {
		visibility  string
		memberships []string
		want        bool
	}{
		{"shared", nil, true},
		{"world_readable", nil, true},
		{"неизвестное", nil, true},
		{"joined", nil, false},
		{"joined", []string{"leave", "join"}, true},
		{"joined", []string{"invite"}, false},
		{"invited", []string{"invite"}, true},
		{"invited", []string{"join"}, true},
		{"invited", []string{"leave"}, false},
	}

	for _, tc := range cases {
		if got := serverMaySee(tc.visibility, tc.memberships); got != tc.want {
			t.Errorf("%s %v: %v, ожидали %v", tc.visibility, tc.memberships, got, tc.want)
		}
	}
}
