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
func switchClientArgs(session string, client clientTTY) (args []string, ok bool) {
	// client is the work-pane tty the caller snapshotted (env.client, or the
	// last-focused split pane in #777); threaded in rather than read from a
	// package global so the exec goroutine never races the UI goroutine.
	if client != "" {
		return []string{"switch-client", "-c", string(client), "-t", session}, true
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
	// The launch modal runs in the sidebar's own (primary) work pane, so the
	// switch scopes to env.client — a split's focus-follow never applies here.
	args, ok := switchClientArgs(session, env.client)
	if !ok {
		return errUnscopedSwitch
	}
	return runTmux(args...)
}

// switchClient is a var so tests can observe the switch without a live tmux.
// main() binds it to (*model).switchClientBound so the exec reads the model's
// focus-follow snapshot; the package-level default is the single-pane path used
// before that binding and by the unwrapped/legacy modes.
//
// KNOWN VIOLATION, tracked in orchardist#726. RULES L7 / M2 and ADR-018 line
// 20 put "switch tmux session" on the daemon, but no switchTmuxSession
// mutation exists yet — sendTextToPane is the only tmux mutation in the
// schema. Replace this exec with the mutation when #726 lands.
var switchClient = func(session string, handBack bool) {
	switchClientExec(session, handBack, env.client, env.outer)
}

// switchClientBound is the runtime switch: it snapshots the client and outer
// targets ON THE UI GOROUTINE (via m.activeClient/m.activeOuter, which read
// m.workOverride) and hands the snapshots to switchClientExec, which captures
// them in its closure. Snapshotting here is what keeps the exec goroutine off
// the shared focus state the UI goroutine is concurrently updating.
func (m *model) switchClientBound(session string, handBack bool) {
	client := m.activeClient()
	// Click/Enter (handBack) is the deliberate "go there" action, and the one
	// #787's silent failure struck: validate the wrapper's client tty against the
	// live inner clients now, falling back to our own inner attach (outer pane
	// 0.1) when the launcher handed a stale one, and showing a footer error when
	// nothing can be resolved. j/k browsing (handBack=false) keeps the exec-light
	// snapshot path — a held key must not fire a tmux read every frame.
	if handBack {
		resolved, ok := resolveClientTTY(client)
		if !ok {
			resolveFailOnce.Do(func() {
				logf("switch: no live inner client for tty %q — stale outer-shell launcher (#787)", string(client))
			})
			m.setStatus(clientNotFoundStatus)
			return
		}
		client = resolved
	}
	switchClientExec(session, handBack, client, m.activeOuter())
}

// switchClientExec runs the switch for the snapshotted client/outer targets.
// client and outer are captured in the goroutine closure, so the wait-and-hand-
// back never reads state another goroutine may be writing.
func switchClientExec(session string, handBack bool, client clientTTY, outer outerPane) {
	args, ok := switchClientArgs(session, client)
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
			handBackFocus(outer)
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
func handBackFocusArgs(outer outerPane) (args []string, ok bool) {
	// outer is env.outer until a split retargets it at the last-focused work
	// pane (#777); threaded in so the caller snapshots it, never a global.
	// Rejected unless it is a %N pane id that is not the sidebar's own pane: a
	// stale launcher can hand a tty, a window address, or pane 0.0 itself (#787
	// AC2), any of which would focus the wrong thing or nothing.
	if !validOuterPane(outer, env.self) {
		return nil, false
	}
	return selectPaneArgs(outer), true
}

// handBackFocus is a var so tests can observe the hand-back without a live
// tmux. Runs synchronously on switchClientExec's own goroutine, after
// cmd.Wait() confirms the inner switch itself succeeded -- never hands back
// focus onto a switch that failed. The outer target is passed in (a snapshot
// captured on the UI goroutine), so this reads no shared state. Logs failure,
// never fatal: a stuck outer pane is recoverable by hand (M-Left/M-Right), a
// crashed sidebar is not.
var handBackFocus = func(outer outerPane) {
	if args, ok := handBackFocusArgs(outer); ok {
		runOuter(args...)
		return
	}
	// The wrapper handed a bad ORCHARD_OUTER_PANE (#787 AC2). Fall back to our
	// own inner attach pane (outer 0.1). Logged once per process: a per-click
	// line on every switch would bury the log. Runs off the UI goroutine
	// (switchClientExec's own goroutine), so the extra read never stalls a paint.
	id, _, ok := outerInnerPane()
	outerPaneGuardOnce.Do(func() {
		logf("hand-back: ORCHARD_OUTER_PANE %q is not a usable pane id; falling back to outer pane 0.1 (#787)", string(outer))
	})
	if !ok {
		return
	}
	if args, ok := handBackFocusArgs(id); ok {
		runOuter(args...)
	}
}
