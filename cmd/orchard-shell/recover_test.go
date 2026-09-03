package main

import (
	"strings"
	"testing"
	"time"
)

// recover_test.go — issue #796: outer-shell pane recovery.
//
// Contract under test (not yet implemented; lives in a future recover.go):
//
//	type recoverAction int
//	const (
//		actReattachInner recoverAction = iota
//		actNewInnerSession
//		actRespawnSidebar
//		actCrashLoopHalt
//	)
//
//	type recoverInput struct {
//		Pane             string // "sidebar" | "inner"
//		ExitStatus       int
//		InnerHasSessions bool      // only meaningful when Pane == "inner"
//		History          []time.Time // prior restart timestamps for this pane
//		Now              time.Time
//	}
//
//	func decideRecovery(in recoverInput) (recoverAction, string)
//
// decideRecovery is the pure decision: given which pane died, its exit
// status, whether the inner server still has sessions, and the pane's own
// restart history, what should orchard-shell do about it and what should
// the status line say.
//
// Crash-loop bound (AC3, amended for AC1): more than 5 restarts of the SAME
// pane within the trailing 60s window (relative to Now) halts recovery for
// that pane rather than respawning it again.

func at(seconds int) time.Time {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(seconds) * time.Second)
}

// historyWithin builds n restart timestamps, evenly spaced across the last
// windowSeconds before now.
func historyWithin(n, windowSeconds int, now time.Time) []time.Time {
	var h []time.Time
	for i := 0; i < n; i++ {
		h = append(h, now.Add(-time.Duration(windowSeconds)*time.Second/time.Duration(n+1)*time.Duration(i+1)))
	}
	return h
}

func TestDecideRecovery_Table(t *testing.T) {
	now := at(1000)

	cases := []struct {
		name       string
		in         recoverInput
		wantAction recoverAction
		wantMsg    string
	}{
		{
			name:       "inner client exited cleanly, session still there — reattach",
			in:         recoverInput{Pane: "inner", ExitStatus: 0, InnerHasSessions: true, Now: now},
			wantAction: actReattachInner,
			wantMsg:    "inner tmux exited (status 0) — reattached",
		},
		{
			name:       "inner client exited with a nonzero status, session still there — reattach",
			in:         recoverInput{Pane: "inner", ExitStatus: 1, InnerHasSessions: true, Now: now},
			wantAction: actReattachInner,
			wantMsg:    "inner tmux exited (status 1) — reattached",
		},
		{
			name:       "inner server has no sessions — new session",
			in:         recoverInput{Pane: "inner", ExitStatus: 0, InnerHasSessions: false, Now: now},
			wantAction: actNewInnerSession,
		},
		{
			name:       "sidebar exited — respawn",
			in:         recoverInput{Pane: "sidebar", ExitStatus: 1, Now: now},
			wantAction: actRespawnSidebar,
		},
		{
			name: "sidebar restarted 5 times in the last 60s — still under the bound, respawn again",
			in: recoverInput{
				Pane: "sidebar", ExitStatus: 1,
				History: historyWithin(5, 60, now),
				Now:     now,
			},
			wantAction: actRespawnSidebar,
		},
		{
			name: "sidebar restarted more than 5 times in the last 60s — crash loop halt",
			in: recoverInput{
				Pane: "sidebar", ExitStatus: 1,
				History: historyWithin(6, 60, now),
				Now:     now,
			},
			wantAction: actCrashLoopHalt,
			wantMsg:    "sidebar keeps crashing — see sidebar.log; press M-r to retry",
		},
		{
			name: "sidebar's crash history is old (outside the 60s window) — bound does not trip",
			in: recoverInput{
				Pane: "sidebar", ExitStatus: 1,
				History: []time.Time{now.Add(-5 * time.Minute), now.Add(-4 * time.Minute), now.Add(-3 * time.Minute), now.Add(-2 * time.Minute), now.Add(-90 * time.Second), now.Add(-70 * time.Second)},
				Now:     now,
			},
			wantAction: actRespawnSidebar,
		},
		{
			// AC amendment: the inner pane gets the same crash-loop bound as
			// the sidebar.
			name: "inner pane restarted more than 5 times in the last 60s — crash loop halt",
			in: recoverInput{
				Pane: "inner", ExitStatus: 0, InnerHasSessions: true,
				History: historyWithin(6, 60, now),
				Now:     now,
			},
			wantAction: actCrashLoopHalt,
			wantMsg:    "inner tmux keeps exiting — press M-r to retry",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotAction, gotMsg := decideRecovery(c.in)
			if gotAction != c.wantAction {
				t.Errorf("decideRecovery(%+v) action = %v; want %v", c.in, gotAction, c.wantAction)
			}
			if c.wantMsg != "" && gotMsg != c.wantMsg {
				t.Errorf("decideRecovery(%+v) message = %q; want %q", c.in, gotMsg, c.wantMsg)
			}
		})
	}
}

// M-r must retry whichever pane is currently halted by the crash-loop bound
// — the amendment applies the same recovery path to both panes.
func TestDecideRecovery_CrashLoopHaltAppliesToBothPanes(t *testing.T) {
	now := at(2000)
	for _, pane := range []string{"sidebar", "inner"} {
		in := recoverInput{Pane: pane, InnerHasSessions: true, History: historyWithin(6, 60, now), Now: now}
		action, msg := decideRecovery(in)
		if action != actCrashLoopHalt {
			t.Errorf("pane %q: action = %v; want actCrashLoopHalt", pane, action)
		}
		if !strings.Contains(msg, "press M-r to retry") {
			t.Errorf("pane %q: message %q does not mention the M-r retry", pane, msg)
		}
	}
}

// --- outer.conf: the pane-died hook and the M-r bind -----------------------

// AC4: recovery hooks live in outer.conf and call back into orchard-shell —
// they must not depend on ~/.tmux.conf, so the wiring itself lives here in
// the embedded conf, not scattered send-keys from Go.
func TestEmbeddedConf_HasPaneDiedHookCallingRecoverPane(t *testing.T) {
	conf := string(embeddedConf)
	if !strings.Contains(conf, "pane-died") && !strings.Contains(conf, "pane-exited") {
		t.Fatal("outer.conf has no pane-died/pane-exited hook")
	}
	if !strings.Contains(conf, "recover-pane") {
		t.Error("outer.conf's death hook does not call an orchard-shell recover-pane subcommand")
	}
}

// AC3: a root-table M-r bind restarts a crash-looped pane on demand.
func TestEmbeddedConf_HasRootTableRetryBind(t *testing.T) {
	conf := string(embeddedConf)
	found := false
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "bind -n M-r ") {
			found = true
			if !strings.Contains(line, "recover-pane") {
				t.Errorf("M-r bind %q does not invoke recover-pane", line)
			}
		}
	}
	if !found {
		t.Error("outer.conf has no root-table (bind -n) M-r retry bind")
	}
}

// --- detach-on-destroy off on the inner server (AC0) ------------------------

// AC0: orchard-shell itself sets detach-on-destroy off on the INNER server
// it attaches to, not the user's ~/.tmux.conf — so that killing the client's
// current session switches the client to another session instead of
// detaching it and leaving pane 0.1 a corpse.
func TestBoot_SetsDetachOnDestroyOffOnInnerServer(t *testing.T) {
	f := newFakeTmux()
	w := testWrapper(f)

	if err := w.boot("work"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	want := innerCall("set-option", "-g", "detach-on-destroy", "off")
	if !f.called(want) {
		t.Errorf("boot did not set detach-on-destroy off on the inner server; calls: %v", f.calls)
	}
}

// ensureReady() also reattaches on a plain rerun against a live wrapper —
// AC0 must hold on that path too, not only on a fresh boot.
func TestEnsureReady_SetsDetachOnDestroyOffOnInnerServer(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "0 0 /dev/ttys013\n1 0 /dev/ttys004").
		reply(innerCall("list-clients", "-F", "#{client_tty}"), "/dev/ttys004")
	w := testWrapper(f)

	if err := w.ensureReady(); err != nil {
		t.Fatalf("ensureReady: %v", err)
	}
	want := innerCall("set-option", "-g", "detach-on-destroy", "off")
	if !f.called(want) {
		t.Errorf("ensureReady did not set detach-on-destroy off on the inner server; calls: %v", f.calls)
	}
}

// --- doctor: dead panes + last recovery event (AC4) -------------------------

// Contract: a recoveryEvent is what recover-pane appends to the recovery
// log every time it acts; readLastRecoveryEvent reads the most recent one
// back for doctor to surface.
//
//	type recoveryEvent struct {
//		Pane    string
//		Action  recoverAction
//		Message string
//		At      time.Time
//	}
//
//	func appendRecoveryLog(path string, ev recoveryEvent) error
//	func readLastRecoveryEvent(path string) (recoveryEvent, bool)

func TestRecoveryLog_RoundTripsTheLastEvent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/recovery.log"

	first := recoveryEvent{Pane: "inner", Action: actReattachInner, Message: "inner tmux exited (status 0) — reattached", At: at(10)}
	second := recoveryEvent{Pane: "sidebar", Action: actRespawnSidebar, Message: "sidebar exited (status 1) — respawned", At: at(20)}

	if err := appendRecoveryLog(path, first); err != nil {
		t.Fatalf("appendRecoveryLog(first): %v", err)
	}
	if err := appendRecoveryLog(path, second); err != nil {
		t.Fatalf("appendRecoveryLog(second): %v", err)
	}

	got, ok := readLastRecoveryEvent(path)
	if !ok {
		t.Fatal("readLastRecoveryEvent reported no event after two appends")
	}
	if got.Pane != second.Pane || got.Message != second.Message {
		t.Errorf("readLastRecoveryEvent = %+v; want the most recently appended event %+v", got, second)
	}
}

func TestRecoveryLog_NoFileYetReportsNoEvent(t *testing.T) {
	if _, ok := readLastRecoveryEvent("/nonexistent/recovery.log"); ok {
		t.Error("readLastRecoveryEvent reported an event for a log that has never been written")
	}
}

// AC4: `orchard shell doctor` reports dead panes and the last recovery
// event. checkRecoveryStatus is the new doctor check (added to runChecks
// alongside checkOuterSocket).
func TestCheckRecoveryStatus_ReportsDeadPanesAndLastEvent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/recovery.log"
	if err := appendRecoveryLog(path, recoveryEvent{Pane: "sidebar", Action: actRespawnSidebar, Message: "sidebar exited (status 1) — respawned", At: at(5)}); err != nil {
		t.Fatalf("appendRecoveryLog: %v", err)
	}

	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "0 1 /dev/ttys013\n1 0 /dev/ttys004")
	env := doctorEnv{
		tmux:        f.exec,
		conf:        "/conf/outer.conf",
		innerSocket: "inner-test",
		outerSocket: "outer-test",
	}

	got := checkRecoveryStatus(env, path)
	if !strings.Contains(got.Detail, "0.0") {
		t.Errorf("checkRecoveryStatus detail %q does not mention the dead pane 0.0", got.Detail)
	}
	if !strings.Contains(got.Detail, "sidebar exited (status 1) — respawned") {
		t.Errorf("checkRecoveryStatus detail %q does not surface the last recovery event", got.Detail)
	}
}

func TestCheckRecoveryStatus_NoDeadPanesNoEventsIsPass(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("has-session", "-t", outerSessionName), "").
		reply(panesCall(), "0 0 /dev/ttys013\n1 0 /dev/ttys004").
		reply(innerCall("list-clients", "-F", "#{client_tty}"), "/dev/ttys004")
	env := doctorEnv{
		tmux:        f.exec,
		conf:        "/conf/outer.conf",
		innerSocket: "inner-test",
		outerSocket: "outer-test",
	}

	got := checkRecoveryStatus(env, "/nonexistent/recovery.log")
	if got.Status != statusPass {
		t.Errorf("Status = %v; want pass for a healthy wrapper with no recovery history", got.Status)
	}
}
