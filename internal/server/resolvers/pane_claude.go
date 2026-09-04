// pane_claude.go — pane-first ClaudeInstances implementation (ADR-022 Phase 4).
//
// Query.claudeInstances is a view over Pane nodes filtered by command "claude".
// This file contains the projection logic that converts []*graphql.TmuxPane
// into []*graphql.ClaudeInstance without going through the heartbeat subsystem.
package resolvers

import (
	"context"
	"fmt"
	"sort"
	"time"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	"github.com/drewdrewthis/orchardist/internal/server/loaders"
	claudeinstance "github.com/drewdrewthis/orchardist/internal/server/providers/claudeinstance"
	claudesessions "github.com/drewdrewthis/orchardist/internal/server/providers/claudesessions"
	psprovider "github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	"github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// paneSessionLoader adapts the request-scoped SessionByPid dataloader (or the
// direct provider when no loader is in context) to the paneSessionRegistry
// interface resolveSessionUUID needs, keeping that helper pure. Host is captured
// for the loader key; resolveSessionUUID only calls SessionByPid for local
// panes, so the batch fn's local-only registry is never asked about a remote host.
type paneSessionLoader struct {
	ctx    context.Context
	host   string
	loader *loaders.Loaders         // nil when no middleware is wired
	direct *claudesessions.Provider // fallback path
}

// SessionByPid resolves one registry entry by pid, batched through the loader
// when present and via the direct provider otherwise. A missing entry (or any
// load error) is reported as not-found so resolveSessionUUID falls through to
// its cwd join rather than surfacing an error.
func (a paneSessionLoader) SessionByPid(pid int) (claudesessions.Session, bool) {
	if a.loader != nil {
		s, err := a.loader.SessionByPid.Load(a.ctx, loaders.SessionPidKey{Host: a.host, Pid: pid})()
		if err != nil || s == nil {
			return claudesessions.Session{}, false
		}
		return *s, true
	}
	if a.direct == nil {
		return claudesessions.Session{}, false
	}
	return a.direct.SessionByPid(pid)
}

// projectPanesToClaudeInstances converts a slice of tmux panes (all presumed
// to be running claude) into ClaudeInstance graph nodes. For each pane it:
//
//  1. Resolves the Process via the ps provider / loader.
//  2. Finds the matching Conversation by cwd (via claudeprojects).
//  3. Derives state from the jsonl snapshot.
//  4. Attaches the active ClaudeAccount.
//
// Returns [] (never nil).
func (r *queryResolver) projectPanesToClaudeInstances(ctx context.Context, panes []*graphql1.TmuxPane) []*graphql1.ClaudeInstance {
	if len(panes) == 0 {
		return []*graphql1.ClaudeInstance{}
	}

	host := tmux.LocalHostID
	if r.Tmux != nil {
		host = string(r.Tmux.Host())
	}

	// Resolve the active account once — same account for every instance.
	var account *graphql1.ClaudeAccount
	if r.ClaudeAccount != nil {
		accts, err := r.ClaudeAccount.List(ctx)
		if err == nil && len(accts) > 0 {
			account = r.ClaudeAccount.ToGraphQL(accts[0])
		}
	}

	// Build a production SnapshotReader for jsonl state derivation.
	snapshotReader := claudeinstance.NewFsSnapshotReader("")

	// Build the ambiguity-aware cwd→sessionUUID index ONCE for the whole
	// request — previously every pane re-fetched the conversation list and
	// re-scanned linearly (N panes × M conversations); now it's one fetch +
	// one O(M) pass. The index counts conversations per cwd (#743): a cwd
	// shared by two or more REPLs is ambiguous, so the cwd fallback declines
	// to guess and the registry-by-pid path (buildClaudeInstanceFromPane)
	// carries the real disambiguation.
	var cwdIndex map[string]cwdMatch
	if r.ClaudeProjects != nil {
		if convs, err := r.ClaudeProjects.List(ctx); err == nil {
			cwdIndex = buildCwdIndex(convs)
		}
	}

	// #711: build the process tree ONCE so each pane's shell-wrapper pane_pid
	// can be resolved to the real foreground claude pid via a descendant walk.
	// Mirrors the cwdToSession one-shot index above — O(procs) instead of a
	// full ps snapshot per pane.
	var procTree *psprovider.ProcTree
	if r.PS != nil {
		procTree = psprovider.NewProcTree(r.PS.List())
	}

	out := make([]*graphql1.ClaudeInstance, 0, len(panes))
	for _, pane := range panes {
		if pane == nil {
			continue
		}
		inst := r.buildClaudeInstanceFromPane(ctx, pane, host, account, snapshotReader, cwdIndex, procTree)
		out = append(out, inst)
	}

	// Sort by id for deterministic output — mirrors Provider.List sort.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// buildClaudeInstanceFromPane constructs one ClaudeInstance from a TmuxPane.
// cwdIndex is a pre-built ambiguity-aware index from projectPanesToClaudeInstances;
// it turns the conversation lookup into an O(1) map hit instead of a per-pane
// linear scan over r.ClaudeProjects.List(ctx). The single-pane caller passes
// nil and we build a one-off index here.
func (r *queryResolver) buildClaudeInstanceFromPane(
	ctx context.Context,
	pane *graphql1.TmuxPane,
	host string,
	account *graphql1.ClaudeAccount,
	snapshotReader claudeinstance.SnapshotReader,
	cwdIndex map[string]cwdMatch,
	procTree *psprovider.ProcTree,
) *graphql1.ClaudeInstance {
	var pid int
	if pane.CurrentPid != nil {
		pid = int(*pane.CurrentPid)
	}

	// #711: pane.CurrentPid is tmux's pane_pid — the pane's ROOT process, which
	// is the bash wrapper for every `bash -> claude` launch. Resolve the real
	// foreground claude pid by walking descendants so id / process / cwd key on
	// claude (schema.graphql's identity contract), not the shell. The hot-path
	// caller passes a shared procTree; the single-pane caller passes nil and we
	// build a one-off tree here.
	if pid > 0 {
		tree := procTree
		if tree == nil && r.PS != nil {
			tree = psprovider.NewProcTree(r.PS.List())
		}
		if tree != nil {
			pid = tree.ResolveClaudePid(pid)
		}
	}

	id := buildClaudeIDFromPane(host, pid, pane)
	inst := &graphql1.ClaudeInstance{
		ID:      id,
		Pane:    pane,
		Account: account,
	}

	// Resolve Process via loader when available, otherwise direct ps call.
	if pid > 0 {
		if l := loaders.FromContext(ctx); l != nil {
			if proc, err := l.Process.Load(ctx, loaders.ProcessKey{HostID: host, Pid: pid})(); err == nil && proc != nil {
				inst.Process = proc
			}
		} else if r.PS != nil {
			if proc, _, err := r.PS.Get(ctx, psprovider.ProcessID{Host: host, PID: pid}); err == nil {
				inst.Process = projectProcessFromPsProcess(&proc, host)
			}
		}
	}

	// Resolve cwd from ps — required to locate the conversation.
	var cwd string
	if r.PS != nil && pid > 0 {
		if resolved, err := r.PS.LoadCwd(ctx, pid); err == nil {
			cwd = resolved
		}
	}

	// Resolve sessionUuid by the pane's own live pid via the claudesessions
	// registry FIRST, then fall back to the cwd join (#743). Keying on the
	// pane's resolved live pid is what distinguishes two REPLs sharing one
	// worktree cwd; the cwd fallback returns "" when the cwd is ambiguous
	// rather than a shared wrong guess.
	//   - cwdIndex != nil: hot-path caller pre-built the index once.
	//   - cwdIndex == nil: single-pane caller (tmuxPane.claudeInstance) —
	//     build a one-off index so the ambiguity count is available here too.
	var sessionUUID string
	if cwd != "" {
		idx := cwdIndex
		if idx == nil && r.ClaudeProjects != nil {
			if convs, err := r.ClaudeProjects.List(ctx); err == nil {
				idx = buildCwdIndex(convs)
			}
		}
		var reg paneSessionRegistry
		if l := loaders.FromContext(ctx); l != nil {
			reg = paneSessionLoader{ctx: ctx, host: host, loader: l}
		} else if r.ClaudeSessions != nil {
			reg = paneSessionLoader{ctx: ctx, host: host, direct: r.ClaudeSessions}
		}
		sessionUUID = resolveSessionUUID(host, pid, cwd, reg, idx)
	}

	// Derive state from jsonl snapshot.
	state, snap := claudeinstance.DeriveInstanceState(ctx, claudeinstance.DeriveState{
		Cwd:         cwd,
		SessionUUID: sessionUUID,
		Pid:         pid,
		Snapshot:    snapshotReader,
	})
	inst.State = state
	inst.InflightToolCount = int64(snap.InflightToolCount)
	if snap.Model != "" {
		v := snap.Model
		inst.Model = &v
	}
	if !snap.LastActivityAt.IsZero() {
		quantized := snap.LastActivityAt.UTC().Truncate(time.Second)
		v := quantized.Format(time.RFC3339)
		inst.LastActivityAt = &v
	}
	if sessionUUID != "" {
		v := sessionUUID
		inst.SessionUUID = &v
	}

	// Fallback lastActivityAt from the pane's session (mirrors Composer).
	if inst.LastActivityAt == nil &&
		pane.Window != nil && pane.Window.Session != nil &&
		pane.Window.Session.LastActivityAt != nil {
		v := *pane.Window.Session.LastActivityAt
		inst.LastActivityAt = &v
	}

	return inst
}

// buildClaudeIDFromPane constructs the stable ClaudeInstance node id from a
// pane. Mirrors claudeinstance.buildID: pid-keyed when pid > 0, pane-keyed
// otherwise.
func buildClaudeIDFromPane(host string, pid int, pane *graphql1.TmuxPane) string {
	if pid > 0 {
		return fmt.Sprintf("ClaudeInstance:%s:%d", host, pid)
	}
	return fmt.Sprintf("ClaudeInstance:%s:pane-%s", host, pane.PaneID)
}

// projectProcessFromPsProcess projects a psprovider.Process onto
// *graphql1.Process. Mirrors loader_bridge.go:projectTmuxPane's pattern
// and the loaders.projectProcess function.
func projectProcessFromPsProcess(p *psprovider.Process, hostID string) *graphql1.Process {
	startedAt := p.StartedRaw
	if !p.StartedAt.IsZero() {
		startedAt = p.StartedAt.Format(time.RFC3339)
	}
	out := &graphql1.Process{
		ID:         p.ID.String(),
		Host:       &graphql1.Host{ID: hostID},
		Pid:        int64(p.ID.PID),
		Ppid:       int64(p.PPID),
		Command:    p.Command,
		StartedAt:  startedAt,
		CPUPercent: p.CPUPercent,
		MemBytes:   p.MemBytes,
	}
	if p.TTY != "" {
		tty := p.TTY
		out.Tty = &tty
	}
	return out
}
