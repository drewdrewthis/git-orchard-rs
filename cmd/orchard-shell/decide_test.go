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
		{"wrapper up, inner client live", outerState{sessionExists: true, paneExists: true, innerLive: true}, actionAttach},
		{"wrapper up, inner client dead", outerState{sessionExists: true, paneExists: true}, actionRespawn},
		{"session exists but has no pane 0.1", outerState{sessionExists: true}, actionBroken},
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

// @scenario Rerunning orchard shell reattaches instead of erroring
//
// AC3: a re-run against a live wrapper attaches and creates nothing.
func TestEnsureReady_LiveWrapperOnlyFocusesAndAttaches(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_tty}"), "/dev/ttys004").
		reply(innerCall("list-clients", "-F", "#{client_tty}"), "/dev/ttys004\n/dev/ttys009")
	w := testWrapper(f)

	if err := w.ensureReady(); err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	if f.called("new-session") || f.called("split-window") || f.called("respawn-pane") {
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
		reply(outerCall("display", "-p", "-t", paneInner, "#{pane_tty}"), "/dev/ttys004").
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

func TestEnsureReady_SessionWithoutPane01IsRefused(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		fail(outerCall("display", "-p", "-t", paneInner, "#{pane_tty}"), "can't find pane")
	w := testWrapper(f)

	err := w.ensureReady()
	if err == nil {
		t.Fatal("ensureReady succeeded against a session with no pane 0.1")
	}
	if !strings.Contains(err.Error(), "kill-session") {
		t.Errorf("error %q does not name the remedy", err)
	}
	if got := f.mutations(); len(got) != 0 {
		t.Errorf("a broken wrapper was mutated: %v", got)
	}
}
