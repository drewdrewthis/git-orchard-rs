package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The badge counts what the list shows; the bell rings for what ENTERS it.
// The two are deliberately not the same set — a synthetic row belongs in the
// count and nowhere near the speaker.

// countBells stubs the terminal for one test and returns how many rings it
// saw so far.
func countBells(t *testing.T) func() int {
	t.Helper()
	n := 0
	prev := emitBell
	emitBell = func() { n++ }
	t.Cleanup(func() { emitBell = prev })
	return func() int { return n }
}

// attnModel is a model that has already seen one list, so the next rebuild is
// a transition rather than a startup snapshot.
func attnModel(t *testing.T, rows ...row) *model {
	t.Helper()
	m := &model{rows: rows, bell: true}
	m.bellCheck()
	if !m.attnSeeded {
		t.Fatalf("a %d-row model did not seed the bell", len(rows))
	}
	return m
}

func TestAttentionCountCountsTheBucket(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []row
		want int
	}{
		{"nothing waiting", []row{{session: "a", state: "working"}}, 0},
		{"an input prompt", []row{{session: "a", state: "input"}}, 1},
		{"a stalled turn", []row{{session: "a", state: "stalled"}}, 1},
		{"both, plus noise", []row{
			{session: "a", state: "input"},
			{session: "b", state: "stalled"},
			{session: "c", state: "idle", hooked: true},
			{session: "d", state: "shell"},
		}, 2},
		{"synthetic rows count too — the badge counts what is drawn", []row{
			{session: "f", state: "input", fake: true},
			{session: "g", state: "stalled", fake: true},
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{rows: tc.rows}
			if got := m.attnCount(); got != tc.want {
				t.Errorf("attnCount = %d, want %d", got, tc.want)
			}
		})
	}
}

// A zero badge is a badge that teaches you to stop reading badges.
func TestAttentionBadgeIsAbsentAtZero(t *testing.T) {
	quiet := &model{rows: []row{{session: "a", state: "working"}}}
	if got := quiet.attnBadge(); got != "" {
		t.Errorf("badge = %q with nothing waiting, want none", got)
	}
	busy := &model{rows: []row{{session: "a", state: "input"}, {session: "b", state: "stalled"}}}
	if got := ansi.Strip(busy.attnBadge()); got != attnGlyph+"2" {
		t.Errorf("badge = %q, want %q", got, attnGlyph+"2")
	}
}

// The header carries it, and the filter — which only decides what is DRAWN —
// must not change it.
func TestAttentionBadgeRendersInTheHeaderAndSurvivesTheFilter(t *testing.T) {
	m := fakeModel(t, 30, 42, 40)
	want := ansi.Strip(m.attnBadge())
	if want == "" {
		t.Fatal("the synthetic list has no attention rows to badge")
	}
	head := strings.Split(ansi.Strip(viewOf(m)), "\n")[0]
	if !strings.Contains(head, want) {
		t.Errorf("header = %q, want it to carry the badge %q", head, want)
	}

	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeInto(m, "payments")
	head = strings.Split(ansi.Strip(viewOf(m)), "\n")[0]
	if !strings.Contains(head, want) {
		t.Errorf("filtered header = %q, want the same badge %q — the filter must not change the count", head, want)
	}
}

// Collapsed, the strip has three columns and one thing worth saying in them.
func TestAttentionCountShowsUnderTheCollapsedStrip(t *testing.T) {
	m := fakeModel(t, 30, collapsedWidth, 40)
	lines := strings.Split(ansi.Strip(viewOf(m)), "\n")
	if !strings.Contains(lines[0], expandGlyph) {
		t.Fatalf("collapsed strip does not open with %q: %q", expandGlyph, lines[0])
	}
	want := strings.TrimPrefix(ansi.Strip(m.attnBadge()), attnGlyph)
	if got := strings.TrimSpace(lines[1]); got != want {
		t.Errorf("line under the » is %q, want the count %q", got, want)
	}

	quiet := fakeModel(t, 30, collapsedWidth, 40)
	for i := range quiet.rows {
		quiet.rows[i].state = "working"
	}
	if got := strings.TrimSpace(strings.Split(ansi.Strip(viewOf(quiet)), "\n")[1]); got != "" {
		t.Errorf("collapsed strip drew %q with nothing waiting, want a blank line", got)
	}
}

// The transition table. Every row of it was a way this could ring wrongly.
func TestBellRingsOnlyOnANewArrival(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start []row
		next  []row
		want  int
	}{
		{"0 -> 1: the first thing to need you",
			[]row{{session: "a", state: "working"}},
			[]row{{session: "a", state: "input"}}, 1},
		{"2 -> 3: another session joins them",
			[]row{{session: "a", state: "input"}, {session: "b", state: "stalled"}},
			[]row{{session: "a", state: "input"}, {session: "b", state: "stalled"},
				{session: "c", state: "input"}}, 1},
		{"3 -> 2: one gets handled",
			[]row{{session: "a", state: "input"}, {session: "b", state: "input"},
				{session: "c", state: "input"}},
			[]row{{session: "a", state: "input"}, {session: "b", state: "input"},
				{session: "c", state: "working"}}, 0},
		{"flat: the same two, still waiting",
			[]row{{session: "a", state: "input"}, {session: "b", state: "stalled"}},
			[]row{{session: "a", state: "input"}, {session: "b", state: "stalled"}}, 0},
		{"a session leaves the bucket and comes back",
			[]row{{session: "a", state: "input"}},
			[]row{{session: "a", state: "working"}}, 0},
		{"synthetic rows never ring",
			[]row{{session: "real", state: "working"}},
			[]row{{session: "real", state: "working"},
				{session: "fake-1", state: "input", fake: true},
				{session: "fake-2", state: "stalled", fake: true}}, 0},
		{"a wiped snapshot coming back is not an arrival",
			[]row{{session: "a", state: "input"}},
			nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rings := countBells(t)
			m := attnModel(t, tc.start...)
			m.rows = tc.next
			m.bellCheck()
			if got := rings(); got != tc.want {
				t.Errorf("%d rings, want %d", got, tc.want)
			}
		})
	}
}

// The wipe in full: a lane fails, every row disappears, the next good poll
// brings the same sessions back. Nothing arrived, so nothing rings.
func TestBellSurvivesARowWipe(t *testing.T) {
	rings := countBells(t)
	waiting := []row{{session: "a", state: "input"}, {session: "b", state: "stalled"}}
	m := attnModel(t, waiting...)
	m.rows = nil
	m.bellCheck()
	m.rows = waiting
	m.bellCheck()
	if got := rings(); got != 0 {
		t.Errorf("%d rings across a wipe and restore, want 0", got)
	}
	// and a genuinely new session still gets through afterwards
	m.rows = append(append([]row{}, waiting...), row{session: "c", state: "input"})
	m.bellCheck()
	if got := rings(); got != 1 {
		t.Errorf("%d rings after a real arrival, want 1", got)
	}
}

// The first list is a snapshot of what was already waiting, not a transition:
// a sidebar that beeps the moment it opens is a sidebar with the bell off.
func TestBellIsSilentOnTheFirstList(t *testing.T) {
	rings := countBells(t)
	m := &model{bell: true}
	m.bellCheck() // still empty: nothing to seed from
	m.rows = []row{{session: "a", state: "input"}, {session: "b", state: "stalled"}}
	m.bellCheck() // the first list, three sessions deep in the bucket
	if got := rings(); got != 0 {
		t.Errorf("%d rings on startup, want 0", got)
	}
}

// Off by default, and off means silent — the tracking still runs, so turning
// it on does not then ring for everything that was already waiting.
func TestBellStaysSilentUntilTurnedOn(t *testing.T) {
	rings := countBells(t)
	m := &model{rows: []row{{session: "a", state: "working"}}}
	m.bellCheck()
	if m.bell {
		t.Error("the bell defaults on")
	}
	m.rows = []row{{session: "a", state: "input"}}
	m.bellCheck()
	if got := rings(); got != 0 {
		t.Errorf("%d rings with the bell off, want 0", got)
	}

	spy := newWidthSpy(t)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if !m.bell {
		t.Fatal("b did not turn the bell on")
	}
	m.bellCheck()
	if got := rings(); got != 0 {
		t.Errorf("turning the bell on rang for %d sessions already waiting, want 0", got)
	}
	if len(spy.saved) != 1 || !spy.saved[0].Bell {
		t.Errorf("saved state = %+v, want one write with Bell true", spy.saved)
	}
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	if last := spy.saved[len(spy.saved)-1]; last.Bell {
		t.Errorf("toggling off saved %+v, want Bell false", last)
	}
}

// The bell rides in the same file as the layout, so a drag must not drop it
// and a toggle must not drop the width.
func TestBellPersistsAlongsideTheLayout(t *testing.T) {
	stateHome(t)
	if err := saveSidebarState(sidebarState{Width: 52, Collapsed: true, Bell: true}); err != nil {
		t.Fatal(err)
	}
	if got := loadSidebarState(); got != (sidebarState{Width: 52, Collapsed: true, Bell: true}) {
		t.Errorf("loaded %+v, want width 52, collapsed, bell on", got)
	}

	spy := newWidthSpy(t)
	m := &model{desiredWidth: 52, collapsed: true, bell: true}
	m.toggleCollapse()
	if last := spy.saved[len(spy.saved)-1]; !last.Bell || last.Width != 52 {
		t.Errorf("a collapse toggle saved %+v, want the bell and the width carried through", last)
	}
	m.publishWidth(60)
	if last := spy.saved[len(spy.saved)-1]; !last.Bell || last.Width != 60 {
		t.Errorf("a drag saved %+v, want the bell carried through", last)
	}
}

// A file written before the bell existed still loads: an absent key is off.
func TestBellDefaultsOffForAPreBellStateFile(t *testing.T) {
	stateHome(t)
	writeStateFile(t, "sidebar-state.json", `{"width":52,"collapsed":false}`)
	if got := loadSidebarState(); got != (sidebarState{Width: 52}) {
		t.Errorf("loaded %+v, want width 52 with the bell off", got)
	}
}
