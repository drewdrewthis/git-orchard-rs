package gh_test

// Tests for the request-scoped enrichment memo (graphql_enrich_memo.go, #813).
//
// The memo collapses repeated enrichment lookups for the same PR key within a
// single GraphQL operation into one GitHub round-trip — even for open PRs with
// mergeable == UNKNOWN, which shouldCacheEnrichment refuses to persist in the
// durable cache (#367). It must be strictly request-scoped: a fresh operation
// context re-fetches, so a value that flipped on GitHub is observed.
//
// No PII fixtures: repo is `alice/repo`.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
)

// swappableBody is a mutex-guarded GraphQL response body a stub can flip
// between calls — used to prove the memo does not harden a stale value across
// operations.
type swappableBody struct {
	mu   sync.Mutex
	body []byte
}

func (s *swappableBody) set(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = b
}

func (s *swappableBody) get() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body
}

// newMemoEnrichProvider builds a gh.Provider pointed at a TLS stub whose
// /graphql body can be swapped mid-test. The atomic counts GraphQL round-trips.
func newMemoEnrichProvider(t *testing.T, initial []byte) (*gh.Provider, *atomic.Int32, *swappableBody) {
	t.Helper()
	var count atomic.Int32
	sb := &swappableBody{body: initial}

	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(sb.get())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
		http.NotFound(w, r)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	auth := &gh.StaticAuthSource{TokenValue: "test-token-memo"}
	p := gh.NewWith(nil, srv.URL, auth, time.Now)
	if err := p.Start(context.Background()); err != nil {
		t.Logf("provider start (non-fatal): %v", err)
	}
	gh.SetHTTPClientForTest(p, srv.Client())
	return p, &count, sb
}

// TestEnrichMemo_HitAvoidsSecondFetch asserts that a second EnrichPullRequest
// for the same open+UNKNOWN key within one memo-bearing context serves from
// the memo — the durable cache would have re-fetched (#367 declines to cache
// UNKNOWN), so the memo is what prevents the hidden second round-trip (#813).
func TestEnrichMemo_HitAvoidsSecondFetch(t *testing.T) {
	body := enrichResponse("UNKNOWN", "UNKNOWN", nil, "")
	p, count, _ := newMemoEnrichProvider(t, body)
	key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 7}

	ctx := gh.WithEnrichMemo(context.Background())

	pr1, err := p.EnrichPullRequest(ctx, key)
	if err != nil {
		t.Fatalf("first enrich: %v", err)
	}
	if pr1.Mergeable != gh.MergeableStateUnknown {
		t.Errorf("mergeable = %q, want UNKNOWN", pr1.Mergeable)
	}
	if c := count.Load(); c != 1 {
		t.Fatalf("after first enrich: GraphQL calls = %d, want 1", c)
	}

	// UNKNOWN must not be in the durable cache — the memo, not the cache, is
	// what serves the second call.
	if at := p.ExportEnrichTimestamp(key); !at.IsZero() {
		t.Fatalf("open+UNKNOWN was written to the durable cache (enrichAt=%v); memo test is not exercising the #813 path", at)
	}

	pr2, err := p.EnrichPullRequest(ctx, key)
	if err != nil {
		t.Fatalf("second enrich: %v", err)
	}
	if c := count.Load(); c != 1 {
		t.Fatalf("after second enrich (same ctx): GraphQL calls = %d, want 1 (memo hit, #813)", c)
	}
	if pr2.Mergeable != pr1.Mergeable {
		t.Errorf("memoized result differs: %q vs %q", pr2.Mergeable, pr1.Mergeable)
	}
}

// TestEnrichMemo_FreshContextRefetches asserts the memo is request-scoped: a
// new operation context (new memo) re-fetches the open+UNKNOWN key rather than
// serving a global cache.
func TestEnrichMemo_FreshContextRefetches(t *testing.T) {
	body := enrichResponse("UNKNOWN", "UNKNOWN", nil, "")
	p, count, _ := newMemoEnrichProvider(t, body)
	key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 7}

	if _, err := p.EnrichPullRequest(gh.WithEnrichMemo(context.Background()), key); err != nil {
		t.Fatalf("first enrich: %v", err)
	}
	if c := count.Load(); c != 1 {
		t.Fatalf("after first operation: GraphQL calls = %d, want 1", c)
	}

	// Fresh operation context — the previous memo is gone.
	if _, err := p.EnrichPullRequest(gh.WithEnrichMemo(context.Background()), key); err != nil {
		t.Fatalf("second enrich: %v", err)
	}
	if c := count.Load(); c != 2 {
		t.Fatalf("after second operation (fresh ctx): GraphQL calls = %d, want 2 (request-scoped, #813)", c)
	}
}

// TestEnrichMemo_FreshContextSeesFlip asserts the #367 staleness contract:
// after the fixture flips UNKNOWN → MERGEABLE, a fresh operation context
// returns MERGEABLE. The memo must not harden the transient UNKNOWN across
// operations.
func TestEnrichMemo_FreshContextSeesFlip(t *testing.T) {
	p, _, sb := newMemoEnrichProvider(t, enrichResponse("UNKNOWN", "UNKNOWN", nil, ""))
	key := gh.PullRequestKey{Owner: "alice", Name: "repo", Number: 7}

	pr1, err := p.EnrichPullRequest(gh.WithEnrichMemo(context.Background()), key)
	if err != nil {
		t.Fatalf("first enrich: %v", err)
	}
	if pr1.Mergeable != gh.MergeableStateUnknown {
		t.Fatalf("first response mergeable = %q, want UNKNOWN", pr1.Mergeable)
	}

	// GitHub finished computing mergeability.
	sb.set(enrichResponse("MERGEABLE", "CLEAN", nil, "SUCCESS"))

	pr2, err := p.EnrichPullRequest(gh.WithEnrichMemo(context.Background()), key)
	if err != nil {
		t.Fatalf("second enrich: %v", err)
	}
	if pr2.Mergeable != gh.MergeableStateMergeable {
		t.Errorf("second response mergeable = %q, want MERGEABLE (memo must not harden UNKNOWN, #367/#813)", pr2.Mergeable)
	}
}
