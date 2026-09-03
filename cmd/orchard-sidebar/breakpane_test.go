package main

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// breakSpy stands in for every tmux surface the break-pane flow reaches, and
// records the argv of each command in order — the undo guard is an ORDERING
// property (kill the empty session AFTER break-pane fails), which a per-call
// flag cannot express. failOn injects a failure on the named tmux verb.
type breakSpy struct {
	tmux     []string
	switched []string
}

func newBreakSpy(t *testing.T, failOn string) *breakSpy {
	t.Helper()
	s := &breakSpy{}
	rt, sc, ts, ss, pi := runTmux, switchClientTo, takenSessions, saveSidebarState, paneInfo
	runTmux = func(args ...string) error {
		s.tmux = append(s.tmux, strings.Join(args, " "))
		if failOn != "" && args[0] == failOn {
			return errors.New(failOn + " boom")
		}
		return nil
	}
	switchClientTo = func(name string) error { s.switched = append(s.switched, name); return nil }
	takenSessions = func() map[string]bool { return map[string]bool{} }
	saveSidebarState = func(sidebarState) error { return nil }
	paneInfo = func(string) (string, int) { return "%9", 2 }
	t.Cleanup(func() {
		runTmux, switchClientTo, takenSessions, saveSidebarState, paneInfo = rt, sc, ts, ss, pi
	})
	return s
}

// openBreak opens the row menu on rowIdx and steps into the name prompt, the
// way clicking the item then does.
func openBreak(m *model, rowIdx int) {
	m.openRowMenu(rowIdx, 0)
	m.menu.item = itemBreakPane
	m.activate()
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// The item is offered only where a pane can actually be broken out: on the
// ATTACHED session, and only when its active window has a pane to spare.
// AC: shown at ≥2 panes on the attached card; absent at one pane; absent on a
// non-attached card.
func TestBreakPaneItemGating(t *testing.T) {
	cases := []struct {
		name   string
		rowIdx int // 0=alpha (attached), 1=beta (not attached)
		panes  int
		want   bool
	}{
		{"attached multi-pane offers it", 0, 2, true},
		{"attached single-pane hides it", 0, 1, false},
		{"non-attached multi-pane hides it", 1, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			newMenuSpy(t)
			paneInfo = func(string) (string, int) { return "%9", c.panes }
			m := realModel()
			m.openRowMenu(c.rowIdx, 0)
			if got := m.menu.canBreakPane; got != c.want {
				t.Fatalf("canBreakPane = %v, want %v", got, c.want)
			}
			if got := hasLabel(m.menuActionLabels(), breakPaneLabel); got != c.want {
				t.Errorf("label present = %v, want %v (labels %v)",
					got, c.want, m.menuActionLabels())
			}
			// Rename is always available regardless of the break item.
			if !hasLabel(m.menuActionLabels(), "Rename") {
				t.Error("Rename went missing")
			}
		})
	}
}

// Esc on the name prompt backs out before anything runs: a cancelled break-out
// must leave zero tmux mutations behind. AC: escape cancels with no mutations.
func TestBreakPaneEscCancelsWithoutTmux(t *testing.T) {
	spy := newBreakSpy(t, "")
	m := realModel()
	openBreak(m, 0)
	if m.menu.mode != menuBreakPane {
		t.Fatalf("did not enter the name prompt: mode %v", m.menu.mode)
	}
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if m.menuOpen() {
		t.Error("esc left the menu open")
	}
	if len(spy.tmux) != 0 || len(spy.switched) != 0 {
		t.Errorf("esc ran tmux: cmds=%v switched=%v", spy.tmux, spy.switched)
	}
}

// A confirmed break-out runs exactly new-session → break-pane → kill-window
// against the active pane, pins the new session to the top of the list, and
// switches the inner client onto it. AC: command sequence, row 0, scoped
// switch-client.
func TestBreakPaneConfirmRunsSequenceAndPromotes(t *testing.T) {
	spy := newBreakSpy(t, "")
	m := realModel()
	openBreak(m, 0) // input prefills with "alpha"
	m.key(tea.KeyMsg{Type: tea.KeyEnter})

	want := []string{
		"new-session -d -s alpha",
		"break-pane -d -s %9 -t alpha:",
		"kill-window -t alpha:0",
	}
	if strings.Join(spy.tmux, " | ") != strings.Join(want, " | ") {
		t.Fatalf("tmux calls were %v, want %v", spy.tmux, want)
	}
	if len(m.pinned) == 0 || m.pinned[0] != "alpha" {
		t.Errorf("new session not promoted to row 0: pinned=%v", m.pinned)
	}
	if len(spy.switched) != 1 || spy.switched[0] != "alpha" {
		t.Errorf("switch-client not aimed at the new session: %v", spy.switched)
	}
	if m.menuOpen() {
		t.Error("a successful break-out left the menu open")
	}
}

// A name that collides with a live session is made unique the same way a launch
// is, and the resolved name is what every tmux step uses. AC: colliding name →
// uniqueName suffix.
func TestBreakPaneCollidingNameIsMadeUnique(t *testing.T) {
	spy := newBreakSpy(t, "")
	takenSessions = func() map[string]bool { return map[string]bool{"alpha": true} }
	m := realModel()
	openBreak(m, 0)
	m.key(tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.pinned) == 0 || m.pinned[0] != "alpha-2" {
		t.Fatalf("collision not resolved: pinned=%v", m.pinned)
	}
	if spy.tmux[0] != "new-session -d -s alpha-2" ||
		spy.tmux[1] != "break-pane -d -s %9 -t alpha-2:" {
		t.Errorf("tmux steps did not use the unique name: %v", spy.tmux)
	}
}

// break-pane failing must undo the empty session step one created and surface
// the failing step in the status line — no silent half-state, no stray
// windowless session. AC: kill-session on break-pane failure; status text.
func TestBreakPaneFailureUndoesEmptySession(t *testing.T) {
	spy := newBreakSpy(t, "break-pane")
	m := realModel()
	openBreak(m, 0)
	m.key(tea.KeyMsg{Type: tea.KeyEnter})

	want := []string{
		"new-session -d -s alpha",
		"break-pane -d -s %9 -t alpha:",
		"kill-session -t alpha",
	}
	if strings.Join(spy.tmux, " | ") != strings.Join(want, " | ") {
		t.Fatalf("undo did not fire: tmux calls %v, want %v", spy.tmux, want)
	}
	if !strings.HasPrefix(m.menu.notice, "pane → session failed: break-pane") {
		t.Errorf("notice = %q, want it to name the failing step", m.menu.notice)
	}
	if m.menu.mode != menuBreakPane {
		t.Errorf("a failed break-out closed the menu: mode %v", m.menu.mode)
	}
	if len(spy.switched) != 0 {
		t.Errorf("switched after a failed break-out: %v", spy.switched)
	}
	if len(m.pinned) != 0 {
		t.Errorf("a failed break-out still pinned: %v", m.pinned)
	}
}
