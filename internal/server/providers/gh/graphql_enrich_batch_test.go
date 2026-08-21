package gh_test

// Batch-level regression test for the enrichment drain.
//
// BatchEnrichPullRequests short-circuits with zero HTTP calls only when every
// requested key is already cached (`len(toFetch) == 0`). Because the batch
// query flattens all repos and PRs into a single GraphQL document, one
// permanently-uncacheable key forces a real GitHub round-trip covering the
// whole view, every cycle. Merged PRs are permanently UNKNOWN, so before the
// fix the short-circuit could never fire for any view containing one.
//
// No PII fixtures: repos are `alice/repo`.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
)

// batchEnrichResponse builds an aliased batch payload (data.r0.pr0..prN-1)
// where every aliased PR block carries the same mergeable value. Keeping the
// blocks identical makes the fixture independent of the order in which the
// provider assigns alias indices to keys.
func batchEnrichResponse(n int, mergeable string) []byte {
	prs := map[string]any{}
	for i := 0; i < n; i++ {
		prs[fmt.Sprintf("pr%d", i)] = map[string]any{
			"mergeable":        mergeable,
			"mergeStateStatus": "UNKNOWN",
			"reviewDecision":   nil,
			"labels":           map[string]any{"nodes": []any{}},
			"commits": map[string]any{
				"nodes": []any{
					map[string]any{"commit": map[string]any{"statusCheckRollup": nil}},
				},
			},
		}
	}
	out, _ := json.Marshal(map[string]any{"data": map[string]any{"r0": prs}})
	return out
}

func TestBatchEnrichPullRequests_TerminalKeysAllowBatchShortCircuit(t *testing.T) {
	cases := []struct {
		name string
		// states seeded for keys #1..#3, in order
		states       []gh.PullRequestState
		wantCalls2nd int32
		why          string
	}{
		{
			name: "all keys terminal: second batch short-circuits",
			states: []gh.PullRequestState{
				gh.PullRequestStateMerged,
				gh.PullRequestStateMerged,
				gh.PullRequestStateClosed,
			},
			wantCalls2nd: 1,
			why:          "every key cacheable => len(toFetch)==0 => no GitHub round-trip",
		},
		{
			name: "one open key still forces a fetch for the batch",
			states: []gh.PullRequestState{
				gh.PullRequestStateMerged,
				gh.PullRequestStateOpen,
				gh.PullRequestStateMerged,
			},
			wantCalls2nd: 2,
			why:          "#367 contract still holds: an open UNKNOWN key must re-fetch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			clock := &fakeClock{t: now}

			// Server always answers UNKNOWN — the state seeded below is what
			// distinguishes terminal from transient.
			p, count := newEnrichProvider(t, batchEnrichResponse(3, "UNKNOWN"), clock.Now)

			keys := make([]gh.PullRequestKey, 0, 3)
			for i, st := range tc.states {
				k := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: i + 1}
				keys = append(keys, k)
				p.ExportSeedPRState(k, st)
			}

			if _, err := p.BatchEnrichPullRequests(context.Background(), keys); err != nil {
				t.Fatalf("first batch enrich: %v", err)
			}
			if c := count.Load(); c != 1 {
				t.Fatalf("expected 1 HTTP call after first batch, got %d", c)
			}

			// Second batch inside the TTL window. The clock is NOT advanced,
			// so any additional call is a cache miss rather than TTL expiry.
			if _, err := p.BatchEnrichPullRequests(context.Background(), keys); err != nil {
				t.Fatalf("second batch enrich: %v", err)
			}
			if c := count.Load(); c != tc.wantCalls2nd {
				t.Errorf("HTTP calls after second batch = %d, want %d — %s",
					c, tc.wantCalls2nd, tc.why)
			}
		})
	}
}
