package main

// Hermetic subscription-lane tests: a fake graphql-transport-ws server built
// on httptest, driven per-test by a script function. These pin the two
// failure modes a live daemon can't reproduce on demand — a half-open socket
// (server vanishes without a FIN) and a server that pings but never sends
// data — plus the ack signal subscribeTmux uses to reset its backoff.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"
)

// streamResult carries streamTmux's returns out of its goroutine so every
// assertion happens on the test goroutine, never after the test ends.
type streamResult struct {
	acked bool
	err   error
}

// runStream starts streamTmux in a goroutine and returns its result struct
// plus a done channel closed on exit. A cleanup cancels the context and
// waits for that exit BEFORE fakeGqlws restores the package globals — the
// goroutine can never outlive the test or race the restore.
func runStream(t *testing.T, send func(tea.Msg)) (*streamResult, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &streamResult{}
	done := make(chan struct{})
	go func() {
		r.acked, r.err = streamTmux(ctx, send)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done // receive on a closed channel never blocks
	})
	return r, done
}

// ackAndSubscribe consumes connection_init and the subscribe frame, replying
// with connection_ack in between — the minimum handshake streamTmux expects.
func ackAndSubscribe(t *testing.T, conn *websocket.Conn) bool {
	t.Helper()
	var env map[string]any
	if err := conn.ReadJSON(&env); err != nil || env["type"] != "connection_init" {
		return false
	}
	if err := conn.WriteJSON(map[string]any{"type": "connection_ack"}); err != nil {
		return false
	}
	if err := conn.ReadJSON(&env); err != nil || env["type"] != "subscribe" {
		return false
	}
	return true
}

// A server that acks and then goes silent — no pings, no data, no close. The
// read deadline is the only thing that can end this connection; without it
// ReadJSON blocks forever and the push lane is dead with no error and no
// redial.
func TestStreamTmuxTimesOutOnSilentSocket(t *testing.T) {
	fakeGqlws(t, 200*time.Millisecond, func(t *testing.T, conn *websocket.Conn) {
		if !ackAndSubscribe(t, conn) {
			return
		}
		time.Sleep(2 * time.Second) // outlive the shrunk readWait, sending nothing
	})

	r, done := runStream(t, func(tea.Msg) {})
	select {
	case <-done:
		if r.err == nil {
			t.Fatal("silent socket: want a read-deadline error, got nil")
		}
		if !r.acked {
			t.Error("handshake completed but acked=false — backoff would never reset")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("streamTmux still blocked on a silent socket — no read deadline")
	}
}

// A healthy-but-quiet server keeps the connection alive purely with protocol
// pings: each arriving frame must re-arm the deadline, and a data frame
// arriving later must still deliver.
func TestStreamTmuxSurvivesPingsThenDelivers(t *testing.T) {
	fakeGqlws(t, 300*time.Millisecond, func(t *testing.T, conn *websocket.Conn) {
		if !ackAndSubscribe(t, conn) {
			return
		}
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

	got := make(chan tmuxSubMsg, 4)
	r, done := runStream(t, func(m tea.Msg) {
		if sm, ok := m.(tmuxSubMsg); ok {
			got <- sm
		}
	})
	select {
	case m := <-got:
		if len(m.sessions) != 1 || m.sessions[0].Name != "pinged" {
			t.Fatalf("delivered snapshot = %+v, want the pinged session", m.sessions)
		}
	case <-done:
		t.Fatalf("connection died during pings (err %v) — frames are not re-arming the deadline", r.err)
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
	acked, err := streamTmux(context.Background(), func(tea.Msg) {})
	if acked || err == nil {
		t.Fatalf("dial failure: acked=%v err=%v, want false + non-nil", acked, err)
	}
}
