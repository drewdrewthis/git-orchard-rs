package main

import (
	"bytes"
	"strings"
	"time"
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

// workOverride is the focused work pane in split mode. The client lane
// discovers it from the inner server's most-recently-active work client
// (pickWork), so it moves as focus moves. Its zero value is the single-pane
// default — env.client / env.outer, unchanged from before #777 — which is why
// every pre-split path reads exactly as it did.
var workOverride workPaneRef

// activeClient / activeOuter are the client tty and outer pane the sidebar
// currently drives. switchClientArgs and handBackFocusArgs read these rather
// than env directly, so a click switches (and hands focus back to) the
// last-focused work pane, not always the one the sidebar launched in.
func activeClient() clientTTY {
	if workOverride.client != "" {
		return workOverride.client
	}
	return env.client
}

func activeOuter() outerPane {
	if workOverride.outer != "" {
		return workOverride.outer
	}
	return env.outer
}

// innerAttachCmd is a new work pane's command line: a second client on the same
// inner server, with $TMUX cleared so tmux does not hard-refuse to nest (the
// same reason orchard-shell's innerAttachCommand clears it). It is one argument
// to split-window, which tmux runs via /bin/sh, so the session is shell-quoted.
func innerAttachCmd(inner innerSocket, session string) string {
	return "TMUX= tmux -L " + shq(string(inner)) + " attach -t " + shq(session)
}

// splitWindowArgs opens a new outer pane to the right of beside, running a
// second inner client attached to session, and prints the new pane's id and
// tty (-P -F) so the sidebar can track it as the second work pane.
func splitWindowArgs(beside outerPane, inner innerSocket, session string) []string {
	return []string{"split-window", "-h", "-t", string(beside),
		"-P", "-F", "#{pane_id} #{pane_tty}", innerAttachCmd(inner, session)}
}

// mainVerticalArgs re-applies the layout that pins the sidebar at
// main-pane-width while the work panes share the remainder — the same layout
// outer.conf's resize hooks use, so the sidebar width survives both opening the
// split and closing it.
func mainVerticalArgs() []string { return []string{"select-layout", "main-vertical"} }

// detachClientArgs detaches the inner client in the split pane. The pane's
// inner attach has no remain-on-exit, so the pane closes when its client exits,
// which restores the two-pane layout.
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
	// pin the sidebar; the two work panes share the rest
	runOuter(mainVerticalArgs()...)
	return workPaneRef{outer: outerPane(id), client: clientTTY(tty)}, true
}

// detachClient detaches the split pane's inner client, then re-pins the layout
// so the sidebar keeps its width once the pane closes. A var for the same
// testing reason as doSplit. Off the UI goroutine — a tmux exec must never
// stall a paint.
var detachClient = func(tty clientTTY) {
	go func() {
		if runTmux(detachClientArgs(tty)...) != nil {
			return // runTmux already logged tmux's own message
		}
		runOuter(mainVerticalArgs()...)
	}()
}

// openInSplit is the reusable entry point: it opens one session in a second
// work pane, refusing when splitBlocked says so. The #776 right-click `Open in
// split` menu item calls this directly on the card it acts on, so the split
// logic never has to leak into menu.go / menuops.go / menuview.go.
func (m *model) openInSplit(session string, fake bool) {
	if !env.wrapped() || activeOuter() == "" {
		m.setStatus("open in split needs the outer shell")
		return
	}
	if m.splitOpen {
		// One split at a time (#777 keep-it-simple): a second would orphan the
		// pane already tracked in m.alt, leaving M-w unable to close it. Close
		// the current split first.
		m.setStatus("split already open — M-w closes it first")
		return
	}
	if why := splitBlocked(session, fake, m.attachedBySess); why != "" {
		m.setStatus(why)
		return
	}
	pane, ok := doSplit(activeOuter(), env.inner, session)
	if !ok {
		m.setStatus("split failed — see log")
		return
	}
	m.splitOpen = true
	m.alt = pane
}

// splitSelected opens the card the pane is describing in a split — the M-Enter
// gesture. It reads railRow (not the raw cursor) so a filtered-away selection
// still splits the card the user sees the rail on, exactly as P and the pin
// reorder keys do.
func (m *model) splitSelected() {
	if r, ok := m.railRow(); ok {
		m.openInSplit(r.session, r.fake)
	}
}

// detachSplit closes the split pane (M-w), restoring the two-pane layout, or
// refuses on the sole work pane with a status message.
func (m *model) detachSplit() {
	if why := detachBlocked(m.splitOpen); why != "" {
		m.setStatus(why)
		return
	}
	detachClient(m.alt.client)
	m.splitOpen = false
	m.alt = workPaneRef{}
	workOverride = workPaneRef{} // drive the original pane again
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
	workOverride = workPaneRef{client: tty, outer: m.outerFor(tty)}
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
func runOuterOut(args ...string) (string, error) {
	cmd := env.outerCmd(args...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		logf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// shq single-quotes s for a send-through-sh command line, leaving ordinary
// socket and session names untouched so the pane's command stays readable.
func shq(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("_.:/@%+=-", r)) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
