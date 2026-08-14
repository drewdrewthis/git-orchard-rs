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

const wsURL = "ws://127.0.0.1:7777/graphql"

const tmuxSubQuery = `subscription { tmuxSessionsChanged { name attached createdAt windows { panes { paneId } } } }`

// tmuxSubMsg is one pushed snapshot. err set means the socket dropped; the
// lane reconnects on its own, so the model only notes it for the status line.
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
		if err := streamTmux(ctx, send); err != nil && ctx.Err() == nil {
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
// caller can redial.
func streamTmux(ctx context.Context, send func(tea.Msg)) error {
	dialer := websocket.Dialer{
		Subprotocols:     []string{"graphql-transport-ws"},
		HandshakeTimeout: 5 * time.Second,
	}
	conn, _, err := dialer.DialContext(ctx, wsURL, http.Header{})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	if err := conn.WriteJSON(map[string]any{"type": "connection_init", "payload": map[string]any{}}); err != nil {
		return err
	}
	started := false
	for {
		var env struct {
			Type    string          `json:"type"`
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := conn.ReadJSON(&env); err != nil {
			return err
		}
		switch env.Type {
		case "connection_ack":
			if started {
				continue
			}
			started = true
			if err := conn.WriteJSON(map[string]any{
				"id":      "tmux",
				"type":    "subscribe",
				"payload": map[string]any{"query": tmuxSubQuery},
			}); err != nil {
				return err
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
			return errSubEnded
		case "ping":
			_ = conn.WriteJSON(map[string]any{"type": "pong"})
		}
	}
}

type subErr string

func (e subErr) Error() string { return string(e) }

const errSubEnded = subErr("subscription ended")
