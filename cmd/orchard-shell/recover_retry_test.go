package main

import (
	"strings"
	"testing"
	"time"
)

// recover_retry_test.go — the M-r retry path and the crash-loop/halt-debounce
// guards of decideRecovery, plus the close-split apply path (the gone-pane
// live-proof fix). Doctor-status and recovery-log tests live in
// recover_doctor_test.go; the decideRecovery table and pane-name mapping in
// recover_test.go.

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

// A pane-died that arrives within haltDebounce of the last halt is the parked
// pane's own hold command churning, not a fresh crash: decideRecovery returns
// actNoop so recover-pane does nothing and logs nothing (the AC3 runaway: a
// non-portable hold exited at once and grew 1442 halt entries in ~8s).
func TestDecideRecovery_HaltDebounceReturnsNoop(t *testing.T) {
	now := at(3000)
	in := recoverInput{
		Pane:     "inner",
		LastHalt: now.Add(-2 * time.Second), // within the 5s debounce
		Now:      now,
	}
	if action, msg := decideRecovery(in); action != actNoop || msg != "" {
		t.Errorf("decideRecovery = (%v, %q); want (actNoop, \"\") for a halt %s ago", action, msg, "2s")
	}
	// Older than the debounce window: recovery resumes normally.
	stale := recoverInput{Pane: "inner", InnerHasSessions: true, LastHalt: now.Add(-10 * time.Second), Now: now}
	if action, _ := decideRecovery(stale); action != actReattachInner {
		t.Errorf("decideRecovery with a stale halt = %v; want actReattachInner (debounce expired)", action)
	}
}

// M-r (Retry) bypasses both the crash-loop bound and the halt debounce: a
// recent halt AND a tripped history still lead to a real recovery action, not
// actNoop or actCrashLoopHalt, because the user is deliberately retrying.
func TestDecideRecovery_RetryBypassesHaltDebounceAndBound(t *testing.T) {
	now := at(4000)
	in := recoverInput{
		Pane:             "inner",
		InnerHasSessions: true,
		History:          historyWithin(6, 60, now), // over the bound
		Retry:            true,
		LastHalt:         now.Add(-1 * time.Second), // inside the debounce
		Now:              now,
	}
	if action, _ := decideRecovery(in); action != actReattachInner {
		t.Errorf("decideRecovery with Retry = %v; want actReattachInner (M-r ignores both guards)", action)
	}
}

// resolveRetryTarget is the pure half of recoverTarget: given the outer state
// and the recovery log, which pane does M-r (--retry, no explicit arg) act on?
// The live-proof FAIL: M-r reported "no dead pane to recover" on a crash-loop-
// halted pane, because the halt holder keeps that pane ALIVE, so probe() sees
// nothing dead. The halted pane must be recovered from the log.
func TestResolveRetryTarget(t *testing.T) {
	halt := func(pane string, sec int) recoveryEvent {
		return recoveryEvent{Pane: pane, Action: actCrashLoopHalt, At: at(sec)}
	}

	cases := []struct {
		name     string
		state    outerState
		events   []recoveryEvent
		wantPane string
		wantOK   bool
	}{
		{
			name:     "held-alive halted inner pane — recover it from the log",
			state:    outerState{}, // nothing dead: the holder keeps 0.1 alive
			events:   []recoveryEvent{halt("inner", 100)},
			wantPane: "inner", wantOK: true,
		},
		{
			name:     "a currently-dead pane wins over the halt log",
			state:    outerState{pane0Dead: true},
			events:   []recoveryEvent{halt("inner", 100)},
			wantPane: "sidebar", wantOK: true,
		},
		{
			name:   "nothing dead and nothing halted — no target",
			state:  outerState{},
			events: nil,
			wantOK: false,
		},
		{
			name:  "a later successful recovery clears the halt — no target",
			state: outerState{},
			events: []recoveryEvent{
				halt("inner", 100),
				{Pane: "inner", Action: actReattachInner, At: at(200)},
			},
			wantOK: false,
		},
		{
			name:     "two halted panes — the most recently halted one wins",
			state:    outerState{},
			events:   []recoveryEvent{halt("sidebar", 100), halt("inner", 200)},
			wantPane: "inner", wantOK: true,
		},
		{
			// AC1: a split pane is closed, never retried — a close-split event
			// in the log is never an M-r target.
			name:   "a split-close event in the log is never an M-r target",
			state:  outerState{},
			events: []recoveryEvent{{Pane: "split", Action: actCloseSplit, At: at(100)}},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPane, gotOK := resolveRetryTarget(c.state, c.events)
			if gotOK != c.wantOK {
				t.Fatalf("resolveRetryTarget ok = %v; want %v", gotOK, c.wantOK)
			}
			if c.wantOK && gotPane != c.wantPane {
				t.Errorf("resolveRetryTarget pane = %q; want %q", gotPane, c.wantPane)
			}
		})
	}
}

// AC1 live-proof fix: split panes are opened remain-on-exit off, so on the
// natural death path tmux has already removed the pane before the pane-died
// hook runs recover-pane. applyRecovery(actCloseSplit) must treat a gone pane
// as already closed — skip kill-pane, but STILL re-pin the two-pane layout —
// instead of erroring out of kill-pane and leaving the layout un-restored.
func TestApplyRecovery_CloseSplit_GonePaneStillRepinsLayout(t *testing.T) {
	// window 0 has only panes 0 and 1: the split pane (index 2) is gone.
	f := newFakeTmux().
		reply(outerCall("list-panes", "-t", outerSessionName+":0", "-F", "#{pane_index}"), "0\n1")
	w := testWrapper(f)
	target := outerSessionName + ":0.2"

	if err := w.applyRecovery(actCloseSplit, target, "split work pane exited — closed, two-pane layout restored", &strings.Builder{}); err != nil {
		t.Fatalf("applyRecovery(actCloseSplit) on a gone pane returned an error: %v", err)
	}
	if f.called("kill-pane") {
		t.Errorf("applyRecovery tried to kill an already-gone split pane; calls: %v", f.calls)
	}
	if !f.called("select-layout") {
		t.Errorf("applyRecovery did not re-pin the layout for a gone split pane; calls: %v", f.calls)
	}
}

// When the split pane is still alive (e.g. an explicit M-w close), applyRecovery
// kills it and then re-pins the layout.
func TestApplyRecovery_CloseSplit_LivePaneIsKilledThenRepinned(t *testing.T) {
	f := newFakeTmux().
		reply(outerCall("list-panes", "-t", outerSessionName+":0", "-F", "#{pane_index}"), "0\n1\n2")
	w := testWrapper(f)
	target := outerSessionName + ":0.2"

	if err := w.applyRecovery(actCloseSplit, target, "split work pane exited — closed, two-pane layout restored", &strings.Builder{}); err != nil {
		t.Fatalf("applyRecovery(actCloseSplit) on a live pane returned an error: %v", err)
	}
	if !f.called("kill-pane -t " + target) {
		t.Errorf("applyRecovery did not kill the live split pane %q; calls: %v", target, f.calls)
	}
	if !f.called("select-layout") {
		t.Errorf("applyRecovery did not re-pin the layout after killing the split pane; calls: %v", f.calls)
	}
}
