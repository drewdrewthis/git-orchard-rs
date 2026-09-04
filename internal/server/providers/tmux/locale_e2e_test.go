// Real-tmux end-to-end guard for the D3 locale fix (#701). With LC_ALL /
// LC_CTYPE / LANG scrubbed (the systemd `--user` and launchd class), FetchAll
// against a throwaway `-S` socket must still return the live session — the
// utf8Env fix forces a UTF-8 ctype on the tmux child so the `-F` TAB survives.
// Skips when tmux is unavailable.

package tmux_test

import (
	"context"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

func TestFetchAll_ScrubbedLocale_ReturnsSession_Issue701(t *testing.T) {
	tmuxAvailable(t)

	// Scrub every locale var before the sandbox and the adapter run, mirroring
	// `env -i` (no LANG/LC_*). Without utf8Env the tmux client marks itself
	// non-UTF-8 and the server sanitizes the TAB separator to `_`, collapsing
	// every list-panes row and yielding zero sessions.
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", "")

	socket := startSandboxTmux(t) // creates session "alpha"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	a := tmux.NewAdapter("h").WithSocket(socket)
	snap, err := a.FetchAll(ctx)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if !snap.Server.Alive {
		t.Fatalf("server should be alive against the sandbox socket")
	}

	found := false
	for k := range snap.Sessions {
		if k.Name == "alpha" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected session %q under scrubbed locale, got %d sessions: %v",
			"alpha", len(snap.Sessions), snap.Sessions)
	}
}
