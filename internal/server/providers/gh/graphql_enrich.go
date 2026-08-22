// Package gh — GraphQL enrichment for PullRequest.
//
// This file adds EnrichPullRequest, which fetches the five fields that
// the GitHub REST list endpoint does not return: mergeable, mergeStateStatus,
// reviewDecision, statusCheckRollup, and labels. These require a dedicated
// GraphQL round-trip.
//
// The result is merged back into the per-key prs cache so subsequent
// GetPullRequest calls return the enriched view. Cache TTL is 60s
// (enrichmentTTL) — shorter than CacheTTL because mergeable and CI
// status change faster than basic PR metadata.
//
// UNKNOWN mergeable on an open PR is never written to the cache so the next call
// always re-fetches. This avoids the #367 flap pattern where a
// transient UNKNOWN hardens into a stale cached value.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// enrichPRFields is the shared field fragment for aliased batch enrichment
// queries. Each aliased PR block expands this.
const enrichPRFields = `mergeable mergeStateStatus reviewDecision labels(first:50){nodes{name color description}} commits(last:1){nodes{commit{statusCheckRollup{state}}}}`

// enrichPRAlias is the wire-level shape of a single pull request block
// inside an aliased batch response. Mirrors enrichRaw.Data.Repository.PullRequest.
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

// enrichmentTTL governs how often we re-fetch PR enrichment from GitHub's
// GraphQL API. PR state doesn't change second-to-second; longer TTL means
// fewer GraphQL calls and graceful behaviour under user-level rate limits
// (5000/hr shared across all gh CLI + scripts).
const enrichmentTTL = 5 * time.Minute

// staleEnrichmentTTL is how long we'll serve a stale enrichment when the
// network call fails (rate limit, network blip). Far longer than the
// freshness TTL — the user's choice is "slightly stale data" vs "broken
// sidebar", and slightly-stale always wins.
const staleEnrichmentTTL = 1 * time.Hour

// enrichPRQuery is the GitHub GraphQL query that fetches the five
// enrichment fields. The statusCheckRollup is read from the head
// commit rather than from a separate check-runs endpoint so we get
// the aggregated state in a single round-trip.
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

// PhaseLabels are orchard lifecycle tags that are excluded from
// PullRequest.Labels. Only user-assigned labels are surfaced.
// See ~/.claude/skills/gh-tag/ for the canonical list.
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

// enrichRaw is the wire-level shape of the GitHub GraphQL enrichment
// response. Kept package-private; callers see the typed PullRequest.
type enrichRaw struct {
	Data struct {
		Repository struct {
			PullRequest struct {
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
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

// prEnrichEntry holds a cached enriched PullRequest and the time it was
// last fetched. Stored in the per-key prs map alongside the base entry.
type prEnrichEntry struct {
	value PullRequest
	at    time.Time
}

// enrichedAt stores the enrichment timestamps separately from the base
// prEntry so we can apply the shorter enrichmentTTL without touching
// the CacheTTL-governed REST data. Protected by prMu.
//
// We reuse the existing prs map value for storage — the enrichment is
// merged into the PullRequest struct in place, and the enrichAt map
// tracks when that last happened.

// EnrichPullRequest fetches the GraphQL-only enrichment fields
// (mergeable, mergeStateStatus, reviewDecision, statusCheckRollup,
// labels) for the given PR key, merges the result into the per-key
// prs cache, and returns the fully-enriched PullRequest.
//
// Cache behaviour:
//   - A hit within enrichmentTTL (60s) returns the cached enriched value.
//   - UNKNOWN mergeable is never cached — the next call re-fetches so
//     the transient computing state does not stick (#367 contract).
//   - A miss fetches from GitHub GraphQL and caches (unless UNKNOWN).
func (p *Provider) EnrichPullRequest(ctx context.Context, key PullRequestKey) (PullRequest, error) {
	// --- cache check ---
	p.prMu.RLock()
	entry, ok := p.prs[key]
	enrichedAt, hasEnriched := p.enrichAt[key]
	rateLimitedUntil := p.rateLimitedUntil
	p.prMu.RUnlock()

	if ok && hasEnriched && p.clock().Sub(enrichedAt) < enrichmentTTL {
		return entry.value, nil
	}

	// serveStale returns the last-known-good enrichment when the network
	// call fails. Falls back to a hard error only when we have nothing
	// cached at all. Keeps the sidebar populated through rate-limit
	// windows + transient network blips.
	serveStale := func(reason error) (PullRequest, error) {
		if ok && hasEnriched && p.clock().Sub(enrichedAt) < staleEnrichmentTTL {
			return entry.value, nil
		}
		return PullRequest{}, reason
	}

	// Rate-limit cooldown: if we're inside the cooldown window, skip the
	// network call entirely. Saves us from hammering GitHub when we
	// already know it'll refuse. Serves stale when we have it.
	if !rateLimitedUntil.IsZero() && p.clock().Before(rateLimitedUntil) {
		return serveStale(fmt.Errorf("EnrichPullRequest: rate limit cooldown until %s", rateLimitedUntil.Format(time.RFC3339)))
	}

	// --- fetch ---
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
		// GraphQL partial success: errors[] alongside populated data means
		// some leaves failed while others resolved. The populated fields are
		// real -- discarding them blanks every PR over one failed leaf.
		// Only a response with no usable data is a total failure.
		var dataProbe struct {
			Data json.RawMessage `json:"data"`
		}
		hasData := json.Unmarshal(raw, &dataProbe) == nil && hasRawData(dataProbe.Data)
		if !hasData {
			msgs := make([]string, 0, len(envelope.Errors))
			for _, e := range envelope.Errors {
				msgs = append(msgs, e.Message)
			}
			joined := strings.Join(msgs, "; ")
			// Detect rate-limit and set a cooldown so we stop hammering. GitHub
			// resets the limit hourly; default to a 5-minute cooldown which is
			// short enough to recover quickly if the user invoked a one-off
			// burst, long enough to avoid waste if we're truly throttled.
			if strings.Contains(strings.ToLower(joined), "rate limit") {
				p.prMu.Lock()
				p.rateLimitedUntil = p.clock().Add(5 * time.Minute)
				p.prMu.Unlock()
			}
			return serveStale(fmt.Errorf("EnrichPullRequest graphql errors: %s", joined))
		}
	}

	if len(envelope.Errors) == 0 {
		// Successful fetch — clear the cooldown.
		p.prMu.Lock()
		p.rateLimitedUntil = time.Time{}
		p.prMu.Unlock()
	}

	wire := envelope.Data.Repository.PullRequest

	// --- map fields ---
	mergeable := mapMergeableState(wire.Mergeable)

	var rd *ReviewDecision
	if wire.ReviewDecision != nil {
		mapped := ReviewDecision(*wire.ReviewDecision)
		rd = &mapped
	}

	labels := make([]Label, 0, len(wire.Labels.Nodes))
	for _, n := range wire.Labels.Nodes {
		labels = append(labels, Label{
			Name:        n.Name,
			Color:       n.Color,
			Description: n.Description,
		})
	}

	ciStatus := CiStatusUnknown
	if len(wire.Commits.Nodes) > 0 {
		commit := wire.Commits.Nodes[0].Commit
		if commit.StatusCheckRollup != nil {
			ciStatus = mapStatusCheckRollup(commit.StatusCheckRollup.State)
		}
	}

	// --- merge into existing base entry ---
	p.prMu.Lock()
	base := p.prs[key]
	base.value.Mergeable = mergeable
	base.value.MergeStateStatus = wire.MergeStateStatus
	base.value.ReviewDecision = rd
	base.value.StatusCheckRollup = ciStatus
	base.value.Labels = filterPhaseLabels(labels)

	wrote := false
	if shouldCacheEnrichment(mergeable, base.value.State) {
		p.prs[key] = base
		p.enrichAt[key] = p.clock()
		wrote = true
	}
	enriched := base.value
	p.prMu.Unlock()

	// R16/M7: broadcast only after the cache write is visible to readers,
	// so a subscriber re-fetching on this event sees the fresh value.
	if wrote {
		p.invalidate(prNodeID(key), "enrich", p.clock())
	}

	return enriched, nil
}

// hasRawData reports whether a GraphQL envelope carried a usable data
// payload: absent and explicit null both mean nothing resolved.
func hasRawData(d json.RawMessage) bool {
	s := strings.TrimSpace(string(d))
	return s != "" && s != "null"
}

// prNodeID formats the invalidation key subscription resolvers match on.
// The webhook handler must produce byte-identical keys, so both go through
// this helper.
func prNodeID(k PullRequestKey) string {
	return fmt.Sprintf("PullRequest:%s/%s#%d", k.Owner, k.Name, k.Number)
}

// shouldCacheEnrichment reports whether an enrichment result may be written
// to the per-key cache.
//
// UNKNOWN is not a verdict, it is an uncomputed value. GitHub computes
// mergeable lazily, so while a pull request is still OPEN the next call must
// re-fetch rather than harden a placeholder into a cached answer (#367
// contract).
//
// Once a PR is MERGED or CLOSED, GitHub stops computing mergeable altogether,
// so UNKNOWN there is terminal rather than transient. Treating it as a cache
// miss kept those keys in every enrichment batch forever; because the batch
// query flattens all repos and PRs into a single GraphQL document, one such
// key forced a real GitHub round-trip for the entire view on every cycle.
//
// State is populated from the REST list payload, so this costs no extra API
// call. When a key has not been seen by a list refresh yet, State is empty
// and the conservative OPEN-PR rule applies.
func shouldCacheEnrichment(mergeable MergeableState, state PullRequestState) bool {
	if mergeable != MergeableStateUnknown {
		return true
	}
	return state == PullRequestStateMerged || state == PullRequestStateClosed
}

// mapMergeableState maps the raw GitHub string to the typed enum.
// Anything unrecognised maps to UNKNOWN, which is the safe fallback.
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

// mapStatusCheckRollup maps GitHub's CommitStatusState / CheckStatusState
// to our CiStatus enum.
//
// Mapping rules (per issue #442 spec):
//   - any FAILURE or ERROR → FAILURE
//   - any PENDING or EXPECTED → PENDING
//   - SUCCESS → SUCCESS
//   - empty / nil / unknown → UNKNOWN
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

// filterPhaseLabels returns the input slice with orchard phase labels
// removed, preserving the relative order of the remaining labels.
func filterPhaseLabels(in []Label) []Label {
	out := make([]Label, 0, len(in))
	for _, l := range in {
		if _, isPhase := phaseLabels[l.Name]; !isPhase {
			out = append(out, l)
		}
	}
	return out
}

// BatchEnrichPullRequests fetches enrichment fields for multiple PRs,
// collapsing all PRs from the same repository into a single GitHub GraphQL
// HTTP request using query aliases. One HTTP call per unique (owner, name)
// pair is fired regardless of how many PRs are requested.
//
// Cache semantics (same as EnrichPullRequest, applied per-key):
//   - Keys fresh within enrichmentTTL are served from cache, no network call.
//   - UNKNOWN mergeable results are not cached — the next batch re-fetches.
//   - When the rate-limit cooldown is active, stale values are returned for
//     all keys that have a cached enrichment; keys with no cache get an error.
//   - On a rate-limit error response, the cooldown is set and stale is served.
//
// The returned map contains an entry for every key in keys. Errors per key
// are embedded in the returned error only when the entire batch fails; per-PR
// parse failures result in a zero PullRequest value for that key.
func (p *Provider) BatchEnrichPullRequests(ctx context.Context, keys []PullRequestKey) (map[PullRequestKey]PullRequest, error) {
	if len(keys) == 0 {
		return map[PullRequestKey]PullRequest{}, nil
	}

	now := p.clock()

	// Snapshot cache state and rate-limit once under read lock.
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

	// Deduplicate keys.
	seen := make(map[PullRequestKey]struct{}, len(keys))
	var unique []PullRequestKey
	for _, k := range keys {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			unique = append(unique, k)
		}
	}

	// Separate keys into: fresh (serve from cache), and stale/missing (need fetch).
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

	// serveStaleForKey returns cached value or zero, matching EnrichPullRequest.
	serveStaleForKey := func(k PullRequestKey) PullRequest {
		snap := snaps[k]
		if snap.hasEntry && snap.hasEnrich && now.Sub(snap.enrichedAt) < staleEnrichmentTTL {
			return snap.entry.value
		}
		return PullRequest{}
	}

	// Rate-limit cooldown: skip network, serve stale for all keys.
	if !rateLimitedUntil.IsZero() && p.clock().Before(rateLimitedUntil) {
		for _, k := range toFetch {
			result[k] = serveStaleForKey(k)
		}
		return result, nil
	}

	c, err := p.httpClient(ctx)
	if err != nil {
		for _, k := range toFetch {
			result[k] = serveStaleForKey(k)
		}
		return result, err
	}

	// Group keys by (owner, name) repo for aliased batch queries.
	type repoKey struct{ owner, name string }
	repoGroups := make(map[repoKey][]PullRequestKey)
	for _, k := range toFetch {
		rk := repoKey{k.Owner, k.Name}
		repoGroups[rk] = append(repoGroups[rk], k)
	}

	// Fire one aliased GraphQL query per repo group.
	// Alias scheme: r<repoIdx>: repository(...) { pr<prIdx>: pullRequest(...) { ... } }
	// We flatten all repos into one query document so a single HTTP call covers
	// everything. GitHub supports multiple top-level aliases in one query.
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
		// Rate-limit HTTP error: set cooldown, serve stale.
		if IsRateLimited(err) {
			p.prMu.Lock()
			p.rateLimitedUntil = p.clock().Add(5 * time.Minute)
			p.prMu.Unlock()
		}
		for _, k := range toFetch {
			result[k] = serveStaleForKey(k)
		}
		return result, err
	}

	// Parse the aliased response envelope.
	// Shape: { "data": { "r0": { "pr0": {...}, "pr1": {...} }, "r1": { ... } }, "errors": [...] }
	var envelope struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		for _, k := range toFetch {
			result[k] = serveStaleForKey(k)
		}
		return result, fmt.Errorf("BatchEnrichPullRequests decode: %w", err)
	}

	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		joined := strings.Join(msgs, "; ")
		if strings.Contains(strings.ToLower(joined), "rate limit") {
			p.prMu.Lock()
			p.rateLimitedUntil = p.clock().Add(5 * time.Minute)
			p.prMu.Unlock()
		}
		// Partial success: aliases that resolved are real; only a response
		// with no usable data discards them all.
		if len(envelope.Data) == 0 {
			for _, k := range toFetch {
				result[k] = serveStaleForKey(k)
			}
			return result, fmt.Errorf("BatchEnrichPullRequests graphql errors: %s", joined)
		}
	}

	if len(envelope.Errors) == 0 {
		// Successful response — clear the cooldown.
		p.prMu.Lock()
		p.rateLimitedUntil = time.Time{}
		p.prMu.Unlock()
	}

	// For each aliased repo block, decode each aliased PR block.
	// Then each repo block is itself a map[string]json.RawMessage.
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

// applyEnrichment maps an enrichPRAlias wire value onto the provider cache and
// returns the enriched PullRequest. Called by BatchEnrichPullRequests after a
// successful GraphQL response. Mirrors EnrichPullRequest's cache-write logic.
func (p *Provider) applyEnrichment(key PullRequestKey, wire enrichPRAlias, now time.Time) PullRequest {
	mergeable := mapMergeableState(wire.Mergeable)

	var rd *ReviewDecision
	if wire.ReviewDecision != nil {
		mapped := ReviewDecision(*wire.ReviewDecision)
		rd = &mapped
	}

	labels := make([]Label, 0, len(wire.Labels.Nodes))
	for _, n := range wire.Labels.Nodes {
		labels = append(labels, Label{
			Name:        n.Name,
			Color:       n.Color,
			Description: n.Description,
		})
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

	var emitted []PullRequestKey
	if shouldCacheEnrichment(mergeable, base.value.State) {
		p.prs[key] = base
		p.enrichAt[key] = now
		emitted = append(emitted, key)
	}
	enriched := base.value
	p.prMu.Unlock()

	for _, k := range emitted {
		p.invalidate(prNodeID(k), "enrich", now)
	}

	return enriched
}
