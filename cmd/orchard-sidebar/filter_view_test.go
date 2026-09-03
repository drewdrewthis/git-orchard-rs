package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// typeInto feeds a string to the model one rune at a time — the same shape
// bubbletea delivers real typing in, and the same one the filter field has to
// survive.
func typeIntoView(m *model, s string) {
	for _, r := range s {
		m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// Esc clears and closes; Enter keeps the query and gives the keys back, so the
// very next j walks the FILTERED list instead of typing a j into the field.
func TestFilterEscClearsAndEnterKeeps(t *testing.T) {
	prev := switchClient
	switchClient = func(string, bool) {}
	t.Cleanup(func() { switchClient = prev })

	m := fakeModel(t, 30, 42, 40)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeIntoView(m, "payments")
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if m.filter.open {
		t.Error("enter left the field focused")
	}
	if got := m.filterQuery(); got != "payments" {
		t.Errorf("enter dropped the query: %q", got)
	}
	vis := m.visibleRows()
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if got := m.cursor; got != vis[1] {
		t.Errorf("j after enter selected row %d, want the 2nd VISIBLE row (%d)", got, vis[1])
	}

	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if m.filterOn() || m.filterQuery() != "" {
		t.Errorf("esc left a filter on: open=%v query=%q", m.filter.open, m.filterQuery())
	}
	if got := len(m.visibleRows()); got != len(m.rows) {
		t.Errorf("%d cards visible after esc, want all %d", got, len(m.rows))
	}
}

// Reopening with / keeps what was typed: / is how you get back INTO a filter
// you left applied, not how you throw it away.
func TestFilterReopenKeepsTheQuery(t *testing.T) {
	m := fakeModel(t, 30, 42, 40)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeIntoView(m, "auth")
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !m.filter.open || m.filterQuery() != "auth" {
		t.Errorf("reopen: open=%v query=%q, want true/\"auth\"", m.filter.open, m.filterQuery())
	}
	// and typing appends rather than retyping from scratch
	typeIntoView(m, "x")
	if got := m.filterQuery(); got != "authx" {
		t.Errorf("query = %q, want \"authx\" (cursor at the end)", got)
	}
}

// While the field has focus every printable key is text — including the ones
// that are commands in the list. A / filter you cannot type "q" or "b" into is
// not a text field.
func TestFilterSwallowsTheListKeys(t *testing.T) {
	m := fakeModel(t, 30, 42, 40)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	before := m.cursor
	for _, r := range []rune{'j', 'k', 'q', 'b'} {
		if cmd := m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}); cmd != nil {
			t.Fatalf("%q produced a command while the filter was open", r)
		}
	}
	if m.cursor != before {
		t.Errorf("typing into the filter moved the cursor %d -> %d", before, m.cursor)
	}
	if m.bell {
		t.Error("typing 'b' into the filter toggled the bell")
	}
	if got := m.filterQuery(); got != "jkqb" {
		t.Errorf("query = %q, want \"jkqb\"", got)
	}
}

// A burst arriving in one read — a fast typist, or a paste — that OPENS the
// filter must not lose the rest of itself: bubbletea folds those runes into a
// single message, and the list handler would have eaten "payments" as nine
// separate list commands.
func TestFilterOpenedMidBurstKeepsTheRest(t *testing.T) {
	m := fakeModel(t, 30, 42, 40)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/payments")})
	if got := m.filterQuery(); got != "payments" {
		t.Errorf("query = %q after one coalesced \"/payments\", want \"payments\"", got)
	}
	if got := len(m.visibleRows()); got != 3 {
		t.Errorf("%d cards visible, want 3", got)
	}
}

// A filter that matches nothing says so. An empty band reads as "the sidebar
// broke", and the footer's fixed furniture still has to draw under it.
func TestFilterNoMatchSaysSo(t *testing.T) {
	m := fakeModel(t, 30, 42, 40)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeIntoView(m, "zzzznothing")
	view := ansi.Strip(viewOf(m))
	if !strings.Contains(view, "no match") {
		t.Errorf("no no-match line in:\n%s", view)
	}
	if !strings.Contains(view, hintLine(m.bell)) {
		t.Error("the footer stopped drawing under an empty filter")
	}
	lines := strings.Split(view, "\n")
	if len(lines) != m.height {
		t.Errorf("pane is %d lines, want the full %d", len(lines), m.height)
	}
}

// The footer's git box is a projection of the card the rail is on, so with the
// selection filtered away it describes the card the user can actually see —
// never a session that is not on screen at all.
func TestFilterFooterDescribesTheVisibleCard(t *testing.T) {
	m := fakeModel(t, 30, 42, 40)
	for i, r := range m.rows {
		if strings.Contains(r.session, "auth") {
			m.cursor, m.cursorSess = i, r.session
			break
		}
	}
	hidden := m.cursorSess

	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeIntoView(m, "payments")
	view := ansi.Strip(viewOf(m))

	shown, _ := m.railRow()
	if !strings.Contains(shown.session, "payments") {
		t.Fatalf("the rail is on %q, which the filter should have hidden", shown.session)
	}
	if !strings.Contains(view, shown.branch) {
		t.Errorf("the git box does not carry the rail card's branch %q:\n%s", shown.branch, view)
	}
	if strings.Contains(view, hidden) {
		t.Errorf("the pane still names the filtered-away selection %q", hidden)
	}
}

// A click with a filter on selects the session under the pointer, not the row
// that happens to sit at that index in the unfiltered model — the line map is
// built from indices INTO m.rows, never from screen positions.
func TestFilterKeepsClickTargetsHonest(t *testing.T) {
	m := fakeModel(t, 30, 42, 40)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeIntoView(m, "payments")
	viewOf(m)

	want, clicked := "", false
	for y, ri := range m.pane.lineToRow {
		if ri < 0 {
			continue
		}
		want, clicked = m.rows[ri].session, true
		m.mouse(tea.MouseMsg{X: 5, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		break
	}
	if !clicked {
		t.Fatal("the filtered pane mapped no line to a row")
	}
	if !strings.Contains(want, "payments") {
		t.Errorf("a click landed on %q, which the filter should have hidden", want)
	}
	if m.cursorSess != want {
		t.Errorf("the click selected %q, want the card under the pointer (%q)", m.cursorSess, want)
	}
}
