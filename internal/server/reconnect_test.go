package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/angelospk/soniox-core/internal/config"
)

// A /live handshake that can't resolve a Soniox key must tag the error with
// kind:"key" so the client can auto-heal (force a reconnect) instead of just
// surfacing it.
func TestLiveKeyErrorTaggedForAutoHeal(t *testing.T) {
	srv := &Server{KeyResolver: func(context.Context) (string, error) {
		return "", errors.New("no Soniox key (key command failed)")
	}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/live", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var m struct {
		Type, Kind string
	}
	if _, data, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read: %v", err)
	} else if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode: %v (%q)", err, data)
	}
	if m.Type != "error" || m.Kind != "key" {
		t.Fatalf("got type=%q kind=%q, want error/key", m.Type, m.Kind)
	}
}

func TestReconnect_Success(t *testing.T) {
	s := &Server{Reconnect: func(context.Context) (config.ReconnectStatus, error) {
		return config.ReconnectStatus{OK: true, Source: "minted", ExpiresAt: "2026-07-06T12:00:00Z"}, nil
	}}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/reconnect", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got config.ReconnectStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rr.Body.String())
	}
	if !got.OK || got.Source != "minted" || got.ExpiresAt == "" {
		t.Fatalf("got %+v, want OK minted with expiry", got)
	}
}

func TestReconnect_FailureStillJSON(t *testing.T) {
	s := &Server{Reconnect: func(context.Context) (config.ReconnectStatus, error) {
		return config.ReconnectStatus{OK: false, Hint: "network"},
			errors.New("SONIOX_KEY_COMMAND failed: exit status 1")
	}}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/reconnect", nil))

	// A classified failure is still a 200 with the hint in the body — the client
	// shows the hint, it isn't an HTTP-level error.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got config.ReconnectStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OK || got.Hint != "network" {
		t.Fatalf("got %+v, want !OK with the network hint", got)
	}
}

func TestReconnect_MethodGuard(t *testing.T) {
	s := &Server{Reconnect: func(context.Context) (config.ReconnectStatus, error) {
		return config.ReconnectStatus{OK: true}, nil
	}}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/reconnect", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rr.Code)
	}
}

func TestReconnect_Unavailable(t *testing.T) {
	s := &Server{} // no Reconnect seam wired
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/reconnect", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}
