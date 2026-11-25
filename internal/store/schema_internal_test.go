package store

import (
	"errors"
	"strings"
	"testing"
)

// TestCompareSchema - старт допустим только на применённой, чистой
// схеме ровно ожидаемой версии.
func TestCompareSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version uint
		dirty   bool
		found   bool
		wantErr string
	}{
		{name: "совпадает", version: 24, found: true},
		{name: "не накатана", found: false, wantErr: "not applied"},
		{name: "dirty", version: 24, dirty: true, found: true, wantErr: "dirty"},
		{name: "старая", version: 23, found: true, wantErr: "run `syngo-migrate -up`"},
		{name: "новее бинаря", version: 25, found: true, wantErr: "newer than this build"},
	}

	for _, tt := range tests {
		err := compareSchema(tt.version, tt.dirty, tt.found, 24)
		if tt.wantErr == "" {
			if err != nil {
				t.Errorf("%s: %v", tt.name, err)
			}

			continue
		}

		if !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: err = %v, ожидали %q", tt.name, err, tt.wantErr)
		}
	}
}
