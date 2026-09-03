package main

import (
	"sort"
	"time"

	"github.com/drewdrewthis/orchardist/internal/release"
)

type row struct {
	session  string
	state    string // input | stalled | working | idle | shell
	attached bool
	hooked   bool // state came from the state dir; false = daemon inference
	fake     bool // synthetic scroll-test row (ORCHARD_SIDEBAR_FAKE); never attachable
	mission  string
	lastAct  time.Time
	cwd      string
	// slow-lane join, may be zero-valued until the first slow fetch lands
	branch     string
	repo       string
	ahead      *int
	behind     *int
	pr         *prInfo
	model      string
	issueNum   int
	issueTitle string
}

// The sidebar answers one question — what needs me? — so rows fall into three
// sections, not five states:
//
//	bucketAttention  a human has to do something: Claude asked a question, is
//	                 waiting on a permission prompt (state "input"), or the
//	                 session stopped mid-turn and needs a nudge ("stalled").
//	bucketDone       a turn finished and nobody has read the result: state
//	                 "idle", the state file said so (hooked), and you are not
//	                 attached to it. Attached-and-idle is not "done" — you are
//	                 looking at it right now.
//	bucketRunning    everything else: working sessions (the spinner already
//	                 says they're busy, so they need no section of their own),
//	                 attached idle ones, plain shells, and any session whose
//	                 state we only inferred.
//
// Nothing outside this function decides which section a row lands in.
type bucket int

const (
	bucketAttention bucket = iota
	bucketDone
	bucketRunning
)

func rowBucket(r row) bucket {
	switch r.state {
	case "input", "stalled":
		return bucketAttention
	case "idle":
		if r.hooked && !r.attached {
			return bucketDone
		}
	}
	return bucketRunning
}

type model struct {
	rows           []row
	fakes          []row // synthetic rows, resolved once at startup (fake.go)
	wtBySession    map[string]wtInfo
	repoBySess     map[string]string
	wtByPath       map[string]wtInfo // cwd fallback: worktree path -> info
	repoByPath     map[string]string
	hooksBySess    map[string]hookState
	paneToSess     map[string]string // daemon-served pane id -> session
	frame          int               // animation frame, advanced by animTickMsg
	stateDirOK     bool
	cursor         int
	cursorSess     string    // session under the cursor, so re-sorts don't drag it
	copiedUntil    time.Time // git box shows "✓ copied" until this passes
	err            error
	subErr         error           // websocket lane only; a drop degrades to polling
	subAt          time.Time       // last pushed snapshot
	attachedBySess map[string]bool // from the push lane; outranks the poll's copy
	fastAt         time.Time
	width          int
	height         int  // pane rows; 0 until the first WindowSizeMsg
	sized          bool // a WindowSizeMsg has landed; the next one can be a drag
	collapsed      bool // pane shrunk to the 3-column strip
	scroll         int  // first list line on screen; the wheel drives this directly
	// snapSel is set by the events that MOVE the selection (a click, j/k, an
	// attach that happened elsewhere) and cleared by the compose that acts on
	// it. It is what keeps the viewport still for everything else: a data
	// refresh, a re-sort, or a wheel roll must never yank the list back to the
	// selected card.
	snapSel      bool
	anchorSess   string    // session of the card the viewport is anchored to
	anchorDelta  int       // lines from that card's first line to the top of the view
	desiredWidth int       // the width this sidebar last published; 0 until sized
	clientGen    int       // bumped on switch; older in-flight reads are stale
	menu         rowMenu   // the right-click row menu; zero value = closed (menu.go)
	pane         paneFrame // the composed pane: what View paints, what a click reads
	// the / filter (filter.go). Zero value = closed with no query, which is
	// also "every row is visible" — nothing has to be initialised for the
	// unfiltered list to be the list.
	filter filterState
	// the bell (bell.go): whether it rings at all, which sessions have already
	// been counted as needing attention, and whether the first list has landed
	// (a startup snapshot is not a transition).
	bell       bool
	attnSeen   map[string]bool
	attnSeeded bool
	// the update indicator (update.go/updateview.go): the last cache read,
	// whether its overlay is open, and whether a state-dir resolve failure
	// has already been logged once this run.
	updateCheck     release.Check
	updateOpen      bool
	updateLogFailed bool
}

// rowAt is a bounds-checked row lookup, and selRow the same for the selection.
// Every reader goes through them: the cursor can legitimately be -1 (a session
// the daemon has not served a row for yet, see reanchorCursor), and an
// open-coded `>= 0 && < len` at each call site is one edit away from a panic.
func (m *model) rowAt(i int) (row, bool) {
	if i < 0 || i >= len(m.rows) {
		return row{}, false
	}
	return m.rows[i], true
}

// selRow is the ATTACHED session's row. What the pane draws and describes is
// railRow (filter.go), which is this one until a filter hides it.
func (m *model) selRow() (row, bool) { return m.rowAt(m.cursor) }

type fastTickMsg struct{}

type slowTickMsg struct{}

type fastDataMsg struct {
	rows []row
	// pane id (%5) -> session name: daemon-served, replacing what the client
	// used to get by exec'ing tmux.
	paneToSess map[string]string
	err        error
}

type slowDataMsg struct {
	wtBySession map[string]wtInfo
	repoBySess  map[string]string
	wtByPath    map[string]wtInfo
	repoByPath  map[string]string
	err         error
}

// subFresh is how long a pushed snapshot outranks the poll. The server pings
// every 10s, so anything past that and the socket is not delivering.
const subFresh = 30 * time.Second

// daemonGone is how long the fast lane must stay broken before the sidebar
// believes the daemon is actually gone and falls back to the hook lane alone.
// Comfortably above the 4s client timeout so one slow response cannot trip it.
const daemonGone = 15 * time.Second

type animTickMsg struct{}

type clientTickMsg struct{}

// sortRows: section first, then most recent activity within the section, then
// name. The section jump is the only movement a user should ever see — a card
// changing state — so activity is the tie-break the eye can follow: within a
// section the thing you touched last is the thing you are coming back to.
// A session with no activity timestamp sorts last rather than first, so an
// unknown can't displace a settled card.
func sortRows(rows []row) {
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := rowBucket(rows[i]), rowBucket(rows[j]); a != b {
			return a < b
		}
		li, lj := rows[i].lastAct, rows[j].lastAct
		if li.IsZero() != lj.IsZero() {
			return lj.IsZero()
		}
		if !li.Equal(lj) {
			return li.After(lj)
		}
		return rows[i].session < rows[j].session
	})
}

// daemonDown is the one judgment "the daemon is actually unreachable" —
// shared by the row wipe and the offline banner so they can never disagree.
// A recent fast-lane success holds through a transient spike, and a live push
// lane is itself proof of reachability.
func (m *model) daemonDown() bool {
	return m.err != nil && !m.subLive() &&
		(m.fastAt.IsZero() || time.Since(m.fastAt) > daemonGone)
}

// subLive reports whether the push lane is delivering. One missed keepalive
// window is enough slack that a momentary hiccup doesn't hand attach back to
// the poll; a real drop degrades to polling within it.
func (m *model) subLive() bool {
	return m.subErr == nil && !m.subAt.IsZero() && time.Since(m.subAt) < subFresh
}
