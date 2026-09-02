// Package gh — batched PullRequest enrichment.
//
// One HTTP request covers every PR the caller asks for: PRs are grouped by
// repository, each group becomes an aliased `repository(...)` block, and each
// PR inside it an aliased `pullRequest(...)` block expanding enrichPRFields.
// The DataLoader in internal/server/loaders funnels a whole GraphQL operation
// through this, which is what keeps the review surface off an N+1.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// BatchEnrichPullRequests fetches enrichment fields for multiple PRs,
// collapsing all PRs from the same repository into a single GitHub GraphQL
// HTTP request using query aliases. One HTTP call is fired regardless of how
// many PRs are requested.
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
	staleAll := func(err error) (map[PullRequestKey]PullRequest, error) {
		served := 0
		for _, k := range toFetch {
			v := serveStaleForKey(k)
			if v.Number != 0 {
				served++
			}
			result[k] = v
		}
		p.logServingStaleBatch(served, len(toFetch), err)
		return result, err
	}

	// Rate-limit cooldown: skip network, serve stale for all keys.
	if !rateLimitedUntil.IsZero() && p.clock().Before(rateLimitedUntil) {
		return staleAll(nil)
	}

	c, err := p.httpClient(ctx)
	if err != nil {
		return staleAll(err)
	}

	query, positions := buildBatchEnrichQuery(toFetch)

	raw, err := c.GraphQL(ctx, query, nil)
	if err != nil {
		// Rate-limit HTTP error: set cooldown, serve stale.
		if IsRateLimited(err) {
			p.enterRateLimitCooldown("BatchEnrichPullRequests", err.Error())
		}
		return staleAll(err)
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
		return staleAll(fmt.Errorf("BatchEnrichPullRequests decode: %w", err))
	}

	if len(envelope.Errors) > 0 {
		joined := p.noteGraphQLErrors("BatchEnrichPullRequests", envelope.Errors)
		// Partial success: aliases that resolved are real; only a response
		// with no usable data discards them all.
		if len(envelope.Data) == 0 {
			return staleAll(fmt.Errorf("BatchEnrichPullRequests graphql errors: %s", joined))
		}
	}

	if len(envelope.Errors) == 0 {
		// Successful response — clear the cooldown.
		p.clearRateLimitCooldown()
	}

	// For each aliased repo block, decode each aliased PR block.
	for _, pos := range positions {
		wire, ok := decodeAliasedPR(envelope.Data, pos)
		if !ok {
			result[pos.key] = serveStaleForKey(pos.key)
			continue
		}
		result[pos.key] = p.applyEnrichment(pos.key, wire, now)
	}

	return result, nil
}

// prPosition records where a PR key's block sits in the aliased document.
type prPosition struct {
	key     PullRequestKey
	repoIdx int
	prIdx   int
}

// buildBatchEnrichQuery flattens keys into one GraphQL document with a
// `r<n>` alias per repository and a `pr<n>` alias per PR inside it. GitHub
// supports multiple top-level aliases in one query, so the whole view costs
// a single HTTP call.
func buildBatchEnrichQuery(keys []PullRequestKey) (string, []prPosition) {
	type repoKey struct{ owner, name string }
	repoGroups := make(map[repoKey][]PullRequestKey)
	var repoOrder []repoKey
	for _, k := range keys {
		rk := repoKey{k.Owner, k.Name}
		if _, seen := repoGroups[rk]; !seen {
			repoOrder = append(repoOrder, rk)
		}
		repoGroups[rk] = append(repoGroups[rk], k)
	}

	positions := make([]prPosition, 0, len(keys))
	var qb strings.Builder
	qb.WriteString("{")
	for repoIdx, rk := range repoOrder {
		fmt.Fprintf(&qb, " r%d: repository(owner:%q,name:%q){", repoIdx, rk.owner, rk.name)
		for prIdx, k := range repoGroups[rk] {
			fmt.Fprintf(&qb, " pr%d: pullRequest(number:%d){%s}", prIdx, k.Number, enrichPRFields)
			positions = append(positions, prPosition{key: k, repoIdx: repoIdx, prIdx: prIdx})
		}
		qb.WriteString(" }")
	}
	qb.WriteString(" }")
	return qb.String(), positions
}

// decodeAliasedPR pulls one PR block out of the aliased response. Returns
// ok=false when the alias is missing or undecodable, which the caller treats
// as "serve stale for this key" rather than failing the whole batch.
func decodeAliasedPR(data map[string]json.RawMessage, pos prPosition) (enrichPRAlias, bool) {
	repoRaw, ok := data[fmt.Sprintf("r%d", pos.repoIdx)]
	if !ok {
		return enrichPRAlias{}, false
	}
	var repoBlock map[string]json.RawMessage
	if err := json.Unmarshal(repoRaw, &repoBlock); err != nil {
		return enrichPRAlias{}, false
	}
	prRaw, ok := repoBlock[fmt.Sprintf("pr%d", pos.prIdx)]
	if !ok {
		return enrichPRAlias{}, false
	}
	var wire enrichPRAlias
	if err := json.Unmarshal(prRaw, &wire); err != nil {
		return enrichPRAlias{}, false
	}
	return wire, true
}
