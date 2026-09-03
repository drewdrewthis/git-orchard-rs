package main

// Regression tests for the review finding on PR #745
// (discussion_r3918434325): fetchClientSession counted tmux list-clients
// lines with strings.Split(strings.TrimSpace(out), "\n"), which returns
// []string{""} -- a one-element slice, not an empty one -- for empty input.
// A genuinely empty `tmux list-clients` read (nobody attached anywhere)
// therefore reported clients == 1, silently defeating the m.clients == 0
// detached-resize branch from #736 in exactly the all-detached case it
// exists to catch.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestParseListClients(t *testing.T) {
	cases := []struct {
		name        string
		out         string
		wantClients int
		wantSession string
		wantWidth   int
	}{
		{"empty output has zero clients", "", 0, "", 0},
		{"whitespace-only output has zero clients", "   \n", 0, "", 0},
		{"one line", "100 42 sess-a", 1, "sess-a", 42},
		{"multiple lines picks most recently active session", "100 42 sess-a\n200 50 sess-b", 2, "sess-b", 50},
		{"trailing newline does not inflate the count", "100 42 sess-a\n", 1, "sess-a", 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clients, session, width := parseListClients([]byte(c.out))
			if clients != c.wantClients {
				t.Errorf("clients = %d, want %d", clients, c.wantClients)
			}
			if session != c.wantSession {
				t.Errorf("session = %q, want %q", session, c.wantSession)
			}
			if width != c.wantWidth {
				t.Errorf("width = %d, want %d", width, c.wantWidth)
			}
		})
	}
}

// The miscount above mattered specifically because it fed straight into the
// m.clients == 0 detached-resize branch (#736). This proves that path
// end-to-end through the real parser on a real empty read, rather than by
// setting m.clients directly the way TestDetachedResizeReassertsDesired does.
func TestDetachedBranchTriggersFromParsedEmptyClientRead(t *testing.T) {
	var wrote []int
	var resized []int
	origSet := setWidthOption
	setWidthOption = func(w int) { wrote = append(wrote, w) }
	origRes := resizePane
	resizePane = func(w int) { resized = append(resized, w) }
	defer func() { setWidthOption = origSet; resizePane = origRes }()

	// Stand-in for a genuinely empty `tmux list-clients` Output() -- the
	// all-detached case the review flagged.
	clients, session, width := parseListClients([]byte(""))

	m := &model{width: 42, desiredWidth: 42} // armed; parser says nobody attached
	m.Update(clientSessMsg{name: session, clients: clients, width: width})
	if m.clients != 0 {
		t.Fatalf("m.clients = %d, want 0 from an empty tmux read", m.clients)
	}

	m.Update(tea.WindowSizeMsg{Width: 86, Height: 50})
	if len(wrote) != 0 {
		t.Errorf("detached resize published to the shared option: %v", wrote)
	}
	if len(resized) != 1 || resized[0] != 42 {
		t.Fatalf("detached redistribution not reasserted: %v", resized)
	}
}
