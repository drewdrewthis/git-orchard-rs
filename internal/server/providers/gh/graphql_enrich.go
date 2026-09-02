// Package gh — GraphQL enrichment for PullRequest.
//
// This file adds EnrichPullRequest, which fetches the fields the GitHub REST
// list endpoint does not return: mergeable, mergeStateStatus, reviewDecision,
// statusCheckRollup, labels, headRefOid, reviews and reviewThreads. These
// require a dedicated GraphQL round-trip. The selection set and wire shapes
// live in graphql_enrich_query.go; the batched multi-PR path lives in
// graphql_enrich_batch.go. Both funnel through applyEnrichment here, so the
// wire→domain mapping exists exactly once.
//
// The result is merged back into the per-key prs cache so subsequent
// GetPullRequest calls return the enriched view.
//
// UNKNOWN mergeable on an open PR is never written to the cache so the next
// call always re-fetches. This avoids the #367 flap pattern where a transient
// UNKNOWN hardens into a stale cached value.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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

// EnrichPullRequest fetches the GraphQL-only enrichment fields for the given
// PR key, merges the result into the per-key prs cache, and returns the
// fully-enriched PullRequest.
//
// Cache behaviour:
//   - A hit within enrichmentTTL returns the cached enriched value.
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
			p.logServingStale(key, reason)
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
			joined := p.noteGraphQLErrors("EnrichPullRequest", envelope.Errors)
			return serveStale(fmt.Errorf("EnrichPullRequest graphql errors: %s", joined))
		}
	}

	if len(envelope.Errors) == 0 {
		// Successful fetch — clear the cooldown.
		p.clearRateLimitCooldown()
	}

	return p.applyEnrichment(key, envelope.Data.Repository.PullRequest, p.clock()), nil
}

// applyEnrichment maps an enrichPRAlias wire value onto the provider cache and
// returns the enriched PullRequest. Both enrichment paths land here, so the
// wire→domain projection and the cache-write rules exist in one place.
func (p *Provider) applyEnrichment(key PullRequestKey, wire enrichPRAlias, now time.Time) PullRequest {
	mergeable := mapMergeableState(wire.Mergeable)

	var rd *ReviewDecision
	if wire.ReviewDecision != nil {
		mapped := ReviewDecision(*wire.ReviewDecision)
		rd = &mapped
	}

	p.prMu.Lock()
	base := p.prs[key]
	base.value.Mergeable = mergeable
	base.value.MergeStateStatus = wire.MergeStateStatus
	base.value.ReviewDecision = rd
	base.value.StatusCheckRollup = mapRollupFromCommits(wire.Commits.Nodes)
	base.value.Labels = filterPhaseLabels(mapLabels(wire.Labels.Nodes))
	base.value.HeadRefOid = wire.HeadRefOid
	base.value.Reviews = mapReviews(wire.Reviews.Nodes)
	base.value.ReviewThreads = mapReviewThreads(wire.ReviewThreads.Nodes)

	wrote := false
	if shouldCacheEnrichment(mergeable, base.value.State) {
		p.prs[key] = base
		p.enrichAt[key] = now
		wrote = true
	}
	enriched := base.value
	p.prMu.Unlock()

	// R16/M7: broadcast only after the cache write is visible to readers,
	// so a subscriber re-fetching on this event sees the fresh value.
	if wrote {
		p.invalidate(prNodeID(key), "enrich", now)
	}

	return enriched
}

// noteGraphQLErrors joins the GraphQL error messages and, when they signal a
// rate limit, arms the cooldown via enterRateLimitCooldown (Warn logging +
// backoff). Returns the joined message so the caller can wrap it. site names
// the calling path so the two enrichment paths back off and log independently.
func (p *Provider) noteGraphQLErrors(site string, errs []struct {
	Message string `json:"message"`
}) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Message)
	}
	joined := strings.Join(msgs, "; ")
	if strings.Contains(strings.ToLower(joined), "rate limit") {
		p.enterRateLimitCooldown(site, joined)
	}
	return joined
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

// mapRollupFromCommits reads the aggregated CI state off the PR's head
// commit. The enrichment query selects `commits(last:1)`, so the single node
// (when present) is the head commit — the same commit headRefOid names.
func mapRollupFromCommits(nodes []enrichCommitNode) CiStatus {
	if len(nodes) == 0 {
		return CiStatusUnknown
	}
	rollup := nodes[0].Commit.StatusCheckRollup
	if rollup == nil {
		return CiStatusUnknown
	}
	return mapStatusCheckRollup(rollup.State)
}

// mapLabels projects the label connection onto the domain type.
func mapLabels(nodes []enrichLabelNode) []Label {
	out := make([]Label, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, Label{Name: n.Name, Color: n.Color, Description: n.Description})
	}
	return out
}

// mapReviews projects the reviews connection onto the domain type (#651).
func mapReviews(nodes []enrichReviewNode) []PullRequestReview {
	out := make([]PullRequestReview, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, PullRequestReview{
			GitHubID:    n.DatabaseID,
			AuthorLogin: n.Author.login(),
			State:       n.State,
			Body:        n.Body,
			SubmittedAt: n.SubmittedAt,
		})
	}
	return out
}

// mapReviewThreads projects the reviewThreads connection onto the domain
// type (#607). AuthorLogin comes from the thread's first comment (whoever
// opened it); LastUpdatedAt from its last (how fresh the conversation is).
func mapReviewThreads(nodes []enrichThreadNode) []ReviewThread {
	out := make([]ReviewThread, 0, len(nodes))
	for _, n := range nodes {
		t := ReviewThread{
			GitHubID:     n.ID,
			IsResolved:   n.IsResolved,
			IsOutdated:   n.IsOutdated,
			Path:         n.Path,
			CommentCount: n.Comments.TotalCount,
		}
		if first := n.Comments.Nodes; len(first) > 0 {
			t.AuthorLogin = first[0].Author.login()
			t.LastUpdatedAt = first[0].CreatedAt
		}
		if last := n.LatestComment.Nodes; len(last) > 0 {
			t.LastUpdatedAt = last[0].CreatedAt
		}
		out = append(out, t)
	}
	return out
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
