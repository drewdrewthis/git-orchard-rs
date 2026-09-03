package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
	"github.com/drewdrewthis/orchardist/internal/server/resolvers"
)

// TestWebsocketPingPongIdleSubscription proves a graphql-transport-ws client
// with an idle subscription receives a `ping` frame within 15s — the fix for
// #788, where only KeepAlivePingInterval (graphql-ws) was set and idle
// graphql-transport-ws subscriptions got no keepalive and timed out.
func TestWebsocketPingPongIdleSubscription(t *testing.T) {
	// Shrink the ping cadence so the test is fast and deterministic; the
	// assertion (ping within 15s) still holds for the production 10s default.
	orig := wsPingPongInterval
	wsPingPongInterval = 200 * time.Millisecond
	defer func() { wsPingPongInterval = orig }()

	// A tmux provider that is never Started: its Sessions subscription channel
	// stays open but never fires, so tmuxSessionsChanged stays silent — the
	// idle subscription the ping must keep alive.
	res := resolvers.New(time.Now())
	res.Tmux = tmux.New(tmux.NewAdapter("test-host"), nil)

	srv := httptest.NewServer(graphqlHandlerFor(res))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/graphql"
	dialer := websocket.Dialer{
		Subprotocols:     []string{"graphql-transport-ws"},
		HandshakeTimeout: 5 * time.Second,
	}
	// Missing Origin header passes checkGUIOrigin (native-client path).
	conn, _, err := dialer.Dial(wsURL, http.Header{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteJSON(map[string]any{"type": "connection_init", "payload": map[string]any{}}); err != nil {
		t.Fatalf("write connection_init: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	acked := false
	for {
		var env struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := conn.ReadJSON(&env); err != nil {
			t.Fatalf("read frame (acked=%v): %v", acked, err)
		}
		switch env.Type {
		case "connection_ack":
			acked = true
			if err := conn.WriteJSON(map[string]any{
				"id":      "sub",
				"type":    "subscribe",
				"payload": map[string]any{"query": "subscription { tmuxSessionsChanged { name } }"},
			}); err != nil {
				t.Fatalf("write subscribe: %v", err)
			}
		case "ping":
			if !acked {
				t.Fatalf("ping before connection_ack")
			}
			// Answering with pong is optional for the assertion; do it so the
			// server does not trip its missing-pong read deadline.
			_ = conn.WriteJSON(map[string]any{"type": "pong"})
			return // success: idle subscription received a ping
		case "next", "error", "complete":
			t.Fatalf("unexpected %q frame on an idle subscription: %s", env.Type, env.Payload)
		}
	}
}
