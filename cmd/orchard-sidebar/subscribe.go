package main

// Subscription lane: the daemon pushes a full tmux session snapshot on
// tmuxSessionsChanged whenever its tmux provider invalidates, so an attach
// that happens in another pane reaches this sidebar as fast as the daemon
// notices it instead of waiting out our 2s poll.
//
// This does NOT replace the fast poll. claudeInstances has no subscription
// (there is no claudeInstancesChanged in the schema), so the poll still owns
// state/model/title; the subscription owns attach, session inventory and the
// pane->session map. Both write through foldSessions, so they can't disagree
// about shape — only about freshness, and the subscription is always fresher.
//
// Transport is graphql-transport-ws over the same /graphql endpoint
// (internal/server/server.go: transport.Websocket). The daemon's CheckOrigin
// allows a missing Origin header for native clients, which is what we send.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"
)

// wsURL is a var so hermetic tests can point the lane at an httptest server.
var wsURL = "ws://127.0.0.1:7777/graphql"

// readWait bounds how long a read may sit with no frame at all. The server
// pings every 10s, so a healthy connection always delivers well inside it —
// only a half-open socket (daemon host gone without a FIN) goes silent this
// long. Without the deadline that socket parks ReadJSON forever: no error,
// no redial, and the push lane is dead while looking merely quiet. A var so
// tests can shrink it.
var readWait = 30 * time.Second

const tmuxSubQuery = `subscription { tmuxSessionsChanged { name attached createdAt windows { panes { paneId } } } }`

// tmuxSubMsg is one pushed snapshot. err set means the socket dropped; the
// lane reconnects on its own, so the model only records it (subErr) to know
// the push lane is stale and hand attach authority back to the poll.
type tmuxSubMsg struct {
	sessions []tmuxSession
	err      error
}

// subscribeTmux runs for the life of the process, redialing with backoff.
// It is started after the program is constructed and delivers through
// p.Send, which is the supported way to push into a bubbletea loop from a
// goroutine. send is prog.Send in production; tests pass their own sink.
func subscribeTmux(ctx context.Context, send func(tea.Msg)) {
	backoff := time.Second
	for ctx.Err() == nil {
		acked, err := streamTmux(ctx, send)
		if acked {
			// a real connection happened; the next failure starts the climb
			// from the bottom instead of wherever the last outage left it
			backoff = time.Second
		}
		if err != nil && ctx.Err() == nil {
			send(tmuxSubMsg{err: err})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		// The daemon restarting is the common cause of a drop, so climb
		// slowly and cap low: a sidebar that takes a minute to notice the
		// daemon came back is worse than a few extra dials.
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}

// streamTmux holds one connection open, returning on the first error so the
// caller can redial. acked reports whether the server completed the handshake
// — the caller's signal to reset its backoff.
func streamTmux(ctx context.Context, send func(tea.Msg)) (acked bool, _ error) {
	dialer := websocket.Dialer{
		Subprotocols:     []string{"graphql-transport-ws"},
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialer.DialContext(ctx, wsURL, http.Header{})
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	if err := conn.WriteJSON(map[string]any{"type": "connection_init", "payload": map[string]any{}}); err != nil {
		return false, err
	}
	for {
		var env struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"payload"`
		}
		// each read gets a fresh deadline, so any frame — data or keepalive —
		// re-arms it; only total silence trips it
		_ = conn.SetReadDeadline(time.Now().Add(readWait))
		if err := conn.ReadJSON(&env); err != nil {
			return acked, err
		}
		switch env.Type {
		case "connection_ack":
			if acked {
				continue
			}
			acked = true
			if err := conn.WriteJSON(map[string]any{
				"id":      "tmux",
				"type":    "subscribe",
				"payload": map[string]any{"query": tmuxSubQuery},
			}); err != nil {
				return acked, err
			}
		case "next":
			var data struct {
				Data struct {
					TmuxSessionsChanged []tmuxSession `json:"tmuxSessionsChanged"`
				} `json:"data"`
			}
			if json.Unmarshal(env.Payload, &data) != nil {
				continue
			}
			send(tmuxSubMsg{sessions: data.Data.TmuxSessionsChanged})
		case "error", "complete":
			// server-side end of this operation: drop the socket and redial
			// rather than sitting on a connection with no live subscription
			return acked, errSubEnded
		case "ping":
			_ = conn.WriteJSON(map[string]any{"type": "pong"})
		}
	}
}

type subErr string

func (e subErr) Error() string { return string(e) }

const errSubEnded = subErr("subscription ended")
