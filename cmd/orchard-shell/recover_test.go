package main

import (
	"testing"
	"time"
)

// recover_test.go — issue #796: outer-shell pane recovery.
//
// decideRecovery is the pure decision: given which pane died, its exit
// status, whether the inner server still has sessions, and the pane's own
// restart history, what should orchard-shell do about it and what should
// the status line say.
//
// Crash-loop bound (AC3, amended for AC1): more than 5 restarts of the SAME
// pane within the trailing 60s window (relative to Now) halts recovery for
// that pane rather than respawning it again.
//
// The retry/halt-debounce, M-r target, embedded-conf, doctor-status and
// recovery-log tests live in recover_retry_test.go — this file owns the
// shared helpers, the decideRecovery table and the pane-name mapping.

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
		{
			// AC1: a died #777 split work pane (outer index >= 2) is closed,
			// not respawned — close-always, so no crash-loop consideration.
			name:       "split work pane died — close it and restore the layout",
			in:         recoverInput{Pane: "split", Now: now},
			wantAction: actCloseSplit,
			wantMsg:    "split work pane exited — closed, two-pane layout restored",
		},
		{
			// AC1: close-always ignores a tripped restart history — a split
			// pane never loops into a halt, it is simply closed.
			name: "split work pane with a tripped history — still just closed",
			in: recoverInput{
				Pane:    "split",
				History: historyWithin(6, 60, now),
				Now:     now,
			},
			wantAction: actCloseSplit,
			wantMsg:    "split work pane exited — closed, two-pane layout restored",
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

// AC1: paneNameFromArg maps the pane-died hook's #{pane_index} to a pane
// class. Index 0/1 are unchanged (sidebar/inner); any numeric index >= 2 is a
// #777 split work pane; a non-numeric, non-keyword argument is still the empty
// "no dead pane" result.
func TestPaneNameFromArg(t *testing.T) {
	cases := []struct {
		arg  string
		want string
	}{
		{"0", "sidebar"},
		{"sidebar", "sidebar"},
		{"1", "inner"},
		{"inner", "inner"},
		{"2", "split"},
		{"3", "split"},
		{"12", "split"},
		{" 2 ", "split"},
		{"", ""},
		{"nope", ""},
		{"-1", ""},
	}
	for _, c := range cases {
		if got := paneNameFromArg(c.arg); got != c.want {
			t.Errorf("paneNameFromArg(%q) = %q; want %q", c.arg, got, c.want)
		}
	}
}
