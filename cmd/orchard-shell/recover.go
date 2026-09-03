package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/drewdrewthis/orchardist/internal/orchpaths"
)

// recover.go — issue #796: the pure decision and the recovery log.
//
// A pane that dies (an inner client whose session was killed, a crashed
// sidebar, an inner server that exited) should self-heal rather than sit
// there as a dead pane. decideRecovery is the whole policy, kept pure so the
// table test owns it; recover_pane.go is the imperative shell that reads pane
// state, calls this, and acts on the answer.

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

// recoveryEvent is one line of the recovery log: what recover-pane did, when,
// and the status message it showed. doctor reads the most recent one back.
type recoveryEvent struct {
	Pane    string        `json:"pane"`
	Action  recoverAction `json:"action"`
	Message string        `json:"message"`
	At      time.Time     `json:"at"`
}

// recoveryLogPath is the recovery log, alongside the sidebar's own log under
// the orchard state dir.
func recoveryLogPath() (string, error) {
	dir, err := orchpaths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "recovery.log"), nil
}

// appendRecoveryLog appends ev as one JSON line, creating the file and its
// directory on first write.
func appendRecoveryLog(path string, ev recoveryEvent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// readRecoveryEvents reads every event in the log, oldest first. A missing
// log is not an error: there is simply no history yet.
func readRecoveryEvents(path string) []recoveryEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var events []recoveryEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev recoveryEvent
		if json.Unmarshal(line, &ev) == nil {
			events = append(events, ev)
		}
	}
	return events
}

// readLastRecoveryEvent returns the most recently appended event, or ok=false
// when the log has never been written.
func readLastRecoveryEvent(path string) (recoveryEvent, bool) {
	events := readRecoveryEvents(path)
	if len(events) == 0 {
		return recoveryEvent{}, false
	}
	return events[len(events)-1], true
}

// recoveryHistory returns the restart timestamps recorded for one pane, for
// the crash-loop bound.
func recoveryHistory(path, pane string) []time.Time {
	var out []time.Time
	for _, ev := range readRecoveryEvents(path) {
		if ev.Pane == pane {
			out = append(out, ev.At)
		}
	}
	return out
}

// lastHaltAt returns when this pane was most recently halted, or the zero time
// if it never was — feeding decideRecovery's halt debounce.
func lastHaltAt(path, pane string) time.Time {
	var t time.Time
	for _, ev := range readRecoveryEvents(path) {
		if ev.Pane == pane && ev.Action == actCrashLoopHalt {
			t = ev.At
		}
	}
	return t
}
