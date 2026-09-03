package main

import (
	"errors"
	"testing"
	"time"
)

// Which lane wins, and what a lane failing is allowed to wipe. The sidebar
// reads the same facts from three places at different freshnesses, and every
// rule here was learned from a defect.

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

// One flat list ordered by attach recency: most recently attached first, so a
// card moves only when you attach it — never because its state ticked. Sessions
// never attached fall below every attached one and order among themselves by
// creation time (newest first), then name, so the order is total and stable.
func TestSortIsMostRecentlyAttachedFirst(t *testing.T) {
	now := time.Now()
	rows := []row{
		{session: "never-old", state: "working", created: now.Add(-2 * time.Hour)},
		{session: "old", state: "idle", lastAttached: now.Add(-time.Hour)},
		{session: "never-new", state: "input", created: now.Add(-time.Minute)},
		{session: "fresh", state: "working", lastAttached: now.Add(-time.Minute)},
		{session: "newest", state: "idle", lastAttached: now},
	}
	sortRows(rows)
	// attached (by last_attached desc) before never-attached (by created desc);
	// state is irrelevant to position
	want := []string{"newest", "fresh", "old", "never-new", "never-old"}
	got := make([]string, len(rows))
	for i := range rows {
		got[i] = rows[i].session
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// The last tie-break is the session name, so two sessions with identical
// timestamps (or none at all) still have one fixed order rather than shimmering
// between paints.
func TestSortTieBreaksByName(t *testing.T) {
	rows := []row{{session: "charlie"}, {session: "alpha"}, {session: "bravo"}}
	sortRows(rows)
	want := []string{"alpha", "bravo", "charlie"}
	for i := range want {
		if rows[i].session != want[i] {
			t.Fatalf("order = %v, want %v", rows, want)
		}
	}
}

// Attach state, and the pane->session map the state-dir lane folds against,
// both come from the daemon now — the sidebar no longer shells out to tmux for
// either. foldSessions is the one place that reads the snapshot.
func TestFoldSessionsReadsDaemonSnapshot(t *testing.T) {
	ss := []tmuxSession{
		{Name: "with panes", Attached: true,
			Windows: []struct {
				Panes []struct {
					PaneId string `json:"paneId"`
				} `json:"panes"`
			}{{Panes: []struct {
				PaneId string `json:"paneId"`
			}{{PaneId: "%0"}, {PaneId: "%28"}}}}},
		{Name: "detached"},
	}
	attached, p2s := foldSessions(ss)
	if !attached["with panes"] || attached["detached"] {
		t.Errorf("attached = %v", attached)
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
		{Name: "a", Attached: false},
		{Name: "b", Attached: true},
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
		{Name: "a", Attached: true},
		{Name: "b"},
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
