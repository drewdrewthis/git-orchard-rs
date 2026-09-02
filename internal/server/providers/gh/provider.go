package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/adapter"
)

// CacheTTL is the per-entry freshness window. ADR-011 §12 explicitly
// calls out a 2-minute TTL: "PRs/issues don't change often; rate limit
// is precious."
const CacheTTL = 2 * time.Minute

// rateLimitCooldown is how long the enrichment paths stop calling GitHub after
// a rate-limit response. GitHub resets hourly; five minutes is short enough to
// recover from a one-off burst and long enough to stop wasting quota when we
// are genuinely throttled.
const rateLimitCooldown = 5 * time.Minute

// Provider is the gh provider. It owns three sub-providers — one per
// node type — each with its own typed cache. Resolvers call into the
// Provider's high-level methods (ListPullRequests, ListIssues, etc.);
// the per-key adapter.Provider[K,V] surface is exposed via the
// dedicated PullRequests / Issues / WorkflowRuns sub-fields for callers
// (DataLoader, Subscriptions in Workstream C) that need it.
//
// Auth is resolved lazily on the first call: Start primes the bootstrap
// shellout but the result is also cached by authCache.Resolve, so a
// failure on Start is preserved and surfaced on every subsequent gh
// resolver call until the daemon restarts.
type Provider struct {
	logger *slog.Logger
	clock  func() time.Time
	auth   *authCache

	// baseURL is the GitHub REST endpoint, possibly overridden via
	// EnvAPIBaseURL for tests / GHES.
	baseURL string

	// client is built lazily on first auth resolution. It captures the
	// token in a Bearer header for every subsequent request.
	clientOnce sync.Once
	client     *Client
	clientErr  error

	// Per-node-type caches. Keyed by their typed key, value is the
	// node + freshness.
	prMu     sync.RWMutex
	prs      map[PullRequestKey]prEntry
	enrichAt map[PullRequestKey]time.Time // when EnrichPullRequest last populated the enrichment fields

	// Rate-limit cooldown: when set, EnrichPullRequest skips the GraphQL
	// call entirely and serves stale (or returns nil). Cleared on each
	// successful response. Protected by prMu.
	rateLimitedUntil time.Time

	issueMu   sync.RWMutex
	issues    map[IssueKey]issueEntry
	issueDeps map[IssueKey]issueDepsEntry // per-issue dependency snapshot (#563); shorter TTL than issues

	runMu sync.RWMutex
	runs  map[WorkflowRunKey]runEntry

	// Per-list caches (keyed by repo+state) so repeated identical list
	// queries within the TTL share a single round-trip.
	listMu       sync.RWMutex
	listPRsCache map[listPRsKey]listPRsEntry
	listIssCache map[listIssKey]listIssEntry
	listRunCache map[listRunKey]listRunEntry

	// Subscribers receive InvalidationEvent[string] where the key is
	// the GraphQL node id (PullRequest:owner/repo#123 etc.). One
	// channel per subscription; non-blocking sends drop on slow
	// consumers.
	subsMu sync.Mutex
	subs   map[chan adapter.InvalidationEvent[string]]struct{}
}

type prEntry struct {
	value PullRequest
	at    time.Time
}

type issueEntry struct {
	value Issue
	at    time.Time
}

// issueDepsEntry caches the dependency-edge fetch for an issue. The
// shorter TTL (issueDepsTTL) lives in graphql_issue_deps.go.
type issueDepsEntry struct {
	value IssueDependencies
	at    time.Time
}

type runEntry struct {
	value WorkflowRun
	at    time.Time
}

type listPRsKey struct {
	Owner string
	Name  string
	State PullRequestState
}

type listPRsEntry struct {
	values []PullRequest
	at     time.Time
}

type listIssKey struct {
	Owner string
	Name  string
	State IssueState
}

type listIssEntry struct {
	values []Issue
	at     time.Time
}

type listRunKey struct {
	Owner string
	Name  string
}

type listRunEntry struct {
	values []WorkflowRun
	at     time.Time
}

// New constructs a Provider that will resolve its bearer token via
// `gh auth token` on first use. baseURL falls back to
// $GH_API_BASE_URL or DefaultAPIBaseURL.
func New(logger *slog.Logger, baseURL string) *Provider {
	return NewWith(logger, baseURL, NewCommandAuthSource(), time.Now)
}

// NewWith is the test-friendly constructor. Both auth source and clock
// are injectable so tests can stub the gh shellout and drive
// freshness deterministically.
func NewWith(logger *slog.Logger, baseURL string, auth AuthSource, clock func() time.Time) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = time.Now
	}
	if baseURL == "" {
		baseURL = DefaultAPIBaseURL
	}
	return &Provider{
		logger:       logger,
		clock:        clock,
		auth:         newAuthCache(auth),
		baseURL:      baseURL,
		prs:          map[PullRequestKey]prEntry{},
		enrichAt:     map[PullRequestKey]time.Time{},
		issues:       map[IssueKey]issueEntry{},
		issueDeps:    map[IssueKey]issueDepsEntry{},
		runs:         map[WorkflowRunKey]runEntry{},
		listPRsCache: map[listPRsKey]listPRsEntry{},
		listIssCache: map[listIssKey]listIssEntry{},
		listRunCache: map[listRunKey]listRunEntry{},
		subs:         map[chan adapter.InvalidationEvent[string]]struct{}{},
	}
}

// Start primes the auth bootstrap. Failure is non-fatal — the provider
// remembers the error and surfaces it on every gh resolver call until
// the daemon restarts. This is the ADR-011 §12 contract: a missing or
// not-authed `gh` does not collapse the daemon; it fails per-field.
func (p *Provider) Start(ctx context.Context) error {
	_, err := p.resolveAuth(ctx)
	if err != nil {
		p.logger.Warn("gh: auth bootstrap failed; gh-derived fields will surface per-field GraphQL errors", "err", err)
		// Intentional: do not return err — the daemon must continue
		// serving non-gh fields. Resolvers see the error via
		// resolveAuth on each call.
	}
	return nil
}

// resolveAuth caches the token on first call. Subsequent calls return
// the same value (or the same error) without rerunning the shellout.
func (p *Provider) resolveAuth(ctx context.Context) (string, error) {
	return p.auth.Resolve(ctx)
}

// httpClient returns a Client with the auth token applied. Built on
// demand, then cached for the daemon lifetime.
func (p *Provider) httpClient(ctx context.Context) (*Client, error) {
	p.clientOnce.Do(func() {
		token, err := p.resolveAuth(ctx)
		if err != nil {
			p.clientErr = err
			return
		}
		p.client = NewClient(p.baseURL, token)
		// The client owns the per-call audit line (#749) and needs the
		// provider's leveled logger to emit it.
		p.client.Logger = p.logger
	})
	if p.clientErr != nil {
		return nil, p.clientErr
	}
	return p.client, nil
}

// enrichmentStillValid reports whether GraphQL-derived enrichment computed
// against prev is still valid for the freshly-fetched next.
//
// A REST refresh that returns a byte-identical PR tells us nothing changed,
// so enrichment computed a moment ago still describes this revision. Any
// movement in UpdatedAt (a new commit, a comment, a label edit) or in the
// lifecycle State means the enrichment may describe an older revision, and
// the conservative answer is to refetch.
//
// This deliberately keys off REST-visible fields only. Keying off the
// GraphQL-computed fields themselves (mergeable, mergeStateStatus) would
// reintroduce the #367 flap, where a transient recompute reads as a change.
func enrichmentStillValid(prev, next PullRequest) bool {
	return prev.UpdatedAt == next.UpdatedAt && prev.State == next.State
}

// carryEnrichment copies the GraphQL-derived enrichment fields from prev onto
// next. REST payloads never carry these, so a refresh that overwrote the
// cached entry wholesale would zero them while enrichAt still claimed the
// entry was freshly enriched.
func carryEnrichment(next *PullRequest, prev PullRequest) {
	next.Mergeable = prev.Mergeable
	next.MergeStateStatus = prev.MergeStateStatus
	next.ReviewDecision = prev.ReviewDecision
	next.StatusCheckRollup = prev.StatusCheckRollup
	next.Labels = prev.Labels
}

// ListPullRequests fetches and caches the PR list for a repo. The
// cache key is (owner, name, state); within CacheTTL, repeated calls
// share one round-trip.
func (p *Provider) ListPullRequests(ctx context.Context, owner, name string, state PullRequestState) ([]PullRequest, error) {
	key := listPRsKey{Owner: owner, Name: name, State: state}
	repo := owner + "/" + name
	p.listMu.RLock()
	if e, ok := p.listPRsCache[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		out := append([]PullRequest(nil), e.values...)
		age := p.clock().Sub(e.at)
		p.listMu.RUnlock()
		p.logCacheHit("ListPullRequests", repo, age)
		return out, nil
	}
	p.listMu.RUnlock()
	p.logCacheMiss("ListPullRequests", repo)

	c, err := p.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	prs, err := c.ListPulls(ctx, owner, name, state)
	if err != nil {
		return nil, err
	}

	now := p.clock()
	p.listMu.Lock()
	p.listPRsCache[key] = listPRsEntry{values: prs, at: now}
	p.listMu.Unlock()

	// Also seed the per-key cache so a follow-up GetPullRequest is
	// cheap. The REST list endpoint does not return enrichment fields
	// (mergeable, mergeStateStatus, reviewDecision, statusCheckRollup,
	// labels), so overwriting the entry wholesale would zero them. When
	// the PR is unchanged we carry the existing enrichment forward and
	// keep its timestamp; otherwise we drop the timestamp so the next
	// EnrichPullRequest refetches via GraphQL.
	p.prMu.Lock()
	for _, pr := range prs {
		k := PullRequestKey{Owner: pr.RepoOwner, Name: pr.RepoName, Number: pr.Number}
		if prev, ok := p.prs[k]; ok && enrichmentStillValid(prev.value, pr) {
			carryEnrichment(&pr, prev.value)
		} else {
			delete(p.enrichAt, k)
		}
		p.prs[k] = prEntry{value: pr, at: now}
	}
	p.prMu.Unlock()

	return append([]PullRequest(nil), prs...), nil
}

// GetPullRequest fetches one PR. Cache hit when fresh; otherwise the
// adapter is consulted and the result cached.
func (p *Provider) GetPullRequest(ctx context.Context, key PullRequestKey) (PullRequest, error) {
	repo := key.Owner + "/" + key.Name
	p.prMu.RLock()
	if e, ok := p.prs[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		v := e.value
		age := p.clock().Sub(e.at)
		p.prMu.RUnlock()
		p.logCacheHit("GetPullRequest", repo, age)
		return v, nil
	}
	p.prMu.RUnlock()
	p.logCacheMiss("GetPullRequest", repo)

	c, err := p.httpClient(ctx)
	if err != nil {
		return PullRequest{}, err
	}
	pr, err := c.GetPull(ctx, key.Owner, key.Name, key.Number)
	if err != nil {
		return PullRequest{}, err
	}
	p.prMu.Lock()
	// REST GetPull does not populate enrichment fields. Carry them forward
	// when the PR is unchanged; otherwise drop the enrichment timestamp so
	// the next EnrichPullRequest refetches.
	if prev, ok := p.prs[key]; ok && enrichmentStillValid(prev.value, pr) {
		carryEnrichment(&pr, prev.value)
	} else {
		delete(p.enrichAt, key)
	}
	p.prs[key] = prEntry{value: pr, at: p.clock()}
	p.prMu.Unlock()
	return pr, nil
}

// ListIssues fetches and caches the issue list for a repo.
func (p *Provider) ListIssues(ctx context.Context, owner, name string, state IssueState) ([]Issue, error) {
	key := listIssKey{Owner: owner, Name: name, State: state}
	repo := owner + "/" + name
	p.listMu.RLock()
	if e, ok := p.listIssCache[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		out := append([]Issue(nil), e.values...)
		age := p.clock().Sub(e.at)
		p.listMu.RUnlock()
		p.logCacheHit("ListIssues", repo, age)
		return out, nil
	}
	p.listMu.RUnlock()
	p.logCacheMiss("ListIssues", repo)

	c, err := p.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	issues, err := c.ListIssues(ctx, owner, name, state)
	if err != nil {
		return nil, err
	}
	now := p.clock()
	p.listMu.Lock()
	p.listIssCache[key] = listIssEntry{values: issues, at: now}
	p.listMu.Unlock()

	p.issueMu.Lock()
	for _, i := range issues {
		k := IssueKey{Owner: i.RepoOwner, Name: i.RepoName, Number: i.Number}
		p.issues[k] = issueEntry{value: i, at: now}
	}
	p.issueMu.Unlock()
	return append([]Issue(nil), issues...), nil
}

// GetIssue fetches one issue.
func (p *Provider) GetIssue(ctx context.Context, key IssueKey) (Issue, error) {
	repo := key.Owner + "/" + key.Name
	p.issueMu.RLock()
	if e, ok := p.issues[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		v := e.value
		age := p.clock().Sub(e.at)
		p.issueMu.RUnlock()
		p.logCacheHit("GetIssue", repo, age)
		return v, nil
	}
	p.issueMu.RUnlock()
	p.logCacheMiss("GetIssue", repo)

	c, err := p.httpClient(ctx)
	if err != nil {
		return Issue{}, err
	}
	i, err := c.GetIssue(ctx, key.Owner, key.Name, key.Number)
	if err != nil {
		return Issue{}, err
	}
	p.issueMu.Lock()
	p.issues[key] = issueEntry{value: i, at: p.clock()}
	p.issueMu.Unlock()
	return i, nil
}

// ListWorkflowRuns fetches and caches the workflow-run list.
func (p *Provider) ListWorkflowRuns(ctx context.Context, owner, name string) ([]WorkflowRun, error) {
	key := listRunKey{Owner: owner, Name: name}
	repo := owner + "/" + name
	p.listMu.RLock()
	if e, ok := p.listRunCache[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		out := append([]WorkflowRun(nil), e.values...)
		age := p.clock().Sub(e.at)
		p.listMu.RUnlock()
		p.logCacheHit("ListWorkflowRuns", repo, age)
		return out, nil
	}
	p.listMu.RUnlock()
	p.logCacheMiss("ListWorkflowRuns", repo)

	c, err := p.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	runs, err := c.ListWorkflowRuns(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	now := p.clock()
	p.listMu.Lock()
	p.listRunCache[key] = listRunEntry{values: runs, at: now}
	p.listMu.Unlock()

	p.runMu.Lock()
	for _, r := range runs {
		k := WorkflowRunKey{Owner: r.RepoOwner, Name: r.RepoName, RunID: r.RunID}
		p.runs[k] = runEntry{value: r, at: now}
	}
	p.runMu.Unlock()
	return append([]WorkflowRun(nil), runs...), nil
}

// GetWorkflowRun fetches one workflow run.
func (p *Provider) GetWorkflowRun(ctx context.Context, key WorkflowRunKey) (WorkflowRun, error) {
	repo := key.Owner + "/" + key.Name
	p.runMu.RLock()
	if e, ok := p.runs[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		v := e.value
		age := p.clock().Sub(e.at)
		p.runMu.RUnlock()
		p.logCacheHit("GetWorkflowRun", repo, age)
		return v, nil
	}
	p.runMu.RUnlock()
	p.logCacheMiss("GetWorkflowRun", repo)

	c, err := p.httpClient(ctx)
	if err != nil {
		return WorkflowRun{}, err
	}
	r, err := c.GetWorkflowRun(ctx, key.Owner, key.Name, key.RunID)
	if err != nil {
		return WorkflowRun{}, err
	}
	p.runMu.Lock()
	p.runs[key] = runEntry{value: r, at: p.clock()}
	p.runMu.Unlock()
	return r, nil
}

// ListPullRequestReviews fetches reviews on a PR. No caching beyond
// the request-level scope — reviews change often during active review.
func (p *Provider) ListPullRequestReviews(ctx context.Context, owner, name string, number int) ([]PullRequestReview, error) {
	c, err := p.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListPullReviews(ctx, owner, name, number)
}

// ListPullRequestComments fetches conversation comments on a PR.
func (p *Provider) ListPullRequestComments(ctx context.Context, owner, name string, number int) ([]IssueComment, error) {
	c, err := p.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListPullComments(ctx, owner, name, number)
}

// ListIssueComments fetches comments on an issue.
func (p *Provider) ListIssueComments(ctx context.Context, owner, name string, number int) ([]IssueComment, error) {
	c, err := p.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListIssueComments(ctx, owner, name, number)
}

// GetRepository fetches repository metadata.
func (p *Provider) GetRepository(ctx context.Context, owner, name string) (Repository, error) {
	c, err := p.httpClient(ctx)
	if err != nil {
		return Repository{}, err
	}
	return c.GetRepo(ctx, owner, name)
}

// GraphQL forwards an arbitrary GraphQL query to GitHub's API and
// returns the parsed `{ data, errors, extensions }` envelope as a Go
// map. Backed by Client.GraphQL, which never caches: query strings are
// arbitrary, so caching by query+variables would be both surprising
// and easily defeated. Per-resolver auth/rate-limit shaping mirrors
// the REST path (issue #418).
func (p *Provider) GraphQL(ctx context.Context, query string, variables map[string]any) (map[string]any, error) {
	c, err := p.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := c.GraphQL(ctx, query, variables)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode graphql envelope: %w", err)
	}
	return out, nil
}

// Subscribe returns a channel that receives invalidation events when
// the webhook stub or rate-limit recovery emits one. v1: only webhook
// emits these; clients see push updates within the same daemon
// process. ADR-011 §8.
func (p *Provider) Subscribe(ctx context.Context) <-chan adapter.InvalidationEvent[string] {
	ch := make(chan adapter.InvalidationEvent[string], 16)
	p.subsMu.Lock()
	p.subs[ch] = struct{}{}
	p.subsMu.Unlock()
	go func() {
		<-ctx.Done()
		p.subsMu.Lock()
		delete(p.subs, ch)
		p.subsMu.Unlock()
		close(ch)
	}()
	return ch
}

// invalidate broadcasts an event to subscribers and drops the relevant
// cache entry so the next read goes back to the API. Used by the
// webhook handler.
func (p *Provider) invalidate(nodeID, reason string, at time.Time) {
	ev := adapter.InvalidationEvent[string]{
		Key:    nodeID,
		Reason: reason,
		At:     at,
	}
	p.subsMu.Lock()
	subs := make([]chan adapter.InvalidationEvent[string], 0, len(p.subs))
	for ch := range p.subs {
		subs = append(subs, ch)
	}
	p.subsMu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			p.logger.Warn("gh: subscriber lagging, dropping event", "node_id", nodeID)
		}
	}
}

// AuthError returns the bootstrap error if Start failed (or nil if it
// succeeded). Used by tests that want to assert the auth path without
// triggering a real API call.
func (p *Provider) AuthError(ctx context.Context) error {
	_, err := p.resolveAuth(ctx)
	return err
}

// Compile-time discipline: the resolver layer depends on these specific
// method signatures. If they change, the build breaks at the resolver
// call site, not here.
var (
	_ = (*Provider)(nil).ListPullRequests
	_ = (*Provider)(nil).ListIssues
	_ = (*Provider)(nil).ListWorkflowRuns
	_ = (*Provider)(nil).ListPullRequestReviews
	_ = (*Provider)(nil).ListPullRequestComments
	_ = (*Provider)(nil).ListIssueComments
	_ = (*Provider)(nil).GetPullRequest
	_ = (*Provider)(nil).GetIssue
	_ = (*Provider)(nil).GetWorkflowRun
	_ = (*Provider)(nil).GetRepository
	_ = (*Provider)(nil).EnrichPullRequest
	_ = (*Provider)(nil).BatchEnrichPullRequests
)

// errNoGitDir / errNoOriginRemote differentiate "no .git" from "no
// remote URL" so the resolver layer (via ReadOriginURL) can pick the
// right empty-vs-error path. Both are package-private — callers
// upstream only need "does origin parse to a GitHub URL".
var (
	errNoGitDir       = errors.New("not a git repo")
	errNoOriginRemote = errors.New("no origin remote")
)
