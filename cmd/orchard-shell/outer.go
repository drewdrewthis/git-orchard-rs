package main

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// The two panes of the wrapper's single window. 0.0 is the sidebar, 0.1 the
// nested inner client.
const (
	paneSidebar = outerSessionName + ":0.0"
	paneInner   = outerSessionName + ":0.1"
)

// wrapper drives one outer tmux server.
type wrapper struct {
	opts Options
	conf string
	tmux tmuxExec
	log  io.Writer
}

func (w *wrapper) outer(args ...string) (string, error) {
	return w.tmux(outerArgs(w.opts.OuterSocket, w.conf, args...)...)
}

func (w *wrapper) inner(args ...string) (string, error) {
	return w.tmux(innerArgs(w.opts.InnerSocket, args...)...)
}

// outerState is what already exists when orchard shell starts.
type outerState struct {
	sessionExists bool // the outer session is up
	paneExists    bool // its pane 0.1 is addressable
	innerLive     bool // 0.1's tty is an attached client on the inner server
}

// action is what orchard shell does about that state.
type action int

const (
	actionBoot    action = iota // nothing there: build the wrapper
	actionAttach                // healthy: just attach
	actionRespawn               // 0.1's inner client is dead: rebuild it first
	actionBroken                // the session exists but has no pane 0.1
)

// decide is the reattach decision table. Re-running orchard shell is the
// normal way to get back to the wrapper, so the only interesting question is
// whether pane 0.1 still holds a LIVE inner client — attaching to a corpse
// presents as "the right pane is a dead shell", which is easy to mistake for
// the TMUX= nesting bug when triaging.
func decide(s outerState) action {
	switch {
	case !s.sessionExists:
		return actionBoot
	case !s.paneExists:
		return actionBroken
	case !s.innerLive:
		return actionRespawn
	default:
		return actionAttach
	}
}

// probe reads the current state of the outer session.
func (w *wrapper) probe() outerState {
	if _, err := w.outer("has-session", "-t", outerSessionName); err != nil {
		return outerState{}
	}
	tty, err := w.outer("display", "-p", "-t", paneInner, "#{pane_tty}")
	if err != nil || tty == "" {
		return outerState{sessionExists: true}
	}
	return outerState{sessionExists: true, paneExists: true, innerLive: w.innerHasClient(tty)}
}

// innerHasClient reports whether tty is attached as a client on the inner
// server. Outer pane 0.1 runs the inner client on the pane's own pty, so that
// pane's #{pane_tty} is exactly the inner server's #{client_tty} for it.
func (w *wrapper) innerHasClient(tty string) bool {
	out, err := w.inner("list-clients", "-F", "#{client_tty}")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == tty {
			return true
		}
	}
	return false
}

// innerSessions lists the inner server's sessions, most recently attached
// first, so an omitted --session picks up where the user left off.
func (w *wrapper) innerSessions() ([]string, error) {
	out, err := w.inner("list-sessions", "-F", "#{session_last_attached} #{session_name}")
	if err != nil {
		return nil, &noInnerServerError{socket: w.opts.InnerSocket, cause: err}
	}
	return sortSessionsByRecency(out), nil
}

// sortSessionsByRecency parses `<last_attached> <name>` lines into names,
// newest first. A session that has never been attached reports 0 and sorts
// last, keeping the listing stable rather than random.
func sortSessionsByRecency(out string) []string {
	type entry struct {
		at   int64
		name string
	}
	var entries []entry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// A tmux old enough not to know #{session_last_attached} renders it
		// empty, leaving the name alone on the line. Anything whose first
		// field is not a number is therefore a bare name — including one
		// with a space in it, which splitting blindly would truncate.
		stamp, name, ok := strings.Cut(line, " ")
		at, err := strconv.ParseInt(stamp, 10, 64)
		if !ok || err != nil {
			entries = append(entries, entry{name: line})
			continue
		}
		entries = append(entries, entry{at: at, name: name})
	}
	slices.SortStableFunc(entries, func(a, b entry) int { return cmp.Compare(b.at, a.at) })

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.name)
	}
	return names
}

// resolveSession picks the inner session to attach, or explains what exists
// instead of failing with a bare error.
func (w *wrapper) resolveSession() (string, error) {
	have, err := w.innerSessions()
	if err != nil {
		return "", err
	}
	if len(have) == 0 {
		return "", &noInnerServerError{socket: w.opts.InnerSocket}
	}
	if w.opts.Session == "" {
		return have[0], nil
	}
	for _, s := range have {
		if s == w.opts.Session {
			return s, nil
		}
	}
	return "", &sessionMissingError{want: w.opts.Session, socket: w.opts.InnerSocket, have: have}
}

// boot builds the wrapper from nothing.
func (w *wrapper) boot(session string) error {
	cols, rows := termSize()
	if _, err := w.outer("new-session", "-d", "-s", outerSessionName,
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows)); err != nil {
		return err
	}

	// split-window -h -b -l <width>: the new pane goes before (-b, left of)
	// the target at an exact width. The NEW pane becomes 0.0; the pane that
	// existed before the split becomes 0.1.
	//
	// The split MUST happen before either send-keys. Sending to "0.0" first
	// targets the pre-split sole pane — which the split then renumbers to 0.1
	// — so both commands land in the SAME physical pane while the true 0.0
	// gets nothing. That was the original bug: the sidebar ended up in 0.1
	// with the attach keystrokes typed into its TUI and swallowed.
	if _, err := w.outer("split-window", "-h", "-b", "-l", strconv.Itoa(w.opts.Width),
		"-t", outerSessionName+":0"); err != nil {
		return err
	}
	if _, err := w.outer("send-keys", "-t", paneInner, innerAttachCommand(w.opts.InnerSocket, session), "Enter"); err != nil {
		return err
	}
	return w.startSidebar()
}

// startSidebar launches pane 0.0 with the env the sidebar needs, resolved
// from pane 0.1 AFTER its inner attach has been sent — 0.1's #{pane_tty} is
// the inner client's own tty only once that client exists.
func (w *wrapper) startSidebar() error {
	tty, err := w.outer("display", "-p", "-t", paneInner, "#{pane_tty}")
	if err != nil {
		return err
	}
	// #{pane_id} (e.g. %1), not a tty path: it is stable across resizes and
	// redraws, and it is what the sidebar hands keyboard focus back to.
	paneID, err := w.outer("display", "-p", "-t", paneInner, "#{pane_id}")
	if err != nil {
		return err
	}

	cmd := placeholderCommand(w.opts.InnerSocket)
	if bin := resolveSidebar(); bin != "" {
		cmd = sidebarCommand(bin, w.opts.InnerSocket, tty, paneID)
	} else {
		fmt.Fprintf(w.log, "orchard shell: no orchard-sidebar found beside %s or on $PATH; using the watch(1) placeholder\n", selfPath())
	}
	_, err = w.outer("send-keys", "-t", paneSidebar, cmd, "Enter")
	return err
}

// respawn rebuilds pane 0.1's inner client, and then pane 0.0's sidebar.
//
// Both, in that order, because respawn-pane gives the pane a new pty: the
// sidebar's ORCHARD_TMUX_CLIENT names 0.1's OLD tty and would scope every
// switch-client to a client that no longer exists. Re-reading the tty first
// and relaunching the sidebar second is what keeps the two facts agreeing.
func (w *wrapper) respawn(session string) error {
	if _, err := w.outer("respawn-pane", "-k", "-t", paneInner,
		innerAttachCommand(w.opts.InnerSocket, session)); err != nil {
		return err
	}
	tty, err := w.outer("display", "-p", "-t", paneInner, "#{pane_tty}")
	if err != nil {
		return err
	}
	paneID, err := w.outer("display", "-p", "-t", paneInner, "#{pane_id}")
	if err != nil {
		return err
	}
	cmd := placeholderCommand(w.opts.InnerSocket)
	if bin := resolveSidebar(); bin != "" {
		cmd = sidebarCommand(bin, w.opts.InnerSocket, tty, paneID)
	}
	_, err = w.outer("respawn-pane", "-k", "-t", paneSidebar, cmd)
	return err
}

// focusInner moves focus to 0.1, unconditionally, on boot AND on every
// re-run. tmux leaves the newly-split pane (0.0) active by default, and with
// mouse-only focus and no prefix a user landing on 0.0 has no way to move
// off it at all — #747's original live defect.
func (w *wrapper) focusInner() error {
	_, err := w.outer("select-pane", "-t", paneInner)
	return err
}
