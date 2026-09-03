package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// tmuxExec runs tmux with args and returns its stdout, trimmed of the
// trailing newline every tmux command emits. Injected so the decision logic
// above it is testable without a tmux server.
type tmuxExec func(args ...string) (string, error)

// runTmux is the production tmuxExec.
func runTmux(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := strings.TrimRight(stdout.String(), "\n")
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("tmux %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

// outerArgs prefixes an outer-server invocation.
//
// `-f` is not optional: a `-L` socket alone does not stop tmux loading the
// user's real ~/.tmux.conf, so an invocation without it silently pulls their
// prefix, plugins and status line into what is supposed to be a minimal
// wrapper.
func outerArgs(socket, conf string, args ...string) []string {
	return append([]string{"-L", socket, "-f", conf}, args...)
}

// innerArgs prefixes an inner-server invocation. Deliberately no `-f`: the
// inner server is the user's own, and must keep loading the user's config.
func innerArgs(socket string, args ...string) []string {
	return append([]string{"-L", socket}, args...)
}

// shellQuote quotes s for a command line sent through tmux send-keys, leaving
// ordinary socket and session names untouched so the pane's command line
// stays readable.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("_.:/@%+=-", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// innerAttachCommand is pane 0.1's command line.
//
// `TMUX=` clears the outer session's own $TMUX before the inner attach.
// Without it tmux hard-refuses to nest — "sessions should be nested with
// care, unset $TMUX to force" — and the attach never connects, leaving the
// pane at a dead shell prompt. This is the one place in the wrapper where it
// matters, because it is the only point where a second tmux client is created
// inside a pane the first one already owns.
func innerAttachCommand(socket, session string) string {
	return fmt.Sprintf("TMUX= tmux -L %s attach -t %s", shellQuote(socket), shellQuote(session))
}

// innerNewSessionCommand is pane 0.1's command line when the inner server has
// no session left to attach: `new-session -A` creates one (or attaches an
// existing one of that name), so a killed-off inner server heals into a fresh
// session instead of a dead pane. `TMUX=` is cleared for the same nesting
// reason as innerAttachCommand.
func innerNewSessionCommand(socket, session string) string {
	return fmt.Sprintf("TMUX= tmux -L %s new-session -A -s %s", shellQuote(socket), shellQuote(session))
}

// sidebarCommand is pane 0.0's command line.
//
// ORCHARD_TMUX_SOCKET tells the sidebar's own tmux execs (switch-client,
// list-clients, list-panes, width sync) to target the INNER server instead of
// the outer one they would otherwise resolve to by virtue of running as an
// outer-server pane's command. ORCHARD_TMUX_CLIENT scopes those client-
// targeting execs to the inner client on THIS wrapper's pane 0.1 — on a
// shared inner server an unscoped switch-client lets tmux move an arbitrary
// attached client instead (#747 defect 2). ORCHARD_OUTER_PANE is the outer
// pane the sidebar hands keyboard focus back to after driving that switch.
func sidebarCommand(bin, innerSocket, innerTTY, outerPane string) string {
	return fmt.Sprintf("ORCHARD_TMUX_SOCKET=%s ORCHARD_TMUX_CLIENT=%s ORCHARD_OUTER_PANE=%s %s",
		shellQuote(innerSocket), shellQuote(innerTTY), shellQuote(outerPane), shellQuote(bin))
}

// placeholderCommand is what pane 0.0 runs when no sidebar binary can be
// found — a live view of the inner server, so the wrapper is still legible on
// a machine with a partial install.
func placeholderCommand(innerSocket string) string {
	return fmt.Sprintf("watch -n1 %s", shellQuote("tmux -L "+innerSocket+" list-windows -a"))
}
