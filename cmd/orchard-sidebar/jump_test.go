package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// M-1..M-9. The index math is over the VISIBLE list, which is the whole point:
// with a filter on, the third card on screen is the third card the chord goes
// to, and the ninth chord over four matches goes nowhere at all.

// altKey is the chord as bubbletea delivers it once outer.conf has forwarded
// it into this pane: an alt-modified rune, not a named key.
func altKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true}
}

func TestJumpDigitReadsOnlyOneToNine(t *testing.T) {
	for _, tc := range []struct {
		r    rune
		n    int
		want bool
	}{
		{'1', 1, true},
		{'9', 9, true},
		{'0', 0, false}, // M-0 is not a tenth slot; it stays the inner session's
		{'a', 0, false},
		{'-', 0, false},
	} {
		n, ok := jumpDigit(tc.r)
		if ok != tc.want || n != tc.n {
			t.Errorf("jumpDigit(%q) = %d/%v, want %d/%v", tc.r, n, ok, tc.n, tc.want)
		}
	}
}

// The nth chord selects the nth VISIBLE row and hands focus back — a jump is a
// finished gesture, like a click, not a browse step like j/k.
func TestJumpSelectsTheNthVisibleRow(t *testing.T) {
	var handBacks []bool
	prev := switchClient
	switchClient = func(_ string, hb bool) { handBacks = append(handBacks, hb) }
	t.Cleanup(func() { switchClient = prev })

	m := &model{rows: []row{
		{session: "a"}, {session: "b"}, {session: "c"}, {session: "d"},
	}}
	m.key(altKey('3'))
	if m.cursorSess != "c" {
		t.Errorf("M-3 selected %q, want \"c\"", m.cursorSess)
	}
	if len(handBacks) != 1 || !handBacks[0] {
		t.Errorf("M-3 hand-backs = %v, want exactly one true", handBacks)
	}

	// past the end of the list: no switch, no error, selection unchanged
	m.key(altKey('9'))
	if m.cursorSess != "c" {
		t.Errorf("M-9 over 4 rows moved the selection to %q", m.cursorSess)
	}
	if len(handBacks) != 1 {
		t.Errorf("M-9 over 4 rows switched a session: %v", handBacks)
	}
}

// With a filter on, M-2 is the second FILTERED card. Counting into m.rows
// instead would attach a session the user cannot even see.
func TestJumpCountsFilteredCards(t *testing.T) {
	var got []string
	prev := switchClient
	switchClient = func(s string, _ bool) { got = append(got, s) }
	t.Cleanup(func() { switchClient = prev })

	m := &model{rows: []row{
		{session: "alpha"}, {session: "pay-1"}, {session: "beta"},
		{session: "pay-2"}, {session: "pay-3"},
	}}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeInto(m, "pay")
	m.key(altKey('2'))
	if m.cursorSess != "pay-2" {
		t.Errorf("M-2 with \"pay\" filtered selected %q, want \"pay-2\"", m.cursorSess)
	}
	// and the 4th filtered slot does not exist even though row 4 does
	m.key(altKey('4'))
	if m.cursorSess != "pay-2" {
		t.Errorf("M-4 over 3 matches walked into a hidden row (%q)", m.cursorSess)
	}
	if len(got) != 1 || got[0] != "pay-2" {
		t.Errorf("switches = %v, want one to pay-2", got)
	}
}

// Every other alt chord is the wrapper's: M-s collapses the sidebar, M-p opens
// a popup, M-Left/M-Right move focus, and none of them may reach the list.
func TestJumpLeavesOtherAltChordsAlone(t *testing.T) {
	prev := switchClient
	switchClient = func(s string, _ bool) { t.Fatalf("an alt chord attached %q", s) }
	t.Cleanup(func() { switchClient = prev })

	m := &model{rows: []row{{session: "a"}, {session: "b"}}, cursor: 0, cursorSess: "a"}
	for _, r := range []rune{'s', 'p', 'd', 'j', '0'} {
		m.key(altKey(r))
	}
	if m.cursor != 0 || m.cursorSess != "a" {
		t.Errorf("an alt chord moved the selection to %d/%q", m.cursor, m.cursorSess)
	}
}

// The ordinals mark exactly the cards the chords reach: the first nine visible
// ones, one cell each in the selection gutter so no card shifts to make room.
func TestJumpOrdinalMarksTheFirstNineCards(t *testing.T) {
	if got := jumpOrdinal(0); got != "¹" {
		t.Errorf("jumpOrdinal(0) = %q, want ¹", got)
	}
	if got := jumpOrdinal(maxJump - 1); got != "⁹" {
		t.Errorf("jumpOrdinal(%d) = %q, want ⁹", maxJump-1, got)
	}
	for _, n := range []int{-1, maxJump, maxJump + 5} {
		if got := jumpOrdinal(n); got != "" {
			t.Errorf("jumpOrdinal(%d) = %q, want no marker", n, got)
		}
	}
	for _, o := range jumpOrdinals {
		if got := ansi.StringWidth(o); got != 1 {
			t.Errorf("ordinal %q is %d cells, want 1: the gutter would widen", o, got)
		}
	}
}

// Rendered: nine markers on screen, in display order, in the gutter column —
// and none on the tenth card, which no chord can reach.
func TestJumpOrdinalsRenderInDisplayOrder(t *testing.T) {
	m := fakeModel(t, 30, 42, 120)
	m.cursor, m.cursorSess = -1, "" // no rail, so every ordinal is on screen
	lines := strings.Split(ansi.Strip(viewOf(m)), "\n")

	var seen []string
	for _, l := range lines {
		r := []rune(l)
		if len(r) == 0 {
			continue
		}
		for _, o := range jumpOrdinals {
			if string(r[0]) == o {
				seen = append(seen, o)
			}
		}
	}
	if got := strings.Join(seen, ""); got != "¹²³⁴⁵⁶⁷⁸⁹" {
		t.Errorf("ordinals on screen = %q, want ¹²³⁴⁵⁶⁷⁸⁹ in that order", got)
	}
}

// A card marked ⁶ is the card M-6 goes to. The marker and the chord are two
// readings of the same index and must never drift apart.
func TestJumpOrdinalMatchesTheChord(t *testing.T) {
	m := fakeModel(t, 30, 42, 120)
	m.cursor, m.cursorSess = -1, ""
	lines := strings.Split(ansi.Strip(viewOf(m)), "\n")

	marked := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "⁶") {
			marked = strings.TrimSpace(l)
			break
		}
	}
	if marked == "" {
		t.Fatal("no card carries the ⁶ marker")
	}
	m.key(altKey('6'))
	if !strings.Contains(marked, m.cursorSess) {
		t.Errorf("M-6 selected %q, but ⁶ marks %q", m.cursorSess, marked)
	}
}

// Collapsed, the pane is a 3-column strip: no cards, so no ordinals to put in
// a gutter that isn't drawn.
func TestJumpOrdinalsAreNotDrawnCollapsed(t *testing.T) {
	m := fakeModel(t, 30, collapsedWidth, 40)
	view := ansi.Strip(viewOf(m))
	for _, o := range jumpOrdinals {
		if strings.Contains(view, o) {
			t.Errorf("collapsed strip drew the ordinal %q:\n%s", o, view)
		}
	}
}
