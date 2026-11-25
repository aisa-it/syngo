package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSynapseStateNotFound — если пир уже не знает комнату (вышел), сверять
// нечего: возвращаем nil без ошибки, иначе интероп падает на leave/ban
// с ложным «расхождением».
func TestSynapseStateNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"Room not found"}`))
	}))
	defer srv.Close()

	state, _, err := synapseState(t.Context(), srv.URL, "t", "!r:test")
	if err != nil {
		t.Fatalf("synapseState: %v", err)
	}

	if state != nil {
		t.Fatalf("ожидали nil для неизвестной комнаты, получили %d строк", len(state))
	}
}
