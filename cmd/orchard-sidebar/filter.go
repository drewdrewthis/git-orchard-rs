package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The `/` filter: a substring over the list, typed into the header row.
//
// It is a VIEW, not a mode. Narrowing changes which cards are drawn and
// nothing else — not the attached session, not the Needs-attention count, and
// never a switch-client. The one thing it does move is the rail, and only when
// the filter has hidden the card the rail was on (railIndex).

// filterState is the header's filter. The field is the single source of truth
// for the query: Enter closes the field and keeps its value, Esc drops both,
// so there is no second copy of the string to fall out of step with it.
type filterState struct {
	open  bool
	field textField
}

// filterFieldWidth is the field's own horizontal budget. The header re-clamps
// it to whatever the pane actually leaves (filterHead), so this only has to be
// wide enough that a fresh field does not open already scrolled.
const filterFieldWidth = 32

func (m *model) filterQuery() string { return m.filter.field.value() }

// filterOn reports whether the header shows the filter instead of the title:
// while the field has focus, and afterwards for as long as a query is still
// narrowing the list.
func (m *model) filterOn() bool { return m.filter.open || m.filterQuery() != "" }

// openFilter puts the keyboard in the field. Re-opening keeps what was typed —
// `/` is how you get back into a filter you left applied with Enter.
func (m *model) openFilter() {
	m.filter.field = newTextField(m.filterQuery(), filterFieldWidth)
	m.filter.open = true
}

// clearFilter drops the query and the field together: Esc means "show me
// everything again", from inside the field or from the list.
func (m *model) clearFilter() { m.filter = filterState{} }

// filterKey drives the field while it has focus. Esc and Enter are the
// filter's own; everything else is the text field's, which is what buys the
// editing keys (word moves, ^U, ^K, a pasted burst landing whole) without this
// file growing an editor.
func (m *model) filterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.clearFilter()
		return nil
	case tea.KeyEnter:
		// keep the query, give the keys back to the list: a filter you have to
		// leave before you can walk the list it produced is a mode
		m.filter.open = false
		return nil
	}
	return m.filter.field.key(msg)
}

// visibleRows is the display order of the rows the filter keeps, as indices
// into m.rows. Indices, not rows: every click map, every jump and the rail all
// have to name the row in the model, not its position on screen.
func (m *model) visibleRows() []int {
	q := m.filterQuery()
	out := make([]int, 0, len(m.rows))
	for i, r := range m.rows {
		if q == "" || rowMatches(r, q) {
			out = append(out, i)
		}
	}
	return out
}

// rowMatches is the filter's whole rule: a case-insensitive substring over
// everything a card can say a session IS — its name, what it is doing, where
// it lives, its branch, and the issue or PR it is for. One function, so what
// the filter matches on can never drift from what the user is reading it off.
func rowMatches(r row, q string) bool {
	q = strings.ToLower(q)
	if q == "" {
		return true
	}
	for _, f := range matchFields(r) {
		if f != "" && strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

func matchFields(r row) []string {
	out := []string{r.session, r.mission, r.cwd, r.branch, r.repo}
	if r.issueNum > 0 {
		out = append(out, issueRef(r.issueNum), r.issueTitle)
	}
	if r.pr != nil {
		out = append(out, prRef(*r.pr))
	}
	return out
}

// railIndex is the row the selection rail draws on. The selected session keeps
// it whenever the filter still shows that card; when the filter has hidden it,
// the rail moves to the first visible card so a narrowed list still has a
// "you are here" — and moves there WITHOUT attaching, since filtering must
// never switch a session.
//
// With no filter this is exactly m.cursor, parked -1 and all: a cursor sitting
// on a session the daemon has not served a row for deliberately draws no rail,
// and inventing one would put the bar on a card the user never chose.
func (m *model) railIndex(vis []int) int {
	for _, i := range vis {
		if i == m.cursor {
			return m.cursor
		}
	}
	if m.filterQuery() != "" && len(vis) > 0 {
		return vis[0]
	}
	return -1
}

// railRow is the card the pane is currently DESCRIBING: the one wearing the
// rail, which is the selection except when the filter has hidden it. The
// footer's git box and the launch modal's starting directory are both
// projections of it, so all three readings of "this card" come from one place
// and cannot end up pointing at a session that is not even on screen.
func (m *model) railRow() (row, bool) { return m.rowAt(m.railIndex(m.visibleRows())) }

// filterHead is the header's left-hand text while the filter is on:
// `/query (n)`, n being how many cards survive it. Focused, the query is the
// live field with its cursor; after Enter it is the same string, static.
func (m *model) filterHead(w int) string {
	count := styDim.Render(fmt.Sprintf(" (%d)", len(m.visibleRows())))
	// What is left after the slash and the count, so a long query scrolls
	// under the cursor instead of pushing the count off the line. Two cells
	// come off rather than one: a focused textinput draws its Width PLUS the
	// cell its cursor sits in, and a count clipped to "(…" is a count that
	// says nothing.
	fw := max(1, w-2-cellWidth(count))
	if m.filter.open {
		return styDim.Render("/") + m.filter.field.view(fw) + count
	}
	return stySelHead.Render("/"+trunc(m.filterQuery(), fw+1)) + count
}

// noMatchLines is the list band when the filter matches nothing. An explicit
// line, never an empty band: a blank list reads as "the sidebar broke", which
// is the one thing it must not say while it is working correctly.
func (m *model) noMatchLines(iw int) []viewLine {
	return []viewLine{
		{text: "", row: -1},
		{text: " " + styDim.Render(trunc("no match for “"+m.filterQuery()+"”", iw)), row: -1},
	}
}
