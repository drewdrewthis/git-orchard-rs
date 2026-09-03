package main

import (
	"cmp"
	"io"
	"strings"
)

// doctor_recovery.go — issue #796: doctor's recovery-status check.
//
// Split out of doctor_checks.go (file ≤ 300 lines): the recovery check needs
// both the wrapper probe and the recovery log, and reads enough of each to
// earn its own file.

// paneIndexOf maps decideRecovery's pane name to the outer window's index the
// user sees ("0.0" / "0.1").
var paneIndexOf = map[string]string{"sidebar": "0.0", "inner": "0.1"}

// checkRecoveryStatus reports any dead panes in the outer wrapper, any panes
// still parked by the crash-loop halt, and the last self-heal recover-pane
// performed (AC4). It reuses the wrapper's own probe so "which pane is dead"
// is read exactly as orchard-shell reads it. A dead pane warns (the pane-died
// hook should have healed it). A halted pane also warns but is a distinct
// state: the crash-loop holder keeps it ALIVE showing its message, so probe()
// never sees it dead — the recovery log is the only evidence it is parked, and
// M-r is what revives it. A healthy wrapper with no such state passes.
func checkRecoveryStatus(env doctorEnv, logPath string) checkResult {
	w := &wrapper{
		opts: Options{
			OuterSocket: cmp.Or(env.outerSocket, defaultOuterSocket),
			InnerSocket: cmp.Or(env.innerSocket, defaultInnerSocket),
		},
		conf: env.conf, tmux: env.tmux, log: io.Discard,
	}
	s := w.probe()
	paneDead := map[string]bool{"sidebar": s.pane0Dead, "inner": s.pane1Dead}
	var dead []string
	for _, pane := range []string{"sidebar", "inner"} {
		if paneDead[pane] {
			dead = append(dead, paneIndexOf[pane])
		}
	}

	// A pane whose last recovery event is a halt but which is NOT currently
	// dead is parked-and-alive — the state the dead-pane probe cannot see.
	var halted []string
	for _, pane := range haltedPanes(readRecoveryEvents(logPath)) {
		if !paneDead[pane] {
			halted = append(halted, paneIndexOf[pane])
		}
	}

	last, haveEvent := readLastRecoveryEvent(logPath)

	var parts []string
	if len(dead) > 0 {
		parts = append(parts, "dead panes: "+strings.Join(dead, ", "))
	}
	if len(halted) > 0 {
		parts = append(parts, "halted panes: "+strings.Join(halted, ", ")+" (press M-r)")
	}
	if len(parts) == 0 {
		parts = append(parts, "no dead panes")
	}
	detail := strings.Join(parts, "; ")
	if haveEvent {
		detail += "; last recovery: " + last.Message
	} else {
		detail += "; no recovery events recorded"
	}

	status := statusPass
	remedy := ""
	if len(dead) > 0 || len(halted) > 0 {
		status = statusWarn
		remedy = "orchard shell   (reattaches), or press M-r inside the wrapper to retry"
	}
	return checkResult{ID: "recovery", Status: status, Detail: detail, Remedy: remedy}
}
