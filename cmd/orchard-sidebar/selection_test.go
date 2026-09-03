package main

import (
	"testing"
	"time"
)

// Which card carries the bar, and what is allowed to move it. The list
// re-sorts under the user every couple of seconds, so "the selection" is a
// session name, never an index — every test here is a way that used to break.

// Selection and the switch are one action: every gesture that moves the cursor
// must also attach that session, and must not run off either end of the list.
func TestSelectRowSwitchesSession(t *testing.T) {
	var got []string
	orig := switchClient
	switchClient = func(s string, handBack bool) { got = append(got, s) }
	defer func() { switchClient = orig }()

	m := &model{rows: []row{{session: "a"}, {session: "b"}, {session: "c"}}}
	m.selectRow(1, true)
	// a deliberate select attaches "b", which makes it the most recently
	// attached session and so the top card; the cursor follows it there
	if m.cursorSess != "b" {
		t.Fatalf("cursorSess = %q, want b", m.cursorSess)
	}
	if m.cursor != 0 || m.rows[0].session != "b" {
		t.Fatalf("selected session did not rise to the top: cursor=%d, top=%q", m.cursor, m.rows[0].session)
	}
	m.selectRow(-1, true) // off the top: no move, no switch
	m.selectRow(3, true)  // off the bottom: same
	if m.cursorSess != "b" {
		t.Errorf("out-of-range select changed the selection to %q", m.cursorSess)
	}
	if want := []string{"b"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("switched to %v, want %v", got, want)
	}
}

// Rows re-sort whenever a session changes state, so the cursor is anchored by
// session name — otherwise the selection (and thus the attached session) would
// drift onto whichever session happened to land at that index.
func TestReanchorCursorFollowsSession(t *testing.T) {
	m := &model{
		rows:       []row{{session: "a"}, {session: "b"}, {session: "c"}},
		cursor:     2,
		cursorSess: "c",
	}
	m.rows = []row{{session: "c"}, {session: "a"}, {session: "b"}}
	m.reanchorCursor()
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0 (followed session c)", m.cursor)
	}

	// no user input and no client-lane answer yet: first attached row is a
	// sane opening guess until clientSessMsg lands (~150ms later)
	fresh := &model{rows: []row{
		{session: "a"},
		{session: "b", attached: true},
		{session: "c", attached: true},
	}}
	fresh.reanchorCursor()
	if fresh.cursor != 1 || fresh.cursorSess != "b" {
		t.Errorf("fallback picked %d/%q, want 1/\"b\"", fresh.cursor, fresh.cursorSess)
	}

	// a vanished (or not-yet-served) session parks the bar rather than
	// leaving an out-of-range index or walking to a card the user never
	// chose; the client lane re-reads the real session within ~150ms
	gone := &model{rows: []row{{session: "a"}}, cursor: 5, cursorSess: "zz"}
	gone.reanchorCursor()
	if gone.cursor != -1 {
		t.Errorf("stale cursor = %d, want parked -1", gone.cursor)
	}
}

// The local tmux lane is the sole authority for the bar: whatever session
// tmux says the client is on, the cursor follows — instantly, no grace window,
// and nothing the daemon reports can override it.
func TestClientLaneOwnsCursor(t *testing.T) {
	m := &model{
		rows:       []row{{session: "a", attached: true}, {session: "b"}},
		cursor:     0,
		cursorSess: "a",
	}
	m.Update(clientSessMsg{name: "b"})
	if m.cursor != 1 || m.cursorSess != "b" {
		t.Errorf("client lane: cursor = %d/%q, want 1/\"b\"", m.cursor, m.cursorSess)
	}
	// a daemon snapshot still claiming "a" attached must not move it back
	m.Update(tmuxSubMsg{sessions: []tmuxSession{
		{Name: "a", Attached: true},
		{Name: "b", Attached: false},
	}})
	if m.cursorSess != "b" {
		t.Errorf("daemon snapshot clobbered the client lane: %q, want \"b\"", m.cursorSess)
	}
	// an empty read (tmux hiccup, no clients) changes nothing
	m.Update(clientSessMsg{})
	if m.cursorSess != "b" {
		t.Errorf("empty client read moved the cursor: %q", m.cursorSess)
	}
}

// The hook lane re-sorts on every 2s tick. If it doesn't re-anchor afterwards,
// m.cursor keeps pointing at the slot rather than the session, and the bar
// silently walks to a different card while the attached session never moved.
func TestHookTickKeepsCursorOnItsSession(t *testing.T) {
	m := &model{
		rows: []row{
			{session: "b", state: "idle"},
			{session: "c", state: "idle"},
		},
		cursor: 1, cursorSess: "c",
	}
	// every row shares an activity time, so the sort falls through to the name
	// and "a" lands at index 0, pushing every existing card down one slot
	now := time.Now()
	m.hooksBySess = map[string]hookState{
		"a": {state: "idle", lastAct: now},
		"b": {state: "idle", lastAct: now},
		"c": {state: "idle", lastAct: now},
	}
	// go through Update, not applyHooks directly: the bug was the hook branch
	// re-sorting and returning without re-anchoring
	m.Update(hookDataMsg{bySession: m.hooksBySess, dirOK: true})
	if m.cursorSess != "c" {
		t.Fatalf("cursor jumped session: %q, want \"c\"", m.cursorSess)
	}
	if m.rows[m.cursor].session != "c" {
		t.Errorf("cursor %d is on %q, want the row for \"c\"",
			m.cursor, m.rows[m.cursor].session)
	}
}

// A client read naming a session with no row yet (brand-new tmux session)
// must not leave the bar on the previous card. cursorSess is the truth; the
// bar goes dark until the row arrives, and reanchor then finds it.
func TestClientLaneSessionWithoutRowParksBar(t *testing.T) {
	m := &model{rows: []row{{session: "a"}, {session: "b"}}, cursor: 0, cursorSess: "a"}
	m.Update(clientSessMsg{name: "new"})
	if m.cursorSess != "new" {
		t.Fatalf("cursorSess = %q, want \"new\"", m.cursorSess)
	}
	if m.cursor != -1 {
		t.Errorf("bar left on a stale card: cursor = %d, want -1", m.cursor)
	}
	m.rows = append(m.rows, row{session: "new"})
	m.reanchorCursor()
	if m.cursor != 2 {
		t.Errorf("row arrived but cursor = %d, want 2", m.cursor)
	}
}

// A read already in flight when the user presses j/k comes back carrying the
// pre-switch session; applying it is the visible flicker (bar bounces back,
// then forward on the next tick). Reads started before the switch are stale
// and dropped; only a read from the current generation may move the bar.
func TestStaleClientReadDroppedAfterSelect(t *testing.T) {
	orig := switchClient
	switchClient = func(string, bool) {}
	defer func() { switchClient = orig }()

	m := &model{rows: []row{{session: "a"}, {session: "b"}}}
	m.selectRow(1, false) // bumps clientGen: gen-0 reads are now stale
	m.Update(clientSessMsg{name: "a", gen: 0})
	if m.cursorSess != "b" {
		t.Errorf("stale read yanked the bar back to %q", m.cursorSess)
	}
	m.Update(clientSessMsg{name: "a", gen: m.clientGen})
	if m.cursorSess != "a" {
		t.Errorf("fresh read not applied: cursorSess = %q", m.cursorSess)
	}
}

// A brand-new session can be cursorSess before the daemon serves its row
// (clientSessMsg parks the bar at -1). The next data message must keep it
// parked — falling back to "any attached row" would clobber cursorSess with
// a session the user never chose.
func TestRebuildKeepsBarParkedWhileRowUnserved(t *testing.T) {
	m := &model{cursorSess: "brand-new", cursor: -1}
	m.Update(fastDataMsg{rows: []row{{session: "old", state: "idle", attached: true}}})
	if m.cursorSess != "brand-new" {
		t.Fatalf("cursorSess walked to %q while its row was unserved", m.cursorSess)
	}
	if m.cursor != -1 {
		t.Fatalf("cursor = %d, want parked at -1", m.cursor)
	}
	// once the daemon serves the row, the bar lands on it
	m.Update(fastDataMsg{rows: []row{
		{session: "brand-new", state: "shell"},
		{session: "old", state: "idle", attached: true},
	}})
	if m.cursor < 0 || m.rows[m.cursor].session != "brand-new" {
		t.Fatalf("cursor = %d (%v), want the brand-new row", m.cursor, m.cursorSess)
	}
}
