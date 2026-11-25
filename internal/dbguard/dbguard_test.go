package dbguard_test

import (
	"testing"

	"github.com/aisa-it/syngo/internal/dbguard"
)

func TestRequireTestDatabase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		dsn     string
		wantErr bool
	}{
		{"postgres://syngo:syngo@127.0.0.1:5432/syngo_test_a3?sslmode=disable", false},
		{"postgres://syngo:syngo@postgres:5432/syngo_test?sslmode=disable", false},
		{"host=db user=syngo dbname=TEST_syngo sslmode=disable", false},
		{"postgres://syngo:syngo@127.0.0.1:5432/syngo?sslmode=disable", true},
		{"postgres://syngo:syngo@test-host:5432/syngo?sslmode=disable", true}, // test в хосте не считается
		{"host=db user=test dbname=prod", true},
		{"::not a dsn::", true},
	}

	for _, tt := range tests {
		err := dbguard.RequireTestDatabase(tt.dsn)
		if (err != nil) != tt.wantErr {
			t.Errorf("RequireTestDatabase(%q) = %v, wantErr=%t", tt.dsn, err, tt.wantErr)
		}
	}
}
