package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// fakeModel is the pane as the scroll-test environment builds it: enough rows
// that the list cannot fit, at a real pane size. The synthetic rows are
// injected the way main() resolves them — once, up front — rather than
// re-derived from the environment on every rebuild.
func fakeModel(t *testing.T, n, w, h int) *model {
	t.Helper()
	m := &model{stateDirOK: true, fakes: fakeRows(n)}
	m.appendFakes()
	sortRows(m.rows)
	if len(m.rows) != n {
		t.Fatalf("appendFakes made %d rows, want %d", len(m.rows), n)
	}
	m.cursorSess = m.rows[0].session
	mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = mm.(*model)
	// the pane is on screen before anyone scrolls it, and that first paint is
	// the one place a selection legitimately pulls the viewport
	viewOf(m)
	return m
}

// The wheel scrolls the viewport and nothing else. It must not move the cursor:
// selection is an attach, so scrolling past a session would yank the user's
// terminal into it.
func TestWheelScrollsWithoutSelecting(t *testing.T) {
	prev := switchClient
	switchClient = func(string, bool) { t.Fatal("scrolling attached a session") }
	t.Cleanup(func() { switchClient = prev })

	m := fakeModel(t, 30, 42, 40)
	first := firstListLine(viewOf(m))
	mm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	m = mm.(*model)
	viewOf(m)
	if m.cursor != 0 {
		t.Fatalf("wheel moved the cursor to %d", m.cursor)
	}
	if m.scroll != wheelStep {
		t.Fatalf("scroll = %d after one notch, want %d", m.scroll, wheelStep)
	}
	if got := firstListLine(viewOf(m)); got == first {
		t.Errorf("view did not scroll; still starts %q", got)
	}
	// scrolling up past the top stops at the top rather than going negative
	for i := 0; i < 10; i++ {
		mm, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
		m = mm.(*model)
	}
	viewOf(m)
	if m.scroll != 0 {
		t.Fatalf("scroll = %d at the top, want 0", m.scroll)
	}
	// and past the bottom it stops with the last line on screen
	for i := 0; i < 200; i++ {
		mm, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = mm.(*model)
	}
	viewOf(m)
	lines := strings.Split(viewOf(m), "\n")
	if len(lines) != 40 {
		t.Fatalf("pane is %d lines, want 40", len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "M-1-9") {
		t.Errorf("footer left the bottom of the pane: %q", lines[len(lines)-1])
	}
}

// j/k must always drag the viewport back to the selection, however far the
// wheel has wandered — otherwise pressing j appears to do nothing.
func TestKeysPullTheSelectionBackIntoView(t *testing.T) {
	prev := switchClient
	switchClient = func(string, bool) {}
	t.Cleanup(func() { switchClient = prev })

	m := fakeModel(t, 30, 42, 40)
	for i := 0; i < 40; i++ {
		mm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = mm.(*model)
	}
	viewOf(m)
	if m.scroll == 0 {
		t.Fatal("wheel did not scroll")
	}
	// walk down one row at a time; the selected card has to be on screen at
	// every step, at both ends of the list
	for i := 0; i < 25; i++ {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		m = mm.(*model)
		assertCursorVisible(t, m, i)
	}
	for i := 0; i < 25; i++ {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
		m = mm.(*model)
		assertCursorVisible(t, m, i)
	}
}

func assertCursorVisible(t *testing.T, m *model, step any) {
	t.Helper()
	view := viewOf(m)
	if !strings.Contains(view, m.rows[m.cursor].session) {
		t.Fatalf("step %v: selected %q is off screen (scroll %d)\n%s",
			step, m.rows[m.cursor].session, m.scroll, view)
	}
	for _, l := range m.pane.lineToRow {
		if l == m.cursor {
			return
		}
	}
	t.Fatalf("step %v: no rendered line maps to the selected row %d", step, m.cursor)
}

func firstListLine(view string) string {
	lines := strings.Split(view, "\n")
	for _, l := range lines[1:] {
		if s := strings.TrimSpace(ansi.Strip(l)); s != "" {
			return s
		}
	}
	return ""
}

// topCard is the session whose card the viewport currently starts on — what
// the user perceives as "where I scrolled to".
func topCard(m *model) string {
	for _, r := range m.pane.lineToRow {
		if r >= 0 && r < len(m.rows) {
			return m.rows[r].session
		}
	}
	return ""
}

// The live bug: the list re-renders every 2s, and every refresh threw the
// user's scroll position away and jumped back to the selected card. The
// scroll position is the user's; a refresh is not a reason to take it.
func TestRefreshKeepsTheScrollPosition(t *testing.T) {
	prev := switchClient
	switchClient = func(string, bool) {}
	t.Cleanup(func() { switchClient = prev })

	m := fakeModel(t, 30, 42, 40)
	for i := 0; i < 5; i++ {
		mm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = mm.(*model)
	}
	viewOf(m)
	off, top, sel := m.scroll, topCard(m), m.rows[m.cursor].session
	if off == 0 || top == "" {
		t.Fatalf("setup did not scroll: off=%d top=%q", off, top)
	}

	// a) the poll lands with nothing changed at all
	m.rebuild()
	viewOf(m)
	if m.scroll != off || topCard(m) != top {
		t.Errorf("an unchanged refresh moved the viewport: off %d->%d, top %q->%q",
			off, m.scroll, top, topCard(m))
	}

	// b) rows re-sort under the user, which is what every refresh does as
	//    activity timestamps change. The viewport follows the card it was on,
	//    not the line number that card used to occupy.
	m.rows[len(m.rows)-1].lastAct = time.Now()
	m.rows[len(m.rows)-1].state = "input" // and into another bucket entirely
	sortRows(m.rows)
	m.reanchorCursor()
	viewOf(m)
	if got := topCard(m); got != top {
		t.Errorf("a re-sort changed the card at the top of the viewport: %q -> %q", top, got)
	}
	if m.rows[m.cursor].session != sel {
		t.Errorf("a re-sort changed the selection: %q -> %q", sel, m.rows[m.cursor].session)
	}

	// c) rows disappear (sessions ended). The offset clamps to the new end of
	//    the list; it does not reset to the top.
	m.rows = m.rows[:14]
	m.reanchorCursor()
	viewOf(m)
	if m.scroll == 0 {
		t.Errorf("a shrunk list reset the scroll to the top (14 rows still overflow the pane)")
	}
	lines := strings.Split(viewOf(m), "\n")
	if !strings.Contains(lines[len(lines)-1], "M-1-9") {
		t.Errorf("footer left the bottom after the list shrank: %q", lines[len(lines)-1])
	}

	// d) and the keyboard still works afterwards
	before := m.rows[m.cursor].session
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = mm.(*model)
	if m.rows[m.cursor].session == before {
		t.Fatalf("j did not move the selection after the refreshes")
	}
	assertCursorVisible(t, m, 0)
}

// Selection is compared by session, not by cursor index: a refresh that
// re-sorts rows moves the index without the user touching anything, and that
// must not be mistaken for a selection change.
func TestReSortAloneDoesNotCountAsSelecting(t *testing.T) {
	rows := []row{
		{session: "a", state: "idle", lastAttached: time.Now().Add(-time.Hour)},
		{session: "b", state: "idle", lastAttached: time.Now().Add(-2 * time.Hour)},
	}
	m := &model{rows: rows, cursor: 0, cursorSess: "a", stateDirOK: true}
	sel := func() string {
		r, _ := m.selRow()
		return r.session
	}
	if got := sel(); got != "a" {
		t.Fatalf("the selected row is %q, want a", got)
	}
	// "b" is attached elsewhere and becomes the most recently attached, so the
	// two swap places and the cursor index follows its session from 0 to 1 —
	// a background re-sort, not a user gesture
	m.rows[1].lastAttached = time.Now()
	sortRows(m.rows)
	m.reanchorCursor()
	if m.cursor == 0 {
		t.Fatal("setup failed: the rows did not swap")
	}
	if got := sel(); got != "a" {
		t.Errorf("the selection changed to %q when only the ordering changed", got)
	}
	if m.snapSel {
		t.Error("a re-sort was recorded as a selection move; the viewport would snap")
	}
}

// Arrow keys and j/k are the same gesture said two ways: each must move the
// cursor AND take the viewport with it, identically. They used to be separate
// string cases ("j", "down") that could drift; now they share selectRow.
func TestArrowKeysMatchJK(t *testing.T) {
	arrows := fakeModel(t, 30, 40, 30)
	letters := fakeModel(t, 30, 40, 30)
	seq := []struct {
		key  tea.KeyMsg
		rune tea.KeyMsg
	}{
		{tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}},
		{tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}},
		{tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}},
		{tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}},
	}
	for i, s := range seq {
		arrows.key(s.key)
		letters.key(s.rune)
		viewOf(arrows)
		viewOf(letters)
		if arrows.cursor != letters.cursor {
			t.Fatalf("step %d: arrow cursor %d, letter cursor %d", i, arrows.cursor, letters.cursor)
		}
		if arrows.scroll != letters.scroll {
			t.Fatalf("step %d: arrow scroll %d, letter scroll %d", i, arrows.scroll, letters.scroll)
		}
	}
	if arrows.cursor == 0 {
		t.Fatal("neither key moved the cursor at all")
	}
	// and the selection is actually on screen, not just counted
	assertCursorVisible(t, arrows, "after arrow keys")
}

// Holding a key down delivers the repeats as ONE KeyRunes message with several
// runes in it. Switching on msg.String() would read "jjj", match nothing, and
// leave the list frozen under exactly the input that means "move a lot".
func TestKeyBurstMovesOncePerRune(t *testing.T) {
	m := fakeModel(t, 30, 40, 30)
	start := m.cursor
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j', 'j', 'j'}})
	if got := m.cursor - start; got != 3 {
		t.Fatalf("burst of 3 j moved %d rows, want 3", got)
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k', 'k'}})
	if got := m.cursor - start; got != 1 {
		t.Fatalf("after burst of 2 k, net move %d rows, want 1", got)
	}
	viewOf(m)
	assertCursorVisible(t, m, "after key bursts")
	// alt-modified runes belong to the outer wrapper's bindings, not to us
	before := m.cursor
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}, Alt: true})
	if m.cursor != before {
		t.Errorf("M-j moved the cursor from %d to %d", before, m.cursor)
	}
}
