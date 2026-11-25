package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSynapseStateEmpty — после выхода пира /state отдаёт 200 с пустым списком:
// это не расхождение.
func TestSynapseStateEmpty(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":[]}`))
	}))
	defer srv.Close()

	state, _, err := synapseState(t.Context(), srv.URL, "t", "!r:test")
	if err != nil {
		t.Fatalf("synapseState: %v", err)
	}

	if len(state) != 0 {
		t.Fatalf("ожидали пустой state, получили %d", len(state))
	}
}
