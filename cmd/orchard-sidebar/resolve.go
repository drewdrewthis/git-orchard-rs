package main

import (
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Click-time client resolution and env-shape validation (orchardist#787).
//
// The wrapper hands the sidebar ORCHARD_TMUX_CLIENT (the inner client's tty)
// and ORCHARD_OUTER_PANE (the pane focus is handed back to). A stale/prototype
// outer-shell launcher can hand a tty that no longer belongs to any client and
// a pane id that is the sidebar's own — every switch then runs `switch-client
// -c <dead tty>` and fails silently. Rather than trust the env verbatim, the
// switch validates it at the point of use and, when it is stale, falls back
// ONLY to the wrapper's own inner attach: outer pane 0.1, whose #{pane_tty} is
// exactly the inner server's #{client_tty} for it (cmd/orchard-shell/outer.go).
// It never retargets to "any" or "the only" live client — that would hijack a
// foreign terminal, the boundary docs/outer-shell.md draws (#747 defect 2).

// clientNotFoundStatus is the footer error a click shows when no trustworthy
// inner client can be resolved — the visible signal that replaces the old
// silent failure (#787 AC1).
const clientNotFoundStatus = "inner client not found — restart the outer shell"

// launcherOutdatedStatus is the one-time startup hint that the env shape is one
// only a stale launcher produces; a fresh build from main trips neither leg of
// the drift check (#787 AC3).
const launcherOutdatedStatus = "outer-shell launcher outdated — reinstall from main"

// onceGuard logs (or runs) its first call and drops the rest: a stale env is
// stale for the whole process life, so a per-click log line would bury
// sidebar.log under identical failures. Unlike sync.Once it exposes reset, so a
// test can restore the fresh-process state a package var otherwise carries
// between cases. Its atomic makes it safe for the concurrent hand-back
// goroutines switchClientExec spawns.
type onceGuard struct{ done atomic.Bool }

func (o *onceGuard) do(f func()) {
	if o.done.CompareAndSwap(false, true) {
		f()
	}
}

func (o *onceGuard) reset() { o.done.Store(false) }

// resolveFailGuard and outerPaneGuard hold the once-per-process semantics for
// the two stale-env log lines (the click's no-client failure and the
// hand-back's bad-outer-pane fallback).
var (
	resolveFailGuard onceGuard
	outerPaneGuard   onceGuard
)

// resolveClientTTY validates want (the wrapper's ORCHARD_TMUX_CLIENT, or a
// split pane's client) against the inner server's live clients at click time,
// returning it unchanged when it is live. When it is not, it falls back to
// outer pane 0.1's tty — and only if that too is a live inner client. ok=false
// means nothing trustworthy to switch: the caller shows clientNotFoundStatus
// instead of scoping a switch to a client that is not there.
//
// Unwrapped (legacy) mode is left entirely to switchClientArgs: there is one
// tmux server and no inner client to validate against.
func resolveClientTTY(want clientTTY) (clientTTY, bool) {
	if !env.wrapped() {
		return want, true
	}
	live, ok := liveInnerClients()
	if !ok {
		return "", false
	}
	if clientAttached(want, live) {
		return want, true
	}
	_, tty, ok := outerInnerPane()
	if !ok || tty == "" || !live[tty] {
		return "", false
	}
	// Memoize the working tty so the browse / switch / popup paths — which read
	// the current target through activeClientTTY() without resolving — track the
	// fallback too, instead of re-failing `switch-client -c <stale>` on every
	// keypress. Written to the synchronized accessor, not env.client: env.client
	// stays the immutable launch-time shape the drift check judges, and the
	// accessor is race-safe across the UI, drift and popup goroutines (#787).
	setActiveClientTTY(tty)
	return tty, true
}

// liveInnerClients reads the inner server's attached client ttys as a set.
// ok=false when the read errored or returned no clients: both mean "no client
// to trust", which forces the footer error rather than a fallback (#787 AC1).
func liveInnerClients() (map[clientTTY]bool, bool) {
	out, err := runTmuxOutput("list-clients", "-F", "#{client_tty}")
	if err != nil {
		return nil, false
	}
	set := map[clientTTY]bool{}
	for _, ln := range strings.Split(out, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			set[clientTTY(ln)] = true
		}
	}
	if len(set) == 0 {
		return nil, false
	}
	return set, true
}

// outerInnerPane reads outer pane 0.1's id and tty — the pane running THIS
// wrapper's inner attach. Pane index 1 is addressed directly (0.0 is the
// sidebar, 0.1 the inner client) rather than "the other pane": a #777 split
// adds a third pane and only index 1 is the inner attach. The read runs on the
// outer server (runOuterOut, via $TMUX) — the same server every outerPane id
// belongs to.
func outerInnerPane() (id outerPane, tty clientTTY, ok bool) {
	out, err := runOuterOut("list-panes", "-F", "#{pane_index} #{pane_id} #{pane_tty}")
	if err != nil {
		return "", "", false
	}
	for _, ln := range strings.Split(out, "\n") {
		idx, rest, k := strings.Cut(strings.TrimSpace(ln), " ")
		if !k || idx != "1" {
			continue
		}
		pid, ptty, k := strings.Cut(rest, " ")
		if !k {
			continue
		}
		return outerPane(pid), clientTTY(ptty), true
	}
	return "", "", false
}

// validOuterPane reports whether p is a usable focus target: a %N pane id
// (never a tty path or a window address — #747) that is not the sidebar's own
// pane (handing focus to ourselves leaves the shell unfocused, #787 AC2).
func validOuterPane(p, self outerPane) bool {
	return isPaneID(p) && p != self
}

// isPaneID reports whether p is tmux's %N pane-id form.
func isPaneID(p outerPane) bool {
	if len(p) < 2 || p[0] != '%' {
		return false
	}
	for _, r := range p[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// clientAttached reports whether tty names a currently attached inner client.
// The shared predicate for "trust this client tty" that both the click-time
// resolver and the startup drift check turn on.
func clientAttached(tty clientTTY, live map[clientTTY]bool) bool {
	return tty != "" && live[tty]
}

// Startup drift-check polling knobs. envDriftStatus judges the client-attached
// leg by RETRYING, not at t0: Init fires the check before pane 0.1's `TMUX=
// tmux attach` has connected, so a single read sees zero inner clients and
// every healthy launch would false-positive as "stale launcher" (#787). Vars
// so a test drives the retry loop with a no-op sleep instead of real time.
var (
	driftPollEvery    = 250 * time.Millisecond
	driftPollAttempts = 12 // ~3s ceiling at 250ms between reads
	driftSleep        = time.Sleep
)

// waitClientAttached polls the inner server until env.client shows up as an
// attached client, returning true the moment it does. It returns false only
// after driftPollAttempts reads still find it absent — the settled "the inner
// attach never connected" verdict, not the t0 startup race.
func waitClientAttached() bool {
	for i := 0; i < driftPollAttempts; i++ {
		if live, ok := liveInnerClients(); ok && clientAttached(env.client, live) {
			return true
		}
		if i < driftPollAttempts-1 {
			driftSleep(driftPollEvery)
		}
	}
	return false
}

// envDriftStatus is the startup drift check: it returns launcherOutdatedStatus
// (and logs one line) when the wrapper's env shape is one a current launcher
// would not produce — a non-%N outer pane, or a client tty that never attaches
// within the poll window (#787 AC3) — or ok=false when it is healthy or the
// sidebar is unwrapped. The client-attached leg is settled via
// waitClientAttached so a fresh healthy launch, whose inner attach connects a
// beat after startup, no longer trips a false positive.
func envDriftStatus() (string, bool) {
	if !env.wrapped() {
		return "", false
	}
	outerValid := validOuterPane(env.outer, env.self)
	attached := waitClientAttached()
	if outerValid && attached {
		return "", false
	}
	logf("env drift: outer pane %q is-pane-id=%v, client %q attached=%v — stale outer-shell launcher (#787)",
		string(env.outer), outerValid, string(env.client), attached)
	return launcherOutdatedStatus, true
}

// driftMsg carries the settled drift verdict from driftCheck (run as a tea.Cmd,
// off the UI goroutine) to Update, which applies the footer hint on the UI
// goroutine (R13 shared-state discipline). show=false leaves the footer alone.
type driftMsg struct {
	status string
	show   bool
}

// driftCheck runs the startup drift check as a tea.Cmd: waitClientAttached
// polls off the UI goroutine so the inner attach can settle, rather than being
// judged synchronously at boot when it always reads as not-yet-connected.
// Fired once from Init, so the hint shows once per process.
func driftCheck() tea.Msg {
	status, show := envDriftStatus()
	return driftMsg{status: status, show: show}
}
