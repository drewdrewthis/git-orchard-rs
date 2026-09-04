// Package loaders builds a request-scoped bag of DataLoader instances
// per ADR-011 §6 (resolver composition rules). One loader per
// (provider, request) — fresh on every GraphQL operation, never shared
// across requests, never owned by the global Resolver.
//
// The middleware in this package attaches a *Loaders pointer to the
// request context; resolvers retrieve it via FromContext and call the
// per-edge helpers that keep the dataloader/v7 generics out of the
// resolver code.
//
// Why per-request: DataLoader's whole job is "coalesce duplicate keys
// inside one logical batch." Sharing a loader across requests would
// either mix unrelated batches and serve stale reads, or require
// per-cache-tag invalidation we don't want to maintain.
package loaders

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/graph-gophers/dataloader/v7"

	graphql1 "github.com/drewdrewthis/orchardist/internal/server/graphql"
	claudesessions "github.com/drewdrewthis/orchardist/internal/server/providers/claudesessions"
	configprovider "github.com/drewdrewthis/orchardist/internal/server/providers/config"
	ghprovider "github.com/drewdrewthis/orchardist/internal/server/providers/gh"
	gitprovider "github.com/drewdrewthis/orchardist/internal/server/providers/git"
	hostprovider "github.com/drewdrewthis/orchardist/internal/server/providers/host"
	psprovider "github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	tmuxprovider "github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// GHPullRequestLister is the narrow gh surface the PullRequestsForRepo
// loader needs. *gh.Provider satisfies this automatically. Defined as
// an interface so tests can inject a stub without standing up HTTP.
type GHPullRequestLister interface {
	ListPullRequests(ctx context.Context, owner, name string, state ghprovider.PullRequestState) ([]ghprovider.PullRequest, error)
}

// GHPREnricher is the narrow gh surface the PullRequestEnrichment loader
// needs. *gh.Provider satisfies this automatically. Tests can inject a
// stub without standing up HTTP.
type GHPREnricher interface {
	BatchEnrichPullRequests(ctx context.Context, keys []ghprovider.PullRequestKey) (map[ghprovider.PullRequestKey]ghprovider.PullRequest, error)
}

// ProvidersBundle is the read-side surface the loaders need from the
// resolver root. A struct (not an interface) because the loaders only
// ever read pointers off it and tests can swap individual fields
// without writing N stub methods.
type ProvidersBundle struct {
	Host  *hostprovider.Provider
	Git   *gitprovider.Provider
	Ps    *psprovider.Provider
	Tmux  *tmuxprovider.Provider
	Repos configprovider.Lister
	// GH is the narrow gh surface the PullRequestsForRepo loader needs.
	// *gh.Provider satisfies GHPullRequestLister automatically; tests
	// can inject a stub without standing up HTTP.
	GH GHPullRequestLister
	// GHEnricher is the narrow gh surface the PullRequestEnrichment loader
	// needs. *gh.Provider satisfies GHPREnricher automatically.
	GHEnricher GHPREnricher
	// ClaudeSessions is the ~/.claude/sessions registry surface the
	// SessionByPid loader reads. *claudesessions.Provider satisfies it.
	ClaudeSessions ClaudeSessionByPid
}

// loaderKey is the private context key for the per-request loaders.
type loaderKey struct{}

// Loaders is the per-request bundle. One Loaders value lives for the
// lifetime of one GraphQL operation (query, mutation, or subscription
// emission). Every dataloader instance holds its own batched promise
// state.
type Loaders struct {
	Host                  *dataloader.Loader[string, *graphql1.Host]
	WorktreeForCwd        *dataloader.Loader[string, *graphql1.Worktree]
	Process               *dataloader.Loader[ProcessKey, *graphql1.Process]
	PullRequestsForRepo   *dataloader.Loader[RepoKey, []*graphql1.PullRequest]
	PullRequestEnrichment *dataloader.Loader[ghprovider.PullRequestKey, ghprovider.PullRequest]

	// Pane loaders (ADR-022): one node, three lookup axes.
	PaneByID       *dataloader.Loader[PaneKey, *graphql1.TmuxPane]
	PanesByCwd     *dataloader.Loader[CwdKey, []*graphql1.TmuxPane]
	PanesByCommand *dataloader.Loader[CommandKey, []*graphql1.TmuxPane]

	// SessionByPid (ADR-022): ClaudeInstance.sessionUuid by the pane's own pid.
	SessionByPid *dataloader.Loader[SessionPidKey, *claudesessions.Session]

	// metrics — provider call counts, used by the n+1 detector test.
	hostBatches           *batchCounter
	worktreeBatches       *batchCounter
	processBatches        *batchCounter
	prBatches             *batchCounter
	prEnrichmentBatches   *batchCounter
	paneByIDBatches       *batchCounter
	panesByCwdBatches     *batchCounter
	panesByCommandBatches *batchCounter
	sessionByPidBatches   *batchCounter
}

// RepoKey is the composite key for the PullRequestsForRepo loader.
// It identifies a GitHub repository by owner and name.
type RepoKey struct {
	Owner string
	Name  string
}

// String renders a RepoKey as "owner/name" — the canonical repo slug
// used in GraphQL IDs and human-readable log output.
func (k RepoKey) String() string {
	return fmt.Sprintf("%s/%s", k.Owner, k.Name)
}

// ProcessKey is the composite key for the [TmuxPane].process edge.
type ProcessKey struct {
	HostID string
	Pid    int
}

// String renders a ProcessKey as the canonical Process node id —
// useful for logging and for keeping the loader cache key
// human-readable.
func (k ProcessKey) String() string {
	return psprovider.ProcessID{Host: k.HostID, PID: k.Pid}.String()
}

// PaneKey is the composite key for the PaneByID loader.
type PaneKey struct {
	HostID string
	PaneID string
}

// String renders a PaneKey as the canonical TmuxPane node id.
func (k PaneKey) String() string {
	return fmt.Sprintf("TmuxPane:%s:%s", k.HostID, k.PaneID)
}

// CwdKey is the composite key for the PanesByCwd loader.
type CwdKey struct {
	HostID string
	Cwd    string
}

// CommandKey is the composite key for the PanesByCommand loader.
type CommandKey struct {
	HostID  string
	Command string // case-insensitive basename substring
}

// NewLoaders builds a fresh bundle of loaders bound to the given
// providers. The 1ms wait window matches the gqlgen handler tick —
// short enough that a query asking for many edges still feels
// synchronous, long enough that resolver fan-outs collapse into one
// batch.
func NewLoaders(providers *ProvidersBundle) *Loaders {
	hostBatches := &batchCounter{}
	worktreeBatches := &batchCounter{}
	processBatches := &batchCounter{}
	prBatches := &batchCounter{}
	prEnrichmentBatches := &batchCounter{}
	paneByIDBatches := &batchCounter{}
	panesByCwdBatches := &batchCounter{}
	panesByCommandBatches := &batchCounter{}
	sessionByPidBatches := &batchCounter{}

	// Build the narrow PanePsGetter adapter once — shared across all pane
	// loaders inside this request. Nil when ps provider is not wired.
	panePsGetter := newPanePsGetterAdapter(providers.Ps)

	hostBatch := func(_ context.Context, ids []string) []*dataloader.Result[*graphql1.Host] {
		hostBatches.inc()
		return loadHosts(providers, ids)
	}
	worktreeBatch := func(_ context.Context, cwds []string) []*dataloader.Result[*graphql1.Worktree] {
		worktreeBatches.inc()
		return loadWorktreesForCwds(providers, cwds)
	}
	processBatch := func(_ context.Context, keys []ProcessKey) []*dataloader.Result[*graphql1.Process] {
		processBatches.inc()
		return loadProcesses(providers, keys)
	}
	prBatch := func(ctx context.Context, keys []RepoKey) []*dataloader.Result[[]*graphql1.PullRequest] {
		prBatches.inc()
		return loadPullRequestsForRepo(ctx, providers, keys)
	}
	prEnrichmentBatch := func(ctx context.Context, keys []ghprovider.PullRequestKey) []*dataloader.Result[ghprovider.PullRequest] {
		prEnrichmentBatches.inc()
		return loadPullRequestEnrichments(ctx, providers, keys)
	}
	paneByIDBatch := func(_ context.Context, keys []PaneKey) []*dataloader.Result[*graphql1.TmuxPane] {
		paneByIDBatches.inc()
		return loadPanesByID(providers, keys)
	}
	panesByCwdBatch := func(_ context.Context, keys []CwdKey) []*dataloader.Result[[]*graphql1.TmuxPane] {
		panesByCwdBatches.inc()
		return loadPanesByCwd(providers, keys, panePsGetter)
	}
	panesByCommandBatch := func(_ context.Context, keys []CommandKey) []*dataloader.Result[[]*graphql1.TmuxPane] {
		panesByCommandBatches.inc()
		return loadPanesByCommand(providers, keys, panePsGetter)
	}
	sessionByPidBatch := func(_ context.Context, keys []SessionPidKey) []*dataloader.Result[*claudesessions.Session] {
		sessionByPidBatches.inc()
		return loadSessionsByPid(providers, keys)
	}

	hostOpts := []dataloader.Option[string, *graphql1.Host]{
		dataloader.WithWait[string, *graphql1.Host](1 * time.Millisecond),
		dataloader.WithCache[string, *graphql1.Host](&dataloader.NoCache[string, *graphql1.Host]{}),
	}
	worktreeOpts := []dataloader.Option[string, *graphql1.Worktree]{
		dataloader.WithWait[string, *graphql1.Worktree](1 * time.Millisecond),
		dataloader.WithCache[string, *graphql1.Worktree](&dataloader.NoCache[string, *graphql1.Worktree]{}),
	}
	processOpts := []dataloader.Option[ProcessKey, *graphql1.Process]{
		dataloader.WithWait[ProcessKey, *graphql1.Process](1 * time.Millisecond),
		dataloader.WithCache[ProcessKey, *graphql1.Process](&dataloader.NoCache[ProcessKey, *graphql1.Process]{}),
	}
	prOpts := []dataloader.Option[RepoKey, []*graphql1.PullRequest]{
		dataloader.WithWait[RepoKey, []*graphql1.PullRequest](1 * time.Millisecond),
		dataloader.WithCache[RepoKey, []*graphql1.PullRequest](&dataloader.NoCache[RepoKey, []*graphql1.PullRequest]{}),
	}
	prEnrichmentOpts := []dataloader.Option[ghprovider.PullRequestKey, ghprovider.PullRequest]{
		dataloader.WithWait[ghprovider.PullRequestKey, ghprovider.PullRequest](1 * time.Millisecond),
		dataloader.WithCache[ghprovider.PullRequestKey, ghprovider.PullRequest](&dataloader.NoCache[ghprovider.PullRequestKey, ghprovider.PullRequest]{}),
	}
	paneByIDOpts := []dataloader.Option[PaneKey, *graphql1.TmuxPane]{
		dataloader.WithWait[PaneKey, *graphql1.TmuxPane](1 * time.Millisecond),
		dataloader.WithCache[PaneKey, *graphql1.TmuxPane](&dataloader.NoCache[PaneKey, *graphql1.TmuxPane]{}),
	}
	panesByCwdOpts := []dataloader.Option[CwdKey, []*graphql1.TmuxPane]{
		dataloader.WithWait[CwdKey, []*graphql1.TmuxPane](1 * time.Millisecond),
		dataloader.WithCache[CwdKey, []*graphql1.TmuxPane](&dataloader.NoCache[CwdKey, []*graphql1.TmuxPane]{}),
	}
	panesByCommandOpts := []dataloader.Option[CommandKey, []*graphql1.TmuxPane]{
		dataloader.WithWait[CommandKey, []*graphql1.TmuxPane](1 * time.Millisecond),
		dataloader.WithCache[CommandKey, []*graphql1.TmuxPane](&dataloader.NoCache[CommandKey, []*graphql1.TmuxPane]{}),
	}
	sessionByPidOpts := []dataloader.Option[SessionPidKey, *claudesessions.Session]{
		dataloader.WithWait[SessionPidKey, *claudesessions.Session](1 * time.Millisecond),
		dataloader.WithCache[SessionPidKey, *claudesessions.Session](&dataloader.NoCache[SessionPidKey, *claudesessions.Session]{}),
	}

	return &Loaders{
		Host:                  dataloader.NewBatchedLoader(hostBatch, hostOpts...),
		WorktreeForCwd:        dataloader.NewBatchedLoader(worktreeBatch, worktreeOpts...),
		Process:               dataloader.NewBatchedLoader(processBatch, processOpts...),
		PullRequestsForRepo:   dataloader.NewBatchedLoader(prBatch, prOpts...),
		PullRequestEnrichment: dataloader.NewBatchedLoader(prEnrichmentBatch, prEnrichmentOpts...),
		PaneByID:              dataloader.NewBatchedLoader(paneByIDBatch, paneByIDOpts...),
		PanesByCwd:            dataloader.NewBatchedLoader(panesByCwdBatch, panesByCwdOpts...),
		PanesByCommand:        dataloader.NewBatchedLoader(panesByCommandBatch, panesByCommandOpts...),
		SessionByPid:          dataloader.NewBatchedLoader(sessionByPidBatch, sessionByPidOpts...),
		hostBatches:           hostBatches,
		worktreeBatches:       worktreeBatches,
		processBatches:        processBatches,
		prBatches:             prBatches,
		prEnrichmentBatches:   prEnrichmentBatches,
		paneByIDBatches:       paneByIDBatches,
		panesByCwdBatches:     panesByCwdBatches,
		panesByCommandBatches: panesByCommandBatches,
		sessionByPidBatches:   sessionByPidBatches,
	}
}

// HostBatchCount returns the number of host-loader batch invocations
// since this Loaders was constructed.
func (l *Loaders) HostBatchCount() int { return l.hostBatches.value() }

// WorktreeBatchCount returns the number of worktree-loader batch
// invocations.
func (l *Loaders) WorktreeBatchCount() int { return l.worktreeBatches.value() }

// ProcessBatchCount returns the number of process-loader batch
// invocations.
func (l *Loaders) ProcessBatchCount() int { return l.processBatches.value() }

// PullRequestsForRepoBatchCount returns the number of PR-loader batch
// invocations since this Loaders was constructed. Used by n+1 tests.
func (l *Loaders) PullRequestsForRepoBatchCount() int { return l.prBatches.value() }

// PullRequestEnrichmentBatchCount returns the number of PR enrichment
// loader batch invocations since this Loaders was constructed.
func (l *Loaders) PullRequestEnrichmentBatchCount() int { return l.prEnrichmentBatches.value() }

// PaneByIDBatchCount returns the number of PaneByID loader batch invocations.
func (l *Loaders) PaneByIDBatchCount() int { return l.paneByIDBatches.value() }

// PanesByCwdBatchCount returns the number of PanesByCwd loader batch invocations.
func (l *Loaders) PanesByCwdBatchCount() int { return l.panesByCwdBatches.value() }

// PanesByCommandBatchCount returns the number of PanesByCommand loader batch invocations.
func (l *Loaders) PanesByCommandBatchCount() int { return l.panesByCommandBatches.value() }

// Middleware wraps an http.Handler to attach a fresh *Loaders to the
// request context. Mount it once around the GraphQL handler.
func Middleware(providers *ProvidersBundle, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithLoaders(r.Context(), NewLoaders(providers))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WithLoaders attaches the Loaders to ctx. Useful for subscription
// emissions where the handler-level middleware doesn't apply (gqlgen's
// websocket transport spawns its own goroutine per emission).
func WithLoaders(ctx context.Context, l *Loaders) context.Context {
	return context.WithValue(ctx, loaderKey{}, l)
}

// FromContext pulls the Loaders out of ctx. Returns nil when no
// middleware has been wired — resolvers should fall back to the
// non-batched provider call in that case.
func FromContext(ctx context.Context) *Loaders {
	l, _ := ctx.Value(loaderKey{}).(*Loaders)
	return l
}

// loadHosts batches host id -> Host. v1 only knows the local host so
// the inner loop is trivial; the batch shape is what makes the test
// assertion possible.
func loadHosts(providers *ProvidersBundle, ids []string) []*dataloader.Result[*graphql1.Host] {
	out := make([]*dataloader.Result[*graphql1.Host], len(ids))
	hp := providers.Host
	if hp == nil {
		// No host provider — return a stub Host{ID:...} so the
		// resolver still gets *something* to project. Tests that wire
		// only the ps provider rely on this.
		for i, id := range ids {
			out[i] = &dataloader.Result[*graphql1.Host]{Data: &graphql1.Host{ID: id}}
		}
		return out
	}
	hosts, _, err := hp.GetMany(context.Background(), idsToHostKeys(ids))
	if err != nil {
		for i := range out {
			out[i] = &dataloader.Result[*graphql1.Host]{Error: err}
		}
		return out
	}
	for i, id := range ids {
		if h, ok := hosts[hostprovider.HostID(id)]; ok {
			out[i] = &dataloader.Result[*graphql1.Host]{Data: h}
			continue
		}
		// Foreign host id — fall back to a stub so node(id) lookups
		// keep the schema's nullable contract.
		out[i] = &dataloader.Result[*graphql1.Host]{Data: &graphql1.Host{ID: id}}
	}
	return out
}

func idsToHostKeys(ids []string) []hostprovider.HostID {
	out := make([]hostprovider.HostID, len(ids))
	for i, id := range ids {
		out[i] = hostprovider.HostID(id)
	}
	return out
}

// loadWorktreesForCwds is the dataloader batch fn — wraps the public
// WorktreesForCwds in the dataloader.Result envelope.
func loadWorktreesForCwds(providers *ProvidersBundle, cwds []string) []*dataloader.Result[*graphql1.Worktree] {
	values := WorktreesForCwds(providers, cwds)
	out := make([]*dataloader.Result[*graphql1.Worktree], len(cwds))
	for i, v := range values {
		out[i] = &dataloader.Result[*graphql1.Worktree]{Data: v}
	}
	return out
}

// WorktreesForCwds runs the cwd-prefix join in one pass: list every
// repo, expand each repo's worktrees, then for each requested cwd
// find the longest worktree path that is a prefix.
func WorktreesForCwds(providers *ProvidersBundle, cwds []string) []*graphql1.Worktree {
	out := make([]*graphql1.Worktree, len(cwds))
	gp := providers.Git
	pp := providers.Repos
	if gp == nil || pp == nil {
		return out
	}

	ctx := context.Background()
	rows, err := pp.List(ctx)
	if err != nil {
		return out
	}

	type record struct {
		worktree gitprovider.Worktree
	}
	var records []record
	for _, row := range rows {
		ws, listErr := gp.ListByProject(ctx, string(row.ID))
		if listErr != nil {
			continue
		}
		for _, w := range ws {
			records = append(records, record{worktree: w})
		}
	}

	for i, cwd := range cwds {
		var bestPath string
		var bestWT gitprovider.Worktree
		for _, rec := range records {
			if rec.worktree.Path == "" {
				continue
			}
			if !cwdInWorktree(cwd, rec.worktree.Path) {
				continue
			}
			if len(rec.worktree.Path) > len(bestPath) {
				bestPath = rec.worktree.Path
				bestWT = rec.worktree
			}
		}
		if bestPath == "" {
			out[i] = nil
			continue
		}
		out[i] = &graphql1.Worktree{
			ID:     string(bestWT.ID),
			Path:   bestWT.Path,
			Branch: bestWT.Branch,
			Head:   bestWT.Head,
			Bare:   bestWT.Bare,
		}
	}
	return out
}

// cwdInWorktree reports whether cwd is the worktree's path or a descendant.
//
// Uses filepath.Rel rather than strings.HasPrefix so that a worktree at
// `/repo/foo` does not match cwd `/repo/foobar` — the classic prefix-bypass
// anti-pattern. Mirrors the validatePath idiom in
// internal/server/conversations_jsonl_handler.go, the codebase's canonical
// shape for path-descent checks.
//
// The match is purely lexical (no symlink resolution): cwd and worktreePath
// come from tmux/process introspection and provider listings respectively;
// neither is user-controlled URL input. We Clean both so trailing slashes,
// `./` segments, and double separators don't perturb the result.
func cwdInWorktree(cwd, worktreePath string) bool {
	if cwd == "" || worktreePath == "" {
		return false
	}
	cleanCwd := filepath.Clean(cwd)
	cleanWT := filepath.Clean(worktreePath)
	rel, err := filepath.Rel(cleanWT, cleanCwd)
	if err != nil {
		return false
	}
	// rel == "." → exact match (cwd IS the worktree).
	// rel starting with ".." → cwd is outside the worktree.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// loadProcesses batches (host, pid) keys -> Process via one
// providers.Ps.GetMany call.
func loadProcesses(providers *ProvidersBundle, keys []ProcessKey) []*dataloader.Result[*graphql1.Process] {
	out := make([]*dataloader.Result[*graphql1.Process], len(keys))
	psp := providers.Ps
	if psp == nil {
		for i := range out {
			out[i] = &dataloader.Result[*graphql1.Process]{Data: nil}
		}
		return out
	}
	pids := make([]psprovider.ProcessID, len(keys))
	for i, k := range keys {
		pids[i] = psprovider.ProcessID{Host: k.HostID, PID: k.Pid}
	}
	values, _, err := psp.GetMany(context.Background(), pids)
	if err != nil {
		for i := range out {
			out[i] = &dataloader.Result[*graphql1.Process]{Error: err}
		}
		return out
	}
	for i, k := range keys {
		key := psprovider.ProcessID{Host: k.HostID, PID: k.Pid}
		v, ok := values[key]
		if !ok {
			out[i] = &dataloader.Result[*graphql1.Process]{Data: nil}
			continue
		}
		out[i] = &dataloader.Result[*graphql1.Process]{Data: projectProcess(&v, k.HostID)}
	}
	return out
}

// loadPullRequestsForRepo is the DataLoader batch function for the
// PullRequestsForRepo loader. Each key is a RepoKey (owner/name pair);
// the batch collapses all concurrent resolver calls for the same repo
// into one underlying gh.Provider.ListPullRequests call.
//
// When NoCache is configured (as in production), the DataLoader may
// present duplicate keys in the same batch. This function deduplicates
// internally so the gh provider is called at most once per unique repo,
// regardless of how many Load calls arrived with the same key.
//
// Returns PRs across all states (open + closed + merged) for each
// position in keys, sorted by UpdatedAt DESC (GitHub's sort order on
// the list endpoint). The resolver layer does the headRef→branch match
// locally against this slice and chooses OPEN over CLOSED/MERGED per
// the Worktree.pr semantics defined in schema.graphql (issue #489).
// The all-states fetch costs one round-trip per repo per request — the
// same number as the previous open-only fetch — at the cost of a larger
// response body.
//
// Pagination (issue #579): the gh client walks GitHub's Link rel="next"
// chain up to gh.MaxPages (1000 items at per_page=100), so matches
// across multiple pages are now visible to the headRef→branch join.
// Repos with more than gh.MaxPages × per_page PRs surface as a
// truncated slice rather than an unbounded fetch.
//
// Slice-sharing contract: when multiple positions in keys map to the same
// RepoKey, every position receives the SAME *graphql1.PullRequest slice
// pointer. Callers MUST treat the slice as read-only — appending,
// sorting, or otherwise mutating the slice would corrupt sibling
// consumers in the same batch. The Worktree.pr resolver only iterates
// for headRef equality, which is safe.
func loadPullRequestsForRepo(ctx context.Context, providers *ProvidersBundle, keys []RepoKey) []*dataloader.Result[[]*graphql1.PullRequest] {
	out := make([]*dataloader.Result[[]*graphql1.PullRequest], len(keys))
	ghp := providers.GH
	if ghp == nil {
		for i := range out {
			out[i] = &dataloader.Result[[]*graphql1.PullRequest]{Data: nil}
		}
		return out
	}

	// Deduplicate: fetch each unique repo exactly once, even when the
	// NoCache loader sends the same key multiple times in one batch.
	type repoResult struct {
		prs []*graphql1.PullRequest
		err error
	}
	cache := make(map[RepoKey]*repoResult, len(keys))
	for _, key := range keys {
		if _, seen := cache[key]; seen {
			continue
		}
		// state=ALL so the Worktree.pr resolver can return the most-recent
		// CLOSED/MERGED PR when no OPEN PR matches the branch (issue #489).
		// Stays a single round-trip — the GitHub /pulls endpoint accepts a
		// state query param and returns one page (per_page=100) sorted by
		// UpdatedAt DESC.
		prs, err := ghp.ListPullRequests(ctx, key.Owner, key.Name, ghprovider.PullRequestStateAll)
		if err != nil {
			cache[key] = &repoResult{err: err}
			continue
		}
		gqlPRs := make([]*graphql1.PullRequest, 0, len(prs))
		for _, p := range prs {
			pr := p // avoid loop-var capture
			gqlPRs = append(gqlPRs, projectPullRequest(pr))
		}
		cache[key] = &repoResult{prs: gqlPRs}
	}

	for i, key := range keys {
		r := cache[key]
		if r.err != nil {
			out[i] = &dataloader.Result[[]*graphql1.PullRequest]{Error: r.err}
		} else {
			out[i] = &dataloader.Result[[]*graphql1.PullRequest]{Data: r.prs}
		}
	}
	return out
}

// projectPullRequest projects a provider PullRequest into the GraphQL
// model. Duplicated from resolvers/gh.go (toGraphQLPullRequest) to
// keep the loaders package free of a circular import — the resolvers
// package imports loaders, so loaders cannot import resolvers.
//
// MUST mirror resolvers/gh.go::toGraphQLPullRequest field-for-field.
// When you add a field to one, add it to the other in the same commit
// (and consider extracting to a shared internal/server/graphql/projection
// package if the duplication grows beyond two sites).
func projectPullRequest(p ghprovider.PullRequest) *graphql1.PullRequest {
	return &graphql1.PullRequest{
		ID:          p.ID(),
		RepoOwner:   p.RepoOwner,
		RepoName:    p.RepoName,
		Number:      int64(p.Number),
		Title:       p.Title,
		Body:        p.Body,
		State:       mapPullRequestState(p.State),
		Draft:       p.Draft,
		AuthorLogin: p.AuthorLogin,
		BaseRef:     p.BaseRef,
		HeadRef:     p.HeadRef,
		URL:         p.URL,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
}

// mapPullRequestState maps the provider's PullRequestState enum to the
// generated GraphQL enum. Mirrors resolvers/gh.go mapPRStateBack without
// importing that package.
func mapPullRequestState(s ghprovider.PullRequestState) graphql1.PullRequestState {
	switch s {
	case ghprovider.PullRequestStateOpen:
		return graphql1.PullRequestStateOpen
	case ghprovider.PullRequestStateClosed:
		return graphql1.PullRequestStateClosed
	case ghprovider.PullRequestStateMerged:
		return graphql1.PullRequestStateMerged
	case ghprovider.PullRequestStateAll:
		return graphql1.PullRequestStateAll
	default:
		return graphql1.PullRequestStateOpen
	}
}

// loadPullRequestEnrichments is the DataLoader batch function for the
// PullRequestEnrichment loader. It collapses all concurrent requests for
// enrichment fields (mergeable, mergeStateStatus, reviewDecision,
// statusCheckRollup, labels) into one BatchEnrichPullRequests call per
// batch window, which in turn fires one HTTP request per unique repository.
//
// When providers.GHEnricher is nil (e.g. gh auth is not configured), every
// position returns a zero PullRequest so the resolver falls back to its
// empty-field defaults without a hard error.
func loadPullRequestEnrichments(ctx context.Context, providers *ProvidersBundle, keys []ghprovider.PullRequestKey) []*dataloader.Result[ghprovider.PullRequest] {
	out := make([]*dataloader.Result[ghprovider.PullRequest], len(keys))
	enricher := providers.GHEnricher
	if enricher == nil {
		for i := range out {
			out[i] = &dataloader.Result[ghprovider.PullRequest]{Data: ghprovider.PullRequest{}}
		}
		return out
	}

	results, err := enricher.BatchEnrichPullRequests(ctx, keys)
	if err != nil {
		// Partial failure: BatchEnrichPullRequests returns stale values for
		// keys it couldn't fetch. We surface the error per-key only when the
		// map has no entry for that key (total failure path).
		for i, k := range keys {
			if pr, ok := results[k]; ok {
				out[i] = &dataloader.Result[ghprovider.PullRequest]{Data: pr}
			} else {
				out[i] = &dataloader.Result[ghprovider.PullRequest]{Error: err}
			}
		}
		return out
	}

	for i, k := range keys {
		pr := results[k]
		out[i] = &dataloader.Result[ghprovider.PullRequest]{Data: pr}
	}
	return out
}

// projectProcess mirrors the resolver-layer projection so the loader
// returns a fully-formed Process value. Kept here so loaders is
// self-contained.
func projectProcess(p *psprovider.Process, hostID string) *graphql1.Process {
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

// ---------------------------------------------------------------------------
// Pane loaders (ADR-022 Phase 2): PaneByID, PanesByCwd, PanesByCommand.
// ---------------------------------------------------------------------------

// panePsGetterAdapter bridges *psprovider.Provider to the narrow
// tmuxprovider.PanePsGetter interface. Nil when ps is not wired.
type panePsGetterAdapter struct {
	ps *psprovider.Provider
}

// newPanePsGetterAdapter returns nil when ps is nil so callers can
// pass it to PanesByCommand / PanesByCwd without checking.
func newPanePsGetterAdapter(ps *psprovider.Provider) tmuxprovider.PanePsGetter {
	if ps == nil {
		return nil
	}
	return &panePsGetterAdapter{ps: ps}
}

func (a *panePsGetterAdapter) CwdForPid(host string, pid int) string {
	if pid <= 0 {
		return ""
	}
	cwd, err := a.ps.LoadCwd(context.Background(), pid)
	if err != nil {
		return ""
	}
	return cwd
}

func (a *panePsGetterAdapter) CommandForPid(host string, pid int) string {
	if pid <= 0 {
		return ""
	}
	proc, _, err := a.ps.Get(context.Background(), psprovider.ProcessID{Host: host, PID: pid})
	if err != nil {
		return ""
	}
	return proc.Command
}

// loadPanesByID is the DataLoader batch function for the PaneByID loader.
// A single tmux snapshot read serves all keys in the batch.
func loadPanesByID(providers *ProvidersBundle, keys []PaneKey) []*dataloader.Result[*graphql1.TmuxPane] {
	out := make([]*dataloader.Result[*graphql1.TmuxPane], len(keys))
	tp := providers.Tmux
	if tp == nil {
		for i := range out {
			out[i] = &dataloader.Result[*graphql1.TmuxPane]{Data: nil}
		}
		return out
	}
	for i, k := range keys {
		pn, ok := tp.PaneByID(k.HostID, k.PaneID)
		if !ok {
			out[i] = &dataloader.Result[*graphql1.TmuxPane]{Data: nil}
			continue
		}
		out[i] = &dataloader.Result[*graphql1.TmuxPane]{Data: projectLoaderPane(pn)}
	}
	return out
}

// loadPanesByCwd is the DataLoader batch function for the PanesByCwd loader.
// Groups keys by (host, cwd) and calls PanesByCwd once per unique pair.
func loadPanesByCwd(providers *ProvidersBundle, keys []CwdKey, ps tmuxprovider.PanePsGetter) []*dataloader.Result[[]*graphql1.TmuxPane] {
	out := make([]*dataloader.Result[[]*graphql1.TmuxPane], len(keys))
	tp := providers.Tmux
	if tp == nil {
		for i := range out {
			out[i] = &dataloader.Result[[]*graphql1.TmuxPane]{Data: []*graphql1.TmuxPane{}}
		}
		return out
	}
	// Deduplicate: same key may appear multiple times in one batch.
	type result struct{ panes []*graphql1.TmuxPane }
	cache := make(map[CwdKey]*result, len(keys))
	for _, k := range keys {
		if _, seen := cache[k]; seen {
			continue
		}
		raw := tp.PanesByCwd(k.HostID, k.Cwd, ps)
		gql := make([]*graphql1.TmuxPane, len(raw))
		for i, pn := range raw {
			gql[i] = projectLoaderPane(pn)
		}
		cache[k] = &result{panes: gql}
	}
	for i, k := range keys {
		out[i] = &dataloader.Result[[]*graphql1.TmuxPane]{Data: cache[k].panes}
	}
	return out
}

// loadPanesByCommand is the DataLoader batch function for the PanesByCommand loader.
// Groups keys by (host, command) and calls PanesByCommand once per unique pair.
func loadPanesByCommand(providers *ProvidersBundle, keys []CommandKey, ps tmuxprovider.PanePsGetter) []*dataloader.Result[[]*graphql1.TmuxPane] {
	out := make([]*dataloader.Result[[]*graphql1.TmuxPane], len(keys))
	tp := providers.Tmux
	if tp == nil {
		for i := range out {
			out[i] = &dataloader.Result[[]*graphql1.TmuxPane]{Data: []*graphql1.TmuxPane{}}
		}
		return out
	}
	type result struct{ panes []*graphql1.TmuxPane }
	cache := make(map[CommandKey]*result, len(keys))
	for _, k := range keys {
		if _, seen := cache[k]; seen {
			continue
		}
		raw := tp.PanesByCommand(k.HostID, k.Command, ps)
		gql := make([]*graphql1.TmuxPane, len(raw))
		for i, pn := range raw {
			gql[i] = projectLoaderPane(pn)
		}
		cache[k] = &result{panes: gql}
	}
	for i, k := range keys {
		out[i] = &dataloader.Result[[]*graphql1.TmuxPane]{Data: cache[k].panes}
	}
	return out
}

// projectLoaderPane projects a tmuxprovider.Pane onto *graphql1.TmuxPane.
// Mirrors loader_bridge.go:projectTmuxPane — kept here so the loaders
// package is self-contained.
func projectLoaderPane(pn tmuxprovider.Pane) *graphql1.TmuxPane {
	out := &graphql1.TmuxPane{
		ID:             "TmuxPane:" + string(pn.Key.Host) + ":" + pn.Key.PaneID,
		PaneID:         pn.Key.PaneID,
		CurrentCommand: pn.CurrentCommand,
	}
	if pn.CurrentPid > 0 {
		pid := int64(pn.CurrentPid)
		out.CurrentPid = &pid
	}
	return out
}
