package main

import (
	"fmt"
	"time"
)

// recover.go — issue #796: the pure recovery decision.
//
// A pane that dies (an inner client whose session was killed, a crashed
// sidebar, an inner server that exited) should self-heal rather than sit
// there as a dead pane. decideRecovery is the whole policy, kept pure so the
// table test owns it; recover_pane.go is the imperative shell that reads pane
// state, calls this, and acts on the answer. The recovery log's own I/O lives
// in recover_log.go — this file stays free of the filesystem so its decisions
// are exercised entirely in-memory.

// recoverAction is what orchard-shell does about a dead pane.
type recoverAction int

const (
	actReattachInner   recoverAction = iota // 0.1's client exited, sessions remain — reattach
	actNewInnerSession                      // the inner server is gone — start a fresh session
	actRespawnSidebar                       // 0.0's sidebar exited — relaunch it
	actCrashLoopHalt                        // too many restarts too fast — stop and wait for M-r
	actNoop                                 // already halted a moment ago — leave the pane parked
)

// recoverInput is everything decideRecovery needs: which pane died, its exit
// status, whether the inner server still has sessions (inner only), the
// pane's own restart history and the current time.
type recoverInput struct {
	Pane             string // "sidebar" | "inner"
	ExitStatus       int
	InnerHasSessions bool // only meaningful when Pane == "inner"
	History          []time.Time
	Retry            bool      // M-r: ignore both the crash-loop bound and the halt debounce
	LastHalt         time.Time // when this pane was last halted (zero if never)
	Now              time.Time
}

// recoveryEvent is one line of the recovery log: what recover-pane did, when,
// and the status message it showed. The pure decision helpers below read a
// slice of these; recover_log.go owns reading and writing them to disk.
type recoveryEvent struct {
	Pane    string        `json:"pane"`
	Action  recoverAction `json:"action"`
	Message string        `json:"message"`
	At      time.Time     `json:"at"`
}

// crashLoopWindow / crashLoopBound bound automatic recovery: more than
// crashLoopBound restarts of the SAME pane inside the trailing window halts
// recovery for that pane rather than respawning it again. Exactly the bound
// still respawns; only strictly more trips the halt.
const (
	crashLoopWindow = 60 * time.Second
	crashLoopBound  = 5
)

// haltDebounce guards against a halted pane's own churn re-triggering
// recovery. Once a pane is halted, its hold command should keep it alive; a
// pane-died that arrives within this window of the last halt is that command
// misbehaving (e.g. a non-portable hold that exits at once), not a fresh
// crash, so recovery does nothing and logs nothing rather than re-halting in a
// tight loop. M-r (Retry) bypasses this, since the user is deliberately asking
// to try again.
const haltDebounce = 5 * time.Second

// decideRecovery is the pure recovery policy.
//
// The halt debounce comes first: a pane-died within haltDebounce of the last
// halt is the parked pane churning, not a fresh failure, so it is a silent
// no-op. Then the crash-loop check, which applies to BOTH panes (the AC
// amendment): a pane that keeps dying is a symptom the user has to look at,
// not something to respawn into an infinite loop. Below the bound, the sidebar
// always respawns; an inner pane reattaches when the server still has sessions
// and starts a fresh one when it does not. Retry (M-r) skips both guards — the
// user is deliberately asking to try again.
func decideRecovery(in recoverInput) (recoverAction, string) {
	// Defense-in-depth against a halt re-firing itself: unless the user asked
	// (M-r), a pane-died within haltDebounce of the last halt is the parked
	// pane churning, not a new failure — leave it parked, silently.
	if !in.Retry && !in.LastHalt.IsZero() && in.Now.Sub(in.LastHalt) <= haltDebounce {
		return actNoop, ""
	}

	if !in.Retry && restartsWithin(in.History, in.Now, crashLoopWindow) > crashLoopBound {
		if in.Pane == "sidebar" {
			return actCrashLoopHalt, "sidebar keeps crashing — see sidebar.log; press M-r to retry"
		}
		return actCrashLoopHalt, "inner tmux keeps exiting — press M-r to retry"
	}

	if in.Pane == "sidebar" {
		return actRespawnSidebar, fmt.Sprintf("sidebar exited (status %d) — respawned", in.ExitStatus)
	}

	if !in.InnerHasSessions {
		return actNewInnerSession, "inner tmux server gone — new session created"
	}
	return actReattachInner, fmt.Sprintf("inner tmux exited (status %d) — reattached", in.ExitStatus)
}

// restartsWithin counts the timestamps no older than window relative to now.
func restartsWithin(history []time.Time, now time.Time, window time.Duration) int {
	n := 0
	for _, t := range history {
		if now.Sub(t) <= window {
			n++
		}
	}
	return n
}

// lastActionByPane reduces the log to each pane's MOST RECENT recovery action,
// in log order. A pane whose newest event is actCrashLoopHalt is still parked:
// a later successful recovery (reattach/respawn/new-session) would have
// overwritten it here.
func lastActionByPane(events []recoveryEvent) map[string]recoverAction {
	last := make(map[string]recoverAction, 2)
	for _, ev := range events {
		last[ev.Pane] = ev.Action
	}
	return last
}

// haltedPanes returns every pane whose most recent event is a crash-loop halt
// — the panes still parked by the bound, in "sidebar", "inner" order. doctor
// surfaces these: the crash-loop holder keeps such a pane ALIVE, so a probe
// for #{pane_dead} never sees them.
func haltedPanes(events []recoveryEvent) []string {
	last := lastActionByPane(events)
	var out []string
	for _, pane := range []string{"sidebar", "inner"} {
		if last[pane] == actCrashLoopHalt {
			out = append(out, pane)
		}
	}
	return out
}

// mostRecentlyHaltedPane returns the pane whose most recent event is a
// crash-loop halt and whose halt is the newest of any such pane — the pane M-r
// should revive when no pane is currently dead (the holder keeps the halted
// pane alive, so probe() reports nothing dead). ok is false when no pane is
// parked.
func mostRecentlyHaltedPane(events []recoveryEvent) (string, bool) {
	last := lastActionByPane(events)
	best, ok := "", false
	var bestAt time.Time
	for _, ev := range events {
		if ev.Action != actCrashLoopHalt || last[ev.Pane] != actCrashLoopHalt {
			continue
		}
		if !ok || ev.At.After(bestAt) {
			best, bestAt, ok = ev.Pane, ev.At, true
		}
	}
	return best, ok
}
