// Package resolvers — helpers shared across the gqlgen-managed resolver
// stubs. Lives in its own file because gqlgen v0.17.46+ relocates
// non-resolver declarations out of schema.resolvers.go on every regen.
package resolvers

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	"github.com/drewdrewthis/orchardist/internal/server/loaders"
	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
	gitprovider "github.com/drewdrewthis/orchardist/internal/server/providers/git"
	"github.com/drewdrewthis/orchardist/internal/server/providers/hostservice"
	"github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	"github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

func prKeyFromGraphQL(r *Resolver, obj *graphql1.PullRequest) (gh.PullRequestKey, bool) {
	if r.GH == nil || obj == nil {
		return gh.PullRequestKey{}, false
	}
	owner, name, number, ok := splitGHNodeID(obj.ID, "PullRequest:")
	if !ok {
		return gh.PullRequestKey{}, false
	}
	return gh.PullRequestKey{Owner: owner, Name: name, Number: number}, true
}

// enrichPR fetches the enrichment fields for a PR, routing through the
// PullRequestEnrichment dataloader when one is available in ctx (batching all
// enrichment calls within one GraphQL operation into one HTTP request per
// repo). A loader is present for every operation that reaches the /graphql
// handler — including subscription emissions: loaders.Middleware wraps the
// whole handler (server.go), and gqlgen's websocket transport captures that
// request context once at connection open and derives every operation from it
// (see primeEnrichment), so the connection-scoped *Loaders is in ctx on each
// emission too. The direct EnrichPullRequest fallback is therefore only for
// callers with no loaders middleware in ctx at all — e.g. direct provider
// calls in tests.
func enrichPR(ctx context.Context, r *Resolver, key gh.PullRequestKey) (gh.PullRequest, error) {
	if l := loaders.FromContext(ctx); l != nil && l.PullRequestEnrichment != nil {
		pr, err := l.PullRequestEnrichment.Load(ctx, key)()
		if err != nil {
			return gh.PullRequest{}, err
		}
		return pr, nil
	}
	return r.GH.EnrichPullRequest(ctx, key)
}

// enrichmentFieldNames is the set of PullRequest fields whose values come
// from the GraphQL enrichment fetch rather than the REST list payload.
var enrichmentFieldNames = map[string]struct{}{
	"headRefOid":            {},
	"reviews":               {},
	"mergeable":             {},
	"mergeStateStatus":      {},
	"reviewDecision":        {},
	"statusCheckRollup":     {},
	"labels":                {},
	"reviewThreads":         {},
	"unresolvedThreadCount": {},
}

// primeEnrichment collapses PR enrichment for a whole list into one GitHub
// round-trip via a single direct BatchEnrichPullRequests call with every key.
// Each enrichment field resolver Loads its PR key independently, spread across
// gqlgen's per-item goroutines; routing the warm-up through the DataLoader
// would depend on its wall-clock wait window, so under scheduling jitter the
// keys split across batches and fire more than one GraphQL request (a latent
// N+1 — issue #773). The one deterministic batch here warms the provider's
// per-key cache, so the field resolvers' later Loads read that cache instead
// of the network.
//
// The warm-up is the ONLY mechanism. It does NOT prime the request DataLoader,
// because that cannot be made real here: the loaders are NoCache
// (internal/server/loaders/loaders.go), so Loader.Prime discards, and the
// loaders cannot be given an in-memory cache instead — gqlgen's websocket
// transport captures ONE *Loaders (from loaders.Middleware) at connection open
// and reuses it for every operation for the whole connection lifetime
// (websocket.go: wsConnection.ctx = r.Context(), and each subscribe derives
// from c.ctx). A cached enrichment loader would therefore serve stale
// mergeable/CI/reviews across a long-lived socket.
//
// Consequence: open PRs with mergeable == UNKNOWN are re-fetched by the field
// resolvers. gh.shouldCacheEnrichment refuses to cache that combination —
// UNKNOWN is an uncomputed value, not a verdict (#367) — so the warm-up leaves
// those keys cold in the durable provider cache. For query and mutation
// operations the per-operation memo (gh.WithEnrichMemo, installed by
// resolvers.EnrichMemoMiddleware, #813) captures the warm-up result for the
// life of the operation, so the field resolvers read it instead of the network
// and that second GitHub round-trip collapses. Subscriptions are deliberately
// excluded from the memo (it would outlive the emissions and serve stale
// mergeable/CI/reviews across a long-lived socket), so the extra open+UNKNOWN
// round-trip remains ONLY on subscription emissions (tracked in #822).
//
// No-op when no enrichment field is selected, so a bare `{ number }` query
// still pays nothing.
func primeEnrichment(ctx context.Context, provider *gh.Provider, prs []gh.PullRequest) {
	if provider == nil || len(prs) == 0 || !enrichmentSelected(ctx) {
		return
	}
	keys := make([]gh.PullRequestKey, 0, len(prs))
	for _, p := range prs {
		keys = append(keys, gh.PullRequestKey{Owner: p.RepoOwner, Name: p.RepoName, Number: p.Number})
	}
	// Best-effort warm-up; the field resolvers surface any real error.
	_, _ = provider.BatchEnrichPullRequests(ctx, keys)
}

// enrichmentSelected reports whether the PullRequest selection set for the
// current list field requests any enrichment-sourced field.
func enrichmentSelected(ctx context.Context) bool {
	fc := graphql.GetFieldContext(ctx)
	octx := graphql.GetOperationContext(ctx)
	if fc == nil || octx == nil {
		return false
	}
	for _, f := range graphql.CollectFields(octx, fc.Field.Selections, nil) {
		if _, ok := enrichmentFieldNames[f.Name]; ok {
			return true
		}
	}
	return false
}

func (r *subscriptionResolver) streamLocalEvents(ctx context.Context) (<-chan graphql1.Node, error) {
	if r.LocalEvents == nil {
		out := make(chan graphql1.Node)
		go func() {
			<-ctx.Done()
			close(out)
		}()
		return out, nil
	}
	stream := r.LocalEvents.Subscribe(ctx)
	out := make(chan graphql1.Node, 16)
	go func() {
		defer close(out)
		for ev := range stream {
			id := string(ev.NodeID)
			if id == "" {
				continue
			}
			node, err := projectLocalInvalidation(id)
			if err != nil || node == nil {
				continue
			}
			select {
			case out <- node:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func buildHostServices(ctx context.Context, p *hostservice.Provider, hostID string) []*graphql1.HostService {
	services := p.Services()
	out := make([]*graphql1.HostService, 0, len(services))
	for _, name := range services {
		key := hostservice.MakeID(hostID, name)
		snap, _, err := p.Get(ctx, key)
		if err != nil {
			out = append(out, hostServiceUnknown(hostID, name))
			_ = errors.Is(err, hostservice.ErrServiceManagerMissing) // documented
			continue
		}
		out = append(out, hostServiceFromSnapshot(snap))
	}
	return out
}

func hostServiceUnknown(hostID, name string) *graphql1.HostService {
	return &graphql1.HostService{
		ID:    "HostService:" + hostID + ":" + name,
		Host:  &graphql1.Host{ID: "Host:" + hostID, MachineID: hostID},
		Name:  name,
		State: graphql1.HostServiceStateUnknown,
	}
}

func hostServiceFromSnapshot(s hostservice.Snapshot) *graphql1.HostService {
	hs := &graphql1.HostService{
		ID:    "HostService:" + s.HostID + ":" + s.Name,
		Host:  &graphql1.Host{ID: "Host:" + s.HostID, MachineID: s.HostID},
		Name:  s.Name,
		State: mapState(s.State),
	}
	if s.Since != nil {
		ts := s.Since.UTC().Format(time.RFC3339Nano)
		hs.Since = &ts
	}
	if s.ExitCode != nil {
		v := int64(*s.ExitCode)
		hs.ExitCode = &v
	}
	if s.LogTail != nil {
		tail := *s.LogTail
		hs.LogTail = &tail
	}
	return hs
}

func mapState(s hostservice.State) graphql1.HostServiceState {
	switch s {
	case hostservice.StateActive:
		return graphql1.HostServiceStateActive
	case hostservice.StateInactive:
		return graphql1.HostServiceStateInactive
	case hostservice.StateFailed:
		return graphql1.HostServiceStateFailed
	case hostservice.StateNotInstalled:
		return graphql1.HostServiceStateNotInstalled
	default:
		return graphql1.HostServiceStateUnknown
	}
}

func toGraphQLWorktree(w gitprovider.Worktree) *graphql1.Worktree {
	// Host is the single source of truth for the worktree's host sentinel.
	// "local" is the v1 sentinel for locally-discovered worktrees.
	// Workstream F will replace this with the actual remote host id once
	// federated discovery lands. The worktreeResolver.Host field resolver
	// reads obj.Host (set here) rather than hardcoding "local". (#511)
	return &graphql1.Worktree{
		ID:     string(w.ID),
		Path:   w.Path,
		Branch: w.Branch,
		Head:   w.Head,
		Bare:   w.Bare,
		Host:   "local",
		Ahead:  w.Ahead,
		Behind: w.Behind,
	}
}

func projectProcess(p *ps.Process, hostID string) *graphql1.Process {
	tty := p.TTY
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
	if tty != "" {
		out.Tty = &tty
	}
	return out
}

func applyProcessFilter(ctx context.Context, p *ps.Provider, in []ps.Process, filter *graphql1.ProcessFilter) []ps.Process {
	if filter == nil {
		return in
	}
	out := make([]ps.Process, len(in))
	copy(out, in)

	if pids := filter.PidIn; len(pids) > 0 {
		want := make(map[int]struct{}, len(pids))
		for _, pid := range pids {
			want[int(pid)] = struct{}{}
		}
		next := out[:0]
		for _, proc := range out {
			if _, ok := want[proc.ID.PID]; ok {
				next = append(next, proc)
			}
		}
		out = next
	}

	if cmds := filter.CommandIn; len(cmds) > 0 {
		want := make(map[string]struct{}, len(cmds))
		for _, c := range cmds {
			want[c] = struct{}{}
		}
		next := out[:0]
		for _, proc := range out {
			if _, ok := want[proc.Command]; ok {
				next = append(next, proc)
			}
		}
		out = next
	}

	if filter.CwdPrefix != nil && *filter.CwdPrefix != "" {
		prefix := *filter.CwdPrefix
		pids := make([]int, 0, len(out))
		for _, proc := range out {
			pids = append(pids, proc.ID.PID)
		}
		cwds, err := p.LoadCwds(ctx, pids)
		if err != nil {
			return nil
		}
		next := out[:0]
		for _, proc := range out {
			if cwd, ok := cwds[proc.ID.PID]; ok && strings.HasPrefix(cwd, prefix) {
				next = append(next, proc)
			}
		}
		out = next
	}

	return out
}

func splitProcessNodeID(s string) (string, string) {
	idx := strings.LastIndexByte(s, ':')
	if idx <= 0 {
		return "local", s
	}
	return s[:idx], s[idx+1:]
}

func projectServer(host tmux.HostID, info tmux.ServerInfo) *graphql1.TmuxServer {
	return &graphql1.TmuxServer{
		ID:         "TmuxServer:" + string(host) + ":" + info.SocketPath,
		SocketPath: info.SocketPath,
	}
}

func projectSession(s tmux.Session) *graphql1.TmuxSession {
	return &graphql1.TmuxSession{
		ID:   "TmuxSession:" + string(s.Key.Host) + ":" + s.Key.Name,
		Name: s.Key.Name,
	}
}

func projectWindow(w tmux.Window) *graphql1.TmuxWindow {
	return &graphql1.TmuxWindow{
		ID:    "TmuxWindow:" + string(w.Key.Host) + ":" + w.Key.Session + ":" + strconv.Itoa(w.Key.Index),
		Index: int64(w.Key.Index),
	}
}

func projectPane(p tmux.Pane) *graphql1.TmuxPane {
	return &graphql1.TmuxPane{
		ID:     "TmuxPane:" + string(p.Key.Host) + ":" + p.Key.PaneID,
		PaneID: p.Key.PaneID,
	}
}

// projectPaneRich extends projectPane with CurrentCommand and CurrentPid,
// and does not walk the window/session tree (the loaders path returns richer
// objects; this variant is for the no-loader-context fallback).
func projectPaneRich(p tmux.Pane) *graphql1.TmuxPane {
	out := &graphql1.TmuxPane{
		ID:             "TmuxPane:" + string(p.Key.Host) + ":" + p.Key.PaneID,
		PaneID:         p.Key.PaneID,
		CurrentCommand: p.CurrentCommand,
	}
	if p.CurrentPid > 0 {
		pid := int64(p.CurrentPid)
		out.CurrentPid = &pid
	}
	return out
}

func projectClient(c tmux.Client) *graphql1.TmuxClient {
	return &graphql1.TmuxClient{
		ID: "TmuxClient:" + string(c.Key.Host) + ":" + c.Key.ClientName,
	}
}

func stripPrefix(s, prefix string) string {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):]
	}
	return s
}

func sessionMatchesFilter(s tmux.Session, f *graphql1.TmuxSessionFilter) bool {
	if f == nil {
		return true
	}
	if names := f.NameIn; len(names) > 0 && !contains(names, s.Key.Name) {
		return false
	}
	if f.AttachedOnly != nil && *f.AttachedOnly && !s.Attached {
		return false
	}
	if f.ActiveAttachedOnly != nil && *f.ActiveAttachedOnly && !s.Attached {
		return false
	}
	return true
}

func paneMatchesFilter(p tmux.Pane, f *graphql1.TmuxPaneFilter) bool {
	if f == nil {
		return true
	}
	if ids := f.PaneIDIn; len(ids) > 0 && !contains(ids, p.Key.PaneID) {
		return false
	}
	if cmds := f.CurrentCommandIn; len(cmds) > 0 && !contains(cmds, p.CurrentCommand) {
		return false
	}
	if sessions := f.SessionIn; len(sessions) > 0 && !contains(sessions, p.WindowKey.Session) {
		return false
	}
	if f.TitleContains != nil && *f.TitleContains != "" && !strings.Contains(p.Title, *f.TitleContains) {
		return false
	}
	if f.Dead != nil && *f.Dead != p.Dead {
		return false
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// projectPanesWithFilter projects a []tmux.Pane to []*graphql1.TmuxPane and
// applies the cheap scalar filters (paneIdIn, currentCommandIn, sessionIn,
// titleContains, dead). Used by TmuxPanes after the cwd/command axis returns
// a pre-filtered result set.
func projectPanesWithFilter(raw []tmux.Pane, filter *graphql1.TmuxPaneFilter) []*graphql1.TmuxPane {
	out := make([]*graphql1.TmuxPane, 0, len(raw))
	for _, p := range raw {
		if !paneMatchesFilter(p, filter) {
			continue
		}
		out = append(out, projectPane(p))
	}
	if out == nil {
		return []*graphql1.TmuxPane{}
	}
	return out
}

// paneGraphQLMatchesFilter applies cheap scalar predicates to an already-projected
// *graphql1.TmuxPane — used after cwd/command axis loading returns gql-typed results.
// The cwd/command fields are intentionally NOT re-checked here (they were already
// the dispatch key).
func paneGraphQLMatchesFilter(p *graphql1.TmuxPane, f *graphql1.TmuxPaneFilter) bool {
	if f == nil || p == nil {
		return true
	}
	if ids := f.PaneIDIn; len(ids) > 0 && !contains(ids, p.PaneID) {
		return false
	}
	if cmds := f.CurrentCommandIn; len(cmds) > 0 && !contains(cmds, p.CurrentCommand) {
		return false
	}
	return true
}

// resolverPanePsGetter adapts *ps.Provider to tmux.PanePsGetter for use in
// TmuxPanes when no loader context is available (e.g. subscription emissions).
type resolverPanePsGetter struct {
	ps *ps.Provider
}

// newResolverPanePsGetter returns nil when ps is nil — PanesByCwd /
// PanesByCommand handle a nil getter by falling back to CurrentCommand.
func newResolverPanePsGetter(p *ps.Provider) tmux.PanePsGetter {
	if p == nil {
		return nil
	}
	return &resolverPanePsGetter{ps: p}
}

func (g *resolverPanePsGetter) CwdForPid(_ string, pid int) string {
	cwd, err := g.ps.LoadCwd(context.Background(), pid)
	if err != nil {
		return ""
	}
	return cwd
}

func (g *resolverPanePsGetter) CommandForPid(host string, pid int) string {
	proc, _, err := g.ps.Get(context.Background(), ps.ProcessID{Host: host, PID: pid})
	if err != nil {
		return ""
	}
	return proc.Command
}

func (r *tmuxClientResolver) lookupClient(id string) (tmux.Client, bool) {
	if r.Tmux == nil {
		return tmux.Client{}, false
	}
	host := r.Tmux.Host()
	name := stripPrefix(id, "TmuxClient:"+string(host)+":")
	return r.Tmux.ClientByName(string(host), name)
}

func (r *tmuxPaneResolver) lookupPane(id string) (tmux.Pane, bool) {
	if r.Tmux == nil {
		return tmux.Pane{}, false
	}
	host := r.Tmux.Host()
	paneID := stripPrefix(id, "TmuxPane:"+string(host)+":")
	return r.Tmux.PaneByID(string(host), paneID)
}

func (r *tmuxSessionResolver) lookupSession(id string) (tmux.Session, bool) {
	if r.Tmux == nil {
		return tmux.Session{}, false
	}
	host := r.Tmux.Host()
	name := stripPrefix(id, "TmuxSession:"+string(host)+":")
	return r.Tmux.SessionByName(string(host), name)
}

func (r *tmuxWindowResolver) lookupWindow(id string) (tmux.Window, bool) {
	if r.Tmux == nil {
		return tmux.Window{}, false
	}
	host := r.Tmux.Host()
	rest := stripPrefix(id, "TmuxWindow:"+string(host)+":")
	idx := strings.LastIndex(rest, ":")
	if idx == -1 {
		return tmux.Window{}, false
	}
	session := rest[:idx]
	indexN, err := strconv.Atoi(rest[idx+1:])
	if err != nil {
		return tmux.Window{}, false
	}
	return r.Tmux.WindowByKey(string(host), session, indexN)
}
