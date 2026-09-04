package resolvers_test

// Resolver tests for the review-surface fields added by #658, #651 and #607:
//
//	PullRequest.headRefOid            (#658)
//	PullRequest.reviews               (#651 — declared but resolved null before)
//	PullRequest.reviewThreads         (#607 / #661)
//	PullRequest.unresolvedThreadCount (#607 / #661)
//
// All four are enrichment-sourced, so these exercise the production path:
// the loaders middleware is installed, which routes every enrichment field
// through BatchEnrichPullRequests and therefore through the aliased batch
// query — not the single-PR query. The GitHub half of the wire is stubbed
// from testdata/pr_enrich_reviews_threads.json.
//
// No PII fixtures: the repo is `alice/repo`, users are `bob` and `carol`.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	gqlgen "github.com/drewdrewthis/orchardist/internal/server/graphql"
	"github.com/drewdrewthis/orchardist/internal/server/loaders"
	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
	"github.com/drewdrewthis/orchardist/internal/server/resolvers"
)

// reviewFixture loads the canned GitHub `pullRequest` block shared by the
// tests in this file. Returned as a decoded map so callers can splice it
// under whatever alias the batch query assigns.
func reviewFixture(t *testing.T) map[string]any {
	t.Helper()
	// The fixture lives with the gh provider — that package owns the GitHub
	// wire shape (R5). One copy, read from both sides, so it cannot drift.
	raw, err := os.ReadFile(filepath.Join("..", "providers", "gh", "testdata", "pr_enrich_reviews_threads.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return out
}

// reviewPullsREST renders the REST /pulls payload for n PRs numbered 42..42+n-1.
func reviewPullsREST(n int) string {
	items := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, map[string]any{
			"number":     42 + i,
			"title":      fmt.Sprintf("Add widget API %d", i),
			"body":       "",
			"state":      "open",
			"draft":      false,
			"html_url":   fmt.Sprintf("https://github.com/alice/repo/pull/%d", 42+i),
			"created_at": "2026-04-01T10:00:00Z",
			"updated_at": "2026-04-02T11:00:00Z",
			"merged_at":  nil,
			"user":       map[string]any{"login": "bob"},
			"base":       map[string]any{"ref": "main"},
			"head":       map[string]any{"ref": fmt.Sprintf("feature/widget-%d", i)},
		})
	}
	out, _ := json.Marshal(items)
	return string(out)
}

// stubReviewAPI serves the REST /pulls list for n PRs plus a GraphQL
// enrichment response in the aliased batch shape (`data.r0.pr0..prN-1`),
// every alias carrying the same fixture. The returned counter records how
// many GraphQL round-trips were made — the no-N+1 assertion.
func stubReviewAPI(t *testing.T, n int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	fixture := reviewFixture(t)
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

// newReviewDaemon stands up the daemon GraphQL handler wrapped in the
// loaders middleware, mirroring internal/server/server.go. Without the
// middleware the enrichment fields would take the single-PR fallback and
// the test would not cover the path production actually runs.
func newReviewDaemon(t *testing.T, p *gh.Provider) *httptest.Server {
	t.Helper()
	res := resolvers.New(time.Now()).WithGH(p)
	gqlSrv := handler.New(gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: res}))
	// Mirror server.go: install the per-operation enrichment memo (#813).
	gqlSrv.AroundOperations(resolvers.EnrichMemoMiddleware)
	gqlSrv.AddTransport(transport.POST{})
	mux := http.NewServeMux()
	mux.Handle("/graphql", loaders.Middleware(res.LoaderBundle(), gqlSrv))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newReviewProvider builds a gh.Provider pointed at the stub API server.
func newReviewProvider(t *testing.T, api *httptest.Server) *gh.Provider {
	t.Helper()
	provider := gh.NewWith(nil, api.URL, &gh.StaticAuthSource{TokenValue: "test-token-review"}, time.Now)
	if err := provider.Start(context.Background()); err != nil {
		t.Logf("provider start (non-fatal): %v", err)
	}
	gh.SetHTTPClientForTest(provider, api.Client())
	return provider
}

// TestPRReviewSurface_E2E asserts all four new fields resolve from one
// enrichment round-trip: the head sha (#658), the review list that used to
// come back null (#651), the review threads and the derived unresolved count
// (#607 / #661).
func TestPRReviewSurface_E2E(t *testing.T) {
	api, graphqlCalls := stubReviewAPI(t, 1)
	srv := newReviewDaemon(t, newReviewProvider(t, api))

	resp := postEnrichQuery(t, srv.URL, `query {
		pullRequests(repo: "alice/repo", state: OPEN) {
			number
			headRefOid
			unresolvedThreadCount
			reviews { id authorLogin state body submittedAt }
			reviewThreads { id isResolved isOutdated path commentCount authorLogin lastUpdatedAt }
		}
	}`)
	if errs, ok := resp["errors"]; ok {
		t.Fatalf("graphql errors: %v", errs)
	}

	data, _ := resp["data"].(map[string]any)
	prs, _ := data["pullRequests"].([]any)
	if len(prs) != 1 {
		t.Fatalf("expected 1 PR, got %d: %v", len(prs), prs)
	}
	pr, _ := prs[0].(map[string]any)

	// --- #658: headRefOid ---
	if got := pr["headRefOid"]; got != "d3adb33fd3adb33fd3adb33fd3adb33fd3adb33f" {
		t.Errorf("headRefOid = %v, want d3adb33f...", got)
	}

	// --- #651: reviews is a populated list, not null ---
	reviews, ok := pr["reviews"].([]any)
	if !ok {
		t.Fatalf("reviews = %v (%T), want a list — #651 regression", pr["reviews"], pr["reviews"])
	}
	if len(reviews) != 2 {
		t.Fatalf("len(reviews) = %d, want 2", len(reviews))
	}
	first, _ := reviews[0].(map[string]any)
	if got := first["authorLogin"]; got != "bob" {
		t.Errorf("reviews[0].authorLogin = %v, want bob", got)
	}
	if got := first["state"]; got != "COMMENTED" {
		t.Errorf("reviews[0].state = %v, want COMMENTED", got)
	}
	if got := first["submittedAt"]; got != "2026-04-02T10:23:41Z" {
		t.Errorf("reviews[0].submittedAt = %v, want 2026-04-02T10:23:41Z", got)
	}
	if got := first["id"]; got != "PullRequestReview:4336053435" {
		t.Errorf("reviews[0].id = %v, want PullRequestReview:4336053435", got)
	}
	second, _ := reviews[1].(map[string]any)
	if got := second["authorLogin"]; got != "carol" {
		t.Errorf("reviews[1].authorLogin = %v, want carol", got)
	}

	// --- #607: reviewThreads projection ---
	threads, ok := pr["reviewThreads"].([]any)
	if !ok {
		t.Fatalf("reviewThreads = %v (%T), want a list", pr["reviewThreads"], pr["reviewThreads"])
	}
	if len(threads) != 4 {
		t.Fatalf("len(reviewThreads) = %d, want 4 (all threads surface; only the count filters)", len(threads))
	}
	t0, _ := threads[0].(map[string]any)
	if got := t0["id"]; got != "ReviewThread:PRRT_kwDOblocking1" {
		t.Errorf("reviewThreads[0].id = %v, want ReviewThread:PRRT_kwDOblocking1", got)
	}
	if got := t0["isResolved"]; got != false {
		t.Errorf("reviewThreads[0].isResolved = %v, want false", got)
	}
	if got := t0["isOutdated"]; got != false {
		t.Errorf("reviewThreads[0].isOutdated = %v, want false", got)
	}
	if got := t0["path"]; got != "internal/server/providers/gh/provider.go" {
		t.Errorf("reviewThreads[0].path = %v, want internal/server/providers/gh/provider.go", got)
	}
	if got := t0["commentCount"]; got != float64(3) {
		t.Errorf("reviewThreads[0].commentCount = %v, want 3", got)
	}
	if got := t0["authorLogin"]; got != "bob" {
		t.Errorf("reviewThreads[0].authorLogin = %v, want bob (thread opener)", got)
	}
	if got := t0["lastUpdatedAt"]; got != "2026-04-02T12:47:02Z" {
		t.Errorf("reviewThreads[0].lastUpdatedAt = %v, want the newest comment's createdAt", got)
	}

	// --- #607: unresolvedThreadCount excludes resolved threads only ---
	// Fixture: 4 threads — unresolved, resolved, unresolved-but-outdated,
	// unresolved. GitHub's merge gate does not exempt outdated threads, so
	// three of the four still block merge; only the resolved one is excluded.
	if got := pr["unresolvedThreadCount"]; got != float64(3) {
		t.Errorf("unresolvedThreadCount = %v, want 3 (only resolved threads excluded)", got)
	}

	// One enrichment round-trip covers all four fields.
	if got := graphqlCalls.Load(); got != 1 {
		t.Errorf("GitHub GraphQL calls = %d, want 1", got)
	}
}

// TestPRReviewSurface_NoNPlusOne asserts the review surface stays on one
// GitHub round-trip as the PR count grows: three PRs selecting every new
// field must still batch into a single GraphQL request.
func TestPRReviewSurface_NoNPlusOne(t *testing.T) {
	api, graphqlCalls := stubReviewAPI(t, 3)
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
		if got := pr["unresolvedThreadCount"]; got != float64(3) {
			t.Errorf("pr[%d].unresolvedThreadCount = %v, want 3", i, got)
		}
	}
	if got := graphqlCalls.Load(); got != 1 {
		t.Errorf("GitHub GraphQL calls = %d for 3 PRs × 5 fields, want 1 (no N+1)", got)
	}
}

// TestPRReviewSurface_EmptyConnections asserts the empty case resolves to
// empty lists and a zero count rather than null — the merge gate must be
// able to read `unresolvedThreadCount == 0` on a PR nobody has reviewed.
func TestPRReviewSurface_EmptyConnections(t *testing.T) {
	empty := map[string]any{
		"headRefOid":       "",
		"mergeable":        "MERGEABLE",
		"mergeStateStatus": "CLEAN",
		"reviewDecision":   nil,
		"labels":           map[string]any{"nodes": []any{}},
		"commits":          map[string]any{"nodes": []any{}},
		"reviews":          map[string]any{"nodes": []any{}},
		"reviewThreads":    map[string]any{"nodes": []any{}},
	}
	body, _ := json.Marshal(map[string]any{
		"data": map[string]any{"r0": map[string]any{"pr0": empty}},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/alice/repo/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, reviewPullsREST(1))
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	api := httptest.NewTLSServer(mux)
	t.Cleanup(api.Close)

	srv := newReviewDaemon(t, newReviewProvider(t, api))
	resp := postEnrichQuery(t, srv.URL, `query {
		pullRequests(repo: "alice/repo", state: OPEN) {
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
	pr, _ := prs[0].(map[string]any)

	if got := pr["headRefOid"]; got != "" {
		t.Errorf("headRefOid = %v, want empty string when GitHub omits it", got)
	}
	if got := pr["unresolvedThreadCount"]; got != float64(0) {
		t.Errorf("unresolvedThreadCount = %v, want 0", got)
	}
	threads, ok := pr["reviewThreads"].([]any)
	if !ok || len(threads) != 0 {
		t.Errorf("reviewThreads = %v, want []", pr["reviewThreads"])
	}
	reviews, ok := pr["reviews"].([]any)
	if !ok || len(reviews) != 0 {
		t.Errorf("reviews = %v, want []", pr["reviews"])
	}
}
