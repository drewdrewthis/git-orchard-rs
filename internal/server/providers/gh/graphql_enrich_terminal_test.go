package gh_test

// Regression tests for the enrichment cache-write condition.
//
// Background: applyEnrichment refused to cache any PR whose mergeable was
// UNKNOWN. That rule exists to honour the #367 contract — GitHub computes
// mergeable lazily, so a transient UNKNOWN must not harden into a stale
// cached answer (see TestEnrichPullRequest_UnknownMergeableNotCachedAsDefinitive).
//
// The rule is correct only while the PR is still open. GitHub stops
// computing mergeable once a PR is MERGED or CLOSED, so UNKNOWN there is
// terminal, not transient. Refusing to cache those keys leaves them
// permanently in every enrichment batch; because the batch query flattens
// all repos and PRs into one document, a single such key forces a real
// GitHub round-trip for every PR in the view, on every cycle.
//
// No PII fixtures: repos are `alice/repo`.

import (
	"context"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
)

// TestEnrichPullRequest_TerminalStateCachesDespiteUnknownMergeable pins BOTH
// directions of the distinction: terminal-UNKNOWN must cache, transient-UNKNOWN
// must not. Asserting only one side would let a fix that simply deletes the
// mergeable check pass while regressing #367.
func TestEnrichPullRequest_TerminalStateCachesDespiteUnknownMergeable(t *testing.T) {
	cases := []struct {
		name      string
		state     gh.PullRequestState
		mergeable string
		wantCache bool
		why       string
	}{
		{
			name:      "merged PR with UNKNOWN mergeable caches",
			state:     gh.PullRequestStateMerged,
			mergeable: "UNKNOWN",
			wantCache: true,
			why:       "GitHub never recomputes mergeable for a merged PR; UNKNOWN is terminal",
		},
		{
			name:      "closed PR with UNKNOWN mergeable caches",
			state:     gh.PullRequestStateClosed,
			mergeable: "UNKNOWN",
			wantCache: true,
			why:       "no recompute for closed PRs either; UNKNOWN is terminal",
		},
		{
			name:      "open PR with UNKNOWN mergeable does NOT cache",
			state:     gh.PullRequestStateOpen,
			mergeable: "UNKNOWN",
			wantCache: false,
			why:       "#367 contract: transient computing state must not stick",
		},
		{
			name:      "open PR with definitive mergeable caches",
			state:     gh.PullRequestStateOpen,
			mergeable: "MERGEABLE",
			wantCache: true,
			why:       "definitive answers have always been cacheable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			clock := &fakeClock{t: now}

			body := enrichResponse(tc.mergeable, "UNKNOWN", nil, "")
			p, count := newEnrichProvider(t, body, clock.Now)

			key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 42}

			// Seed the lifecycle state exactly as a REST list refresh would.
			p.ExportSeedPRState(key, tc.state)

			if _, err := p.EnrichPullRequest(context.Background(), key); err != nil {
				t.Fatalf("first enrich: %v", err)
			}
			if c := count.Load(); c != 1 {
				t.Fatalf("expected 1 HTTP call after first enrich, got %d", c)
			}

			// Direct invariant: did the enrichment timestamp get written?
			ts := p.ExportEnrichTimestamp(key)
			switch {
			case tc.wantCache && ts.IsZero():
				t.Errorf("enrichAt[key] is zero, want a written timestamp — %s", tc.why)
			case !tc.wantCache && !ts.IsZero():
				t.Errorf("enrichAt[key] = %v, want zero time — %s", ts, tc.why)
			}

			// Behavioural check: a second call inside the TTL window must
			// reuse the cache exactly when we expect it to. Clock is NOT
			// advanced, so any re-fetch is a cache miss, not a TTL expiry.
			if _, err := p.EnrichPullRequest(context.Background(), key); err != nil {
				t.Fatalf("second enrich: %v", err)
			}
			wantCalls := int32(2)
			if tc.wantCache {
				wantCalls = 1
			}
			if c := count.Load(); c != wantCalls {
				t.Errorf("HTTP calls = %d, want %d — %s", c, wantCalls, tc.why)
			}
		})
	}
}
