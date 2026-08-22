package gh_test

// Regression tests for enrichment survival across a REST list refresh.
//
// ListPullRequests overwrote each per-key cache entry with the REST-only
// PullRequest, which zeroes the GraphQL-derived enrichment fields
// (mergeable, mergeStateStatus, reviewDecision, statusCheckRollup, labels).
// Dropping enrichAt was therefore *required* for correctness — otherwise the
// cache would claim fresh enrichment while serving zero values.
//
// The cost: ListPullRequests refreshes on a 2-minute CacheTTL, so the
// 5-minute enrichmentTTL was capped at 2 minutes and every list refresh
// forced a fresh GraphQL round-trip for every PR in the view.
//
// The fix preserves the enrichment fields across the refresh, so enrichAt
// only has to be dropped when the PR has actually changed in a way that
// invalidates enrichment computed against the older revision.
//
// No PII fixtures: repos are `alice/repo`, users are `bob`.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
)

// restPull builds one element of the `/repos/{o}/{n}/pulls` payload.
func restPull(number int, state, mergedAt, updatedAt string) map[string]any {
	return map[string]any{
		"number":     number,
		"title":      "a pull request",
		"body":       "",
		"state":      state,
		"draft":      false,
		"html_url":   fmt.Sprintf("https://example.invalid/alice/repo/pull/%d", number),
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": updatedAt,
		"merged_at":  mergedAt,
		"user":       map[string]any{"login": "bob"},
		"base":       map[string]any{"ref": "main"},
		"head":       map[string]any{"ref": "feature"},
	}
}

// newListProvider serves both the REST pulls list and the GraphQL endpoint.
// listBody is read through a pointer so a test can change what the second
// refresh returns. graphqlCount counts enrichment round-trips.
func newListProvider(t *testing.T, listBody *[]byte, graphqlBody []byte, clock func() time.Time) (*gh.Provider, *atomic.Int32) {
	t.Helper()
	var graphqlCount atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/alice/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(*listBody)
	})
	mux.HandleFunc("/repos/alice/repo/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		var items []json.RawMessage
		if err := json.Unmarshal(*listBody, &items); err != nil || len(items) == 0 {
			t.Errorf("single-PR handler: bad list fixture: %v", err)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(items[0])
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		graphqlCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(graphqlBody)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
		http.NotFound(w, r)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	auth := &gh.StaticAuthSource{TokenValue: "test-token-fixture"}
	p := gh.NewWith(nil, srv.URL, auth, clock)
	if err := p.Start(context.Background()); err != nil {
		t.Logf("provider start (non-fatal): %v", err)
	}
	gh.SetHTTPClientForTest(p, srv.Client())
	return p, &graphqlCount
}

// TestListPullRequests_RefreshPreservesUnchangedEnrichment pins both
// directions: an unchanged PR must keep its enrichment across a list
// refresh, and a materially changed PR must still discard it.
func TestListPullRequests_RefreshPreservesUnchangedEnrichment(t *testing.T) {
	const t1 = "2026-01-01T10:00:00Z"
	const t2 = "2026-01-01T11:30:00Z" // later — PR was touched

	cases := []struct {
		name string
		// how the PR looks on the SECOND list refresh
		state     string
		mergedAt  string
		updatedAt string
		wantKept  bool
		why       string
	}{
		{
			name:      "unchanged PR keeps enrichment across refresh",
			state:     "open",
			mergedAt:  "",
			updatedAt: t1,
			wantKept:  true,
			why:       "nothing REST-visible changed; enrichment is still valid for this revision",
		},
		{
			name:      "updated_at moved: enrichment discarded",
			state:     "open",
			mergedAt:  "",
			updatedAt: t2,
			wantKept:  false,
			why:       "the PR was touched (e.g. new commit); enrichment may be stale against it",
		},
		{
			name:      "state transition open->merged: enrichment discarded",
			state:     "closed",
			mergedAt:  "2026-01-01T11:00:00Z",
			updatedAt: t2,
			wantKept:  false,
			why:       "lifecycle transition invalidates enrichment computed while open",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			clock := &fakeClock{t: now}

			first, _ := json.Marshal([]any{restPull(42, "open", "", t1)})
			listBody := first

			graphql := enrichResponse("MERGEABLE", "CLEAN", nil, "SUCCESS", "bug")
			p, graphqlCount := newListProvider(t, &listBody, graphql, clock.Now)

			key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 42}

			// Populate the list cache, then enrich once.
			if _, err := p.ListPullRequests(context.Background(), "alice", "repo", gh.PullRequestStateOpen); err != nil {
				t.Fatalf("first list: %v", err)
			}
			pr, err := p.EnrichPullRequest(context.Background(), key)
			if err != nil {
				t.Fatalf("enrich: %v", err)
			}
			if pr.Mergeable != gh.MergeableStateMergeable {
				t.Fatalf("setup: mergeable = %q, want MERGEABLE", pr.Mergeable)
			}
			if c := graphqlCount.Load(); c != 1 {
				t.Fatalf("setup: expected 1 GraphQL call, got %d", c)
			}

			// Advance past the 2min list CacheTTL but stay inside the 5min
			// enrichmentTTL. This is the window the bug destroyed.
			clock.advance(3 * time.Minute)

			// Second list refresh returns the case's version of the PR.
			second, _ := json.Marshal([]any{restPull(42, tc.state, tc.mergedAt, tc.updatedAt)})
			listBody = second
			if _, err := p.ListPullRequests(context.Background(), "alice", "repo", gh.PullRequestStateOpen); err != nil {
				t.Fatalf("second list: %v", err)
			}

			// Invariant: was the enrichment timestamp kept?
			ts := p.ExportEnrichTimestamp(key)
			switch {
			case tc.wantKept && ts.IsZero():
				t.Errorf("enrichAt[key] is zero after refresh, want preserved — %s", tc.why)
			case !tc.wantKept && !ts.IsZero():
				t.Errorf("enrichAt[key] = %v after refresh, want dropped — %s", ts, tc.why)
			}

			// Behaviour: does the next enrichment hit the wire?
			got, err := p.EnrichPullRequest(context.Background(), key)
			if err != nil {
				t.Fatalf("enrich after refresh: %v", err)
			}
			wantCalls := int32(2)
			if tc.wantKept {
				wantCalls = 1
			}
			if c := graphqlCount.Load(); c != wantCalls {
				t.Errorf("GraphQL calls = %d, want %d — %s", c, wantCalls, tc.why)
			}

			// Preserving the timestamp while zeroing the values would be a
			// worse bug than the one being fixed: the cache would report
			// fresh enrichment and serve empty fields. Assert the values.
			if tc.wantKept {
				if got.Mergeable != gh.MergeableStateMergeable {
					t.Errorf("mergeable = %q after refresh, want MERGEABLE preserved", got.Mergeable)
				}
				if string(got.StatusCheckRollup) != "SUCCESS" {
					t.Errorf("statusCheckRollup = %q after refresh, want SUCCESS preserved", got.StatusCheckRollup)
				}
				if len(got.Labels) == 0 {
					t.Errorf("labels empty after refresh, want the enriched labels preserved")
				}
			}

			// The REST-visible fields must always reflect the refresh.
			if got.State != gh.PullRequestStateMerged && tc.mergedAt != "" {
				t.Errorf("state = %q, want MERGED to come through from the refresh", got.State)
			}
		})
	}
}

// TestGetPullRequest_RefreshPreservesUnchangedEnrichment covers the same
// carry-forward on the single-PR path, which shares the defect: GetPullRequest
// also overwrote the cached entry with a REST-only payload.
func TestGetPullRequest_RefreshPreservesUnchangedEnrichment(t *testing.T) {
	const t1 = "2026-01-01T10:00:00Z"
	const t2 = "2026-01-01T11:30:00Z"

	cases := []struct {
		name      string
		updatedAt string
		wantKept  bool
	}{
		{name: "unchanged PR keeps enrichment", updatedAt: t1, wantKept: true},
		{name: "updated_at moved: enrichment discarded", updatedAt: t2, wantKept: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			clock := &fakeClock{t: now}

			first, _ := json.Marshal([]any{restPull(42, "open", "", t1)})
			listBody := first

			graphql := enrichResponse("MERGEABLE", "CLEAN", nil, "SUCCESS", "bug")
			p, graphqlCount := newListProvider(t, &listBody, graphql, clock.Now)

			key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 42}

			if _, err := p.ListPullRequests(context.Background(), "alice", "repo", gh.PullRequestStateOpen); err != nil {
				t.Fatalf("seed list: %v", err)
			}
			if _, err := p.EnrichPullRequest(context.Background(), key); err != nil {
				t.Fatalf("enrich: %v", err)
			}
			if c := graphqlCount.Load(); c != 1 {
				t.Fatalf("setup: expected 1 GraphQL call, got %d", c)
			}

			// Past the per-key CacheTTL, inside enrichmentTTL.
			clock.advance(3 * time.Minute)
			second, _ := json.Marshal([]any{restPull(42, "open", "", tc.updatedAt)})
			listBody = second

			if _, err := p.GetPullRequest(context.Background(), key); err != nil {
				t.Fatalf("get: %v", err)
			}

			ts := p.ExportEnrichTimestamp(key)
			if tc.wantKept && ts.IsZero() {
				t.Errorf("enrichAt[key] zero after GetPullRequest, want preserved")
			}
			if !tc.wantKept && !ts.IsZero() {
				t.Errorf("enrichAt[key] = %v after GetPullRequest, want dropped", ts)
			}

			got, err := p.EnrichPullRequest(context.Background(), key)
			if err != nil {
				t.Fatalf("enrich after get: %v", err)
			}
			wantCalls := int32(2)
			if tc.wantKept {
				wantCalls = 1
			}
			if c := graphqlCount.Load(); c != wantCalls {
				t.Errorf("GraphQL calls = %d, want %d", c, wantCalls)
			}
			if tc.wantKept && got.Mergeable != gh.MergeableStateMergeable {
				t.Errorf("mergeable = %q, want MERGEABLE preserved", got.Mergeable)
			}
		})
	}
}
