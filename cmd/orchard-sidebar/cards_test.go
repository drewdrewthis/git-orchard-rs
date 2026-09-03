package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// What a card and the fixed chrome around it look like: a card is always the
// same height, the header carries both buttons, and the footer never changes
// height with the selection — all three are what keep the list from jumping
// under the cursor.

// The synthetic rows exist to be scrolled through, so they have to be plural,
// stable, obviously synthetic, and cover all three sections — a scroll test
// that never renders a section header tests half the layout.
func TestFakeRowsAreTaggedStableAndCoverEveryBucket(t *testing.T) {
	a, b := fakeRows(30), fakeRows(30)
	seen := map[bucket]int{}
	for i := range a {
		if a[i].session != b[i].session || a[i].state != b[i].state {
			t.Fatalf("row %d differs between calls: %+v vs %+v", i, a[i], b[i])
		}
		if !a[i].fake || !strings.HasPrefix(a[i].session, "fake-") {
			t.Errorf("row %d is not tagged as synthetic: %+v", i, a[i])
		}
		seen[rowBucket(a[i])]++
	}
	for _, want := range []bucket{bucketAttention, bucketDone, bucketRunning} {
		if seen[want] == 0 {
			t.Errorf("no synthetic row lands in bucket %v", want)
		}
	}
}

// Selecting a row attaches its tmux session. A row that names no session must
// never reach switch-client, or scrolling test data spits errors into the pane.
func TestSelectingASyntheticRowNeverSwitches(t *testing.T) {
	prev := switchClient
	switched := ""
	switchClient = func(session string, handBack bool) { switched = session }
	t.Cleanup(func() { switchClient = prev })

	m := &model{rows: append(fakeRows(2), row{session: "real", state: "idle"})}
	m.selectRow(0, true)
	if switched != "" {
		t.Errorf("synthetic row switched to %q", switched)
	}
	if m.cursor != 0 || m.cursorSess != m.rows[0].session {
		t.Errorf("synthetic row did not take the cursor: %d %q", m.cursor, m.cursorSess)
	}
	m.selectRow(2, true)
	if switched != "real" {
		t.Errorf("real row switched to %q, want real", switched)
	}
}

// Every card is the same height whatever the session has to say, so the list
// scans as a grid and scrolling moves by a predictable number of lines.
func TestCardsAreAFixedHeight(t *testing.T) {
	rows := []row{
		{session: "everything", state: "input", hooked: true, model: "opus",
			mission: "a mission", cwd: "/w/everything", branch: "feat/x", issueNum: 700},
		{session: "bare", state: "input", hooked: true}, // no mission, dir, branch
	}
	m := &model{rows: rows, cursor: 0, stateDirOK: true}
	for _, tc := range []struct {
		name    string
		compact bool
		want    int
	}{{"full", false, cardRows}, {"compact", true, compactCardRows}} {
		t.Run(tc.name, func(t *testing.T) {
			lines := m.cards(42, tc.compact)
			counts := map[int]int{}
			for _, l := range lines {
				if l.row >= 0 {
					counts[l.row]++
				}
			}
			for row, n := range counts {
				if n != tc.want {
					t.Errorf("row %d drew %d lines, want %d", row, n, tc.want)
				}
			}
			if len(counts) != len(rows) {
				t.Errorf("drew %d rows, want %d", len(counts), len(rows))
			}
		})
	}
}

// The three fixed pieces of chrome the redesign added: both header buttons, and
// a full-width rule where the scrolling stops.
func TestHeaderButtonsAndFooterRule(t *testing.T) {
	m := fakeModel(t, 8, 42, 30)
	lines := strings.Split(viewOf(m), "\n")
	head := ansi.Strip(lines[0])
	if !strings.Contains(head, launchGlyph) || !strings.Contains(head, collapseGlyph) {
		t.Errorf("header %q is missing a button", head)
	}
	if x := strings.Index(head, launchGlyph); x >= strings.Index(head, collapseGlyph) {
		t.Errorf("the + must sit left of the «: %q", head)
	}
	// the buttons' hit zones must not overlap: they do very different things
	if m.pane.launchZone.hit(42-3, 0) || m.pane.collapseZone.hit(42-6, 0) {
		t.Errorf("button zones overlap: launch %+v collapse %+v", m.pane.launchZone, m.pane.collapseZone)
	}
	rule := ""
	for _, l := range lines {
		if s := ansi.Strip(l); strings.HasPrefix(s, strings.Repeat("─", 10)) {
			rule = s
		}
	}
	if ansi.StringWidth(rule) != 42 {
		t.Errorf("footer rule is %d cells wide, want 42: %q", ansi.StringWidth(rule), rule)
	}
}

// Clicking the + opens the modal on the selected session's directory, and does
// not select, copy, or collapse on the way.
func TestClickingPlusOpensTheLaunchModal(t *testing.T) {
	prevPopup, prevBack := openLaunchPopup, handBackFocus
	gotDir, handedBack := "", false
	openLaunchPopup = func(dir string) { gotDir = dir }
	handBackFocus = func(outerPane) { handedBack = true }
	t.Cleanup(func() { openLaunchPopup, handBackFocus = prevPopup, prevBack })

	m := &model{rows: []row{{session: "s", state: "idle", cwd: "/w/s"}}, width: 42, height: 20}
	viewOf(m) // publishes the zones
	mm, _ := m.Update(tea.MouseMsg{X: 42 - 6, Y: 0,
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = mm.(*model)
	if gotDir != "/w/s" {
		t.Errorf("opened the modal on %q, want /w/s", gotDir)
	}
	if !handedBack {
		t.Error("focus was not handed back to the inner client")
	}
	if m.collapsed {
		t.Error("the + collapsed the pane")
	}
}

// Synthetic rows have no hook file, so the hook overlay used to strip their
// hooked flag on every rebuild and paint a "state unverified" ? on every card.
func TestHookOverlayLeavesSyntheticRowsAlone(t *testing.T) {
	want := map[string]row{}
	for _, r := range fakeRows(4) {
		want[r.session] = r
	}
	m := &model{rows: fakeRows(4), hooksBySess: map[string]hookState{}, fakes: fakeRows(4)}
	for pass := 0; pass < 3; pass++ { // the bug only showed on the second rebuild
		m.applyHooks()
		m.appendFakes()
		if len(m.rows) != len(want) {
			t.Fatalf("pass %d: rows grew to %d", pass, len(m.rows))
		}
		for _, got := range m.rows {
			if got.lastAct.IsZero() {
				t.Fatalf("pass %d: %s lost its activity time", pass, got.session)
			}
			if g, w := rowFacts(got), rowFacts(want[got.session]); g != w {
				t.Fatalf("pass %d: %s was rewritten by the hook overlay:\n got %s\nwant %s",
					pass, got.session, g, w)
			}
		}
	}
}

// rowFacts is everything the hook overlay could overwrite, as a comparable
// string: row holds pointers (fresh addresses per call) and a now-relative
// timestamp, so == on the struct compares identity rather than content.
func rowFacts(r row) string {
	pr := 0
	if r.pr != nil {
		pr = r.pr.Number
	}
	return fmt.Sprintf("state=%s hooked=%v fake=%v mission=%q cwd=%q branch=%q repo=%q model=%s issue=%d pr=%d",
		r.state, r.hooked, r.fake, r.mission, r.cwd, r.branch, r.repo, r.model, r.issueNum, pr)
}

// The footer is fixed furniture: its height cannot depend on how much the
// selected session happens to know about itself, or the list band above it
// changes size and every card jumps as the cursor moves.
func TestFooterHeightIsConstantAcrossSelections(t *testing.T) {
	m := fakeModel(t, 30, 40, 30)
	rich := row{session: "rich", branch: "feat/x", cwd: "/w/rich", repo: "o/r",
		issueNum: 747, issueTitle: "outer wrapper", pr: &prInfo{Number: 12, State: "OPEN"}}
	bare := row{session: "bare", cwd: "/w/bare"} // one item
	none := row{session: "none"}                 // no items at all
	m.rows = []row{rich, bare, none}

	var wantH, wantRule int
	for i, r := range m.rows {
		m.cursor = i
		m.cursorSess = r.session
		h := len(m.footer(40, false))
		// where the rule sits IS where the list stops: the user-visible fact
		lines := strings.Split(viewOf(m), "\n")
		ruleAt := -1
		for j, l := range lines {
			if strings.HasPrefix(ansi.Strip(l), strings.Repeat("─", 10)) {
				ruleAt = j
			}
		}
		if i == 0 {
			wantH, wantRule = h, ruleAt
			if ruleAt < 0 {
				t.Fatal("no footer rule found in the render")
			}
			continue
		}
		if h != wantH {
			t.Errorf("%s: footer is %d lines, %s was %d — the list band moves",
				r.session, h, m.rows[0].session, wantH)
		}
		if ruleAt != wantRule {
			t.Errorf("%s: footer rule at line %d, %s had it at %d",
				r.session, ruleAt, m.rows[0].session, wantRule)
		}
	}
}
