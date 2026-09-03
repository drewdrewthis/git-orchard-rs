// worktree_tmux.go holds the helpers for Worktree.tmuxPanes and
// Worktree.tmuxSession (#511). The two resolver methods themselves live in
// schema.resolvers.go (gqlgen owns that file); only the helpers stay here.
//
// The fields derive a server-side join of pane.process.cwd against the
// worktree path — exact-equality or path-prefix (path + "/") — so no
// client-side heuristics are required to attach a terminal to a worktree.

package resolvers

import (
	"context"
	"strings"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	tmux "github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// matchPanesForWorktree narrows `candidates` to every pane that:
//  1. Lives on the same host as obj (attribution via pane.Key.Host).
//  2. Has a CurrentPid that the ps provider can resolve to a cwd.
//  3. Has a cwd that equals obj.Path exactly OR starts with obj.Path+"/".
//
// Callers pass the host's panes (Provider.PanesByHost) rather than a full
// graph snapshot: cwd is not in the tmux cache, so the ps lookups here are
// the reason every pane has to be considered — and they must not run while
// the provider's store lock is held (#612).
//
// Returns raw tmux.Pane values so callers can read provider-level fields
// (e.g. WindowKey.Session for session lookup) without a second lookup.
// Panes whose cwd cannot be resolved are silently skipped. The returned
// slice is unsorted; callers sort by paneId. Never returns nil.
func matchPanesForWorktree(ctx context.Context, r *worktreeResolver, candidates []tmux.Pane, obj *graphql1.Worktree) []tmux.Pane {
	var matching []tmux.Pane

	for _, pane := range candidates {
		// Federation attribution: use pane.Key.Host (which tracks through
		// pane.window.session.host). Never synthesise from the local daemon.
		if string(pane.Key.Host) != obj.Host {
			continue
		}

		// Skip panes with no foreground pid.
		if pane.CurrentPid == 0 {
			continue
		}

		// Resolve cwd via the ps provider — same code path as
		// processResolver.Cwd (#463).
		if r.PS == nil {
			continue
		}
		cwd, err := r.PS.LoadCwd(ctx, pane.CurrentPid)
		if err != nil {
			// Silently skip panes whose cwd cannot be resolved.
			continue
		}
		if cwd == "" {
			continue
		}

		if !cwdMatchesWorktree(obj.Path, cwd) {
			continue
		}

		matching = append(matching, pane)
	}

	if matching == nil {
		return []tmux.Pane{}
	}
	return matching
}

// cwdMatchesWorktree returns true when cwd equals path exactly or is immediately under path (starts with path+"/"). The trailing "/" guard prevents false positives: given path="/repo/feat-x", cwd "/repo/feat-xtra" must NOT match even though it shares a prefix.
func cwdMatchesWorktree(path, cwd string) bool {
	return cwd == path || strings.HasPrefix(cwd, path+"/")
}
