package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// splitEnv sets up the split test environment with outer/inner panes. The
// focus override now lives on the model (m.workOverride), so there is no
// package global to reset between tests.
func splitEnv(t *testing.T) {
	t.Helper()
	setTmuxEnv(t, tmuxEnv{inner: "default", client: "/dev/ttys001", outer: "%1", self: "%0"})
}

// splitModel returns a two-row pane with selection on an unattached session.
func splitModel() *model {
	m := &model{
		rows: []row{
			{session: "alpha", state: "idle", hooked: true},
			{session: "beta", state: "idle", hooked: true},
		},
		attachedBySess: map[string]bool{"alpha": true},
		cursor:         1,
		cursorSess:     "beta",
		width:          42,
		height:         30,
	}
	return m
}

// The exact tmux each half of the gesture emits (split, layout pin, detach).
func TestSplitCommandArgs(t *testing.T) {
	// innerAttachCmd shell-quotes the socket and session via shellQuote, so both
	// are wrapped even when ordinary — safe through the sh split-window runs.
	t.Run("innerAttachCmd quotes socket and session", func(t *testing.T) {
		if got, want := innerAttachCmd("default", "beta"), "TMUX= tmux -L 'default' attach -t 'beta'"; got != want {
			t.Errorf("innerAttachCmd = %q, want %q", got, want)
		}
	})
	t.Run("splitWindowArgs opens beside and prints id+tty", func(t *testing.T) {
		got := splitWindowArgs("%1", "default", "beta")
		want := []string{"split-window", "-h", "-t", "%1", "-P", "-F",
			"#{pane_id} #{pane_tty}", "TMUX= tmux -L 'default' attach -t 'beta'"}
		if !equalStrings(got, want) {
			t.Errorf("splitWindowArgs = %v\nwant %v", got, want)
		}
	})
	t.Run("mainVerticalArgs pins the sidebar layout", func(t *testing.T) {
		if got := mainVerticalArgs(); !equalStrings(got, []string{"select-layout", "main-vertical"}) {
			t.Errorf("mainVerticalArgs = %v", got)
		}
	})
	t.Run("detachClientArgs detaches by tty", func(t *testing.T) {
		if got := detachClientArgs("/dev/ttys002"); !equalStrings(got, []string{"detach-client", "-t", "/dev/ttys002"}) {
			t.Errorf("detachClientArgs = %v", got)
		}
	})
	t.Run("a spaced session stays one quoted argument", func(t *testing.T) {
		if got := innerAttachCmd("default", "my session"); !strings.Contains(got, "attach -t 'my session'") {
			t.Errorf("innerAttachCmd did not quote a spaced session: %q", got)
		}
	})
}
func TestSplitBlocked(t *testing.T) {
	cases := []struct {
		name     string
		session  string
		fake     bool
		attached map[string]bool
		want     bool // want a refusal
	}{
		{"open an idle session", "beta", false, map[string]bool{"alpha": true}, false},
		{"already attached is refused", "alpha", false, map[string]bool{"alpha": true}, true},
		{"synthetic row is refused", "fake-01", true, nil, true},
		{"no selection is a silent no-op", "", false, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := splitBlocked(c.session, c.fake, c.attached) != ""; got != c.want {
				t.Errorf("splitBlocked refusal = %v, want %v", got, c.want)
			}
		})
	}
}

// Open in split emits the split beside the focused pane and tracks the new one.
func TestOpenInSplitEmitsSplitAndTracksPane(t *testing.T) {
	splitEnv(t)
	var gotBeside outerPane
	var gotInner innerSocket
	var gotSession string
	prev := doSplit
	doSplit = func(beside outerPane, inner innerSocket, session string) (workPaneRef, bool) {
		gotBeside, gotInner, gotSession = beside, inner, session
		return workPaneRef{outer: "%2", client: "/dev/ttys002"}, true
	}
	t.Cleanup(func() { doSplit = prev })

	m := splitModel()
	m.splitSelected()

	if gotBeside != "%1" || gotInner != "default" || gotSession != "beta" {
		t.Errorf("doSplit args = (%q,%q,%q), want (%%1, default, beta)", gotBeside, gotInner, gotSession)
	}
	if !m.splitOpen {
		t.Error("splitOpen not set after a successful split")
	}
	if m.alt.outer != "%2" || m.alt.client != "/dev/ttys002" {
		t.Errorf("alt work pane = %+v, want {%%2 /dev/ttys002}", m.alt)
	}
	if m.statusText() != "" {
		t.Errorf("a successful split left a status: %q", m.statusText())
	}
}

// The reusable entry point opens a split for an EXPLICIT session (#776 menu path).
func TestOpenInSplitTakesAnExplicitSession(t *testing.T) {
	splitEnv(t)
	var gotSession string
	prev := doSplit
	doSplit = func(_ outerPane, _ innerSocket, session string) (workPaneRef, bool) {
		gotSession = session
		return workPaneRef{outer: "%2", client: "/dev/ttys002"}, true
	}
	t.Cleanup(func() { doSplit = prev })

	m := splitModel()
	m.openInSplit("gamma", false) // a card that is neither selected nor attached

	if gotSession != "gamma" || !m.splitOpen {
		t.Errorf("openInSplit did not act on the given session: got %q open=%v", gotSession, m.splitOpen)
	}
}

// Opening split on attached session is refused (#777 keep-it-simple).
func TestOpenInSplitRefusesAttachedSession(t *testing.T) {
	splitEnv(t)
	called := false
	prev := doSplit
	doSplit = func(outerPane, innerSocket, string) (workPaneRef, bool) { called = true; return workPaneRef{}, true }
	t.Cleanup(func() { doSplit = prev })

	m := splitModel()
	m.cursor, m.cursorSess = 0, "alpha" // alpha is attached
	m.splitSelected()

	if called {
		t.Error("a second client was opened on an already-attached session")
	}
	if m.splitOpen {
		t.Error("splitOpen set after a refused split")
	}
	if !strings.Contains(m.statusText(), "already attached") {
		t.Errorf("status = %q, want the refusal reason", m.statusText())
	}
}

// A second split while one is open is refused — would orphan the tracked pane.
func TestSecondSplitRefused(t *testing.T) {
	splitEnv(t)
	called := false
	prev := doSplit
	doSplit = func(outerPane, innerSocket, string) (workPaneRef, bool) { called = true; return workPaneRef{}, true }
	t.Cleanup(func() { doSplit = prev })

	m := splitModel()
	m.splitOpen = true
	m.alt = workPaneRef{outer: "%2", client: "/dev/ttys002"}
	m.openInSplit("beta", false)

	if called {
		t.Error("a second split was opened while one was already open")
	}
	if m.alt.outer != "%2" {
		t.Error("the tracked split pane was overwritten")
	}
}

// The two chords route through key(), same entry as forwarded M-Enter / M-w.
func TestSplitChordsRoute(t *testing.T) {
	splitEnv(t)
	opened, detached := false, false
	pd, pc := doSplit, detachClient
	doSplit = func(outerPane, innerSocket, string) (workPaneRef, bool) {
		opened = true
		return workPaneRef{outer: "%2", client: "/dev/ttys002"}, true
	}
	detachClient = func(clientTTY) { detached = true }
	t.Cleanup(func() { doSplit, detachClient = pd, pc })

	m := splitModel()
	m.key(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if !opened {
		t.Error("M-Enter did not open a split")
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}, Alt: true})
	if !detached {
		t.Error("M-w did not detach the split")
	}
}

// Refusal is drawn on footer's hint line so user sees why.
func TestStatusShownInFooter(t *testing.T) {
	splitEnv(t)
	m := splitModel()
	m.detachSplit() // refused on the sole pane
	if !strings.Contains(ansi.Strip(viewOf(m)), "sole work pane") {
		t.Errorf("the refusal was not drawn:\n%s", ansi.Strip(viewOf(m)))
	}
}
