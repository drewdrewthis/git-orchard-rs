package main

import (
	"strings"
	"testing"
)

// The reattach decision table: what orchard shell does about the wrapper that
// is (or is not) already there.
func TestDecide_ReattachTable(t *testing.T) {
	cases := []struct {
		name  string
		state outerState
		want  action
	}{
		{"nothing running", outerState{}, actionBoot},
		{"wrapper up, both panes alive, inner client live", outerState{sessionExists: true, paneCount: 2, innerLive: true}, actionAttach},
		{"wrapper up, inner client dead", outerState{sessionExists: true, paneCount: 2}, actionRespawn},
		{"wrapper up, sidebar (0.0) dead", outerState{sessionExists: true, paneCount: 2, pane0Dead: true, innerLive: true}, actionRespawn},
		{"wrapper up, inner pane (0.1) itself dead", outerState{sessionExists: true, paneCount: 2, pane1Dead: true}, actionRespawn},
		{"wrapper up, both panes dead", outerState{sessionExists: true, paneCount: 2, pane0Dead: true, pane1Dead: true}, actionRespawn},
		{"session collapsed to one pane", outerState{sessionExists: true, paneCount: 1}, actionRebuild},
		{"session has an extra third pane", outerState{sessionExists: true, paneCount: 3}, actionRebuild},
		{"session has no panes at all", outerState{sessionExists: true, paneCount: 0}, actionRebuild},
	}
	for _, c := range cases {
		if got := decide(c.state); got != c.want {
			t.Errorf("%s: decide(%+v) = %v; want %v", c.name, c.state, got, c.want)
		}
	}
}

// @scenario First run boots the outer session and attaches
func TestProbe_NoOuterSessionMeansBoot(t *testing.T) {
	f := newFakeTmux().fail(outerCall("has-session", "-t", outerSessionName), "no server running")
	w := testWrapper(f)

	if got := decide(w.probe()); got != actionBoot {
		t.Fatalf("decide = %v; want actionBoot", got)
	}
}

// panesCall renders the list-panes argv probe() uses to read window 0's
// shape, for registering fake replies.
func panesCall() string {
	return outerCall("list-panes", "-t", outerSessionName+":0", "-F", "#{pane_index} #{pane_dead} #{pane_tty}")
}

// paneIDsCall renders the list-panes argv rebuild() uses to find what to
// keep and what to kill.
func paneIDsCall() string {
	return outerCall("list-panes", "-t", outerSessionName+":0", "-F", "#{pane_id}")
}

// @scenario Rerunning orchard shell reattaches instead of erroring
//
// AC3: a re-run against a live wrapper attaches and creates nothing.
func TestEnsureReady_LiveWrapperOnlyFocusesAndAttaches(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "0 0 /dev/ttys013\n1 0 /dev/ttys004").
		reply(innerCall("list-clients", "-F", "#{client_tty}"), "/dev/ttys004\n/dev/ttys009")
	w := testWrapper(f)

	if err := w.ensureReady(); err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	if f.called("new-session") || f.called("split-window") || f.called("respawn-pane") || f.called("kill-pane") {
		t.Errorf("a live wrapper was rebuilt; calls: %v", f.mutations())
	}
	want := outerCall("select-pane", "-t", paneInner)
	if !f.called(want) {
		t.Errorf("focus was not returned to the inner pane; calls: %v", f.calls)
	}
}

// @scenario Reattach respawns a dead inner client
//
// AC3: outer session alive, pane 0.1's inner client dead — respawn rather
// than attach to a corpse.
func TestEnsureReady_DeadInnerClientIsRespawned(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "0 0 /dev/ttys013\n1 0 /dev/ttys004").
		reply(innerCall("list-clients", "-F", "#{client_tty}"), "/dev/ttys077").
		reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"), "100 work")
	w := testWrapper(f)

	if err := w.ensureReady(); err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	respawn := outerCall("respawn-pane", "-k", "-t", paneInner, innerAttachCommand("inner-test", "work"))
	if !f.called(respawn) {
		t.Errorf("pane 0.1 was not respawned with a fresh inner attach; calls: %v", f.mutations())
	}
	if f.called("new-session") {
		t.Errorf("a second outer session was created; calls: %v", f.mutations())
	}
}

// @scenario Reattach respawns a dead sidebar pane
//
// #747 live defect: the sidebar process died, and without remain-on-exit
// tmux closed 0.0 and renumbered the survivor down to 0.0, so a bare
// `select-pane -t shell:0.1` then failed with "can't find pane: 1". With
// remain-on-exit, pane 0.0 stays a DEAD pane at its own index instead — this
// is the two-pane, sidebar-dead shape that must respawn, not rebuild.
func TestEnsureReady_DeadSidebarIsRespawned(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "0 1 /dev/ttys013\n1 0 /dev/ttys004").
		reply(innerCall("list-clients", "-F", "#{client_tty}"), "/dev/ttys004").
		reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"), "100 work")
	w := testWrapper(f)

	if err := w.ensureReady(); err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	if !f.called(outerCall("respawn-pane", "-k", "-t", paneSidebar)) {
		t.Errorf("pane 0.0 was not respawned; calls: %v", f.mutations())
	}
	if !f.called(outerCall("respawn-pane", "-k", "-t", paneInner)) {
		t.Errorf("pane 0.1 was not also respawned; calls: %v", f.mutations())
	}
	if f.called("kill-pane") || f.called("new-session") || f.called("split-window") {
		t.Errorf("a dead sidebar in an otherwise-valid two-pane window triggered a rebuild instead of a respawn; calls: %v", f.mutations())
	}
}

// @scenario Reattach respawns when pane 0.1 itself (not just its inner client) is dead
func TestEnsureReady_DeadInnerPaneIsRespawned(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "0 0 /dev/ttys013\n1 1 /dev/ttys004").
		reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"), "100 work")
	w := testWrapper(f)

	if err := w.ensureReady(); err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	if f.called(innerCall("list-clients", "-F", "#{client_tty}")) {
		t.Errorf("checked the inner client of a pane that is itself already dead; calls: %v", f.calls)
	}
	if !f.called(outerCall("respawn-pane", "-k", "-t", paneSidebar)) ||
		!f.called(outerCall("respawn-pane", "-k", "-t", paneInner)) {
		t.Errorf("both panes were not relaunched; calls: %v", f.mutations())
	}
}

// @scenario Reattach respawns when both panes are dead
func TestEnsureReady_BothPanesDeadIsRespawned(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "0 1 /dev/ttys013\n1 1 /dev/ttys004").
		reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"), "100 work")
	w := testWrapper(f)

	if err := w.ensureReady(); err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	if !f.called(outerCall("respawn-pane", "-k", "-t", paneSidebar)) {
		t.Errorf("pane 0.0 was not respawned; calls: %v", f.mutations())
	}
	if !f.called(outerCall("respawn-pane", "-k", "-t", paneInner)) {
		t.Errorf("pane 0.1 was not respawned; calls: %v", f.mutations())
	}
}

// The sidebar's ORCHARD_TMUX_CLIENT names pane 0.1's tty, and respawn-pane
// gives the pane a new one — so the sidebar must be relaunched with the tty
// read AFTER the respawn, or every switch-client is scoped to a dead client.
func TestRespawn_RelaunchesTheSidebarWithTheNewTTY(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_tty}"), "/dev/ttys100").
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_id}"), "%7")
	w := testWrapper(f)

	if err := w.respawn("work"); err != nil {
		t.Fatalf("respawn: %v", err)
	}

	var sidebarCall string
	for _, c := range f.calls {
		if strings.Contains(c, paneSidebar) {
			sidebarCall = c
		}
	}
	if sidebarCall == "" {
		t.Fatalf("pane 0.0 was not relaunched; calls: %v", f.mutations())
	}
	if !strings.Contains(sidebarCall, "ORCHARD_TMUX_CLIENT=/dev/ttys100") {
		t.Errorf("pane 0.0 relaunched with %q; want the post-respawn tty /dev/ttys100", sidebarCall)
	}
	if !strings.Contains(sidebarCall, "ORCHARD_OUTER_PANE=%7") {
		t.Errorf("pane 0.0 relaunched with %q; want ORCHARD_OUTER_PANE=%%7", sidebarCall)
	}
}

// @scenario Reattach rebuilds a collapsed one-pane window
//
// #747 live defect: the sidebar died, and without remain-on-exit tmux closed
// 0.0 and renumbered the survivor down to 0.0, then a bare
// `select-pane -t shell:0.1` failed outright with "can't find pane: 1". A
// rerun must always self-heal to a correct two-pane layout instead of
// refusing — this covers a session left in that one-pane shape (predating
// remain-on-exit, or after a manual pane close).
func TestEnsureReady_OnePaneWindowIsRebuilt(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "0 0 /dev/ttys013").
		reply(paneIDsCall(), "%1").
		reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"), "100 work")
	w := testWrapper(f)

	if err := w.ensureReady(); err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	if f.called("kill-pane") {
		t.Errorf("only one pane existed; nothing should have been killed: %v", f.mutations())
	}
	split := outerCall("split-window", "-h", "-b", "-l", "40", "-t", "%1")
	if !f.called(split) {
		t.Errorf("did not split off the surviving pane; calls: %v", f.mutations())
	}
	if !f.called(outerCall("respawn-pane", "-k", "-t", paneSidebar)) ||
		!f.called(outerCall("respawn-pane", "-k", "-t", paneInner)) {
		t.Errorf("both panes were not relaunched after the rebuild; calls: %v", f.mutations())
	}
	if !f.called(outerCall("select-pane", "-t", paneInner)) {
		t.Errorf("focus was not returned to the inner pane after rebuild; calls: %v", f.calls)
	}
}

// @scenario Reattach rebuilds a window with an extra third pane
func TestEnsureReady_ThreePaneWindowIsRebuilt(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "0 0 /dev/ttys013\n1 0 /dev/ttys004\n2 0 /dev/ttys020").
		reply(paneIDsCall(), "%1\n%2\n%3").
		reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"), "100 work")
	w := testWrapper(f)

	if err := w.ensureReady(); err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	if !f.called(outerCall("kill-pane", "-t", "%2")) || !f.called(outerCall("kill-pane", "-t", "%3")) {
		t.Errorf("the two extra panes were not killed; calls: %v", f.mutations())
	}
	if f.called(outerCall("kill-pane", "-t", "%1")) {
		t.Errorf("the kept pane was killed; calls: %v", f.mutations())
	}
	split := outerCall("split-window", "-h", "-b", "-l", "40", "-t", "%1")
	if !f.called(split) {
		t.Errorf("did not split off the kept pane; calls: %v", f.mutations())
	}
}

// @scenario Reattach rebuilds when the outer session has no panes at all
//
// Unreachable through normal use, but rebuild must not misbehave on it: it
// falls back to a fresh boot.
func TestEnsureReady_ZeroPaneWindowFallsBackToBoot(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "").
		reply(paneIDsCall(), "").
		reply(innerCall("list-sessions", "-F", "#{session_last_attached} #{session_name}"), "100 work")
	w := testWrapper(f)

	if err := w.ensureReady(); err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	if !f.called("new-session") {
		t.Errorf("zero panes did not fall back to boot; calls: %v", f.mutations())
	}
}
