// droplog.go makes the `-F` field-count-mismatch drop observable (issue #701,
// defect D3 / AC12). Previously listAll and listClients dropped a row whose
// field count did not match with a bare `continue` and no signal, so the D3
// locale defect (TAB sanitized to `_`, every row collapsing to one field)
// surfaced as a silently empty tmuxSessions while the server read as alive.

package tmux

import "log/slog"

// WithLogger returns a copy of a that emits adapter-level diagnostics (the
// field-count-mismatch drop warning) to l.
//
// Note: the returned copy SHARES the alive-cache pointer with the receiver —
// see WithSocket.
func (a *Adapter) WithLogger(l *slog.Logger) *Adapter {
	cp := *a
	cp.logger = l
	return &cp
}

// log returns the adapter's logger, defaulting to slog.Default() when none was
// set so callers never nil-check.
func (a *Adapter) log() *slog.Logger {
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

// dropCounter tallies rows dropped in one `-F` parse pass because their
// field count did not match the format's. It records the first mismatching
// row's field count (the D3 case collapses a row to 1 field) so the WARN
// can report what was actually seen. Shared by listAll and listClients so
// the two drop loops stay identical.
type dropCounter struct {
	dropped  int
	firstGot int
}

// skip records one dropped row whose split produced got fields.
func (c *dropCounter) skip(got int) {
	if c.dropped == 0 {
		c.firstGot = got
	}
	c.dropped++
}

// warnDroppedRows emits ONE WARN per parse pass when c recorded any drop.
// It reports counts only — never raw row content, which can carry pane
// titles or cwd paths.
//
// cmd names the tmux subcommand (e.g. "list-panes -a"); expected is the
// format's field count.
func (a *Adapter) warnDroppedRows(cmd string, c dropCounter, expected int) {
	if c.dropped == 0 {
		return
	}
	a.log().Warn("tmux: dropped rows with unexpected field count",
		"cmd", cmd,
		"dropped", c.dropped,
		"expected_fields", expected,
		"got_fields", c.firstGot,
	)
}
