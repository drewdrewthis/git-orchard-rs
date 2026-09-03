package main

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// captureSaves redirects the one state writer to a slice so a test can read
// back every persisted sidebarState without touching disk. Returns a pointer
// to the last-written state (nil until a write happens).
func captureSaves(t *testing.T) *[]sidebarState {
	t.Helper()
	var got []sidebarState
	prev := saveSidebarState
	saveSidebarState = func(st sidebarState) error {
		got = append(got, st)
		return nil
	}
	t.Cleanup(func() { saveSidebarState = prev })
	return &got
}

// pinModel is a bare pane of unpinned sessions with the cursor parked on one.
func pinModel(sessions ...string) *model {
	rows := make([]row, len(sessions))
	for i, s := range sessions {
		rows[i] = row{session: s, state: "idle"}
	}
	m := &model{rows: rows, width: 42, height: 30}
	m.rebuild()
	return m
}

// pinOrder is the rendered top-to-bottom order of session names after a sort.
func pinOrder(m *model) []string {
	out := make([]string, len(m.rows))
	for i, r := range m.rows {
		out[i] = r.session
	}
	return out
}

// P on the selected card pins it into the block; a pinned row carries a rank
// and sorts to the top regardless of attach recency. AC1.
func TestPPinsSelectedCard(t *testing.T) {
	captureSaves(t)
	m := pinModel("a", "b", "c")
	m.cursorSess, m.cursor = "b", 1

	m.keyRune('P')
	if !reflect.DeepEqual(m.pinned, []string{"b"}) {
		t.Fatalf("pinned = %v, want [b]", m.pinned)
	}
	if m.rows[0].session != "b" || m.rows[0].pinRank != 1 {
		t.Fatalf("b did not rise to the top of the block: order=%v rank=%d",
			pinOrder(m), m.rows[0].pinRank)
	}

	// P again unpins it and it returns to the flat order.
	m.keyRune('P')
	if len(m.pinned) != 0 {
		t.Fatalf("pinned = %v, want empty after unpin", m.pinned)
	}
	if m.rows[0].pinRank != 0 {
		t.Errorf("unpinned row still carries rank %d", m.rows[0].pinRank)
	}
}

// A second pin appends below the first, and the block stays in pin order even
// though neither was ever attached. AC1, AC3 (activity-stable order).
func TestPinBlockKeepsPinOrder(t *testing.T) {
	captureSaves(t)
	m := pinModel("a", "b", "c")
	m.togglePin("c")
	m.togglePin("a")
	if want := []string{"c", "a", "b"}; !reflect.DeepEqual(pinOrder(m), want) {
		t.Fatalf("order = %v, want %v (pins first in pin order, then the rest)", pinOrder(m), want)
	}
}

// Pinned rows do not move when an unpinned session becomes most-recently
// attached. AC3.
func TestPinnedRowsAreActivityStable(t *testing.T) {
	captureSaves(t)
	m := pinModel("a", "b", "c")
	m.togglePin("a")
	m.togglePin("b")
	// c gets attached: without pins it would jump to the top. With a and b
	// pinned it must stay below them.
	m.promote("c")
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(pinOrder(m), want) {
		t.Errorf("order = %v, want %v — an attach reordered the pinned block", pinOrder(m), want)
	}
}

// M-Shift-Up/Down swap within the block; both clamp at the ends and are inert
// on an unpinned selection. AC4.
func TestReorderPinWithinBlock(t *testing.T) {
	captureSaves(t)
	m := pinModel("a", "b", "c", "d")
	m.togglePin("a")
	m.togglePin("b")
	m.togglePin("c") // block: a, b, c

	m.reorderPin("c", -1) // c up
	if want := []string{"a", "c", "b"}; !reflect.DeepEqual(m.pinned, want) {
		t.Fatalf("after c up: pinned = %v, want %v", m.pinned, want)
	}
	m.reorderPin("a", -1) // top card up: no-op
	if want := []string{"a", "c", "b"}; !reflect.DeepEqual(m.pinned, want) {
		t.Errorf("reorder at the top edge moved the block: %v", m.pinned)
	}
	m.reorderPin("b", 1) // bottom card down: no-op
	if want := []string{"a", "c", "b"}; !reflect.DeepEqual(m.pinned, want) {
		t.Errorf("reorder at the bottom edge moved the block: %v", m.pinned)
	}
	m.reorderPin("d", -1) // d is unpinned: no-op
	if want := []string{"a", "c", "b"}; !reflect.DeepEqual(m.pinned, want) {
		t.Errorf("reorder on an unpinned selection changed the block: %v", m.pinned)
	}
}

// The Shift-arrow chords reach reorder through key(), keyed to the selected
// card. AC4 (the input path outer.conf forwards).
func TestShiftArrowReordersSelectedPin(t *testing.T) {
	captureSaves(t)
	m := pinModel("a", "b")
	m.togglePin("a")
	m.togglePin("b") // block: a, b
	m.cursorSess = "b"
	m.reanchorCursor()
	m.key(tea.KeyMsg{Type: tea.KeyShiftUp})
	if want := []string{"b", "a"}; !reflect.DeepEqual(m.pinned, want) {
		t.Errorf("M-Shift-Up on b: pinned = %v, want %v", m.pinned, want)
	}
}

// The block draws above the flat list with a separator that maps to no row;
// the separator sits between the last pinned card and the first unpinned one.
// AC3.
func TestSeparatorSitsUnderTheBlock(t *testing.T) {
	captureSaves(t)
	m := pinModel("a", "b", "c")
	m.togglePin("a")
	lines := m.cards(42, false)
	sepAt := -1
	for i, l := range lines {
		if l.sep {
			sepAt = i
		}
	}
	if sepAt < 0 {
		t.Fatal("no separator drawn with a pinned block present")
	}
	// every line above the separator maps to the pinned row; the first line
	// below maps to an unpinned one.
	if r := lines[sepAt-1].row; m.rows[r].pinRank == 0 {
		t.Errorf("line above the separator is not pinned")
	}
	if r := lines[sepAt+1].row; m.rows[r].pinRank != 0 {
		t.Errorf("line below the separator is still pinned")
	}
}

// A filter narrows the block; the separator survives with a matching pinned
// run and is dropped when the filter hides every pinned row. AC12.
func TestFilterNarrowsBlockAndSeparator(t *testing.T) {
	captureSaves(t)
	m := pinModel("alpha", "beta", "gamma")
	m.togglePin("alpha")
	m.togglePin("beta") // block: alpha, beta; flat: gamma

	// "a" matches all three: a pinned run (alpha, beta) with an unpinned
	// survivor (gamma) below it, so the separator is drawn.
	m.filter.field = newTextField("a", filterFieldWidth)
	if !hasSeparator(m.cards(42, false)) {
		t.Errorf("separator missing with a surviving pinned run and a flat list")
	}

	// "lph" matches only alpha: a pinned survivor but no unpinned row, so no
	// dangling separator is drawn.
	m.filter.field = newTextField("lph", filterFieldWidth)
	if hasSeparator(m.cards(42, false)) {
		t.Errorf("separator drawn with no unpinned row surviving the filter")
	}

	// "gamma" hides every pinned card: no block, no separator (AC12).
	m.filter.field = newTextField("gamma", filterFieldWidth)
	if hasSeparator(m.cards(42, false)) {
		t.Errorf("separator drawn when the filter excluded every pinned row")
	}
}

func hasSeparator(lines []viewLine) bool {
	for _, l := range lines {
		if l.sep {
			return true
		}
	}
	return false
}

// M-1..9 ordinals are assigned pinned-first in visible order, and M-1 selects
// the first pinned card. AC10.
func TestJumpCountsPinnedFirst(t *testing.T) {
	captureSaves(t)
	var switched []string
	prev := switchClient
	switchClient = func(s string, _ bool) { switched = append(switched, s) }
	t.Cleanup(func() { switchClient = prev })

	m := pinModel("a", "b", "c")
	m.togglePin("c") // block: c, then a, b
	vis := m.visibleRows()
	if m.rows[vis[0]].session != "c" {
		t.Fatalf("first visible card = %q, want the pinned c", m.rows[vis[0]].session)
	}
	m.jumpTo(1)
	if len(switched) == 0 || switched[len(switched)-1] != "c" {
		t.Errorf("M-1 selected %v, want the pinned card c", switched)
	}
}

// The right-click menu offers Pin on an unpinned row and Unpin on a pinned
// one; activating it toggles the pin without attaching or moving the
// selection. AC9.
func TestMenuPinUnpinByState(t *testing.T) {
	newMenuSpy(t)
	captureSaves(t)
	m := realModel()

	m.menu = rowMenu{mode: menuActions, sess: "alpha"}
	if got := m.menuActionLabels()[itemPin]; got != "Pin" {
		t.Errorf("unpinned row menu offers %q, want Pin", got)
	}
	sel := m.cursorSess
	m.menu.item = itemPin
	m.activate()
	if !m.isPinned("alpha") {
		t.Fatalf("activating Pin did not pin alpha: %v", m.pinned)
	}
	if m.cursorSess != sel {
		t.Errorf("Pin moved the selection: %q -> %q", sel, m.cursorSess)
	}

	m.menu = rowMenu{mode: menuActions, sess: "alpha"}
	if got := m.menuActionLabels()[itemPin]; got != "Unpin" {
		t.Errorf("pinned row menu offers %q, want Unpin", got)
	}
}

// A whole-word helper the filter test above leans on: the separator glyph is
// dim and never blank, so the eye reads a real divider.
func TestSeparatorGlyphIsVisible(t *testing.T) {
	if s := pinSeparator(42); strings.TrimSpace(ansi.Strip(s)) == "" {
		t.Error("the pinned separator rendered blank")
	}
}
