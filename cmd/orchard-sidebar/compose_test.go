package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// View is a pure accessor: Update composes the frame, View hands it over.
//
// It used to lay the pane out itself, which meant the viewport offset, the
// scroll anchor, the mouse maps and the button zones were all written by a
// function bubbletea calls on its own schedule — a repaint was a state change,
// and how the pane behaved depended on how often it happened to be painted.
func TestViewHasNoSideEffects(t *testing.T) {
	m := fakeModel(t, 30, 42, 30)
	m.scrollBy(9) // somewhere in the middle of the list, not at either end
	m.compose()

	type snapshot struct {
		scroll      int
		anchorSess  string
		anchorDelta int
		cursor      int
		cursorSess  string
		snapSel     bool
		text        string
		lines       int
	}
	take := func() snapshot {
		return snapshot{m.scroll, m.anchorSess, m.anchorDelta, m.cursor, m.cursorSess,
			m.snapSel, m.pane.text, len(m.pane.lineToRow)}
	}

	before := take()
	for i := 0; i < 5; i++ {
		if got := m.View(); got != before.text {
			t.Fatalf("paint %d differs from the composed frame", i)
		}
	}
	if after := take(); after != before {
		t.Errorf("painting moved the pane's state:\n before %+v\n after  %+v", before, after)
	}
}

// ...and the frame a click is interpreted against is the one Update composed,
// with no paint in between: bubbletea is free never to call View at all (an
// unchanged frame is not repainted), and a click still has to land.
func TestUpdatePublishesTheFrameWithoutAPaint(t *testing.T) {
	m := &model{rows: rowsForHeight(4), stateDirOK: true}
	m.Update(tea.WindowSizeMsg{Width: 42, Height: 30})

	if len(m.pane.lineToRow) != 30 || len(m.pane.lineToCopy) != 30 {
		t.Fatalf("maps are %d/%d lines, want 30 — Update did not compose",
			len(m.pane.lineToRow), len(m.pane.lineToCopy))
	}
	if !m.pane.collapseZone.hit(42-3, 0) || !m.pane.launchZone.hit(42-6, 0) {
		t.Errorf("the header buttons were not published: %+v / %+v",
			m.pane.collapseZone, m.pane.launchZone)
	}
}

// The viewport follows a selection that MOVED and is off screen, and nothing
// else: a refresh that re-sorts the rows under the user, or a wheel roll away
// from the selection, must leave the list where it was pointed.
func TestOnlyAMovedSelectionPullsTheViewport(t *testing.T) {
	prev := switchClient
	switchClient = func(string, bool) {}
	defer func() { switchClient = prev }()

	m := fakeModel(t, 30, 42, 30)
	m.scrollBy(20) // scroll well away from the selected card at the top
	m.compose()
	scrolled := m.scroll
	if scrolled == 0 {
		t.Fatal("setup: the list did not scroll")
	}

	// a data refresh: same rows, re-applied. The selection has not moved.
	m.Update(hookDataMsg{bySession: map[string]hookState{}, dirOK: true})
	if m.scroll != scrolled {
		t.Errorf("a refresh yanked the viewport from %d to %d", scrolled, m.scroll)
	}

	// now actually move the selection somewhere off screen
	m.selectRow(0, false)
	m.compose()
	if m.scroll == scrolled {
		t.Errorf("the viewport stayed at %d after the selection moved off screen", m.scroll)
	}
	if !strings.Contains(ansi.Strip(m.View()), m.rows[0].session) {
		t.Errorf("the selected card is not on screen:\n%s", ansi.Strip(m.View()))
	}
}

// An attach that happened somewhere else entirely (another pane switched the
// session) moves the selection too, and the viewport follows it — that is the
// other half of the snap rule, and it arrives as a client-lane read.
func TestAnAttachElsewherePullsTheViewport(t *testing.T) {
	m := fakeModel(t, 30, 42, 30)
	m.scrollBy(20)
	m.compose()
	scrolled := m.scroll

	last := m.rows[len(m.rows)-1].session
	m.Update(clientSessMsg{name: last, gen: m.clientGen})
	if m.cursorSess != last {
		t.Fatalf("the bar did not follow the attach: %q", m.cursorSess)
	}
	if m.scroll == scrolled {
		t.Errorf("the viewport ignored an attach onto an off-screen card (still %d)", m.scroll)
	}
}

// The push lane dropping is not an outage — the sidebar degrades to polling —
// so it gets a marker in the title rather than the offline banner.
func TestDegradedPushLaneMarksTheHeader(t *testing.T) {
	m := &model{rows: rowsForHeight(2), stateDirOK: true}
	m.Update(tea.WindowSizeMsg{Width: 42, Height: 30})
	if strings.Contains(m.View(), subDownGlyph) {
		t.Fatal("the marker is drawn with the push lane healthy")
	}
	m.Update(tmuxSubMsg{err: errSubEnded})
	if !strings.Contains(m.View(), subDownGlyph) {
		t.Errorf("a dropped subscription left no mark:\n%s", ansi.Strip(m.View()))
	}
	if strings.Contains(ansi.Strip(m.View()), "DAEMON OFFLINE") {
		t.Error("a dropped subscription claimed the daemon was offline")
	}
}
