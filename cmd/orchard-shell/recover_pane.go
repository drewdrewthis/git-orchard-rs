package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/drewdrewthis/orchardist/internal/orchpaths"
)

// recover_pane.go — the imperative shell for `orchard-shell recover-pane`.
//
// outer.conf's pane-died hook invokes this with the dead pane's index, and
// its M-r bind invokes it with --retry. It reads the pane's real state off
// the outer server, asks decideRecovery (recover.go) what to do, acts, then
// records the outcome in the recovery log for doctor to surface. Everything
// policy lives in decideRecovery; this file only reads state and drives tmux.

// defaultNewSessionName is the session `new-session -A` creates when the
// inner server has none left — a plain, predictable name the user can rename.
const defaultNewSessionName = "work"

// runRecoverPane is the `recover-pane` subcommand. It never fails the caller
// (a tmux hook): a recovery that cannot proceed logs to stderr, which the
// hook discards, and returns 0 so tmux does not surface a run-shell error.
func runRecoverPane(argv []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("orchard-shell recover-pane", flag.ContinueOnError)
	fs.SetOutput(stderr)
	retry := fs.Bool("retry", false, "recover whichever pane is dead, ignoring the crash-loop bound")
	inner := fs.String("inner-socket", defaultInnerSocket, "inner tmux -L socket")
	outer := fs.String("outer-socket", defaultOuterSocket, "outer tmux -L socket")
	conf := fs.String("conf", "", "outer tmux config path")
	if err := fs.Parse(argv); err != nil {
		return 0
	}

	confPath, err := resolveConfFor(*conf, selfPath(), *inner, *outer)
	if err != nil {
		fmt.Fprintf(stderr, "recover-pane: %v\n", err)
		return 0
	}
	w := &wrapper{
		opts: Options{InnerSocket: *inner, OuterSocket: *outer, Width: defaultWidth},
		conf: confPath, tmux: runTmux, log: stderr, lookPath: exec.LookPath,
	}

	pane, ok := recoverTarget(w, fs.Arg(0), *retry)
	if !ok {
		fmt.Fprintln(stderr, "recover-pane: no dead pane to recover")
		return 0
	}
	w.recoverPane(pane, *retry, stderr)
	return 0
}

// recoverTarget resolves which pane to act on. An explicit index/name wins;
// with --retry and no argument, it probes for whichever pane is dead.
func recoverTarget(w *wrapper, arg string, retry bool) (string, bool) {
	if name := paneNameFromArg(arg); name != "" {
		return name, true
	}
	if !retry {
		return "", false
	}
	s := w.probe()
	switch {
	case s.pane0Dead:
		return "sidebar", true
	case s.pane1Dead:
		return "inner", true
	default:
		return "", false
	}
}

// paneNameFromArg maps outer.conf's #{pane_index} (0/1) or an explicit
// sidebar/inner word to the pane name decideRecovery speaks.
func paneNameFromArg(arg string) string {
	switch strings.TrimSpace(arg) {
	case "0", "sidebar":
		return "sidebar"
	case "1", "inner":
		return "inner"
	default:
		return ""
	}
}

// recoverPane reads the pane's exit status and history, decides, and acts.
func (w *wrapper) recoverPane(pane string, retry bool, stderr io.Writer) {
	target := paneSidebar
	if pane == "inner" {
		target = paneInner
	}
	exitStatus := w.paneExitStatus(target)

	logPath, _ := recoveryLogPath()

	// Retry (M-r) is passed through to decideRecovery, which is the single
	// place that decides to ignore the crash-loop bound and the halt debounce;
	// the history and last-halt are read unconditionally so both paths see the
	// same facts.
	in := recoverInput{
		Pane:             pane,
		ExitStatus:       exitStatus,
		InnerHasSessions: pane == "inner" && w.innerSessionCount() > 0,
		History:          recoveryHistory(logPath, pane),
		Retry:            retry,
		LastHalt:         lastHaltAt(logPath, pane),
		Now:              time.Now(),
	}
	action, msg := decideRecovery(in)

	// A halted pane churning within the debounce: do nothing and log nothing,
	// so recovery.log does not balloon and the pane stays parked.
	if action == actNoop {
		return
	}

	if err := w.applyRecovery(action, target, msg, stderr); err != nil {
		fmt.Fprintf(stderr, "recover-pane: %v\n", err)
	}
	if logPath != "" {
		_ = appendRecoveryLog(logPath, recoveryEvent{Pane: pane, Action: action, Message: msg, At: in.Now})
	}
	// Surface the one-line status on the wrapper regardless of the action.
	_, _ = w.outer("display-message", msg)
}

// applyRecovery carries out one decideRecovery outcome against the outer
// server.
func (w *wrapper) applyRecovery(action recoverAction, target, msg string, stderr io.Writer) error {
	switch action {
	case actReattachInner:
		return w.reattachInner()
	case actNewInnerSession:
		_, err := w.outer("respawn-pane", "-k", "-t", paneInner,
			innerNewSessionCommand(w.opts.InnerSocket, defaultNewSessionName))
		return err
	case actRespawnSidebar:
		appendSidebarLog(msg)
		return w.respawnSidebarPane()
	case actCrashLoopHalt:
		_, err := w.outer("respawn-pane", "-k", "-t", target, haltCommand(msg))
		return err
	}
	return nil
}

// reattachInner respawns pane 0.1 onto the inner server's most-recently
// attached session.
func (w *wrapper) reattachInner() error {
	sessions, err := w.innerSessions()
	if err != nil || len(sessions) == 0 {
		_, err := w.outer("respawn-pane", "-k", "-t", paneInner,
			innerNewSessionCommand(w.opts.InnerSocket, defaultNewSessionName))
		return err
	}
	_, err = w.outer("respawn-pane", "-k", "-t", paneInner,
		innerAttachCommand(w.opts.InnerSocket, sessions[0]))
	return err
}

// paneExitStatus reads a pane's #{pane_dead_status}. A pane that is not dead
// (or a read that fails) reports 0 — the same "clean" value a clean exit
// reports, which is the right default for a reattach message.
func (w *wrapper) paneExitStatus(target string) int {
	out, err := w.outer("display", "-p", "-t", target, "#{pane_dead} #{pane_dead_status}")
	if err != nil {
		return 0
	}
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return 0
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return status
}

// innerSessionCount reports how many sessions the inner server has; 0 when it
// is gone.
func (w *wrapper) innerSessionCount() int {
	sessions, err := w.innerSessions()
	if err != nil {
		return 0
	}
	return len(sessions)
}

// haltCommand keeps the pane alive showing msg after the crash-loop bound
// trips, so the wrapper explains itself instead of leaving a dead pane.
//
// A bare `sleep infinity` is NOT portable: macOS's BSD sleep rejects
// "infinity" and exits immediately, which under remain-on-exit re-fires
// pane-died and turns the halt into the very crash loop it exists to stop (a
// live run grew 1442 halt entries in ~8s). A finite sleep in an unbounded loop
// holds the pane forever on both BSD and GNU sleep.
func haltCommand(msg string) string {
	return fmt.Sprintf("printf '%%s\\n' %s; while :; do sleep 3600; done", shellQuote(msg))
}

// appendSidebarLog records a sidebar exit line in the sidebar's own log,
// alongside its runtime diagnostics (cmd/orchard-sidebar/log.go). Best-effort
// — a recovery that cannot open the log still respawns the pane.
func appendSidebarLog(reason string) {
	dir, err := orchpaths.StateDir()
	if err != nil {
		return
	}
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "sidebar.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), reason)
}
