package main

// Live check of the subscription lane against a running daemon. Skipped unless
// ORCHARD_LIVE=1, so `go test ./...` stays hermetic. Run it after touching the
// handshake: a wrong subprotocol or message type degrades silently back to
// polling rather than failing loudly, which is exactly the bug this catches.

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestLiveSubscriptionDelivers(t *testing.T) {
	if os.Getenv("ORCHARD_LIVE") != "1" {
		t.Skip("set ORCHARD_LIVE=1 with the daemon running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	msgs := make(chan tmuxSubMsg, 16)
	go func() {
		_ = streamTmux(ctx, func(m tea.Msg) {
			if sm, ok := m.(tmuxSubMsg); ok {
				select {
				case msgs <- sm:
				default:
				}
			}
		})
	}()

	// A session appearing invalidates the daemon's tmux provider, which is what
	// makes it emit. Give the socket a moment to finish its handshake first.
	time.Sleep(time.Second)
	if err := exec.Command("tmux", "new-session", "-d", "-s", "zzsubprobe").Run(); err != nil {
		t.Fatalf("create probe session: %v", err)
	}
	defer func() { _ = exec.Command("tmux", "kill-session", "-t", "zzsubprobe").Run() }()

	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("no pushed snapshot containing the probe session — lane is dead, sidebar is polling only")
		case m := <-msgs:
			if m.err != nil {
				t.Fatalf("subscription error: %v", m.err)
			}
			for _, s := range m.sessions {
				if s.Name == "zzsubprobe" {
					t.Logf("pushed snapshot with the probe session after %.2fs, %d sessions",
						time.Since(start).Seconds(), len(m.sessions))
					return
				}
			}
		}
	}
}
