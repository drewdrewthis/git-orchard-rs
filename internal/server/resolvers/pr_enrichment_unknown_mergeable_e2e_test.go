package resolvers_test

// Regression test for #813: shouldCacheEnrichment (internal/server/providers/gh/graphql_enrich.go)
// declines to cache enrichment for OPEN PRs whose `mergeable` is UNKNOWN — GitHub computes
// mergeable lazily, so the cache correctly refuses to harden a placeholder. But the request-scoped
// dataloader (internal/server/loaders/loaders.go) uses dataloader.NoCache, so once primeEnrichment
// (internal/server/resolvers/helpers.go) has already batched every PR into one GraphQL round-trip,
// the individual field resolvers for open+UNKNOWN PRs miss that cache and trigger a second, hidden
// GraphQL round-trip for the same batch — an N+1 within a single query.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// stubUnknownMergeableReviewAPI is stubReviewAPI's twin, except every fixture PR carries
// `mergeable: "UNKNOWN"` instead of the base fixture's `"MERGEABLE"` — the #813 trigger condition.
func stubUnknownMergeableReviewAPI(t *testing.T, n int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	fixture := reviewFixture(t)
	fixture["mergeable"] = "UNKNOWN"
	var graphqlCalls atomic.Int32

	prs := map[string]any{}
	for i := 0; i < n; i++ {
		prs[fmt.Sprintf("pr%d", i)] = fixture
	}
	body, err := json.Marshal(map[string]any{"data": map[string]any{"r0": prs}})
	if err != nil {
		t.Fatalf("marshal graphql fixture: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/alice/repo/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, reviewPullsREST(n))
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		graphqlCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
		http.NotFound(w, r)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &graphqlCalls
}

// TestPRReviewSurface_NoNPlusOne_UnknownMergeable asserts the same no-N+1 guarantee as
// TestPRReviewSurface_NoNPlusOne holds when the PRs are open with `mergeable: UNKNOWN` — the
// exact combination shouldCacheEnrichment refuses to cache (correctly, per the #367 contract),
// which must not leak into a second GitHub round-trip inside the same request (#813).
func TestPRReviewSurface_NoNPlusOne_UnknownMergeable(t *testing.T) {
	api, graphqlCalls := stubUnknownMergeableReviewAPI(t, 3)
	srv := newReviewDaemon(t, newReviewProvider(t, api))

	resp := postEnrichQuery(t, srv.URL, `query {
		pullRequests(repo: "alice/repo", state: OPEN) {
			number
			headRefOid
			unresolvedThreadCount
			reviews { authorLogin }
			reviewThreads { path }
		}
	}`)
	if errs, ok := resp["errors"]; ok {
		t.Fatalf("graphql errors: %v", errs)
	}
	data, _ := resp["data"].(map[string]any)
	prs, _ := data["pullRequests"].([]any)
	if len(prs) != 3 {
		t.Fatalf("expected 3 PRs, got %d", len(prs))
	}
	for i, raw := range prs {
		pr, _ := raw.(map[string]any)
		if got := pr["headRefOid"]; got != "d3adb33fd3adb33fd3adb33fd3adb33fd3adb33f" {
			t.Errorf("pr[%d].headRefOid = %v, want d3adb33f...", i, got)
		}
		if got := pr["unresolvedThreadCount"]; got != float64(3) {
			t.Errorf("pr[%d].unresolvedThreadCount = %v, want 3", i, got)
		}
		reviews, ok := pr["reviews"].([]any)
		if !ok || len(reviews) != 2 {
			t.Errorf("pr[%d].reviews = %v, want 2 entries", i, pr["reviews"])
		}
		threads, ok := pr["reviewThreads"].([]any)
		if !ok || len(threads) != 4 {
			t.Errorf("pr[%d].reviewThreads = %v, want 4 entries", i, pr["reviewThreads"])
		}
	}
	if got := graphqlCalls.Load(); got != 1 {
		t.Errorf("GitHub GraphQL calls = %d for 3 open UNKNOWN-mergeable PRs × 5 fields, want 1 (no N+1, #813)", got)
	}
}
