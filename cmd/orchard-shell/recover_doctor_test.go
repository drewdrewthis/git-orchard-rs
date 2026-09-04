package main

import (
	"strings"
	"testing"
)

// recover_doctor_test.go — the outer.conf wiring (pane-died hook + M-r bind),
// the inner-server detach-on-destroy set, the recovery log round-trip and the
// `orchard shell doctor` recovery-status check. The decideRecovery table and
// pane-name mapping live in recover_test.go; the M-r retry path in
// recover_retry_test.go.

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

// AC0: orchard-shell itself sets detach-on-destroy off on the INNER server it
// attaches to, not the user's ~/.tmux.conf — so that killing the client's
// current session switches the client to another session instead of detaching
// it and leaving pane 0.1 a corpse. ensureReady() owns this one set-option for
// every path (boot included), and must hold on a plain rerun against a live
// wrapper too, not only on a fresh boot.
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

// Contract: a recoveryEvent is what recover-pane appends to the recovery log
// every time it acts; readLastRecoveryEvent reads the most recent one back for
// doctor to surface.
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

// AC4: `orchard shell doctor` reports dead panes and the last recovery event.
// checkRecoveryStatus is the new doctor check (added to runChecks alongside
// checkOuterSocket).
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

// A crash-loop-halted pane is ALIVE (its hold command keeps it up), so the
// dead-pane probe reports nothing dead — but the pane is parked and only M-r
// revives it. doctor must surface it as "halted panes: 0.0 (press M-r)" and
// warn, not report "no dead panes" and pass.
func TestCheckRecoveryStatus_ReportsHeldAliveHaltedPane(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/recovery.log"
	if err := appendRecoveryLog(path, recoveryEvent{Pane: "sidebar", Action: actCrashLoopHalt, Message: "sidebar keeps crashing — see sidebar.log; press M-r to retry", At: at(5)}); err != nil {
		t.Fatalf("appendRecoveryLog: %v", err)
	}
	// A healthy two-pane wrapper: nothing dead, inner client attached.
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
	got := checkRecoveryStatus(env, path)
	if got.Status != statusWarn {
		t.Errorf("Status = %v; want warn for a halted-but-alive pane", got.Status)
	}
	if !strings.Contains(got.Detail, "halted panes: 0.0 (press M-r)") {
		t.Errorf("checkRecoveryStatus detail %q does not surface the halted pane 0.0", got.Detail)
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
