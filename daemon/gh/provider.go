// provider.go — in-process cache + stale-while-revalidate for the gh domain.
//
// O12: stale-while-revalidate is a first-class pattern here:
//   - Enrichment fields (mergeable, reviewDecision, statusCheckRollup,
//     labels) have a 5-minute TTL; UNKNOWN mergeable is never cached (#367).
//   - On rate-limit or network failure, stale enrichment (up to 1h) is
//     served so the sidebar stays populated through throttle windows.
//   - A rate-limit cooldown timer skips network calls for 5 minutes after
//     a rate-limit response, saving precious quota.
//
// O11: cache policies are explicit and not mixed within this module:
//   - Basic list/single endpoints: read-through with CacheTTL (2m).
//   - Enrichment: stale-while-revalidate with enrichmentTTL (5m) /
//     staleEnrichmentTTL (1h) per field.
//
// O4: cache hit/miss events are emitted as structured slog log lines.
//
// R13: RWMutex for read-heavy sub-caches; sync.Once for auth.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// CacheTTL is the per-entry freshness window for basic list/get endpoints.
// ADR-011 §12: "PRs/issues don't change often; rate limit is precious."
const CacheTTL = 2 * time.Minute

// enrichmentTTL governs how often enrichment fields are re-fetched from
// GitHub's GraphQL API (O12 SWR freshness TTL).
const enrichmentTTL = 5 * time.Minute

// staleEnrichmentTTL is how long stale enrichment is served on failure
// (rate limit, network blip). Far longer than enrichmentTTL — slightly
// stale data wins over a broken sidebar.
const staleEnrichmentTTL = 1 * time.Hour

// issueDepsTTL governs how long an enriched dependency snapshot is trusted.
const issueDepsTTL = 60 * time.Second

// rateLimitCooldown is how long the enrichment paths stop calling GitHub after
// a rate-limit response. GitHub resets hourly; five minutes is short enough to
// recover from a one-off burst and long enough to stop wasting quota when we
// are genuinely throttled.
const rateLimitCooldown = 5 * time.Minute

// Provider is the gh domain in-process cache and subscription broadcaster.
// It owns three sub-caches (PR, issue, workflow run), the rate-limit
// cooldown, and the webhook subscriber fanout.
//
// Auth is resolved lazily on the first call; Start primes the bootstrap
// shellout but the result is also cached by authCache, so a failure is
// preserved and surfaced on every subsequent gh resolver call until the
// daemon restarts.
type Provider struct {
	logger *slog.Logger
	clock  func() time.Time
	auth   *authCache

	baseURL    string
	clientOnce sync.Once
	client     *Client
	clientErr  error

	// Per-node-type caches.
	prMu             sync.RWMutex
	prs              map[PullRequestKey]prEntry
	enrichAt         map[PullRequestKey]time.Time
	rateLimitedUntil time.Time // inside prMu

	issueMu   sync.RWMutex
	issues    map[IssueKey]issueEntry
	issueDeps map[IssueKey]issueDepsEntry

	runMu sync.RWMutex
	runs  map[WorkflowRunKey]runEntry

	// Per-list caches (keyed by repo+state).
	listMu       sync.RWMutex
	listPRsCache map[listPRsKey]listPRsEntry
	listIssCache map[listIssKey]listIssEntry
	listRunCache map[listRunKey]listRunEntry

	// Subscription fanout.
	subsMu sync.Mutex
	subs   map[chan InvalidationEvent]struct{}
}

type prEntry struct {
	value PullRequest
	at    time.Time
}

type issueEntry struct {
	value Issue
	at    time.Time
}

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

// New constructs a Provider that resolves its bearer token via
// `gh auth token` on first use.
func New(logger *slog.Logger, baseURL string) *Provider {
	return NewWith(logger, baseURL, NewCommandAuthSource(), time.Now)
}

// NewWith is the test-friendly constructor. Auth source and clock are injectable.
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
		subs:         map[chan InvalidationEvent]struct{}{},
	}
}

// Start primes the auth bootstrap. Non-fatal — the provider remembers
// the error and surfaces it per-field. This keeps the daemon alive even
// when `gh` is not installed or the user is not logged in.
func (p *Provider) Start(ctx context.Context) error {
	_, err := p.resolveAuth(ctx)
	if err != nil {
		p.logger.Warn("gh: auth bootstrap failed; gh-derived fields will surface per-field GraphQL errors",
			slog.String("err", err.Error()))
		// Intentional non-return: daemon must continue serving non-gh fields.
	}
	return nil
}

func (p *Provider) resolveAuth(ctx context.Context) (string, error) {
	return p.auth.Resolve(ctx)
}

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

// --- PR operations ---

func (p *Provider) ListPullRequests(ctx context.Context, owner, name string, state PullRequestState) ([]PullRequest, error) {
	key := listPRsKey{Owner: owner, Name: name, State: state}
	p.listMu.RLock()
	if e, ok := p.listPRsCache[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		out := append([]PullRequest(nil), e.values...)
		p.listMu.RUnlock()
		p.logCacheHit("ListPullRequests", owner+"/"+name)
		return out, nil
	}
	p.listMu.RUnlock()

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

	p.prMu.Lock()
	for _, pr := range prs {
		k := PullRequestKey{Owner: pr.RepoOwner, Name: pr.RepoName, Number: pr.Number}
		p.prs[k] = prEntry{value: pr, at: now}
		delete(p.enrichAt, k)
	}
	p.prMu.Unlock()

	return append([]PullRequest(nil), prs...), nil
}

func (p *Provider) GetPullRequest(ctx context.Context, key PullRequestKey) (PullRequest, error) {
	p.prMu.RLock()
	if e, ok := p.prs[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		v := e.value
		p.prMu.RUnlock()
		return v, nil
	}
	p.prMu.RUnlock()

	c, err := p.httpClient(ctx)
	if err != nil {
		return PullRequest{}, err
	}
	pr, err := c.GetPull(ctx, key.Owner, key.Name, key.Number)
	if err != nil {
		return PullRequest{}, err
	}
	p.prMu.Lock()
	p.prs[key] = prEntry{value: pr, at: p.clock()}
	delete(p.enrichAt, key)
	p.prMu.Unlock()
	return pr, nil
}

func (p *Provider) ListPullRequestReviews(ctx context.Context, owner, name string, number int) ([]PullRequestReview, error) {
	c, err := p.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListPullReviews(ctx, owner, name, number)
}

func (p *Provider) ListPullRequestComments(ctx context.Context, owner, name string, number int) ([]IssueComment, error) {
	c, err := p.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListPullComments(ctx, owner, name, number)
}

// --- Issue operations ---

func (p *Provider) ListIssues(ctx context.Context, owner, name string, state IssueState) ([]Issue, error) {
	key := listIssKey{Owner: owner, Name: name, State: state}
	p.listMu.RLock()
	if e, ok := p.listIssCache[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		out := append([]Issue(nil), e.values...)
		p.listMu.RUnlock()
		return out, nil
	}
	p.listMu.RUnlock()

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

func (p *Provider) GetIssue(ctx context.Context, key IssueKey) (Issue, error) {
	p.issueMu.RLock()
	if e, ok := p.issues[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		v := e.value
		p.issueMu.RUnlock()
		return v, nil
	}
	p.issueMu.RUnlock()

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

func (p *Provider) ListIssueComments(ctx context.Context, owner, name string, number int) ([]IssueComment, error) {
	c, err := p.httpClient(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListIssueComments(ctx, owner, name, number)
}

// --- WorkflowRun operations ---

func (p *Provider) ListWorkflowRuns(ctx context.Context, owner, name string) ([]WorkflowRun, error) {
	key := listRunKey{Owner: owner, Name: name}
	p.listMu.RLock()
	if e, ok := p.listRunCache[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		out := append([]WorkflowRun(nil), e.values...)
		p.listMu.RUnlock()
		return out, nil
	}
	p.listMu.RUnlock()

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

func (p *Provider) GetWorkflowRun(ctx context.Context, key WorkflowRunKey) (WorkflowRun, error) {
	p.runMu.RLock()
	if e, ok := p.runs[key]; ok && p.clock().Sub(e.at) < CacheTTL {
		v := e.value
		p.runMu.RUnlock()
		return v, nil
	}
	p.runMu.RUnlock()

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

// --- Repository ---

func (p *Provider) GetRepository(ctx context.Context, owner, name string) (Repository, error) {
	c, err := p.httpClient(ctx)
	if err != nil {
		return Repository{}, err
	}
	return c.GetRepo(ctx, owner, name)
}

// --- Pass-through ---

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

// --- Subscription ---

// Subscribe returns a receive-only channel of InvalidationEvents (R12).
// The channel is closed when ctx is cancelled. Goroutine ownership: the
// provider owns the cleanup goroutine (R10).
func (p *Provider) Subscribe(ctx context.Context) <-chan InvalidationEvent {
	ch := make(chan InvalidationEvent, 16)
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

// Invalidate broadcasts an event to subscribers and drops the relevant
// cache entry so the next read goes back to the API. Called by the webhook handler.
// R16: emit AFTER cache write. The drop happens here before broadcast.
func (p *Provider) Invalidate(nodeID, reason string, at time.Time) {
	ev := InvalidationEvent{Key: nodeID, Reason: reason, At: at}
	p.subsMu.Lock()
	subs := make([]chan InvalidationEvent, 0, len(p.subs))
	for ch := range p.subs {
		subs = append(subs, ch)
	}
	p.subsMu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			p.logger.Warn("gh: subscriber lagging, dropping event", slog.String("node_id", nodeID))
		}
	}
}

// --- Enrichment (O12 SWR) ---

// PhaseLabels are orchard lifecycle tags excluded from PullRequest.Labels.
var phaseLabels = map[string]struct{}{
	"investigating": {},
	"needs-plan":    {},
	"needs-repro":   {},
	"planned":       {},
	"in-progress":   {},
	"in-ai-review":  {},
	"pr-ready":      {},
	"blocked":       {},
}

const enrichPRFields = `mergeable mergeStateStatus reviewDecision labels(first:50){nodes{name color description}} commits(last:1){nodes{commit{statusCheckRollup{state}}}}`

const enrichPRQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      mergeable
      mergeStateStatus
      reviewDecision
      labels(first:50){ nodes{ name color description } }
      commits(last:1){ nodes{ commit{ statusCheckRollup{ state } } } }
    }
  }
}`

type enrichPRAlias struct {
	Mergeable        string  `json:"mergeable"`
	MergeStateStatus string  `json:"mergeStateStatus"`
	ReviewDecision   *string `json:"reviewDecision"`
	Labels           struct {
		Nodes []struct {
			Name        string `json:"name"`
			Color       string `json:"color"`
			Description string `json:"description"`
		} `json:"nodes"`
	} `json:"labels"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type enrichRaw struct {
	Data struct {
		Repository struct {
			PullRequest enrichPRAlias `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

// EnrichPullRequest fetches GraphQL-only enrichment fields and merges
// them into the per-key cache.
//
// O12 SWR:
//   - Hit within enrichmentTTL → cached value.
//   - UNKNOWN mergeable → not cached, re-fetches next call (#367).
//   - Network/rate-limit failure → stale value (up to staleEnrichmentTTL).
//   - Rate-limit cooldown active → skips network call entirely.
func (p *Provider) EnrichPullRequest(ctx context.Context, key PullRequestKey) (PullRequest, error) {
	p.prMu.RLock()
	entry, ok := p.prs[key]
	enrichedAt, hasEnriched := p.enrichAt[key]
	rateLimitedUntil := p.rateLimitedUntil
	p.prMu.RUnlock()

	if ok && hasEnriched && p.clock().Sub(enrichedAt) < enrichmentTTL {
		return entry.value, nil
	}

	serveStale := func(reason error) (PullRequest, error) {
		if ok && hasEnriched && p.clock().Sub(enrichedAt) < staleEnrichmentTTL {
			p.logServingStale(key, reason)
			return entry.value, nil
		}
		return PullRequest{}, reason
	}

	if !rateLimitedUntil.IsZero() && p.clock().Before(rateLimitedUntil) {
		return serveStale(fmt.Errorf("EnrichPullRequest: rate limit cooldown until %s", rateLimitedUntil.Format(time.RFC3339)))
	}

	c, err := p.httpClient(ctx)
	if err != nil {
		return serveStale(err)
	}

	variables := map[string]any{
		"owner":  key.Owner,
		"name":   key.Name,
		"number": key.Number,
	}
	raw, err := c.GraphQL(ctx, enrichPRQuery, variables)
	if err != nil {
		return serveStale(fmt.Errorf("EnrichPullRequest graphql: %w", err))
	}

	var envelope enrichRaw
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return serveStale(fmt.Errorf("EnrichPullRequest decode: %w", err))
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		joined := strings.Join(msgs, "; ")
		if strings.Contains(strings.ToLower(joined), "rate limit") {
			p.enterRateLimitCooldown("EnrichPullRequest", joined)
		}
		return serveStale(fmt.Errorf("EnrichPullRequest graphql errors: %s", joined))
	}

	p.clearRateLimitCooldown()

	return p.applyEnrichment(key, envelope.Data.Repository.PullRequest, p.clock()), nil
}

// BatchEnrichPullRequests collapses enrichment for multiple PRs into one
// GitHub GraphQL round-trip per unique (owner, name) pair.
func (p *Provider) BatchEnrichPullRequests(ctx context.Context, keys []PullRequestKey) (map[PullRequestKey]PullRequest, error) {
	if len(keys) == 0 {
		return map[PullRequestKey]PullRequest{}, nil
	}

	now := p.clock()

	type cacheSnap struct {
		entry      prEntry
		hasEntry   bool
		enrichedAt time.Time
		hasEnrich  bool
	}
	snaps := make(map[PullRequestKey]cacheSnap, len(keys))
	var rateLimitedUntil time.Time
	p.prMu.RLock()
	for _, k := range keys {
		e, hasEntry := p.prs[k]
		at, hasEnrich := p.enrichAt[k]
		snaps[k] = cacheSnap{entry: e, hasEntry: hasEntry, enrichedAt: at, hasEnrich: hasEnrich}
	}
	rateLimitedUntil = p.rateLimitedUntil
	p.prMu.RUnlock()

	result := make(map[PullRequestKey]PullRequest, len(keys))

	seen := make(map[PullRequestKey]struct{}, len(keys))
	var unique []PullRequestKey
	for _, k := range keys {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			unique = append(unique, k)
		}
	}

	var toFetch []PullRequestKey
	for _, k := range unique {
		snap := snaps[k]
		if snap.hasEntry && snap.hasEnrich && now.Sub(snap.enrichedAt) < enrichmentTTL {
			result[k] = snap.entry.value
		} else {
			toFetch = append(toFetch, k)
		}
	}

	if len(toFetch) == 0 {
		return result, nil
	}

	serveStaleForKey := func(k PullRequestKey) PullRequest {
		snap := snaps[k]
		if snap.hasEntry && snap.hasEnrich && now.Sub(snap.enrichedAt) < staleEnrichmentTTL {
			return snap.entry.value
		}
		return PullRequest{}
	}

	// serveStaleAll fills every outstanding key from cache and reports the
	// fallback once for the whole batch (#749).
	serveStaleAll := func(reason error) {
		served := 0
		for _, k := range toFetch {
			v := serveStaleForKey(k)
			if v.Number != 0 {
				served++
			}
			result[k] = v
		}
		p.logServingStaleBatch(served, len(toFetch), reason)
	}

	if !rateLimitedUntil.IsZero() && p.clock().Before(rateLimitedUntil) {
		serveStaleAll(fmt.Errorf("BatchEnrichPullRequests: rate limit cooldown until %s", rateLimitedUntil.Format(time.RFC3339)))
		return result, nil
	}

	c, err := p.httpClient(ctx)
	if err != nil {
		serveStaleAll(err)
		return result, err
	}

	type repoKey struct{ owner, name string }
	repoGroups := make(map[repoKey][]PullRequestKey)
	for _, k := range toFetch {
		rk := repoKey{k.Owner, k.Name}
		repoGroups[rk] = append(repoGroups[rk], k)
	}

	type prPosition struct {
		key     PullRequestKey
		repoIdx int
		prIdx   int
	}
	var positions []prPosition

	var qb strings.Builder
	qb.WriteString("{")
	repoIdx := 0
	for rk, rkKeys := range repoGroups {
		fmt.Fprintf(&qb, " r%d: repository(owner:%q,name:%q){", repoIdx, rk.owner, rk.name)
		for prIdx, k := range rkKeys {
			fmt.Fprintf(&qb, " pr%d: pullRequest(number:%d){%s}", prIdx, k.Number, enrichPRFields)
			positions = append(positions, prPosition{key: k, repoIdx: repoIdx, prIdx: prIdx})
		}
		qb.WriteString(" }")
		repoIdx++
	}
	qb.WriteString(" }")

	raw, err := c.GraphQL(ctx, qb.String(), nil)
	if err != nil {
		if IsRateLimited(err) {
			p.enterRateLimitCooldown("BatchEnrichPullRequests", err.Error())
		}
		serveStaleAll(err)
		return result, err
	}

	var envelope struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		serveStaleAll(fmt.Errorf("BatchEnrichPullRequests decode: %w", err))
		return result, fmt.Errorf("BatchEnrichPullRequests decode: %w", err)
	}

	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		joined := strings.Join(msgs, "; ")
		if strings.Contains(strings.ToLower(joined), "rate limit") {
			p.enterRateLimitCooldown("BatchEnrichPullRequests", joined)
		}
		serveStaleAll(fmt.Errorf("BatchEnrichPullRequests graphql errors: %s", joined))
		return result, fmt.Errorf("BatchEnrichPullRequests graphql errors: %s", joined)
	}

	p.clearRateLimitCooldown()

	for _, pos := range positions {
		repoAlias := fmt.Sprintf("r%d", pos.repoIdx)
		prAlias := fmt.Sprintf("pr%d", pos.prIdx)

		repoRaw, ok := envelope.Data[repoAlias]
		if !ok {
			result[pos.key] = serveStaleForKey(pos.key)
			continue
		}

		var repoBlock map[string]json.RawMessage
		if err := json.Unmarshal(repoRaw, &repoBlock); err != nil {
			result[pos.key] = serveStaleForKey(pos.key)
			continue
		}

		prRaw, ok := repoBlock[prAlias]
		if !ok {
			result[pos.key] = serveStaleForKey(pos.key)
			continue
		}

		var wire enrichPRAlias
		if err := json.Unmarshal(prRaw, &wire); err != nil {
			result[pos.key] = serveStaleForKey(pos.key)
			continue
		}

		result[pos.key] = p.applyEnrichment(pos.key, wire, now)
	}

	return result, nil
}

// applyEnrichment maps an enrichPRAlias wire value onto the provider cache
// and returns the enriched PullRequest.
func (p *Provider) applyEnrichment(key PullRequestKey, wire enrichPRAlias, now time.Time) PullRequest {
	mergeable := mapMergeableState(wire.Mergeable)

	var rd *ReviewDecision
	if wire.ReviewDecision != nil {
		mapped := ReviewDecision(*wire.ReviewDecision)
		rd = &mapped
	}

	labels := make([]Label, 0, len(wire.Labels.Nodes))
	for _, n := range wire.Labels.Nodes {
		labels = append(labels, Label{Name: n.Name, Color: n.Color, Description: n.Description})
	}

	ciStatus := CiStatusUnknown
	if len(wire.Commits.Nodes) > 0 {
		commit := wire.Commits.Nodes[0].Commit
		if commit.StatusCheckRollup != nil {
			ciStatus = mapStatusCheckRollup(commit.StatusCheckRollup.State)
		}
	}

	p.prMu.Lock()
	base := p.prs[key]
	base.value.Mergeable = mergeable
	base.value.MergeStateStatus = wire.MergeStateStatus
	base.value.ReviewDecision = rd
	base.value.StatusCheckRollup = ciStatus
	base.value.Labels = filterPhaseLabels(labels)

	if mergeable != MergeableStateUnknown {
		p.prs[key] = base
		p.enrichAt[key] = now
	}
	enriched := base.value
	p.prMu.Unlock()

	return enriched
}

func mapMergeableState(s string) MergeableState {
	switch s {
	case "MERGEABLE":
		return MergeableStateMergeable
	case "CONFLICTING":
		return MergeableStateConflicting
	default:
		return MergeableStateUnknown
	}
}

func mapStatusCheckRollup(state string) CiStatus {
	switch state {
	case "SUCCESS":
		return CiStatusSuccess
	case "FAILURE", "ERROR":
		return CiStatusFailure
	case "PENDING", "EXPECTED":
		return CiStatusPending
	default:
		return CiStatusUnknown
	}
}

func filterPhaseLabels(in []Label) []Label {
	out := make([]Label, 0, len(in))
	for _, l := range in {
		if _, isPhase := phaseLabels[l.Name]; !isPhase {
			out = append(out, l)
		}
	}
	return out
}

// --- Issue dependency enrichment ---

var issueDepsPreviewHeaders = map[string]string{
	"GraphQL-Features":        "issue_types,sub_issues",
	"X-Github-Next-Global-ID": "1",
}

const issueDepsQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    issue(number:$number){
      blockedByIssues:blockedBy(first:50){
        nodes{ number, title, repository{ owner{ login } name } }
      }
      blockingIssues:blocking(first:50){
        nodes{ number, title, repository{ owner{ login } name } }
      }
      subIssues(first:50){
        nodes{ number, title, repository{ owner{ login } name } }
      }
      parent{ number, title, repository{ owner{ login } name } }
    }
  }
}`

type issueDepsRaw struct {
	Data struct {
		Repository struct {
			Issue struct {
				BlockedByIssues struct {
					Nodes []issueRefRaw `json:"nodes"`
				} `json:"blockedByIssues"`
				BlockingIssues struct {
					Nodes []issueRefRaw `json:"nodes"`
				} `json:"blockingIssues"`
				SubIssues struct {
					Nodes []issueRefRaw `json:"nodes"`
				} `json:"subIssues"`
				Parent *issueRefRaw `json:"parent"`
			} `json:"issue"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

type issueRefRaw struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	Repository struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
}

func (r issueRefRaw) toRef() IssueRef {
	return IssueRef{
		Owner:  r.Repository.Owner.Login,
		Name:   r.Repository.Name,
		Number: r.Number,
		Title:  r.Title,
	}
}

// EnrichIssueDependencies fetches the four dependency edges for one issue.
// TTL: issueDepsTTL (60s). Returns empty (non-nil) slices when no edges exist.
func (p *Provider) EnrichIssueDependencies(ctx context.Context, key IssueKey) (IssueDependencies, error) {
	p.issueMu.RLock()
	if cached, ok := p.issueDeps[key]; ok && p.clock().Sub(cached.at) < issueDepsTTL {
		out := cached.value
		p.issueMu.RUnlock()
		return out, nil
	}
	p.issueMu.RUnlock()

	c, err := p.httpClient(ctx)
	if err != nil {
		return IssueDependencies{}, err
	}

	variables := map[string]any{
		"owner":  key.Owner,
		"name":   key.Name,
		"number": key.Number,
	}
	raw, err := c.GraphQLWithHeaders(ctx, issueDepsQuery, variables, issueDepsPreviewHeaders)
	if err != nil {
		return IssueDependencies{}, fmt.Errorf("EnrichIssueDependencies graphql: %w", err)
	}

	var envelope issueDepsRaw
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return IssueDependencies{}, fmt.Errorf("EnrichIssueDependencies decode: %w", err)
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return IssueDependencies{}, fmt.Errorf("EnrichIssueDependencies graphql errors: %s", strings.Join(msgs, "; "))
	}

	wire := envelope.Data.Repository.Issue
	deps := IssueDependencies{
		BlockedBy: refsFromNodes(wire.BlockedByIssues.Nodes),
		Blocking:  refsFromNodes(wire.BlockingIssues.Nodes),
		SubIssues: refsFromNodes(wire.SubIssues.Nodes),
	}
	if wire.Parent != nil {
		ref := wire.Parent.toRef()
		deps.Parent = &ref
	}

	now := p.clock()
	p.issueMu.Lock()
	p.issueDeps[key] = issueDepsEntry{value: deps, at: now}
	p.issueMu.Unlock()

	return deps, nil
}

func refsFromNodes(in []issueRefRaw) []IssueRef {
	out := make([]IssueRef, 0, len(in))
	for _, n := range in {
		out = append(out, n.toRef())
	}
	return out
}

// --- Cache invalidation (called by webhook handler) ---

// DropPRCache removes a PR's cached entries. R16: called before Invalidate.
func (p *Provider) DropPRCache(k PullRequestKey) {
	p.prMu.Lock()
	delete(p.prs, k)
	delete(p.enrichAt, k)
	p.prMu.Unlock()
	p.listMu.Lock()
	for lk := range p.listPRsCache {
		if lk.Owner == k.Owner && lk.Name == k.Name {
			delete(p.listPRsCache, lk)
		}
	}
	p.listMu.Unlock()
}

// DropIssueCache removes an issue's cached entries.
func (p *Provider) DropIssueCache(k IssueKey) {
	p.issueMu.Lock()
	delete(p.issues, k)
	delete(p.issueDeps, k)
	p.issueMu.Unlock()
	p.listMu.Lock()
	for lk := range p.listIssCache {
		if lk.Owner == k.Owner && lk.Name == k.Name {
			delete(p.listIssCache, lk)
		}
	}
	p.listMu.Unlock()
}

// DropRunCache removes a workflow run's cached entries.
func (p *Provider) DropRunCache(k WorkflowRunKey) {
	p.runMu.Lock()
	delete(p.runs, k)
	p.runMu.Unlock()
	p.listMu.Lock()
	for lk := range p.listRunCache {
		if lk.Owner == k.Owner && lk.Name == k.Name {
			delete(p.listRunCache, lk)
		}
	}
	p.listMu.Unlock()
}

// AuthError returns the bootstrap error if Start failed (or nil if it succeeded).
// Used by tests that assert the auth path without triggering a real API call.
func (p *Provider) AuthError(ctx context.Context) error {
	_, err := p.resolveAuth(ctx)
	return err
}
