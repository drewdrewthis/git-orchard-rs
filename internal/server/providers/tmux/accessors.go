// accessors.go — the read surface resolvers use to reach the tmux cache.
//
// Every accessor here answers a keyed or narrowly-filtered question and
// allocates only for what it returns. Provider.Snapshot() (provider.go)
// clones the entire graph and is reserved for collection entrypoints that
// genuinely need it; a field resolver reaching for it is the #612 60s
// cold-lens-load regression (RULES.md R3, O9).

package tmux

import (
	"path/filepath"
	"strings"
)

// ----------------------------------------------------------------------
// Typed secondary-axis accessors (ADR-022: Pane is the node, one
// snapshot read per accessor, no N+1 in the callers).
// ----------------------------------------------------------------------

// PaneByID returns the pane whose stable pane id (e.g. "%26") matches on
// the given host, or (Pane{}, false) when not found.
func (p *Provider) PaneByID(host, paneID string) (Pane, bool) {
	snap := p.panes.Snapshot()
	key := PaneKey{Host: HostID(host), PaneID: paneID}
	pn, ok := snap[key]
	return pn, ok
}

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
	snap := p.panes.Snapshot()
	var out []Pane
	for _, pn := range snap {
		if string(pn.Key.Host) != host {
			continue
		}
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
	if out == nil {
		return []Pane{}
	}
	return out
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
	snap := p.panes.Snapshot()
	var out []Pane
	for _, pn := range snap {
		if string(pn.Key.Host) != host {
			continue
		}
		if paneCommandMatchesClaude(pn, host, ps, needle) {
			out = append(out, pn)
		}
	}
	if out == nil {
		return []Pane{}
	}
	return out
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

// PanesBySession returns every pane whose tmux session name equals
// sessionName on the given host. Returns [] (never nil).
func (p *Provider) PanesBySession(host, sessionName string) []Pane {
	if sessionName == "" {
		return []Pane{}
	}
	snap := p.panes.Snapshot()
	var out []Pane
	for _, pn := range snap {
		if string(pn.Key.Host) != host {
			continue
		}
		if pn.WindowKey.Session != sessionName {
			continue
		}
		out = append(out, pn)
	}
	if out == nil {
		return []Pane{}
	}
	return out
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
