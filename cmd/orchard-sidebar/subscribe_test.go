package main

// Hermetic subscription-lane tests: a fake graphql-transport-ws server built
// on httptest, driven per-test by a script function. These pin the two
// failure modes a live daemon can't reproduce on demand — a half-open socket
// (server vanishes without a FIN) and a server that pings but never sends
// data — plus the ack signal subscribeTmux uses to reset its backoff.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"
)

// wsScript is the server side of one connection, run after the upgrade.
// Reading client frames is the script's job; returning closes the socket.
type wsScript func(t *testing.T, conn *websocket.Conn)

// fakeGqlws points wsURL at an httptest server for the test's duration.
func fakeGqlws(t *testing.T, script wsScript) {
	t.Helper()
	up := websocket.Upgrader{Subprotocols: []string{"graphql-transport-ws"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		script(t, conn)
	}))
	t.Cleanup(srv.Close)
	oldURL, oldWait := wsURL, readWait
	wsURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	t.Cleanup(func() { wsURL, readWait = oldURL, oldWait })
}

// ackAndSubscribe consumes connection_init and the subscribe frame, replying
// with connection_ack in between — the minimum handshake streamTmux expects.
func ackAndSubscribe(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	var env map[string]any
	if err := conn.ReadJSON(&env); err != nil || env["type"] != "connection_init" {
		t.Errorf("want connection_init, got %v (err %v)", env, err)
		return
	}
	if err := conn.WriteJSON(map[string]any{"type": "connection_ack"}); err != nil {
		t.Errorf("write ack: %v", err)
		return
	}
	if err := conn.ReadJSON(&env); err != nil || env["type"] != "subscribe" {
		t.Errorf("want subscribe, got %v (err %v)", env, err)
	}
}

// A server that acks and then goes silent — no pings, no data, no close. The
// read deadline is the only thing that can end this connection; without it
// ReadJSON blocks forever and the push lane is dead with no error and no
// redial.
func TestStreamTmuxTimesOutOnSilentSocket(t *testing.T) {
	fakeGqlws(t, func(t *testing.T, conn *websocket.Conn) {
		ackAndSubscribe(t, conn)
		time.Sleep(2 * time.Second) // outlive the shrunk readWait, sending nothing
	})
	readWait = 200 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		acked, err := streamTmux(t.Context(), func(tea.Msg) {})
		if !acked {
			t.Error("handshake completed but acked=false — backoff would never reset")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("silent socket: want a read-deadline error, got nil")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("streamTmux still blocked on a silent socket — no read deadline")
	}
}

// A healthy-but-quiet server keeps the connection alive purely with protocol
// pings: each arriving frame must re-arm the deadline, and a data frame
// arriving later must still deliver.
func TestStreamTmuxSurvivesPingsThenDelivers(t *testing.T) {
	fakeGqlws(t, func(t *testing.T, conn *websocket.Conn) {
		ackAndSubscribe(t, conn)
		go func() { // swallow the client's pong replies
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for i := 0; i < 6; i++ { // 600ms of pings, double the shrunk readWait
			if err := conn.WriteJSON(map[string]any{"type": "ping"}); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		payload, _ := json.Marshal(map[string]any{
			"data": map[string]any{"tmuxSessionsChanged": []map[string]any{{"name": "pinged"}}},
		})
		_ = conn.WriteJSON(map[string]any{"type": "next", "id": "tmux", "payload": json.RawMessage(payload)})
		time.Sleep(200 * time.Millisecond)
	})
	readWait = 300 * time.Millisecond

	got := make(chan tmuxSubMsg, 4)
	done := make(chan error, 1)
	go func() {
		_, err := streamTmux(t.Context(), func(m tea.Msg) {
			if sm, ok := m.(tmuxSubMsg); ok {
				got <- sm
			}
		})
		done <- err
	}()
	select {
	case m := <-got:
		if len(m.sessions) != 1 || m.sessions[0].Name != "pinged" {
			t.Fatalf("delivered snapshot = %+v, want the pinged session", m.sessions)
		}
	case err := <-done:
		t.Fatalf("connection died during pings (err %v) — frames are not re-arming the deadline", err)
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot delivered")
	}
}

// Dial failure (daemon down) must report acked=false so subscribeTmux keeps
// climbing its backoff instead of resetting on every failed dial.
func TestStreamTmuxDialFailureIsNotAcked(t *testing.T) {
	old := wsURL
	wsURL = "ws://127.0.0.1:1/graphql" // nothing listens on port 1
	t.Cleanup(func() { wsURL = old })
	acked, err := streamTmux(t.Context(), func(tea.Msg) {})
	if acked || err == nil {
		t.Fatalf("dial failure: acked=%v err=%v, want false + non-nil", acked, err)
	}
}
