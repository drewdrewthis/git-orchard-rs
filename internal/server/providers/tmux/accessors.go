// accessors.go — the read surface resolvers use to reach the tmux cache.
//
// Every accessor here answers a keyed or narrowly-filtered question and
// allocates only for what it returns. Provider.Snapshot() (provider.go)
// clones the entire graph and is reserved for collection entrypoints that
// genuinely need it; a field resolver reaching for it is the #612 60s
// cold-lens-load regression (RULES.md R3, O9).
//
// Immutability: a returned value is never a window onto a cached entry.
// Scalar-only types (Session, Window, Client) are safe as plain struct
// copies; Pane carries a slice, so it goes through Pane.clone().

package tmux

import (
	"path/filepath"
	"strings"
)

// ----------------------------------------------------------------------
// Keyed accessors — one map lookup, no clone.
// ----------------------------------------------------------------------

// PaneByID returns the pane whose stable pane id (e.g. "%26") matches on
// the given host, or (Pane{}, false) when not found.
func (p *Provider) PaneByID(host, paneID string) (Pane, bool) {
	pn, _, ok := p.panes.Get(PaneKey{Host: HostID(host), PaneID: paneID})
	if !ok {
		return Pane{}, false
	}
	return pn.clone(), true
}

// SessionByName returns the session named `name` on the given host, or
// (Session{}, false) when not found.
func (p *Provider) SessionByName(host, name string) (Session, bool) {
	s, _, ok := p.sessions.Get(SessionKey{Host: HostID(host), Name: name})
	return s, ok
}

// WindowByKey returns the window at `index` within `session` on the given
// host, or (Window{}, false) when not found.
func (p *Provider) WindowByKey(host, session string, index int) (Window, bool) {
	w, _, ok := p.windows.Get(WindowKey{Host: HostID(host), Session: session, Index: index})
	return w, ok
}

// ClientByName returns the client named `clientName` on the given host,
// or (Client{}, false) when not found.
func (p *Provider) ClientByName(host, clientName string) (Client, bool) {
	c, _, ok := p.clients.Get(ClientKey{Host: HostID(host), ClientName: clientName})
	return c, ok
}

// ----------------------------------------------------------------------
// Filtered accessors — one pass over a single store, allocating only for
// the entries that match. Order is unspecified; the result is never nil.
// ----------------------------------------------------------------------

// WindowsBySession returns every window belonging to `session` on the given host.
func (p *Provider) WindowsBySession(host, session string) []Window {
	if session == "" {
		return []Window{}
	}
	return orEmpty(p.windows.Filter(func(k WindowKey, _ Window) bool {
		return string(k.Host) == host && k.Session == session
	}))
}

// PanesByWindow returns every pane in the window at `index` within `session` on
// the given host.
func (p *Provider) PanesByWindow(host, session string, index int) []Pane {
	if session == "" {
		return []Pane{}
	}
	return clonePanes(p.panes.Filter(func(k PaneKey, pn Pane) bool {
		return string(k.Host) == host &&
			pn.WindowKey.Session == session &&
			pn.WindowKey.Index == index
	}))
}

// PanesByHost returns every cached pane on the given host.
//
// This is the widest accessor here and exists for the one read that
// genuinely needs every pane: the worktree join resolves each pane's cwd
// through the ps provider, and cwd is not in the tmux cache to filter on.
// It still beats Snapshot() — one slice of one store, not four maps.
func (p *Provider) PanesByHost(host string) []Pane {
	return clonePanes(p.panes.Filter(func(k PaneKey, _ Pane) bool {
		return string(k.Host) == host
	}))
}

// SessionsByHost returns every cached session on the given host.
func (p *Provider) SessionsByHost(host string) []Session {
	return orEmpty(p.sessions.Filter(func(k SessionKey, _ Session) bool {
		return string(k.Host) == host
	}))
}

// ClientsByHost returns every cached client on the given host.
func (p *Provider) ClientsByHost(host string) []Client {
	return orEmpty(p.clients.Filter(func(k ClientKey, _ Client) bool {
		return string(k.Host) == host
	}))
}

// PanesBySession returns every pane whose tmux session name equals
// sessionName on the given host.
func (p *Provider) PanesBySession(host, sessionName string) []Pane {
	if sessionName == "" {
		return []Pane{}
	}
	return clonePanes(p.panes.Filter(func(k PaneKey, pn Pane) bool {
		return string(k.Host) == host && pn.WindowKey.Session == sessionName
	}))
}

// ClientsBySession returns every client attached to `session` on the given
// host.
func (p *Provider) ClientsBySession(host, session string) []Client {
	if session == "" {
		return []Client{}
	}
	return orEmpty(p.clients.Filter(func(k ClientKey, c Client) bool {
		return string(k.Host) == host && c.Session == session
	}))
}

// ClientsByCurrentPane returns every client whose currently-active pane is
// paneID on the given host.
func (p *Provider) ClientsByCurrentPane(host, paneID string) []Client {
	if paneID == "" {
		return []Client{}
	}
	return orEmpty(p.clients.Filter(func(k ClientKey, c Client) bool {
		return string(k.Host) == host && c.CurrentPane == paneID
	}))
}

// ----------------------------------------------------------------------
// ps-backed axis accessors (ADR-022: Pane is the node, one cache read per
// accessor, no N+1 in the callers).
//
// These resolve a pid through the ps provider, which does I/O. They read
// the pane set first and filter afterwards, so no ps call ever runs while
// the store's read lock is held.
// ----------------------------------------------------------------------

// PanesByCwd returns every pane on host whose foreground-process cwd
// equals cwd exactly or has cwd+"/" as a prefix. The cwd is resolved via
// the supplied psGetter; panes whose cwd cannot be resolved are silently
// skipped. Returns [] (never nil).
//
// The psGetter is a narrow interface satisfied by *psprovider.Provider
// via a thin adapter; it is passed in rather than stored on Provider to
// keep the tmux package free of a ps import.
func (p *Provider) PanesByCwd(host, cwd string, ps PanePsGetter) []Pane {
	if cwd == "" {
		return []Pane{}
	}
	var out []Pane
	for _, pn := range p.PanesByHost(host) {
		if pn.CurrentPid <= 0 {
			continue
		}
		paneCwd := ps.CwdForPid(host, pn.CurrentPid)
		if paneCwd == "" {
			continue
		}
		if paneCwd != cwd && !strings.HasPrefix(paneCwd, cwd+"/") {
			continue
		}
		out = append(out, pn)
	}
	return orEmpty(out)
}

// PanesByCommand returns every pane on host whose foreground command
// basename contains basenameContains (case-insensitive). The command is
// cross-checked via the supplied psGetter so node-wrapped CLIs (e.g.
// `node /usr/local/bin/claude`) resolve to their real basename instead of
// the raw tmux pane_current_command string. Returns [] (never nil).
func (p *Provider) PanesByCommand(host, basenameContains string, ps PanePsGetter) []Pane {
	if basenameContains == "" {
		return []Pane{}
	}
	needle := strings.ToLower(basenameContains)
	var out []Pane
	for _, pn := range p.PanesByHost(host) {
		if paneCommandMatchesClaude(pn, host, ps, needle) {
			out = append(out, pn)
		}
	}
	return orEmpty(out)
}

// PaneRunsCommand reports whether pn is running needle, matched
// case-insensitively against the command basename. It is the same detection
// PanesByCommand applies, exported so a resolver holding a single already-known
// pane can ask the question without re-deriving it — one source of truth for
// "is this pane running claude".
func PaneRunsCommand(pn Pane, host string, ps PanePsGetter, needle string) bool {
	if needle == "" {
		return false
	}
	return paneCommandMatchesClaude(pn, host, ps, strings.ToLower(needle))
}

// paneCommandMatchesClaude reports whether a pane is running needle
// (lower-case), consulting two independent signals.
//
// CurrentPid is tmux's pane_pid — the pane's ROOT process, NOT its foreground
// process. A session started as `bash -> claude` has a bash root pid, so asking
// ps about it answers "bash" even though claude is very much running. tmux's own
// pane_current_command resolves the foreground through the shell and answers
// "claude" correctly.
//
// So the two signals are complementary, not ranked: ps sees through wrapper
// processes tmux reports verbatim, and tmux sees through shells ps cannot.
// Either one matching is a match. Previously a non-empty ps answer returned
// early and shadowed tmux's, which made every shell-wrapped claude session
// invisible to Query.claudeInstances — and the concurrency cap that reads it
// counted 0 workers while workers were running (#706).
func paneCommandMatchesClaude(pn Pane, host string, ps PanePsGetter, needle string) bool {
	if ps != nil && pn.CurrentPid > 0 {
		cmd := ps.CommandForPid(host, pn.CurrentPid)
		if cmd != "" && strings.Contains(strings.ToLower(filepath.Base(cmd)), needle) {
			return true
		}
	}
	// tmux pane_current_command (may be a version string on macOS, hence ps above).
	return strings.Contains(strings.ToLower(filepath.Base(pn.CurrentCommand)), needle)
}

// PanePsGetter is the narrow ps surface the secondary-axis accessors need.
// *psprovider.Provider satisfies this via a thin adapter (see loaders package).
// Tests implement it inline.
type PanePsGetter interface {
	// CwdForPid returns the working directory of the process with the given
	// pid on the host, or "" when unavailable.
	CwdForPid(host string, pid int) string
	// CommandForPid returns the command basename (e.g. "claude") for the
	// given pid on the host, or "" when unavailable.
	CommandForPid(host string, pid int) string
}

// ----------------------------------------------------------------------
// Result shaping.
// ----------------------------------------------------------------------

// clonePanes copies each pane's reference-typed fields so the caller holds
// nothing the store also holds. Returns an empty (non-nil) slice for an
// empty input.
func clonePanes(panes []Pane) []Pane {
	out := make([]Pane, len(panes))
	for i, pn := range panes {
		out[i] = pn.clone()
	}
	return out
}

// orEmpty substitutes an empty slice for a nil one, so an accessor never
// hands a caller a nil to distinguish from "no matches".
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
