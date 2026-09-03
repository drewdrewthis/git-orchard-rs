package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// layoutPane is the whole point of the three-band view: whatever the height,
// the header starts at the top, the footer ends at the bottom, and the list
// takes the rest. A band that moved when the list grew would put the git box
// (footer) somewhere different on every repaint.
func TestLayoutPaneBandsStayPinned(t *testing.T) {
	for _, h := range []int{24, 40, 60} {
		for _, footer := range []int{1, 3, 8, 12} {
			lay := layoutPane(h, 2, footer)
			if lay.headerH != 2 {
				t.Errorf("h=%d footer=%d: headerH = %d, want 2", h, footer, lay.headerH)
			}
			if lay.listY != lay.headerH {
				t.Errorf("h=%d footer=%d: list starts at %d, want %d (a gap under the header)",
					h, footer, lay.listY, lay.headerH)
			}
			if lay.footerY != lay.listY+lay.listH {
				t.Errorf("h=%d footer=%d: footer starts at %d, want %d (list and footer overlap or gap)",
					h, footer, lay.footerY, lay.listY+lay.listH)
			}
			if got := lay.footerY + lay.footerH; got != h {
				t.Errorf("h=%d footer=%d: footer ends at %d, want %d (not pinned to the bottom)",
					h, footer, got, h)
			}
			if lay.footerH != footer {
				t.Errorf("h=%d footer=%d: footerH = %d — a footer this small always fits",
					h, footer, lay.footerH)
			}
			if lay.listH < minListRows {
				t.Errorf("h=%d footer=%d: listH = %d, want >= %d", h, footer, lay.listH, minListRows)
			}
		}
	}
}

// A pane too short for all three bands gives up footer rows, never header
// rows: the header carries the collapse button, the only way back out of a
// mis-sized pane. The bands stay pinned and non-overlapping throughout.
func TestLayoutPaneShrinksTheFooterFirst(t *testing.T) {
	for h := 0; h <= 20; h++ {
		lay := layoutPane(h, 2, 9)
		if h <= 0 {
			if lay != (paneLayout{}) {
				t.Errorf("h=%d: got %+v, want zero layout", h, lay)
			}
			continue
		}
		if lay.headerH+lay.listH+lay.footerH != h {
			t.Errorf("h=%d: bands sum to %d, want %d", h,
				lay.headerH+lay.listH+lay.footerH, h)
		}
		if lay.footerY+lay.footerH != h {
			t.Errorf("h=%d: footer ends at %d, want %d", h, lay.footerY+lay.footerH, h)
		}
		if lay.headerH > 2 || (h >= 2 && lay.headerH != 2) {
			t.Errorf("h=%d: headerH = %d, want 2 (or the whole pane when shorter)", h, lay.headerH)
		}
		if lay.footerH > 0 && lay.listH < minListRows {
			t.Errorf("h=%d: footer kept %d rows while the list has only %d", h, lay.footerH, lay.listH)
		}
	}
}

func rowsForHeight(n int) []row {
	rows := make([]row, n)
	for i := range rows {
		rows[i] = row{
			session: fmt.Sprintf("sess-%02d", i), state: "idle", hooked: true,
			mission: "a mission", branch: "feat/x", cwd: "/w/x", repo: "o/r",
			issueNum: 700 + i,
		}
	}
	return rows
}

// The live invariant behind layoutPane: at any terminal height the rendered
// pane is exactly that many lines, the title row is first, and the key hints
// are last — no matter how many sessions the list is carrying.
func TestViewPinsHeaderAndFooterAtEveryHeight(t *testing.T) {
	for _, h := range []int{24, 40, 60} {
		for _, n := range []int{0, 1, 40} {
			m := &model{rows: rowsForHeight(n), stateDirOK: true, cursor: 0}
			if n == 0 {
				m.cursor = -1
			}
			m.Update(tea.WindowSizeMsg{Width: 42, Height: h})
			lines := strings.Split(viewOf(m), "\n")
			if len(lines) != h {
				t.Fatalf("h=%d rows=%d: rendered %d lines, want %d", h, n, len(lines), h)
			}
			if !strings.Contains(lines[0], "orchard") || !strings.Contains(lines[0], collapseGlyph) {
				t.Errorf("h=%d rows=%d: header line = %q, want the title and %q",
					h, n, lines[0], collapseGlyph)
			}
			if !strings.Contains(lines[h-1], "M-1-9") {
				t.Errorf("h=%d rows=%d: last line = %q, want the key hints", h, n, lines[h-1])
			}
			if n > 0 && !strings.Contains(strings.Join(lines, "\n"), "issue#700") {
				t.Errorf("h=%d rows=%d: git box missing from the footer", h, n)
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > 42 {
					t.Errorf("h=%d rows=%d: line %d is %d cells, want <= 42: %q", h, n, i, w, l)
				}
			}
			if len(m.pane.lineToRow) != h || len(m.pane.lineToCopy) != h {
				t.Errorf("h=%d rows=%d: mouse maps are %d/%d lines, want %d",
					h, n, len(m.pane.lineToRow), len(m.pane.lineToCopy), h)
			}
		}
	}
}

// More cards than the list band can hold: the list scrolls to the selection
// instead of pushing the footer off the bottom.
func TestListScrollsToKeepSelectionVisible(t *testing.T) {
	prev := switchClient
	switchClient = func(string, bool) {}
	defer func() { switchClient = prev }()

	m := &model{rows: rowsForHeight(30), stateDirOK: true}
	m.Update(tea.WindowSizeMsg{Width: 42, Height: 24})
	m.selectRow(29, false) // walked to the bottom of the list
	body := viewOf(m)
	if !strings.Contains(body, "sess-29") {
		t.Errorf("selected card scrolled out of view:\n%s", body)
	}
	if strings.Contains(body, "sess-00") {
		t.Errorf("list did not scroll — the first card is still drawn:\n%s", body)
	}
	lines := strings.Split(body, "\n")
	if !strings.Contains(lines[len(lines)-1], "M-1-9") {
		t.Errorf("footer left the bottom edge: %q", lines[len(lines)-1])
	}
}

// Clicking the header button collapses the pane to the strip: the tmux pane
// is resized, the @sidebar_collapsed option follows it, and focus goes back
// to the inner pane. Clicking the strip expands it again.
func TestCollapseButtonTogglesTheStrip(t *testing.T) {
	type call struct {
		collapsed bool
		width     int
	}
	var calls []call
	var handBacks int
	spy := newWidthSpy(t)
	origSet, origHB := setCollapsed, handBackFocus
	setCollapsed = func(c bool, w int) { calls = append(calls, call{c, w}) }
	handBackFocus = func(outerPane) { handBacks++ }
	defer func() { setCollapsed, handBackFocus = origSet, origHB }()

	m := &model{rows: rowsForHeight(3), stateDirOK: true, cursor: 0}
	m.Update(tea.WindowSizeMsg{Width: 42, Height: 40})
	viewOf(m) // publishes the button's hit rectangle

	click := func(x, y int) {
		m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: y})
	}

	click(0, 0) // the title end of the header is not the button
	if len(calls) != 0 {
		t.Fatalf("click on the title collapsed the pane: %+v", calls)
	}

	click(42-3, 0)
	if len(calls) != 1 || calls[0] != (call{true, collapsedWidth}) {
		t.Fatalf("collapse: calls = %+v, want one {true %d}", calls, collapsedWidth)
	}
	if handBacks != 1 {
		t.Errorf("collapse handed focus back %d times, want 1", handBacks)
	}
	if !m.collapsed || m.width != collapsedWidth {
		t.Fatalf("collapsed=%v width=%d, want true/%d", m.collapsed, m.width, collapsedWidth)
	}

	strip := viewOf(m)
	if !strings.Contains(strip, expandGlyph) {
		t.Errorf("collapsed strip has no %q:\n%q", expandGlyph, strip)
	}
	for i, l := range strings.Split(strip, "\n") {
		if w := ansi.StringWidth(l); w > collapsedWidth {
			t.Errorf("strip line %d is %d cells, want <= %d: %q", i, w, collapsedWidth, l)
		}
	}

	click(1, 4) // anywhere in the strip expands
	// back to the width it had before the collapse — the pane the wrapper
	// split, or whatever the user has since dragged it to
	if len(calls) != 2 || calls[1] != (call{false, 42}) {
		t.Fatalf("expand: calls = %+v, want a second {false 42}", calls)
	}
	if m.collapsed {
		t.Error("still collapsed after the expand click")
	}

	// the layout the user chose is remembered, so it survives a restart
	if first := spy.saved[0]; !reflect.DeepEqual(first, sidebarState{Width: 42, Collapsed: true}) {
		t.Errorf("the collapse was persisted as %+v", first)
	}

	// a width the user dragged to is what the pane reopens at, not the default
	m.desiredWidth = 52
	viewOf(m)           // the header is back; republish its hit rectangle
	click(m.width-3, 0) // collapse again
	viewOf(m)
	click(1, 4)
	if last := calls[len(calls)-1]; last != (call{false, 52}) {
		t.Errorf("expand after a drag: %+v, want {false 52}", last)
	}
}

// A collapsed pane's width is not a drag: publishing 3 as the shared width
// would collapse the pane for good, and enforcing the readable floor over it
// would fight M-s open again on the next tick.
func TestCollapsedWidthIsNotADrag(t *testing.T) {
	w := newWidthSpy(t)

	m := &model{desiredWidth: 40, width: 40, sized: true}
	m.Update(tea.WindowSizeMsg{Width: collapsedWidth, Height: 40}) // M-s collapsed it
	if len(w.published) != 0 || len(w.resized) != 0 {
		t.Fatalf("collapse published width=%v resize=%v, want neither", w.published, w.resized)
	}
	if !m.collapsed || m.desiredWidth != 40 {
		t.Fatalf("collapsed=%v desiredWidth=%d, want true/40 (the expanded width is remembered)",
			m.collapsed, m.desiredWidth)
	}

	m.Update(tea.WindowSizeMsg{Width: 40, Height: 40}) // M-s expanded it again
	if m.collapsed {
		t.Error("still collapsed after being resized back to 40")
	}
	if len(w.published) != 0 {
		t.Errorf("the expand republished the width it already had: %v", w.published)
	}
}
