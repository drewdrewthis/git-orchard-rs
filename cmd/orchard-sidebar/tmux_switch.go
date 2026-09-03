package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"
)

// switchClientArgs builds the switch-client argv for session. With a client
// tty set (this sidebar's own client) it scopes the switch to that client via
// -c — a plain `switch-client -t` on a SHARED inner server lets tmux pick an
// arbitrary attached client to move, which hijacked an unrelated terminal in
// the wild (#747 defect 2). Wrapped but not yet told which client is its own,
// ok is false: never fall back to an unscoped switch on a foreign socket.
// Neither set (legacy, unwrapped mode) is unchanged.
func switchClientArgs(session string) (args []string, ok bool) {
	// activeClient is env.client until a split retargets the sidebar at the
	// last-focused work pane (#777, split.go); unchanged in single-pane mode.
	if c := activeClient(); c != "" {
		return []string{"switch-client", "-c", string(c), "-t", session}, true
	}
	if env.wrapped() {
		return nil, false
	}
	return []string{"switch-client", "-t", session}, true
}

// errUnscopedSwitch is the one refusal both switch paths share: wrapped
// without a client tty, an unscoped switch could move a foreign client on the
// shared inner socket, so neither path falls back to one.
var errUnscopedSwitch = errors.New(
	"ORCHARD_TMUX_SOCKET set without ORCHARD_TMUX_CLIENT: refusing an unscoped switch-client")

// switchClientTo runs the switch SYNCHRONOUSLY and returns tmux's own error —
// the launch modal's path, where a failure has a screen to land on. The
// refusal comes back as an error rather than a silent skip: a launch that
// created the session and quietly failed to move you to it looks like the
// session never launched.
// A var so the break-pane flow's test can observe the switch without a live
// tmux, the same reason switchClient is a var.
var switchClientTo = func(session string) error {
	args, ok := switchClientArgs(session)
	if !ok {
		return errUnscopedSwitch
	}
	return runTmux(args...)
}

// switchClient is a var so tests can observe the switch without a live tmux.
//
// KNOWN VIOLATION, tracked in orchardist#726. RULES L7 / M2 and ADR-018 line
// 20 put "switch tmux session" on the daemon, but no switchTmuxSession
// mutation exists yet — sendTextToPane is the only tmux mutation in the
// schema. Replace this exec with the mutation when #726 lands.
var switchClient = func(session string, handBack bool) {
	args, ok := switchClientArgs(session)
	if !ok {
		logf("%v (session %s)", errUnscopedSwitch, session)
		return
	}
	cmd := env.innerCmd(args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Started here, on the UI goroutine, so rapid j/k switches reach tmux in
	// the order they were pressed; only the waiting happens off-thread.
	if err := cmd.Start(); err != nil {
		logf("%s: %v", strings.Join(args, " "), err)
		return
	}
	// selectRow must not block on tmux, so the switch stays fire-and-forget —
	// but a failed switch used to vanish silently (#747 defect 1). Wait for it
	// off-thread and log instead of dropping the exit status.
	go func() {
		if err := cmd.Wait(); err != nil {
			logf("%s: %v: %s",
				strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
			return
		}
		// A click/Enter drove the switch on the INNER server but the OUTER
		// wrapper's own active pane is still wherever the user last left it --
		// #747 defect 3: with `mouse on` and `prefix None` nothing the user
		// typed would ever reach the shell they just switched to. j/k pass
		// handBack=false: still browsing, must not steal focus mid-move.
		if handBack {
			handBackFocus()
		}
	}()
}

// renameSession and killSession are the row menu's two mutations (menu.go),
// vars for the same reason switchClient is: tests drive the menu without a
// tmux server.
//
// Both run SYNCHRONOUSLY, unlike switchClient's fire-and-forget, because the
// menu has to put the failure on screen — a rename that silently did nothing
// is indistinguishable from one that worked. The context bound is what keeps
// that safe: a wedged tmux cannot freeze the UI goroutine for longer than it.
//
// KNOWN VIOLATION of ADR-016 (the daemon owns tmux), tracked with switchClient
// in orchardist#726. There is nothing to call instead: the daemon's Mutation
// type is sendTextToPane, launchSession, worktreeRemove and worktreesCleanup
// and nothing else — no session rename, no session kill (introspected against
// the live daemon, 2026-09-02).
var renameSession = func(old, name string) error {
	return runTmux("rename-session", "-t", old, name)
}

var killSession = func(name string) error {
	return runTmux("kill-session", "-t", name)
}

// tmuxOpTimeout bounds a menu action. Long enough that a busy local server
// still answers, short enough that a wedged one is a visible error rather than
// a frozen sidebar.
const tmuxOpTimeout = 2 * time.Second

// runTmux runs one command against the sessions' server (the INNER one when
// wrapped) and returns tmux's own message as the error — that text is what the
// menu shows, and tmux says "can't find session: x" far better than any
// wrapper could.
// A var so the break-pane flow's test can record the exact argv of each step
// and inject a mid-sequence failure without a live tmux server.
var runTmux = func(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxOpTimeout)
	defer cancel()
	out, err := env.innerCmdContext(ctx, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	logf("%s: %s", strings.Join(args, " "), msg)
	return errors.New(msg)
}

// runTmuxOutput is runTmux's read-back sibling: it runs one command against
// the sessions' server and returns tmux's trimmed stdout on success, so a step
// that must consume what tmux printed (a window id from new-session -P) can.
// On error it returns tmux's own stderr message as the error, exactly like
// runTmux, so a failure reads the same in the status line.
// A var for the same reason runTmux is: the break-pane flow's test injects the
// printed id and a mid-sequence failure without a live tmux server.
var runTmuxOutput = func(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxOpTimeout)
	defer cancel()
	cmd := env.innerCmdContext(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		logf("%s: %s", strings.Join(args, " "), msg)
		return "", errors.New(msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// handBackFocusArgs is the select-pane argv, or ok=false when there is no
// outer pane to hand focus to (legacy unwrapped mode, or wrapped but not yet
// told its outer pane). Split out so the argv is testable without a tmux
// server — the exec itself is one line.
func handBackFocusArgs() (args []string, ok bool) {
	// activeOuter is env.outer until a split retargets it at the last-focused
	// work pane (#777, split.go); unchanged in single-pane mode.
	p := activeOuter()
	if p == "" {
		return nil, false
	}
	return selectPaneArgs(p), true
}

// handBackFocus is a var so tests can observe the hand-back without a live
// tmux. Runs synchronously on switchClient's own goroutine, after cmd.Wait()
// confirms the inner switch itself succeeded -- never hands back focus onto a
// switch that failed. Logs failure, never fatal: a stuck outer pane is
// recoverable by hand (M-Left/M-Right), a crashed sidebar is not.
var handBackFocus = func() {
	args, ok := handBackFocusArgs()
	if !ok {
		return
	}
	runOuter(args...)
}
