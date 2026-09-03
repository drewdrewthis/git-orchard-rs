package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// listBand is the scrolling middle band of a rendered pane: everything below
// the header, above the footer rule. The footer is deliberately excluded — the
// git box is a projection OF the selection, so it is supposed to change when
// the selection does. What must not change is the list under the pointer.
func listBand(view string) string {
	lines := strings.Split(view, "\n")
	end := len(lines)
	for i, l := range lines {
		if strings.HasPrefix(ansi.Strip(l), strings.Repeat("─", 10)) {
			end = i
			break
		}
	}
	var out []string
	for _, l := range lines[:end] {
		out = append(out, blankGutter(ansi.Strip(l)))
	}
	return strings.Join(out, "\n")
}

// blankGutter neutralises the pane's one-cell selection gutter: the rail, and
// the M-1..M-9 ordinal the rail covers on whichever card it lands on
// (railCell). Both are projections OF the selection, so both legitimately move
// when the selection does — everything to the right of them must not.
func blankGutter(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	for _, g := range append([]string{selBar}, jumpOrdinals[:]...) {
		if string(r[0]) == g {
			return " " + string(r[1:])
		}
	}
	return s
}

// clickLine is the pane line of a card that is not the one already selected,
// taken from the middle of the viewport so neither edge is in play.
func clickLine(t *testing.T, m *model) (y, row int) {
	t.Helper()
	for i := len(m.pane.lineToRow) / 2; i < len(m.pane.lineToRow); i++ {
		if r := m.pane.lineToRow[i]; r >= 0 && r != m.cursor {
			return i, r
		}
	}
	t.Fatalf("no mid-list card line to click in a %d-line map", len(m.pane.lineToRow))
	return -1, -1
}

// The live complaint: "clicking something shouldn't reset the position of
// everything." A click can only land on a card that is already drawn, so it
// selects and does nothing else — the viewport does not move by a single line,
// and the rendered list is byte-identical apart from the selection rail (which
// is what lets bubbletea diff the frame instead of repainting the pane).
func TestClickSelectsWithoutMovingTheViewport(t *testing.T) {
	prev := switchClient
	switchClient = func(string, bool) {}
	t.Cleanup(func() { switchClient = prev })

	m := fakeModel(t, 30, 42, 40)
	for i := 0; i < 6; i++ { // six notches down, well away from either end
		mm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = mm.(*model)
	}
	before := viewOf(m)
	off, top := m.scroll, topCard(m)
	if off == 0 || top == "" {
		t.Fatalf("setup did not scroll: off=%d top=%q", off, top)
	}

	y, want := clickLine(t, m)
	mm, _ := m.Update(tea.MouseMsg{X: 5, Y: y,
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = mm.(*model)
	after := viewOf(m)

	if m.cursor != want {
		t.Fatalf("the click selected row %d, want the row under the pointer (%d)", m.cursor, want)
	}
	if m.cursorSess != m.rows[want].session {
		t.Errorf("selection is %q, want %q", m.cursorSess, m.rows[want].session)
	}
	if m.scroll != off {
		t.Errorf("the click moved the viewport: offset %d -> %d", off, m.scroll)
	}
	if got := topCard(m); got != top {
		t.Errorf("the click changed the card at the top of the list: %q -> %q", top, got)
	}
	if b, a := listBand(before), listBand(after); b != a {
		t.Errorf("the click redrew the list beyond the selection rail:\n--- before\n%s\n--- after\n%s", b, a)
	}

	// The refresh the click itself causes: the switch lands, attach flags move,
	// and rows re-sort into other buckets. The anchor holds the same card on
	// top through all of it.
	m.rows[len(m.rows)-1].lastAct = time.Now()
	m.rows[len(m.rows)-1].state = "input"
	sortRows(m.rows)
	m.reanchorCursor()
	viewOf(m)
	if got := topCard(m); got != top {
		t.Errorf("the post-click refresh moved the top card: %q -> %q", top, got)
	}
	if m.cursorSess != m.rows[m.cursor].session {
		t.Errorf("the post-click refresh lost the selection: cursor is on %q, want %q",
			m.rows[m.cursor].session, m.cursorSess)
	}
}

// The other half of the rule, so "a click never scrolls" cannot decay into
// "nothing ever scrolls": a selection the user CANNOT see still pulls the
// viewport. Someone switching this client from another terminal arrives as a
// clientSessMsg, and the sidebar has to go and show what it landed on.
func TestOffScreenSelectionStillPullsTheViewport(t *testing.T) {
	m := fakeModel(t, 30, 42, 40)
	for i := 0; i < 20; i++ {
		mm, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = mm.(*model)
	}
	viewOf(m)
	off := m.scroll
	if off == 0 {
		t.Fatal("setup did not scroll")
	}
	// the LAST card: a different session from the one selected (or there is no
	// selection change to react to), and far below the viewport. Checked
	// against the list band alone — the footer's git box quotes the selected
	// session's own paths, so the whole view carries its name wherever the
	// viewport is pointed.
	want := m.rows[len(m.rows)-1].session
	if strings.Contains(listBand(viewOf(m)), want) {
		t.Fatalf("setup left %q on screen; nothing to pull back to", want)
	}
	if want == m.cursorSess {
		t.Fatalf("setup picked the already-selected session %q", want)
	}

	mm, _ := m.Update(clientSessMsg{name: want, gen: m.clientGen})
	m = mm.(*model)
	viewOf(m)
	if m.scroll == off {
		t.Errorf("an off-screen attach did not move the viewport (offset stayed %d)", off)
	}
	assertCursorVisible(t, m, "external attach")
}

// scrollOffset moves the viewport the MINIMUM needed to bring the selection
// back, in either direction. It used to ignore where the viewport was and
// re-derive an offset from the top of the list, so a single k onto the card
// just above the viewport jumped the whole list back to line 0.
func TestSnapScrollsTheMinimumInEitherDirection(t *testing.T) {
	lines := make([]viewLine, 40)
	for i := range lines {
		lines[i] = viewLine{row: i / 4} // 10 cards, 4 lines each
	}
	const listH = 12
	for _, tc := range []struct {
		name              string
		cursor, cur, want int
	}{
		{"already visible: no move", 4, 16, 16},
		{"one card below the viewport", 7, 16, 20}, // its last line to the bottom
		{"one card above the viewport", 3, 16, 12},
		{"far above: shows the card's top", 0, 28, 0},
		{"far below: shows the card's bottom", 9, 0, 28},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scrollOffset(lines, listH, tc.cursor, tc.cur); got != tc.want {
				t.Errorf("scrollOffset(cursor=%d, cur=%d) = %d, want %d",
					tc.cursor, tc.cur, got, tc.want)
			}
		})
	}
}

// rowOnScreen is the gate on the whole rule, so its edges are the behaviour:
// one visible line is enough, because that is all a click needs to land on.
func TestRowOnScreenCountsASingleVisibleLine(t *testing.T) {
	lines := make([]viewLine, 40)
	for i := range lines {
		lines[i] = viewLine{row: i / 4}
	}
	const listH = 12
	for _, tc := range []struct {
		name     string
		row, off int
		want     bool
	}{
		{"fully inside", 3, 12, true},
		{"last line only, at the top edge", 2, 11, true},
		{"first line only, at the bottom edge", 5, 9, true},
		{"entirely above", 1, 12, false},
		{"entirely below", 6, 8, false},
		{"no such row", 99, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowOnScreen(lines, tc.row, tc.off, listH); got != tc.want {
				t.Errorf("rowOnScreen(row=%d, off=%d) = %v, want %v",
					tc.row, tc.off, got, tc.want)
			}
		})
	}
}

// The hint line names the keys that are not visible anywhere else, and it has
// to do it inside the pane's default width — a hint the pane clips with an
// ellipsis teaches the user only the first half of it. It also names the
// bell's own on/off state, which is why it takes bell rather than being a
// constant. verify.sh finds the footer by its "M-1-9" substring, so that must
// survive any rewording.
func TestHintLineNamesTheInvisibleKeys(t *testing.T) {
	for _, bell := range []bool{false, true} {
		bellWant := "b bell·off"
		if bell {
			bellWant = "b bell·on"
		}
		line := hintLine(bell)
		for _, want := range []string{"/ filter", "M-1-9 jump", bellWant} {
			if !strings.Contains(line, want) {
				t.Errorf("hint line %q is missing %q", line, want)
			}
		}
		if strings.Contains(line, "j/k") {
			t.Errorf("hint line %q still names j/k; arrow keys cover that gesture", line)
		}
		if got := ansi.StringWidth(line); got > hintWidth {
			t.Errorf("hint line is %d cells, want <= %d: it clips at a %d-column pane",
				got, hintWidth, defaultWidth)
		}
	}
	m := fakeModel(t, 8, 42, 30)
	lines := strings.Split(viewOf(m), "\n")
	if got := ansi.Strip(lines[len(lines)-1]); !strings.Contains(got, hintLine(m.bell)) {
		t.Errorf("last pane line is %q, want it to carry %q", got, hintLine(m.bell))
	}
	// and at the default width it is on screen whole, not truncated
	narrow := fakeModel(t, 8, defaultWidth, 30)
	last := ansi.Strip(strings.Split(viewOf(narrow), "\n")[narrow.height-1])
	if !strings.Contains(last, hintLine(narrow.bell)) {
		t.Errorf("at %d columns the hint line renders as %q, want the whole of %q",
			defaultWidth, last, hintLine(narrow.bell))
	}
}

// A row arriving ABOVE the viewport while the user is parked at the top of the
// list has to appear there. Anchoring to the card that was on top pushed the
// viewport down by the new card's height to keep the old one pinned, which hid
// every newly-arrived "Needs attention" session — and its section header —
// from anyone sitting at the top, which is where the sidebar is normally left.
func TestANewTopRowAppearsWhenParkedAtTheTop(t *testing.T) {
	m := &model{
		rows: []row{
			{session: "old-attn", state: "input", hooked: true, lastAct: time.Now().Add(-time.Hour)},
			{session: "quiet", state: "shell", lastAct: time.Now().Add(-2 * time.Hour)},
		},
		stateDirOK: true, width: 42, height: 30,
	}
	m.reanchorCursor()
	viewOf(m)
	if m.scroll != 0 || topCard(m) != "old-attn" {
		t.Fatalf("setup: offset %d, top %q", m.scroll, topCard(m))
	}

	// a session needs a human, and sorts above everything already there
	m.rows = append(m.rows, row{session: "brand-new", state: "input", hooked: true, lastAct: time.Now()})
	sortRows(m.rows)
	m.reanchorCursor()
	view := viewOf(m)

	if m.scroll != 0 {
		t.Errorf("the new row pushed the viewport to offset %d", m.scroll)
	}
	if got := topCard(m); got != "brand-new" {
		t.Errorf("top card is %q, want the row that just arrived", got)
	}
	if !strings.Contains(ansi.Strip(view), "Needs attention") {
		t.Errorf("the section header was pushed off the top:\n%s", ansi.Strip(view))
	}
}
