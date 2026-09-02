package gh_test

// Provider-level tests for the review surface added by #658, #651 and #607:
// headRefOid, reviews, reviewThreads and the derived unresolved-thread count.
//
// The resolver-boundary coverage lives in
// internal/server/resolvers/pr_reviews_threads_e2e_test.go. These tests pin
// the provider contract underneath it: the single-PR enrichment mapping, and
// the carry-forward invariant that a REST refresh must not blank the new
// GraphQL-only fields.
//
// No PII fixtures: the repo is `alice/repo`, users are `bob` and `carol`.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
)

// reviewFixturePath is the canned GitHub `pullRequest` block shared with the
// resolver-boundary test. It lives under this package because the gh provider
// owns the GitHub wire shape (R5 anti-corruption layer).
const reviewFixturePath = "testdata/pr_enrich_reviews_threads.json"

// singlePREnrichBody wraps the shared fixture in the single-PR response
// envelope (`data.repository.pullRequest`) that enrichPRQuery expects.
func singlePREnrichBody(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(reviewFixturePath))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var pr map[string]any
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	out, err := json.Marshal(map[string]any{
		"data": map[string]any{"repository": map[string]any{"pullRequest": pr}},
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return out
}

// newReviewRefreshProvider stands up a provider whose stub serves both the
// REST /pulls list and the GraphQL enrichment endpoint, so a test can
// interleave a list refresh with an enrichment.
func newReviewRefreshProvider(t *testing.T, graphqlBody []byte, clock func() time.Time) *gh.Provider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(graphqlBody)
	})
	mux.HandleFunc("/repos/alice/repo/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, canonPullsBody)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
		fmt.Fprintf(w, `{"error":"unexpected path %s"}`, r.URL.Path)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	p := gh.NewWith(nil, srv.URL, &gh.StaticAuthSource{TokenValue: "test-token-fixture"}, clock)
	if err := p.Start(context.Background()); err != nil {
		t.Logf("provider start (non-fatal): %v", err)
	}
	gh.SetHTTPClientForTest(p, srv.Client())
	return p
}

// TestEnrichPullRequest_MapsReviewSurface asserts the single-PR enrichment
// path maps every new field: head sha, reviews, threads, and the derived
// count that filters resolved and outdated threads.
func TestEnrichPullRequest_MapsReviewSurface(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 4, 2, 14, 0, 0, 0, time.UTC)}
	p, _ := newEnrichProvider(t, singlePREnrichBody(t), clock.Now)

	key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 42}
	pr, err := p.EnrichPullRequest(context.Background(), key)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}

	if got, want := pr.HeadRefOid, "d3adb33fd3adb33fd3adb33fd3adb33fd3adb33f"; got != want {
		t.Errorf("HeadRefOid = %q, want %q", got, want)
	}

	if len(pr.Reviews) != 2 {
		t.Fatalf("len(Reviews) = %d, want 2", len(pr.Reviews))
	}
	if got, want := pr.Reviews[0].AuthorLogin, "bob"; got != want {
		t.Errorf("Reviews[0].AuthorLogin = %q, want %q", got, want)
	}
	if got, want := pr.Reviews[0].NodeID(), "PullRequestReview:4336053435"; got != want {
		t.Errorf("Reviews[0].NodeID() = %q, want %q", got, want)
	}
	if got, want := pr.Reviews[1].State, "CHANGES_REQUESTED"; got != want {
		t.Errorf("Reviews[1].State = %q, want %q", got, want)
	}

	if len(pr.ReviewThreads) != 4 {
		t.Fatalf("len(ReviewThreads) = %d, want 4", len(pr.ReviewThreads))
	}
	first := pr.ReviewThreads[0]
	if first.IsResolved || first.IsOutdated {
		t.Errorf("ReviewThreads[0] resolved=%v outdated=%v, want both false", first.IsResolved, first.IsOutdated)
	}
	if got, want := first.CommentCount, 3; got != want {
		t.Errorf("ReviewThreads[0].CommentCount = %d, want %d", got, want)
	}
	if got, want := first.AuthorLogin, "bob"; got != want {
		t.Errorf("ReviewThreads[0].AuthorLogin = %q, want %q (thread opener)", got, want)
	}
	if got, want := first.LastUpdatedAt, "2026-04-02T12:47:02Z"; got != want {
		t.Errorf("ReviewThreads[0].LastUpdatedAt = %q, want %q", got, want)
	}
	if got, want := first.NodeID(), "ReviewThread:PRRT_kwDOblocking1"; got != want {
		t.Errorf("ReviewThreads[0].NodeID() = %q, want %q", got, want)
	}

	// Thread 2 is resolved, thread 3 is outdated — neither blocks merge.
	if got, want := pr.UnresolvedThreadCount(), 2; got != want {
		t.Errorf("UnresolvedThreadCount() = %d, want %d", got, want)
	}
}

// TestListPullRequests_CarriesReviewSurfaceForward asserts that a REST list
// refresh over an unchanged PR keeps the GraphQL-only review fields. REST
// payloads never carry them, so a refresh that overwrote the cache entry
// wholesale would blank them while enrichAt still claimed the entry fresh.
func TestListPullRequests_CarriesReviewSurfaceForward(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 4, 2, 14, 0, 0, 0, time.UTC)}
	p := newReviewRefreshProvider(t, singlePREnrichBody(t), clock.Now)
	ctx := context.Background()
	key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 42}

	// Seed the per-key cache from REST so the enrichment merges onto a real
	// base entry (updatedAt populated) — otherwise the next refresh reads as
	// a change and legitimately drops the enrichment.
	if _, err := p.ListPullRequests(ctx, "alice", "repo", gh.PullRequestStateOpen); err != nil {
		t.Fatalf("seed list: %v", err)
	}
	enriched, err := p.EnrichPullRequest(ctx, key)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(enriched.ReviewThreads) == 0 || enriched.HeadRefOid == "" || len(enriched.Reviews) == 0 {
		t.Fatalf("precondition: enrichment did not populate the review surface: %+v", enriched)
	}

	// Past the REST TTL but inside the enrichment TTL: the list refetches
	// while enrichAt still marks the entry enriched. This is the window in
	// which a wholesale overwrite silently blanks the review surface.
	clock.advance(3 * time.Minute)
	if _, err := p.ListPullRequests(ctx, "alice", "repo", gh.PullRequestStateOpen); err != nil {
		t.Fatalf("refresh list: %v", err)
	}

	after, err := p.GetPullRequest(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got, want := after.HeadRefOid, enriched.HeadRefOid; got != want {
		t.Errorf("HeadRefOid after REST refresh = %q, want %q", got, want)
	}
	if got, want := len(after.Reviews), len(enriched.Reviews); got != want {
		t.Errorf("len(Reviews) after REST refresh = %d, want %d", got, want)
	}
	if got, want := len(after.ReviewThreads), len(enriched.ReviewThreads); got != want {
		t.Errorf("len(ReviewThreads) after REST refresh = %d, want %d", got, want)
	}
	if got, want := after.UnresolvedThreadCount(), 2; got != want {
		t.Errorf("UnresolvedThreadCount() after REST refresh = %d, want %d", got, want)
	}
}
