package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// press/motion/release as the outer conf forwards them once MouseDrag1Pane is
// left unbound. A press starts a potential drag; the release decides.
func press(m *model, y int) {
	m.mouse(tea.MouseMsg{X: 1, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
}
func motion(m *model, y int) {
	m.mouse(tea.MouseMsg{X: 1, Y: y, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
}
func mrelease(m *model, y int) {
	m.mouse(tea.MouseMsg{X: 1, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
}

// lineFor returns a screen line that maps to a pinned (or unpinned) card.
func lineFor(m *model, pinned bool) (y int, ok bool) {
	for i, r := range m.pane.lineToRow {
		if r < 0 {
			continue
		}
		if (m.rows[r].pinRank > 0) == pinned {
			return i, true
		}
	}
	return 0, false
}

// A press and release with no motion is an ordinary click: it attaches the
// card and never pins. AC2 (no-motion path).
func TestPressReleaseNoMotionAttaches(t *testing.T) {
	captureSaves(t)
	var switched []string
	prev := switchClient
	switchClient = func(s string, _ bool) { switched = append(switched, s) }
	t.Cleanup(func() { switchClient = prev })

	m := pinModel("a", "b", "c")
	viewOf(m)
	y, ok := lineFor(m, false)
	if !ok {
		t.Fatal("no card line rendered")
	}
	target := m.rows[m.pane.lineToRow[y]].session

	press(m, y)
	mrelease(m, y) // same line, no motion
	if len(m.pinned) != 0 {
		t.Errorf("a click pinned a card: %v", m.pinned)
	}
	if len(switched) == 0 || switched[len(switched)-1] != target {
		t.Errorf("a click did not attach %q: %v", target, switched)
	}
}

// Dragging an unpinned card up across the separator into the block pins it;
// dragging a pinned card down below the separator unpins it. AC2.
func TestDragPinsAndUnpins(t *testing.T) {
	captureSaves(t)
	prev := switchClient
	switchClient = func(string, bool) {}
	t.Cleanup(func() { switchClient = prev })

	m := pinModel("a", "b", "c")
	m.togglePin("a") // a is the block; b, c below the separator
	viewOf(m)

	sep := m.pane.pinSep
	if sep < 0 {
		t.Fatal("no separator on screen with a pin present")
	}
	flatY, ok := lineFor(m, false)
	if !ok || flatY <= sep {
		t.Fatalf("no unpinned card line below the separator (sep=%d, flatY=%d)", sep, flatY)
	}
	dragged := m.rows[m.pane.lineToRow[flatY]].session

	// press an unpinned card, move up, release above the separator and at least
	// the drag threshold away from the press: pin.
	press(m, flatY)
	motion(m, sep)
	mrelease(m, flatY-dragThreshold)
	if !m.isPinned(dragged) {
		t.Fatalf("dragging %q into the block did not pin it: %v", dragged, m.pinned)
	}

	// now drag a pinned card down below the separator: unpin.
	viewOf(m)
	sep = m.pane.pinSep
	pinY, ok := lineFor(m, true)
	if !ok {
		t.Fatal("no pinned card line after the pin")
	}
	pinned := m.rows[m.pane.lineToRow[pinY]].session
	press(m, pinY)
	below := sep + dragThreshold
	motion(m, below)
	mrelease(m, below)
	if m.isPinned(pinned) {
		t.Errorf("dragging %q below the separator did not unpin it: %v", pinned, m.pinned)
	}
}

// A press promotes the attached card toward the top (selectRow re-sorts), so a
// release a line or two off the press must read as that click, not a silent pin.
// Only travel of at least the threshold, across the boundary, changes a pin.
// Regression for issue #775 (a near-click left a stray pin).
func TestNearClickDoesNotPin(t *testing.T) {
	captureSaves(t)
	prev := switchClient
	switchClient = func(string, bool) {}
	t.Cleanup(func() { switchClient = prev })

	// first rendered card line at or below minY, with its session name.
	cardLine := func(m *model, minY int) (int, string) {
		for y, r := range m.pane.lineToRow {
			if r >= 0 && y >= minY {
				return y, m.rows[r].session
			}
		}
		return -1, ""
	}

	// pin direction — flat list, no separator yet.
	m := pinModel("a", "b", "c")
	viewOf(m)
	fc := m.pane.firstCardLine()
	pressY, _ := cardLine(m, fc+dragThreshold)
	if pressY < 0 {
		t.Fatal("no card line far enough below the top to drag up")
	}
	// single motion + release one line above the press: sub-threshold click.
	press(m, pressY)
	motion(m, pressY-1)
	mrelease(m, pressY-1)
	if len(m.pinned) != 0 {
		t.Fatalf("a near-click pinned a card: %v", m.pinned)
	}
	// the same card dragged all the way up to the top of the list does pin.
	m = pinModel("a", "b", "c")
	viewOf(m)
	fc = m.pane.firstCardLine()
	pressY, sess := cardLine(m, fc+dragThreshold)
	press(m, pressY)
	motion(m, fc)
	mrelease(m, fc)
	if !m.isPinned(sess) {
		t.Fatalf("a real drag up did not pin %q: %v", sess, m.pinned)
	}

	// unpin direction — a pinned card with the separator on screen.
	m = pinModel("a", "b", "c")
	m.togglePin("a")
	viewOf(m)
	pinY, pinnedSess := cardLine(m, 0)
	// near-click one line below the press stays pinned.
	press(m, pinY)
	motion(m, pinY+1)
	mrelease(m, pinY+1)
	if !m.isPinned(pinnedSess) {
		t.Fatalf("a near-click unpinned %q: %v", pinnedSess, m.pinned)
	}
	// a real drag below the separator unpins.
	sep := m.pane.pinSep
	press(m, pinY)
	motion(m, sep+dragThreshold)
	mrelease(m, sep+dragThreshold)
	if m.isPinned(pinnedSess) {
		t.Fatalf("a real drag below the separator did not unpin %q: %v", pinnedSess, m.pinned)
	}
}
