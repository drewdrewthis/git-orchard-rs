package peerproxy

// Hermetic transport tests for the peer websocket client: a fake
// graphql-transport-ws server on httptest, driven per-test by a script.
// These pin the failure a live peer cannot reproduce on demand — a
// half-open socket (peer host gone without a FIN) that must time out into
// the reconnect path instead of parking ReadJSON forever (issue #732) —
// plus the keepalive frames that must NOT trip that timeout.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsScript is the server side of one connection, run after the upgrade.
// Reading client frames is the script's job. stop closes during cleanup,
// so a script that must stay silent parks on it instead of sleeping —
// httptest.Server.Close blocks on in-flight handlers.
type wsScript func(t *testing.T, conn *websocket.Conn, stop <-chan struct{})

// fakePeerWS serves script on every upgrade and returns a Client pointed at
// it with readWait shrunk to wait. The returned counter tracks accepted
// upgrades, so a test can assert that a failed open redialed rather than
// caching its error.
//
// Cleanups run LIFO: the client closes first (joining its read loop), then
// stop unblocks the script, then the server shuts down.
func fakePeerWS(t *testing.T, wait time.Duration, script wsScript) (*Client, *atomic.Int64) {
	t.Helper()

	var dials atomic.Int64
	stop := make(chan struct{})
	up := websocket.Upgrader{Subprotocols: []string{graphqlTransportWSProtocol}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		dials.Add(1)
		script(t, conn, stop)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })

	c := newClient(strings.TrimPrefix(srv.URL, "http://"), false, nil, websocket.DefaultDialer, nil)
	// Set before the first Subscribe — that call is what starts the read
	// loop, so the write happens-before every read of the field.
	c.readWait = wait
	t.Cleanup(func() { _ = c.Close() })
	return c, &dials
}

// ackAndSubscribe plays the minimum handshake Client.Subscribe expects:
// consume connection_init, reply connection_ack, consume the subscribe
// frame. It returns the subscription id the client chose so the script can
// address `next` frames back at it.
func ackAndSubscribe(t *testing.T, conn *websocket.Conn) (string, bool) {
	t.Helper()
	var env struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&env); err != nil || env.Type != "connection_init" {
		return "", false
	}
	if err := conn.WriteJSON(map[string]any{"type": "connection_ack"}); err != nil {
		return "", false
	}
	if err := conn.ReadJSON(&env); err != nil || env.Type != "subscribe" {
		return "", false
	}
	return env.ID, true
}

// drainClient swallows whatever the client writes back (pong replies) so a
// script that only writes never stalls on a full receive buffer.
func drainClient(conn *websocket.Conn) {
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// subscribeOrFail opens a subscription on a context tied to the test, so
// the teardown goroutine Subscribe spawns never outlives it.
func subscribeOrFail(t *testing.T, c *Client) <-chan QueryResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, err := c.Subscribe(ctx, `subscription { peerChanged { id } }`, nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return ch
}

// nextFrame marshals one graphql-transport-ws `next` carrying a trivial
// payload, addressed at the client's subscription id.
func nextFrame(id string) map[string]any {
	payload, _ := json.Marshal(map[string]any{"data": map[string]any{"peerChanged": map[string]any{"id": "n1"}}})
	return map[string]any{"id": id, "type": "next", "payload": json.RawMessage(payload)}
}

// A peer that acks, accepts the subscription, then goes silent — no data,
// no keepalive, no close. The read deadline is the only thing that can end
// this connection; without it readLoop blocks forever, so the subscription
// never errors, never closes, and Provider.runPeer never redials.
func TestSubscribeSilentSocketFailsStreamIntoReconnect(t *testing.T) {
	c, _ := fakePeerWS(t, 150*time.Millisecond, func(t *testing.T, conn *websocket.Conn, stop <-chan struct{}) {
		if _, ok := ackAndSubscribe(t, conn); !ok {
			return
		}
		<-stop
	})

	ch := subscribeOrFail(t, c)

	select {
	case res, ok := <-ch:
		if !ok {
			t.Fatal("stream closed with no error frame — the caller cannot tell a dead peer from a completed subscription")
		}
		if len(res.Errors) == 0 {
			t.Fatalf("silent socket delivered %+v, want a read-deadline error", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop still parked on a silent socket — no read deadline (#732)")
	}

	// runPeer redials when the stream channel closes, so the close is the
	// reconnect signal — an error frame alone would leave it waiting.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("want the stream closed after the error frame")
		}
	case <-time.After(time.Second):
		t.Fatal("stream never closed after the read-deadline error — no reconnect")
	}
}

// A peer that completes the HTTP upgrade and then never acks. The dialer's
// HandshakeTimeout is already spent by then, so only a read deadline can
// unblock the connection_ack read.
//
// The redial assertion is the other half: Provider.runPeer retries
// Subscribe every 5s, and a failed open that stays cached in connOnce hands
// back the same stale error forever — a timeout that never reconnects is
// just a slower hang.
func TestSubscribeSilentHandshakeErrorsAndRedials(t *testing.T) {
	c, dials := fakePeerWS(t, 150*time.Millisecond, func(t *testing.T, conn *websocket.Conn, stop <-chan struct{}) {
		var env map[string]any
		_ = conn.ReadJSON(&env) // consume connection_init, never ack
		<-stop
	})

	done := make(chan error, 1)
	go func() {
		_, err := c.Subscribe(context.Background(), `subscription { peerChanged { id } }`, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("silent handshake: want an error from Subscribe, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe still parked reading connection_ack — no read deadline (#732)")
	}

	if _, err := c.Subscribe(context.Background(), `subscription { peerChanged { id } }`, nil); err == nil {
		t.Fatal("second Subscribe unexpectedly succeeded against a peer that never acks")
	}
	if got := dials.Load(); got < 2 {
		t.Fatalf("upgrades accepted = %d, want >= 2 — the failed open was cached instead of redialing", got)
	}
}

// A healthy-but-quiet peer keeps the connection alive purely with
// graphql-transport-ws `ping` messages (what gqlgen's
// KeepAlivePingInterval actually sends). Those are data frames, so the
// per-read re-arm must cover them and a later `next` must still deliver.
func TestSubscribeSurvivesProtocolPingsThenDelivers(t *testing.T) {
	c, _ := fakePeerWS(t, 300*time.Millisecond, func(t *testing.T, conn *websocket.Conn, stop <-chan struct{}) {
		id, ok := ackAndSubscribe(t, conn)
		if !ok {
			return
		}
		drainClient(conn)
		for i := 0; i < 6; i++ { // 600ms of pings, double the shrunk readWait
			if err := conn.WriteJSON(map[string]any{"type": "ping"}); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = conn.WriteJSON(nextFrame(id))
		<-stop
	})

	ch := subscribeOrFail(t, c)

	select {
	case res, ok := <-ch:
		if !ok {
			t.Fatal("stream closed during pings — protocol frames are not re-arming the deadline")
		}
		if len(res.Errors) != 0 {
			t.Fatalf("stream errored during pings: %v", res.Errors)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no payload delivered after the ping run")
	}
}

// A peer whose keepalive is websocket control frames rather than protocol
// messages. gorilla consumes ping/pong frames inside ReadJSON without ever
// returning to the caller, so the per-read re-arm alone would let the
// deadline expire on a demonstrably live socket — only the ping/pong
// handlers can keep it open.
func TestSubscribeSurvivesControlFramesThenDelivers(t *testing.T) {
	c, _ := fakePeerWS(t, 200*time.Millisecond, func(t *testing.T, conn *websocket.Conn, stop <-chan struct{}) {
		id, ok := ackAndSubscribe(t, conn)
		if !ok {
			return
		}
		drainClient(conn)
		for i := 0; i < 6; i++ { // 480ms of control traffic, no data frames at all
			kind := websocket.PingMessage
			if i%2 == 1 {
				kind = websocket.PongMessage // unsolicited pong: a valid one-way heartbeat
			}
			if err := conn.WriteControl(kind, nil, time.Now().Add(time.Second)); err != nil {
				return
			}
			time.Sleep(80 * time.Millisecond)
		}
		_ = conn.WriteJSON(nextFrame(id))
		<-stop
	})

	ch := subscribeOrFail(t, c)

	select {
	case res, ok := <-ch:
		if !ok {
			t.Fatal("stream closed during control-frame keepalive — ping/pong handlers are not re-arming the deadline")
		}
		if len(res.Errors) != 0 {
			t.Fatalf("stream errored during control-frame keepalive: %v", res.Errors)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no payload delivered after the control-frame run")
	}
}
