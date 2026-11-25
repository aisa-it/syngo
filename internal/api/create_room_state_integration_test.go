//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateRoomMaterializesState - после createRoom состояние и членство
// материализованы, создатель может писать в свою комнату.
func TestCreateRoomMaterializesState(t *testing.T) {
	srv := newIntegrationServer(t)

	// UIA: пробный запрос даёт session, затем dummy-аутентификация.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/register",
		strings.NewReader(`{"username":"probe","password":"probe-password","device_id":"DEV"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)

	var challenge struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &challenge)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/register",
		strings.NewReader(`{"username":"probe","password":"probe-password","device_id":"DEV","auth":{"type":"m.login.dummy","session":"`+challenge.Session+`"}}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}

	var reg struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reg)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/createRoom",
		strings.NewReader(`{"name":"probe","preset":"private_chat"}`))
	req.Header.Set("Authorization", "Bearer "+reg.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	var state, members int
	_ = srv.db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM room_state WHERE room_id=$1`, created.RoomID).Scan(&state)
	_ = srv.db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM room_memberships WHERE room_id=$1`, created.RoomID).Scan(&members)
	if state < 4 {
		t.Fatalf("room_state = %d строк, ожидали >=4 (create, member, power_levels, name)", state)
	}

	if members < 1 {
		t.Fatalf("room_memberships = %d строк, ожидали join создателя", members)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut,
		"/_matrix/client/v3/rooms/"+created.RoomID+"/send/m.room.message/t1",
		strings.NewReader(`{"msgtype":"m.text","body":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+reg.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send после createRoom: %d %s", rec.Code, rec.Body.String())
	}
}
