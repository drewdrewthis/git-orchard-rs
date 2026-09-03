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
	opts     Options
	conf     string
	tmux     tmuxExec
	log      io.Writer
	lookPath pathLookup // exec.LookPath; injected so sidebar-found/missing is pinnable in tests
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
	paneCount     int  // how many panes window 0 currently has
	pane0Dead     bool // the sidebar pane itself has exited (remain-on-exit)
	pane1Dead     bool // the inner-client pane itself has exited
	innerLive     bool // 0.1's tty is an attached client on the inner server
	// (only meaningful when paneCount==2 and pane1 is alive)
}

// action is what orchard shell does about that state.
type action int

const (
	actionBoot    action = iota // nothing there: build the wrapper
	actionAttach                // healthy: just attach
	actionRespawn               // right shape, something inside it is dead
	actionRebuild               // wrong pane count: reconstruct the layout first
)

// decide is the reattach decision table. Re-running orchard shell is the
// normal way to get back to the wrapper, so the interesting questions are (1)
// does the window even have the right two panes, and (2) if so, is either
// pane itself dead or pane 0.1's inner client gone — attaching to a corpse
// presents as "the right pane is a dead shell", which is easy to mistake for
// the TMUX= nesting bug when triaging.
//
// remain-on-exit (outer.conf) keeps a pane whose process exited addressable
// as a DEAD pane instead of tmux closing it and renumbering its sibling —
// that renumbering was #747's live defect (a rerun's hardcoded "0.1" landed
// on the wrong, surviving pane and failed outright). Any pane count other
// than 2 is therefore something remain-on-exit does not smooth over on its
// own (manual pane close/split, or a session predating this option) and
// needs a full rebuild rather than a targeted respawn.
func decide(s outerState) action {
	switch {
	case !s.sessionExists:
		return actionBoot
	case s.paneCount != 2:
		return actionRebuild
	case s.pane0Dead || s.pane1Dead || !s.innerLive:
		return actionRespawn
	default:
		return actionAttach
	}
}

// probe reads the current state of the outer session's window 0 with a
// single list-panes call. #{pane_id} is deliberately not read here: it is
// only needed by rebuild(), which re-lists it itself when it actually needs
// to target individual panes for kill-pane.
func (w *wrapper) probe() outerState {
	if _, err := w.outer("has-session", "-t", outerSessionName); err != nil {
		return outerState{}
	}
	out, err := w.outer("list-panes", "-t", outerSessionName+":0",
		"-F", "#{pane_index} #{pane_dead} #{pane_tty}")
	if err != nil {
		return outerState{sessionExists: true}
	}

	var ttys [2]string
	var dead [2]bool
	count := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count++
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			continue
		}
		idx, err := strconv.Atoi(fields[0])
		if err != nil || idx < 0 || idx > 1 {
			continue
		}
		dead[idx] = fields[1] == "1"
		ttys[idx] = fields[2]
	}

	s := outerState{sessionExists: true, paneCount: count}
	if count != 2 {
		return s
	}
	s.pane0Dead, s.pane1Dead = dead[0], dead[1]
	if !s.pane1Dead && ttys[1] != "" {
		s.innerLive = w.innerHasClient(ttys[1])
	}
	return s
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

// bootPane is boot's single starting pane before the sidebar split pushes
// it to paneInner (0.1) — the same window-level address boot's own
// split-window targets.
const bootPane = outerSessionName + ":0"

// boot builds the wrapper from nothing. detach-on-destroy is NOT set here:
// ensureReady() sets it once for every path (boot/respawn/rebuild/attach)
// after the inner server is confirmed present, so setting it in boot too was a
// redundant second set-option on the boot path.
func (w *wrapper) boot(session string) error {
	cols, rows := termSize()
	if _, err := w.outer("new-session", "-d", "-s", outerSessionName,
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows)); err != nil {
		return err
	}

	// bootPane is unambiguous here: only one pane exists, so sending the
	// inner attach before splitting cannot land in the wrong pane.
	if _, err := w.outer("send-keys", "-t", bootPane, innerAttachCommand(w.opts.InnerSocket, session), "Enter"); err != nil {
		return err
	}
	return w.startSidebar(bootPane)
}

// startSidebar splits off pane 0.0 running the sidebar directly, as
// split-window's own command argument rather than a follow-up send-keys —
// send-keys races the pane's default shell starting up and reading the
// keystrokes (the 5s flake seen in verify.sh); handing tmux the command
// up front removes the race, matching respawn()'s directness.
//
// inner is the address of the already-created pane that becomes 0.1 —
// bootPane on boot, paneInner on any later rebuild. split-window -b puts
// the NEW pane before it, so inner is read for its env (tty, pane id)
// BEFORE the split, then becomes 0.1 once the split runs.
func (w *wrapper) startSidebar(inner string) error {
	tty, err := w.outer("display", "-p", "-t", inner, "#{pane_tty}")
	if err != nil {
		return err
	}
	// #{pane_id} (e.g. %1), not a tty path: it is stable across resizes and
	// redraws, and it is what the sidebar hands keyboard focus back to.
	paneID, err := w.outer("display", "-p", "-t", inner, "#{pane_id}")
	if err != nil {
		return err
	}

	cmd := placeholderCommand(w.opts.InnerSocket)
	if bin := resolveSidebarWith(w.lookPath); bin != "" {
		cmd = sidebarCommand(bin, w.opts.InnerSocket, tty, paneID)
	} else {
		fmt.Fprintf(w.log, "orchard shell: no orchard-sidebar found beside %s or on $PATH; using the watch(1) placeholder\n", selfPath())
	}
	// split-window -h -b -l <width>: the new pane goes before (-b, left of)
	// the target at an exact width, running cmd directly instead of the
	// default shell — same remain-on-exit semantics (outer.conf) apply to
	// whatever pane process exits, launched or not via send-keys.
	_, err = w.outer("split-window", "-h", "-b", "-l", strconv.Itoa(w.opts.Width), "-t", inner, cmd)
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
	return w.respawnSidebarPane()
}

// respawnSidebarPane rebuilds pane 0.0 with fresh env. It re-reads 0.1's tty
// FIRST: a respawn of 0.1 gives it a new pty, and the sidebar's
// ORCHARD_TMUX_CLIENT must name the tty that is live NOW — the ordering both
// respawn() (after it respawns 0.1) and recover-pane's sidebar path depend on,
// which is why the two share this one helper rather than each keeping a copy.
func (w *wrapper) respawnSidebarPane() error {
	tty, err := w.outer("display", "-p", "-t", paneInner, "#{pane_tty}")
	if err != nil {
		return err
	}
	paneID, err := w.outer("display", "-p", "-t", paneInner, "#{pane_id}")
	if err != nil {
		return err
	}
	cmd := placeholderCommand(w.opts.InnerSocket)
	if bin := resolveSidebarWith(w.lookPath); bin != "" {
		cmd = sidebarCommand(bin, w.opts.InnerSocket, tty, paneID)
	}
	_, err = w.outer("respawn-pane", "-k", "-t", paneSidebar, cmd)
	return err
}

// rebuild reconstructs the wrapper's two-pane layout from whatever window 0
// currently holds — 0, 1 or 3+ panes, any of them dead. It keeps one pane
// (by #{pane_id}, which is stable across kills and splits — unlike
// #{pane_index}, which renumbers as panes are removed), kills the rest,
// splits the survivor to recreate the sidebar/inner shape, re-pins the
// width, and hands off to respawn() to (re)launch both commands. Which pane
// survives the cull is not a decision that matters: respawn() overwrites
// both regardless of what rebuild kept.
func (w *wrapper) rebuild(session string) error {
	out, err := w.outer("list-panes", "-t", outerSessionName+":0", "-F", "#{pane_id}")
	if err != nil {
		return w.boot(session)
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ids = append(ids, line)
		}
	}
	if len(ids) == 0 {
		return w.boot(session)
	}

	for _, id := range ids[1:] {
		if _, err := w.outer("kill-pane", "-t", id); err != nil {
			return err
		}
	}

	// Same split shape as boot(): -b puts the NEW pane at 0.0, pushing the
	// kept survivor to 0.1. respawn() below relaunches both regardless.
	if _, err := w.outer("split-window", "-h", "-b", "-l", strconv.Itoa(w.opts.Width),
		"-t", ids[0]); err != nil {
		return err
	}
	if _, err := w.outer("set-window-option", "-t", outerSessionName+":0",
		"main-pane-width", strconv.Itoa(w.opts.Width)); err != nil {
		return err
	}
	if _, err := w.outer("select-layout", "-t", outerSessionName+":0", "main-vertical"); err != nil {
		return err
	}
	return w.respawn(session)
}

// disarmDetachOnDestroy turns off detach-on-destroy on the INNER server
// (AC0). tmux's default is to DETACH a client when the session it is viewing
// is destroyed; on the inner client running in pane 0.1 that would leave the
// pane a dead shell. Off, tmux switches the client to another session
// instead, so killing the session the user is looking at never strands the
// pane. Set here, on the server orchard-shell attaches to, rather than in the
// user's ~/.tmux.conf, so the guarantee holds regardless of their config.
// Best-effort: an inner server that is not up yet has nothing to set, and the
// caller's own attach will surface that.
func (w *wrapper) disarmDetachOnDestroy() {
	_, _ = w.inner("set-option", "-g", "detach-on-destroy", "off")
}

// focusInner moves focus to 0.1, unconditionally, on boot AND on every
// re-run. tmux leaves the newly-split pane (0.0) active by default, and with
// mouse-only focus and no prefix a user landing on 0.0 has no way to move
// off it at all — #747's original live defect.
func (w *wrapper) focusInner() error {
	_, err := w.outer("select-pane", "-t", paneInner)
	return err
}
