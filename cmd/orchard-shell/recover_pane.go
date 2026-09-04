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

	pane, target, ok := recoverTarget(w, fs.Arg(0), *retry)
	if !ok {
		fmt.Fprintln(stderr, "recover-pane: no dead pane to recover")
		return 0
	}
	w.recoverPane(pane, target, *retry, stderr)
	return 0
}

// recoverTarget resolves which pane to act on and its tmux address. An explicit
// index/name wins; with --retry and no argument, it reads the outer state and
// the recovery log and hands both to resolveRetryTarget. A close-always split
// pane is never an M-r target (there is nothing to retry), so the retry path
// only ever yields sidebar/inner.
func recoverTarget(w *wrapper, arg string, retry bool) (pane, target string, ok bool) {
	if name := paneNameFromArg(arg); name != "" {
		return name, paneTargetFor(name, arg), true
	}
	if !retry {
		return "", "", false
	}
	logPath, _ := recoveryLogPath()
	name, ok := resolveRetryTarget(w.probe(), readRecoveryEvents(logPath))
	if !ok {
		return "", "", false
	}
	return name, paneTargetFor(name, ""), true
}

// resolveRetryTarget is the pure M-r target decision, split out so it is
// exercised without a live tmux server. A currently-dead pane wins (the
// pane-died hook would normally have caught it). Otherwise it falls back to
// the most recently halted pane from the recovery log: the crash-loop halt
// keeps that pane ALIVE showing its hold message, so probe() reports nothing
// dead even though M-r is exactly the key meant to revive it (the live-proof
// bug — M-r reported "no dead pane to recover" on a halted-but-alive pane).
func resolveRetryTarget(s outerState, events []recoveryEvent) (string, bool) {
	switch {
	case s.pane0Dead:
		return "sidebar", true
	case s.pane1Dead:
		return "inner", true
	}
	return mostRecentlyHaltedPane(events)
}

// paneNameFromArg maps outer.conf's #{pane_index} or an explicit
// sidebar/inner word to the pane class decideRecovery speaks. A numeric outer-
// window index of 2 or more is a #777 open-in-split work pane — the "split"
// class — so a died split pane resolves to a real recovery instead of
// short-circuiting to "no dead pane" and staying dead (issue #802 AC1).
func paneNameFromArg(arg string) string {
	arg = strings.TrimSpace(arg)
	switch arg {
	case "0", "sidebar":
		return "sidebar"
	case "1", "inner":
		return "inner"
	}
	if n, err := strconv.Atoi(arg); err == nil && n >= 2 {
		return "split"
	}
	return ""
}

// paneTargetFor resolves a pane class to the tmux pane address recover-pane
// acts on. The split class carries its own outer-window index (arg), since
// there can be more than one split pane.
func paneTargetFor(pane, arg string) string {
	switch pane {
	case "sidebar":
		return paneSidebar
	case "inner":
		return paneInner
	case "split":
		return outerSessionName + ":0." + strings.TrimSpace(arg)
	}
	return ""
}

// recoverPane reads the pane's exit status and history, decides, and acts.
// target is the tmux pane address recoverTarget resolved for pane.
func (w *wrapper) recoverPane(pane, target string, retry bool, stderr io.Writer) {
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
	case actCloseSplit:
		// Close the died split pane and re-pin the known two-pane layout, the
		// same shape boot/rebuild pins. No respawn: the pane is sidebar-owned
		// (see decideRecovery), so respawning it would orphan m.alt.
		//
		// Split panes are opened remain-on-exit off (cmd/orchard-sidebar/
		// split.go), so on the natural death path tmux has already removed the
		// pane before this hook runs — a kill-pane would then fail and short-
		// circuit past the layout re-pin. Treat a gone pane as already closed:
		// kill it only if it still exists, but ALWAYS re-pin the layout.
		if w.paneExists(target) {
			if _, err := w.outer("kill-pane", "-t", target); err != nil {
				return err
			}
		}
		_, err := w.outer("select-layout", "-t", outerSessionName+":0", "main-vertical")
		return err
	}
	return nil
}

// paneExists reports whether target is still a live pane on window 0 of the
// outer server. A split pane opened remain-on-exit off is gone by the time its
// pane-died hook fires, so actCloseSplit must not treat a missing pane as a
// kill-pane error — it checks here first.
func (w *wrapper) paneExists(target string) bool {
	out, err := w.outer("list-panes", "-t", outerSessionName+":0", "-F", "#{pane_index}")
	if err != nil {
		return false
	}
	idx := strings.TrimPrefix(target, outerSessionName+":0.")
	for _, line := range strings.Fields(out) {
		if line == idx {
			return true
		}
	}
	return false
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
