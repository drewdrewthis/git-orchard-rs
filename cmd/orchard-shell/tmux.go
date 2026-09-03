package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// tmuxExec runs tmux with args and returns its stdout, trimmed of the
// trailing newline every tmux command emits. Injected so the decision logic
// above it is testable without a tmux server.
type tmuxExec func(args ...string) (string, error)

// tmuxFallbackPaths are the usual absolute homes of the tmux binary, tried in
// order when $PATH does not resolve "tmux". A tmux run-shell hook (outer.conf's
// pane-died and M-r) inherits a minimal environment whose $PATH need not carry
// Homebrew's bin, so a bare "tmux" LookPath can miss the very binary that is
// running the hook — which is why M-r worked from a login shell but not under
// run-shell. Resolving here, at the one place every tmux invocation funnels
// through, fixes both the hook and the M-r path together.
var tmuxFallbackPaths = []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"}

// tmuxBinary resolves the tmux executable: $PATH first, then the known
// absolute locations. It returns the bare name as a last resort so exec still
// produces a legible "not found" rather than an empty argv.
func tmuxBinary() string {
	if p, err := exec.LookPath("tmux"); err == nil {
		return p
	}
	for _, p := range tmuxFallbackPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "tmux"
}

// runTmux is the production tmuxExec.
func runTmux(args ...string) (string, error) {
	cmd := exec.Command(tmuxBinary(), args...)
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
//
// `exec` makes the attach the pane's OWN process rather than a child of the
// pane's default shell. boot() delivers this line through send-keys into a
// live zsh/bash; without exec, when the inner server dies the attach exits but
// the shell survives, the pane never dies, and pane-died never fires — so
// self-heal (AC1) never triggers. exec replaces the shell, so the pane dies
// with the attach. respawn-pane runs the command directly (its own process
// already), where exec is a harmless no-op that keeps the pane command uniform
// across every launch path.
func innerAttachCommand(socket, session string) string {
	return fmt.Sprintf("TMUX= exec tmux -L %s attach -t %s", shellQuote(socket), shellQuote(session))
}

// innerNewSessionCommand is pane 0.1's command line when the inner server has
// no session left to attach: `new-session -A` creates one (or attaches an
// existing one of that name), so a killed-off inner server heals into a fresh
// session instead of a dead pane. `TMUX=` is cleared for the same nesting
// reason as innerAttachCommand, and `exec` for the same self-heal reason: the
// pane must die with its inner tmux so a later server death re-triggers
// recovery instead of dropping to a surviving shell.
func innerNewSessionCommand(socket, session string) string {
	return fmt.Sprintf("TMUX= exec tmux -L %s new-session -A -s %s", shellQuote(socket), shellQuote(session))
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
