package main

import (
	"bytes"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Open a session in a split (issue #777): a SECOND outer pane to the right of
// the work pane, running a second inner `-L default` client, so two sessions
// are visible at once while the sidebar keeps driving whichever was focused
// last. The sidebar's own pane (0.0) is untouched; only the work area splits.
//
// FOLLOW-UPS (out of scope here):
//   - the #776 right-click `Open in split` menu item calls openInSplit (below);
//     this file owns the logic so menu.go/menuops.go/menuview.go don't.
//   - dragging a card onto the work area to open a split reuses #775's drag
//     infra and lands separately.

// workPaneRef is one outer pane running an inner client. Before any split the
// wrapper builds exactly one (env.outer, driven by env.client); Open in split
// adds a second, and the sidebar drives whichever was focused last.
type workPaneRef struct {
	outer  outerPane // where focus hands back / what a new split sits beside
	client clientTTY // the inner client switch-client / detach-client scope to
}

// activeClient / activeOuter are the client tty and outer pane the sidebar
// currently drives. m.workOverride is the focused work pane in split mode: the
// client lane discovers it from the inner server's most-recently-active work
// client (pickWork), so it moves as focus moves. Its zero value is the
// single-pane default — env.client / env.outer, unchanged from before #777 —
// which is why every pre-split path reads exactly as it did. Kept on the model
// (not a package global) so the exec goroutine reads a snapshot taken on the UI
// goroutine, never a value another goroutine is concurrently writing.
func (m *model) activeClient() clientTTY {
	if m.workOverride.client != "" {
		return m.workOverride.client
	}
	return env.client
}

func (m *model) activeOuter() outerPane {
	if m.workOverride.outer != "" {
		return m.workOverride.outer
	}
	return env.outer
}

// innerAttachCmd is a new work pane's command line: a second client on the same
// inner server, with $TMUX cleared so tmux does not hard-refuse to nest (the
// same reason orchard-shell's innerAttachCommand clears it). It is one argument
// to split-window, which tmux runs via /bin/sh, so the session is shell-quoted.
func innerAttachCmd(inner innerSocket, session string) string {
	return "TMUX= tmux -L " + shellQuote(string(inner)) + " attach -t " + shellQuote(session)
}

// splitWindowArgs opens a new outer pane to the right of beside, running a
// second inner client attached to session, and prints the new pane's id and
// tty (-P -F) so the sidebar can track it as the second work pane.
func splitWindowArgs(beside outerPane, inner innerSocket, session string) []string {
	return []string{"split-window", "-h", "-t", string(beside),
		"-P", "-F", "#{pane_id} #{pane_tty}", innerAttachCmd(inner, session)}
}

// mainVerticalArgs re-applies the layout that pins the sidebar at
// main-pane-width as the main (left) pane while the work panes stack in the
// right column, sharing its height — the same layout outer.conf's resize hooks
// use, so the sidebar width survives both opening the split and closing it.
func mainVerticalArgs() []string { return []string{"select-layout", "main-vertical"} }

// remainOffArgs turns OFF remain-on-exit for the split's own outer pane,
// overriding outer.conf's global `set -g remain-on-exit on` that the new pane
// otherwise inherits. Without it the pane lingers as a DEAD pane once its inner
// client detaches (M-w, or the user detaching from inside), the two-pane layout
// is never restored, and a dead pane piles up on every open/close cycle. `-p`
// scopes the option to just this pane, leaving the global (which the sidebar
// pane relies on) untouched.
func remainOffArgs(pane outerPane) []string {
	return []string{"set-option", "-p", "-t", string(pane), "remain-on-exit", "off"}
}

// killPaneArgs force-closes the split's outer pane. Belt-and-braces after
// detach-client: with remain-on-exit off the pane already closes when its
// client exits, but this guarantees it even if that option did not take. tmux
// tolerates the pane already being gone — the resulting error is logged and
// ignored.
func killPaneArgs(pane outerPane) []string { return []string{"kill-pane", "-t", string(pane)} }

// detachClientArgs detaches the inner client in the split pane. With
// remain-on-exit turned off on the pane (see remainOffArgs), the pane closes
// when its client exits, which restores the two-pane layout.
func detachClientArgs(tty clientTTY) []string { return []string{"detach-client", "-t", string(tty)} }

// splitBlocked reports why a split for session must be refused, or "" to allow
// it. Two inner clients on the SAME session is refused rather than rendered:
// tmux mirrors one session into both panes, which reads as a glitch, not a
// comparison (#777 decision — keep it simple).
func splitBlocked(session string, fake bool, attached map[string]bool) string {
	switch {
	case session == "":
		return ""
	case fake:
		return "synthetic row — nothing to open"
	case attached[session]:
		return session + " is already attached — refusing a second client"
	}
	return ""
}

// detachBlocked reports why M-w must be refused, or "" to allow it. With no
// split the sole work pane's client is the user's only terminal, and tmux
// closes a pane whose client exits — detaching it would drop them out.
func detachBlocked(splitOpen bool) string {
	if !splitOpen {
		return "sole work pane — nothing to detach"
	}
	return ""
}

// doSplit is the split's shell layer: open the pane, then pin the layout. A var
// so tests observe the emitted tmux (and feed back a pane id/tty) without a
// live server. Returns the new work pane, ok=false on any tmux failure.
var doSplit = func(beside outerPane, inner innerSocket, session string) (workPaneRef, bool) {
	out, err := runOuterOut(splitWindowArgs(beside, inner, session)...)
	if err != nil {
		return workPaneRef{}, false
	}
	id, tty, ok := strings.Cut(out, " ")
	if !ok {
		logf("split-window: unexpected -P output %q", out)
		return workPaneRef{}, false
	}
	// Disable the pane's inherited global remain-on-exit so it closes when its
	// inner client detaches instead of lingering as a dead pane (see remainOffArgs).
	runOuter(remainOffArgs(outerPane(id))...)
	// pin the sidebar; the two work panes stack in the right column, sharing its height
	runOuter(mainVerticalArgs()...)
	return workPaneRef{outer: outerPane(id), client: clientTTY(tty)}, true
}

// detachClient detaches the split pane's inner client, kills its outer pane as
// belt-and-braces, then re-pins the layout so the sidebar keeps its width once
// the pane closes. A var for the same testing reason as doSplit. Off the UI
// goroutine — a tmux exec must never stall a paint.
var detachClient = func(tty clientTTY, pane outerPane) {
	go func() {
		if runTmux(detachClientArgs(tty)...) != nil {
			return // runTmux already logged tmux's own message
		}
		// Belt-and-braces: remain-on-exit off already closes the pane when the
		// client exits, but kill-pane guarantees it. tmux tolerates it already gone.
		runOuter(killPaneArgs(pane)...)
		runOuter(mainVerticalArgs()...)
	}()
}

// splitDoneMsg carries the result of the off-UI-goroutine doSplit back to
// Update, where the split's model state (splitOpen, alt) is set — nothing may
// mutate the model from the exec goroutine (R13 shared state).
type splitDoneMsg struct {
	pane workPaneRef
	ok   bool
}

// openInSplit is the reusable entry point: it opens one session in a second
// work pane, refusing when splitBlocked says so. The #776 right-click `Open in
// split` menu item calls this directly on the card it acts on, so the split
// logic never has to leak into menu.go / menuops.go / menuview.go.
//
// The synchronous guards refuse (and set status) inline, returning no command;
// on a pass it returns a tea.Cmd that runs doSplit off the UI goroutine — a
// tmux exec must never stall a paint — and reports the result as a splitDoneMsg
// for Update to apply.
func (m *model) openInSplit(session string, fake bool) tea.Cmd {
	if !env.wrapped() || m.activeOuter() == "" {
		m.setStatus("open in split needs the outer shell")
		return nil
	}
	if m.splitOpen || m.splitPending {
		// One split at a time (#777 keep-it-simple): a second would orphan the
		// pane already tracked in m.alt, leaving M-w unable to close it. Refuse
		// while one is in flight too (splitPending) — the async doSplit window
		// otherwise lets a second M-Enter through before splitOpen is set.
		m.setStatus("split already open — M-w closes it first")
		return nil
	}
	if why := splitBlocked(session, fake, m.attachedBySess); why != "" {
		m.setStatus(why)
		return nil
	}
	beside := m.activeOuter() // snapshot on the UI goroutine; the exec reads no model state
	m.splitPending = true
	return func() tea.Msg {
		pane, ok := doSplit(beside, env.inner, session)
		return splitDoneMsg{pane: pane, ok: ok}
	}
}

// splitSelected opens the card the pane is describing in a split — the M-Enter
// gesture. It reads railRow (not the raw cursor) so a filtered-away selection
// still splits the card the user sees the rail on, exactly as P and the pin
// reorder keys do. It returns openInSplit's command so the caller hands it to
// Bubble Tea.
func (m *model) splitSelected() tea.Cmd {
	if r, ok := m.railRow(); ok {
		return m.openInSplit(r.session, r.fake)
	}
	return nil
}

// detachSplit closes the split pane (M-w), restoring the two-pane layout, or
// refuses on the sole work pane with a status message.
func (m *model) detachSplit() {
	if why := detachBlocked(m.splitOpen); why != "" {
		m.setStatus(why)
		return
	}
	detachClient(m.alt.client, m.alt.outer)
	m.splitOpen = false
	m.alt = workPaneRef{}
	m.workOverride = workPaneRef{} // drive the original pane again
}

// workTTYs is the set of the wrapper's own work-pane clients pickWork chooses
// among in split mode — nil (the scoped, single-pane path) until a split opens.
func (m *model) workTTYs() []clientTTY {
	if !m.splitOpen {
		return nil
	}
	return []clientTTY{env.client, m.alt.client}
}

// retargetWork points the sidebar at the work pane the client lane found
// focused (its tty), so the switch and hand-back follow the focus.
func (m *model) retargetWork(tty clientTTY) {
	if !m.splitOpen {
		return
	}
	m.workOverride = workPaneRef{client: tty, outer: m.outerFor(tty)}
}

// outerFor maps a work client's tty to its outer pane. Only two exist: the
// wrapper's original pane and the one Open in split created.
func (m *model) outerFor(tty clientTTY) outerPane {
	if tty == m.alt.client {
		return m.alt.outer
	}
	return env.outer
}

// statusHold is how long a refusal or notice sits on the hint line before the
// keys return. Long enough to read, short enough not to linger.
const statusHold = 4 * time.Second

func (m *model) setStatus(s string) { m.status, m.statusAt = s, time.Now() }

// statusText is the message the footer shows in place of the key hints, or ""
// once it has aged out.
func (m *model) statusText() string {
	if m.status == "" || time.Since(m.statusAt) > statusHold {
		return ""
	}
	return m.status
}

// runOuterOut is runOuter that also returns stdout — split-window -P prints the
// new pane's id and tty, which the sidebar needs to track it.
var runOuterOut = func(args ...string) (string, error) {
	cmd := env.outerCmd(args...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		logf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}
