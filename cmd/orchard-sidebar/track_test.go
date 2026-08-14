package main

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func strptr(s string) *string { return &s }

// trackLine renders "issue#N | pr#M (status)", showing only the halves that
// exist — no "issue —"/"pr —" placeholders. Exactly one status word per PR.
// prStatus is worst-first; "green" is the narrowest branch on purpose — only a
// literal SUCCESS rollup plus an APPROVED review earns it, so an unknown or
// pending rollup can never read as green (false-green rule).
func TestTrackLine(t *testing.T) {
	cases := []struct {
		name string
		row  row
		want string
	}{
		{"neither", row{}, ""},
		{"issue only", row{issueNum: 719}, "issue#719"},
		{"pr only", row{pr: &prInfo{Number: 9, State: "OPEN"}}, "pr#9 (unresolved)"},
		{
			"both",
			row{issueNum: 719, pr: &prInfo{Number: 9, State: "OPEN", ChecksRollup: "SUCCESS",
				ReviewDecision: strptr("APPROVED")}},
			"issue#719 | pr#9 (green)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trackLine(c.row); got != c.want {
				t.Errorf("trackLine = %q, want %q", got, c.want)
			}
		})
	}
}

// The status word is a precedence ladder, not a set of independent flags: a
// merged PR is "merged" even with a failing rollup, conflicts outrank checks,
// and anything the rollup doesn't positively confirm is "unresolved".
func TestPRStatus(t *testing.T) {
	cases := []struct {
		name string
		pr   prInfo
		want string
	}{
		{"merged wins over everything", prInfo{State: "MERGED", Draft: true,
			ChecksRollup: "FAILURE", MergeStateStatus: "DIRTY"}, "merged"},
		{"closed", prInfo{State: "CLOSED"}, "closed"},
		{"draft outranks conflicts and checks", prInfo{State: "OPEN", Draft: true,
			MergeStateStatus: "DIRTY", ChecksRollup: "FAILURE"}, "draft"},
		{"dirty merge state is conflicts", prInfo{State: "OPEN",
			MergeStateStatus: "DIRTY", ChecksRollup: "SUCCESS"}, "conflicts"},
		{"failure", prInfo{State: "OPEN", ChecksRollup: "FAILURE"}, "failing"},
		{"error", prInfo{State: "OPEN", ChecksRollup: "ERROR"}, "failing"},
		{"timed out", prInfo{State: "OPEN", ChecksRollup: "TIMED_OUT"}, "failing"},
		{"action required", prInfo{State: "OPEN", ChecksRollup: "ACTION_REQUIRED"}, "failing"},
		{"pending is unresolved", prInfo{State: "OPEN", ChecksRollup: "PENDING"}, "unresolved"},
		{"empty rollup is unresolved, never green", prInfo{State: "OPEN",
			ReviewDecision: strptr("APPROVED")}, "unresolved"},
		{"unknown rollup enum is unresolved, never green", prInfo{State: "OPEN",
			ChecksRollup: "NEUTRAL", ReviewDecision: strptr("APPROVED")}, "unresolved"},
		{"success without approval is unresolved", prInfo{State: "OPEN",
			ChecksRollup: "SUCCESS"}, "unresolved"},
		{"changes requested is unresolved", prInfo{State: "OPEN", ChecksRollup: "SUCCESS",
			ReviewDecision: strptr("CHANGES_REQUESTED")}, "unresolved"},
		{"review required is unresolved", prInfo{State: "OPEN", ChecksRollup: "SUCCESS",
			ReviewDecision: strptr("REVIEW_REQUIRED")}, "unresolved"},
		{"success plus approved is green", prInfo{State: "OPEN", ChecksRollup: "SUCCESS",
			MergeStateStatus: "CLEAN", ReviewDecision: strptr("APPROVED")}, "green"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := prStatus(c.pr); got != c.want {
				t.Errorf("prStatus = %q, want %q", got, c.want)
			}
		})
	}
}

// groupLabel headers ship cap-first; the View() doc comment documents this
// casing, so pin it here rather than letting a styling round drift silently.
func TestGroupLabelCasing(t *testing.T) {
	want := map[string]string{
		"input":   "Needs input",
		"stalled": "Stalled",
		"working": "Working",
		"idle":    "Idle",
		"shell":   "Shell",
		"":        "Shell",
	}
	for state, w := range want {
		if got := groupLabel(state); got != w {
			t.Errorf("groupLabel(%q) = %q, want %q", state, got, w)
		}
	}
}

// trunc must clip to the interior width even when a narrow pane drives
// iw = w-3 to zero or below, since View() feeds it unchecked.
func TestTruncAtNarrowInteriorWidth(t *testing.T) {
	for _, w := range []int{0, 1, 2, 3, 4} {
		iw := w - 3
		if got := trunc("Needs input", iw); len([]rune(got)) > max(0, iw) {
			t.Errorf("trunc(_, %d) = %q, longer than %d", iw, got, iw)
		}
	}
}

// Below minWidth the card layout degrades to name-only rather than shredding
// sub-lines; at or above it the full card renders.
func TestCompactModeThreshold(t *testing.T) {
	mk := func(w int) *model {
		return &model{width: w, rows: []row{{
			session: "orchard-sidebar", state: "working", mission: "a mission",
			repo: "git-orchard-rs", issueNum: 719,
		}}}
	}
	narrow := mk(minWidth - 1).View()
	wide := mk(minWidth).View()
	if strings.Contains(narrow, "a mission") || strings.Contains(narrow, "issue#719") {
		t.Errorf("compact view kept detail lines:\n%s", narrow)
	}
	if !strings.Contains(narrow, "orchard-sidebar") {
		t.Errorf("compact view dropped the name:\n%s", narrow)
	}
	if !strings.Contains(wide, "a mission") || !strings.Contains(wide, "issue#719") {
		t.Errorf("full view missing detail lines:\n%s", wide)
	}
	for _, v := range []struct {
		w    int
		body string
	}{{minWidth - 1, narrow}, {minWidth, wide}} {
		for _, l := range strings.Split(v.body, "\n") {
			if ansi.StringWidth(l) > v.w {
				t.Errorf("width %d: line %q is %d cells", v.w, l, ansi.StringWidth(l))
			}
		}
	}
}

// scripts/sidebar-open.sh clamps the pane width to the same floor as minWidth.
// The two live in different languages, so pin them together here rather than
// trusting the cross-reference comments to stay honest.
func TestLauncherWidthFloorMatchesMinWidth(t *testing.T) {
	src, err := os.ReadFile("../../scripts/sidebar-open.sh")
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	re := regexp.MustCompile(`\[ "\$width" -lt (\d+) \] && width=(\d+)`)
	mm := re.FindSubmatch(src)
	if mm == nil {
		t.Fatalf("width clamp not found in sidebar-open.sh — did the clamp move?")
	}
	for _, g := range mm[1:] {
		got, err := strconv.Atoi(string(g))
		if err != nil {
			t.Fatal(err)
		}
		if got != minWidth {
			t.Errorf("launcher floor %d != minWidth %d", got, minWidth)
		}
	}
}

// Selection and the switch are one action: every gesture that moves the cursor
// must also attach that session, and must not run off either end of the list.
func TestSelectRowSwitchesSession(t *testing.T) {
	var got []string
	orig := switchClient
	switchClient = func(s string) { got = append(got, s) }
	defer func() { switchClient = orig }()

	m := &model{rows: []row{{session: "a"}, {session: "b"}, {session: "c"}}}
	m.selectRow(1)
	if m.cursor != 1 || m.cursorSess != "b" {
		t.Fatalf("cursor = %d/%q, want 1/\"b\"", m.cursor, m.cursorSess)
	}
	m.selectRow(-1) // off the top: no move, no switch
	m.selectRow(3)  // off the bottom: same
	if m.cursor != 1 {
		t.Errorf("out-of-range select moved cursor to %d", m.cursor)
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

	// a vanished session must not leave the cursor pointing past the end
	gone := &model{rows: []row{{session: "a"}}, cursor: 5, cursorSess: "zz"}
	gone.reanchorCursor()
	if gone.cursor != 0 {
		t.Errorf("stale cursor = %d, want 0", gone.cursor)
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

// The card quotes the latest ask, not the session's opening one — last_prompt
// is rewritten on every UserPromptSubmit, first_prompt is set once.
func TestPromptOfPrefersLatest(t *testing.T) {
	both := sessFile{FirstPrompt: "the original mission", LastPrompt: "what I just asked"}
	if got := promptOf(both); got != "what I just asked" {
		t.Errorf("promptOf = %q, want the latest prompt", got)
	}
	only := sessFile{FirstPrompt: "the original mission"}
	if got := promptOf(only); got != "the original mission" {
		t.Errorf("promptOf = %q, want the first-prompt fallback", got)
	}
	if got := promptOf(sessFile{}); got != "" {
		t.Errorf("promptOf = %q, want empty", got)
	}
}

// The hook lane re-sorts on every 2s tick. If it doesn't re-anchor afterwards,
// m.cursor keeps pointing at the slot rather than the session, and the bar
// silently walks to a different card while the attached session never moved.
func TestHookTickKeepsCursorOnItsSession(t *testing.T) {
	m := &model{
		rows: []row{
			{session: "b", state: "idle", created: time.Unix(200, 0)},
			{session: "c", state: "idle", created: time.Unix(300, 0)},
		},
		cursor: 1, cursorSess: "c",
	}
	// "a" is older than both, so the re-sort inserts it at index 0 and pushes
	// every existing card down one slot
	now := time.Now()
	m.createdBySess = map[string]time.Time{
		"a": time.Unix(100, 0), "b": time.Unix(200, 0), "c": time.Unix(300, 0)}
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

// Within a group, order is by session creation so a card holds its slot for the
// life of the session. A session tmux hasn't reported yet has a zero creation
// time and must sort last, where it can't displace a settled card.
func TestSortIsCreationOrderWithinGroup(t *testing.T) {
	rows := []row{
		{session: "unknown", state: "idle"},
		{session: "newer", state: "idle", created: time.Unix(300, 0)},
		{session: "busy", state: "working", created: time.Unix(900, 0)},
		{session: "older", state: "idle", created: time.Unix(100, 0)},
	}
	sortRows(rows)
	want := []string{"busy", "older", "newer", "unknown"}
	for i, w := range want {
		if rows[i].session != w {
			t.Fatalf("order = %v, want %v",
				[]string{rows[0].session, rows[1].session, rows[2].session, rows[3].session}, want)
		}
		_ = i
	}
}

// The creation time that orders cards, and the pane->session map the state-dir
// lane folds against, both come from the daemon now — the sidebar no longer
// shells out to tmux for either. foldSessions is the one place that parses it.
func TestFoldSessionsReadsDaemonSnapshot(t *testing.T) {
	ss := []tmuxSession{
		{Name: "with panes", Attached: true, CreatedAt: "2026-08-12T07:12:57Z",
			Windows: []struct {
				Panes []struct {
					PaneId string `json:"paneId"`
				} `json:"panes"`
			}{{Panes: []struct {
				PaneId string `json:"paneId"`
			}{{PaneId: "%0"}, {PaneId: "%28"}}}}},
		{Name: "unparseable", CreatedAt: "not-a-time"},
	}
	attached, created, p2s := foldSessions(ss)
	if !attached["with panes"] || attached["unparseable"] {
		t.Errorf("attached = %v", attached)
	}
	if got := created["with panes"].UTC().Format(time.RFC3339); got != "2026-08-12T07:12:57Z" {
		t.Errorf("createdAt = %q", got)
	}
	// a session whose timestamp we can't read must stay zero, so sortRows puts
	// it last instead of at the top of its group
	if !created["unparseable"].IsZero() {
		t.Errorf("unparseable createdAt = %v, want zero", created["unparseable"])
	}
	// session names contain spaces; the map is keyed by pane id, not parsed text
	if p2s["%0"] != "with panes" || p2s["%28"] != "with panes" {
		t.Errorf("paneToSess = %v", p2s)
	}
}

// A pushed snapshot is fresher than any poll, so it must move attach — that is
// the whole point of the subscription lane. It must NOT clobber state, model or
// title, which it carries nothing about.
func TestSubscriptionSnapshotMovesAttachNotState(t *testing.T) {
	m := &model{
		rows: []row{
			{session: "a", state: "working", model: "opus", attached: true},
			{session: "b", state: "idle"},
		},
		cursorSess: "a",
	}
	m.Update(tmuxSubMsg{sessions: []tmuxSession{
		{Name: "a", Attached: false, CreatedAt: "2026-08-12T07:00:00Z"},
		{Name: "b", Attached: true, CreatedAt: "2026-08-12T08:00:00Z"},
	}})
	byName := map[string]row{}
	for _, r := range m.rows {
		byName[r.session] = r
	}
	if byName["a"].attached || !byName["b"].attached {
		t.Errorf("attach did not follow the snapshot: %+v", byName)
	}
	if byName["a"].state != "working" || byName["a"].model != "opus" {
		t.Errorf("snapshot clobbered fast-lane fields: %+v", byName["a"])
	}
	if m.cursorSess != "a" {
		t.Errorf("subscription moved the cursor: %q, want \"a\" (client lane owns it)", m.cursorSess)
	}
}

// A poll request in flight across a session switch carries pre-switch attach
// flags and lands after the pushed snapshot. If it wins, the selection visibly
// snaps back and the switch appears to take a whole poll cycle (~4s observed).
func TestStalePollDoesNotRevertPushedAttach(t *testing.T) {
	m := &model{}
	m.Update(tmuxSubMsg{sessions: []tmuxSession{
		{Name: "a", Attached: false},
		{Name: "b", Attached: true},
	}})
	if got := m.rows[m.cursor].session; got != "b" {
		t.Fatalf("push should select the attached session, got %q", got)
	}
	// stale poll: still thinks "a" is attached
	m.Update(fastDataMsg{rows: []row{
		{session: "a", state: "idle", attached: true},
		{session: "b", state: "idle", attached: false},
	}})
	for _, r := range m.rows {
		if r.session == "b" && !r.attached {
			t.Error("stale poll reverted the pushed attach for b")
		}
		if r.session == "a" && r.attached {
			t.Error("stale poll re-attached a")
		}
	}
	if got := m.rows[m.cursor].session; got != "b" {
		t.Errorf("cursor snapped back to %q after a stale poll", got)
	}
}

// With the socket down the poll still refreshes the attach flags (they feed
// the display), but the cursor belongs to the client lane and stays put.
func TestPollStillWinsWhenSubscriptionIsDead(t *testing.T) {
	m := &model{}
	m.Update(tmuxSubMsg{sessions: []tmuxSession{{Name: "a"}, {Name: "b", Attached: true}}})
	m.Update(tmuxSubMsg{err: errors.New("socket closed")})
	m.Update(fastDataMsg{rows: []row{
		{session: "a", state: "idle", attached: true},
		{session: "b", state: "idle"},
	}})
	byName := map[string]row{}
	for _, r := range m.rows {
		byName[r.session] = r
	}
	if !byName["a"].attached || byName["b"].attached {
		t.Errorf("dead push lane: poll must drive attach flags, got %+v", byName)
	}
	if got := m.rows[m.cursor].session; got != "b" {
		t.Errorf("cursor moved off %q — attach flags must not drive it", "b")
	}
}

// A single fast-lane timeout must not empty the sidebar. fastQuery spikes past
// its 4s client timeout while tmux churns, i.e. exactly when the user switches.
func TestTransientFastErrorKeepsRows(t *testing.T) {
	m := &model{}
	m.Update(fastDataMsg{rows: []row{
		{session: "a", state: "idle", attached: true},
		{session: "b", state: "idle"},
	}})
	if len(m.rows) != 2 {
		t.Fatalf("setup: want 2 rows, got %d", len(m.rows))
	}
	m.Update(fastDataMsg{err: errors.New("context deadline exceeded")})
	if len(m.rows) != 2 {
		t.Fatalf("a transient fast-lane error emptied the sidebar: %d rows left", len(m.rows))
	}
	if !m.rows[0].attached {
		t.Error("held snapshot lost its attach flag")
	}
}

// ...but a daemon that is genuinely gone must stop being represented.
func TestSustainedFastErrorFallsBackToHookLane(t *testing.T) {
	m := &model{}
	m.Update(fastDataMsg{rows: []row{{session: "a", state: "idle"}}})
	m.fastAt = time.Now().Add(-daemonGone - time.Second)
	m.Update(fastDataMsg{err: errors.New("connection refused")})
	if len(m.rows) != 0 {
		t.Fatalf("daemon gone for >%s but %d stale rows survived", daemonGone, len(m.rows))
	}
}

// The fast lane can time out before it has ever succeeded (measured: fastQuery
// idles at ~0.7s but blows its 4s timeout while tmux churns). A live push lane
// proves the daemon is there, so rows must survive that.
func TestPushLaneAloneKeepsRowsThroughFastLaneError(t *testing.T) {
	m := &model{}
	m.Update(tmuxSubMsg{sessions: []tmuxSession{
		{Name: "a", Attached: true, CreatedAt: "2026-08-13T10:00:00Z"},
		{Name: "b", CreatedAt: "2026-08-13T10:01:00Z"},
	}})
	if len(m.rows) != 2 {
		t.Fatalf("setup: push lane should have seeded 2 rows, got %d", len(m.rows))
	}
	// fastAt is still zero here: the poll has never once come back.
	m.Update(fastDataMsg{err: errors.New("context deadline exceeded")})
	if len(m.rows) != 2 {
		t.Fatalf("fast-lane timeout emptied a sidebar the push lane was feeding: %d rows", len(m.rows))
	}
	if !m.rows[0].attached {
		t.Error("lost the pushed attach flag")
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
	switchClient = func(string) {}
	defer func() { switchClient = orig }()

	m := &model{rows: []row{{session: "a"}, {session: "b"}}}
	m.selectRow(1) // bumps clientGen: gen-0 reads are now stale
	m.Update(clientSessMsg{name: "a", gen: 0})
	if m.cursorSess != "b" {
		t.Errorf("stale read yanked the bar back to %q", m.cursorSess)
	}
	m.Update(clientSessMsg{name: "a", gen: m.clientGen})
	if m.cursorSess != "a" {
		t.Errorf("fresh read not applied: cursorSess = %q", m.cursorSess)
	}
}

// @orchard_sidebar_width is one global setting: a read carrying a different
// value resizes this pane to match. Same value again is a no-op, and junk
// below the readable floor is ignored rather than obeyed.
func TestWidthOptionResizesPane(t *testing.T) {
	var got []int
	orig := resizePane
	resizePane = func(w int) { got = append(got, w) }
	defer func() { resizePane = orig }()

	m := &model{width: 38}
	m.Update(clientSessMsg{name: "a", width: 42})
	if m.desiredWidth != 42 || len(got) != 1 || got[0] != 42 {
		t.Fatalf("desired=%d resizes=%v, want 42/[42]", m.desiredWidth, got)
	}
	m.Update(clientSessMsg{name: "a", width: 42})
	if len(got) != 1 {
		t.Errorf("same width resized again: %v", got)
	}
	m.Update(clientSessMsg{name: "a", width: 10})
	if m.desiredWidth != 42 {
		t.Errorf("sub-floor width obeyed: desired = %d", m.desiredWidth)
	}
}

// Dragging this pane publishes the new width so every session follows (last
// write wins). A size matching what we already want is our own enforcement
// echoing back — no write. Drags below the floor publish the floor, and any
// write bumps the generation so in-flight reads carrying the old width die.
func TestDragWritesWidthBack(t *testing.T) {
	var wrote []int
	origSet := setWidthOption
	setWidthOption = func(w int) { wrote = append(wrote, w) }
	origRes := resizePane
	resizePane = func(int) {}
	defer func() { setWidthOption = origSet; resizePane = origRes }()

	m := &model{}
	m.Update(tea.WindowSizeMsg{Width: 38, Height: 50}) // startup: option unknown, record only
	if len(wrote) != 0 {
		t.Fatalf("startup size published: %v", wrote)
	}
	m.desiredWidth = 42
	gen := m.clientGen
	m.Update(tea.WindowSizeMsg{Width: 45, Height: 50}) // drag
	if len(wrote) != 1 || wrote[0] != 45 || m.desiredWidth != 45 {
		t.Fatalf("drag: wrote=%v desired=%d, want [45]/45", wrote, m.desiredWidth)
	}
	if m.clientGen == gen {
		t.Errorf("drag did not bump clientGen; stale reads would fight the new width")
	}
	m.Update(tea.WindowSizeMsg{Width: 45, Height: 50}) // enforcement echo
	if len(wrote) != 1 {
		t.Errorf("echo republished: %v", wrote)
	}
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 50}) // shoved below the floor
	if wrote[len(wrote)-1] != minWidth {
		t.Errorf("sub-floor drag published %d, want %d", wrote[len(wrote)-1], minWidth)
	}
}

// A session whose name the daemon didn't join to a worktree still gets branch
// data when its cwd is exactly a worktree's path (two sessions sharing one
// checkout: the daemon's tmuxSession join is 1:1, the second session loses).
func TestJoinFallsBackToCwdPathMatch(t *testing.T) {
	m := &model{
		wtBySession: map[string]wtInfo{"named": {Branch: "feat/x"}},
		repoBySess:  map[string]string{"named": "r1"},
		wtByPath:    map[string]wtInfo{"/w/titw": {Branch: "main"}},
		repoByPath:  map[string]string{"/w/titw": "titw"},
		rows: []row{
			{session: "named", cwd: "/w/titw"},          // name join wins over cwd
			{session: "orphan", cwd: "/w/titw/"},        // trailing slash still matches
			{session: "nested", cwd: "/w/titw/sub/dir"}, // subdir: no match (nested worktrees make prefix joins wrong)
			{session: "nocwd"},
		},
	}
	m.join()
	if got := m.rows[0].branch; got != "feat/x" {
		t.Errorf("name-joined row branch = %q, want feat/x (cwd fallback must not override)", got)
	}
	if got := m.rows[1].branch; got != "main" {
		t.Errorf("cwd-matched row branch = %q, want main", got)
	}
	if got := m.rows[1].repo; got != "titw" {
		t.Errorf("cwd-matched row repo = %q, want titw", got)
	}
	if got := m.rows[2].branch; got != "" {
		t.Errorf("subdir row branch = %q, want empty (exact match only)", got)
	}
	if got := m.rows[3].branch; got != "" {
		t.Errorf("cwd-less row branch = %q, want empty", got)
	}
}

// The cwd-fallback join reads row.cwd, which only the hook overlay supplies —
// so every handler that rebuilds or overlays rows must run applyHooks() before
// join(). A session known only to the hook lane (daemon has no row for it)
// exercises the ordering in each handler: with join first (or missing), the
// hook-appended row never gets its branch.
func TestJoinRunsAfterHooksInEveryHandler(t *testing.T) {
	wt := map[string]wtInfo{"/w/titw": {Branch: "main"}}
	repo := map[string]string{"/w/titw": "titw"}
	hooks := map[string]hookState{"hookonly": {state: "idle", cwd: "/w/titw"}}
	base := func() *model {
		return &model{wtByPath: wt, repoByPath: repo, hooksBySess: hooks}
	}
	check := func(t *testing.T, name string, m *model) {
		t.Helper()
		for _, r := range m.rows {
			if r.session == "hookonly" {
				if r.branch != "main" || r.repo != "titw" {
					t.Errorf("%s: hook-only row branch=%q repo=%q, want main/titw", name, r.branch, r.repo)
				}
				return
			}
		}
		t.Errorf("%s: hook-only row missing entirely", name)
	}

	m := base()
	m.Update(fastDataMsg{rows: []row{{session: "daemon"}}})
	check(t, "fastDataMsg success", m)

	m = base()
	m.rows = []row{{session: "daemon"}}
	m.fastAt = time.Now() // transient failure: rows held
	m.Update(fastDataMsg{err: errors.New("spike")})
	check(t, "fastDataMsg failure", m)

	m = base()
	m.hooksBySess = nil
	m.Update(hookDataMsg{bySession: hooks, dirOK: true})
	check(t, "hookDataMsg", m)

	m = base()
	m.Update(tmuxSubMsg{sessions: []tmuxSession{{Name: "daemon"}}})
	check(t, "tmuxSubMsg", m)
}
