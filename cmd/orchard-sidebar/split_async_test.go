package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// runCmd completes the Bubble Tea round trip a test needs now that the split's
// tmux exec is deferred to a tea.Cmd: it runs the command off the return path
// and feeds the msg it produces back through Update, so the split's model state
// (set only in Update — R13) is in place before the caller asserts.
func runCmd(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a split command, got nil")
	}
	m.Update(cmd())
}

// A refused split sets its status synchronously and returns NO command — the
// guard never reaches the exec goroutine.
func TestOpenInSplitGuardReturnsNoCmd(t *testing.T) {
	splitEnv(t)
	called := false
	prev := doSplit
	doSplit = func(outerPane, innerSocket, string) (workPaneRef, bool) { called = true; return workPaneRef{}, true }
	t.Cleanup(func() { doSplit = prev })

	m := splitModel()
	m.splitOpen = true // second split is refused
	m.alt = workPaneRef{outer: "%2", client: "/dev/ttys002"}

	if cmd := m.openInSplit("beta", false); cmd != nil {
		t.Error("a refused split returned a command")
	}
	if called {
		t.Error("doSplit ran for a refused split")
	}
	if !strings.Contains(m.statusText(), "already open") {
		t.Errorf("status = %q, want the refusal set synchronously", m.statusText())
	}
}

// The async window is guarded: a second M-Enter while the first doSplit is still
// in flight (cmd returned, splitDoneMsg not yet applied) is refused, so only one
// pane opens. splitPending clears once the result lands.
func TestSecondSplitRefusedWhilePending(t *testing.T) {
	splitEnv(t)
	calls := 0
	prev := doSplit
	doSplit = func(outerPane, innerSocket, string) (workPaneRef, bool) {
		calls++
		return workPaneRef{outer: "%2", client: "/dev/ttys002"}, true
	}
	t.Cleanup(func() { doSplit = prev })

	m := splitModel()
	cmd := m.openInSplit("beta", false)
	if cmd == nil {
		t.Fatal("the first split returned no command")
	}
	// second M-Enter before the first cmd runs — refused by splitPending
	if m.openInSplit("beta", false) != nil {
		t.Error("a second split was allowed while one was in flight")
	}

	runCmd(t, m, cmd)
	if calls != 1 {
		t.Errorf("doSplit ran %d times, want once", calls)
	}
	if m.splitPending {
		t.Error("splitPending not cleared after the result landed")
	}
	if !m.splitOpen {
		t.Error("splitOpen not set after the first split landed")
	}
}

// The error path: doSplit fails, so the splitDoneMsg carries ok=false and Update
// sets the failure status without opening the split.
func TestOpenInSplitErrorPathSetsStatus(t *testing.T) {
	splitEnv(t)
	prev := doSplit
	doSplit = func(outerPane, innerSocket, string) (workPaneRef, bool) { return workPaneRef{}, false }
	t.Cleanup(func() { doSplit = prev })

	m := splitModel()
	runCmd(t, m, m.openInSplit("beta", false))

	if m.splitOpen {
		t.Error("splitOpen set after doSplit failed")
	}
	if m.alt != (workPaneRef{}) {
		t.Errorf("alt tracked a pane after a failed split: %+v", m.alt)
	}
	if !strings.Contains(m.statusText(), "failed") {
		t.Errorf("status = %q, want the failure reason", m.statusText())
	}
}
